package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Assurance serves the Assurance rules window of the Identity Continuity
// tab, for one IdentityContinuity: its default rule (spec.assurancePolicy),
// each IdP and what its sign-ins prove, its rules (WorkloadProfiles), the
// gateway policies that take its broker's tokens and which rule each asks
// the assurance gate for, and what relies on the broker with no rule. What
// the rules decide comes from the gate itself (its evaluate API), never
// from here. Writes go through as the signed-in admin.
type Assurance struct {
	k     *Kube
	res   *Resources
	index func() *Index
	http  *http.Client
	// gateURL: where to reach the gate's evaluate port instead of its
	// Service (local development, through a port-forward)
	gateURL string
	// writer: the client writes go through, as the signed-in admin
	writer func(*http.Request) (dynamic.Interface, error)
}

func (a *Assurance) client(r *http.Request) (dynamic.Interface, error) {
	if a.writer != nil {
		return a.writer(r)
	}
	return a.res.client(r)
}

var (
	gvrWLP      = schema.GroupVersionResource{Group: "continuity.lab.solo.io", Version: "v1alpha1", Resource: "workloadprofiles"}
	gvrCRD      = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	gvrRefGrant = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1beta1", Resource: "referencegrants"}
	gvrAuthz    = schema.GroupVersionResource{Group: "security.istio.io", Version: "v1", Resource: "authorizationpolicies"}
)

const (
	// gateLabel marks a Service as an assurance gate, whatever it's named:
	// its port grpc is what policy points ask, its port evaluate is what the
	// Observatory asks.
	gateLabel = "continuity.lab.solo.io/assurance-gate"
	// callerLabel marks the grants the Observatory adds beside the gate when
	// an admin turns enforcement on at a gateway policy.
	callerLabel = "continuity.lab.solo.io/assurance-gate-caller"
)

type AssuranceView struct {
	Namespace       string         `json:"namespace"`
	Name            string         `json:"name"`
	ResourceVersion string         `json:"resourceVersion"`
	Issuer          string         `json:"issuer,omitempty"` // the broker's
	Realm           string         `json:"realm,omitempty"`
	Active          string         `json:"active,omitempty"`
	Schema          *RuleSchema    `json:"schema,omitempty"`
	SchemaError     string         `json:"schemaError,omitempty"`
	Gate            *GateRef       `json:"gate,omitempty"`
	Policy          map[string]any `json:"policy"`
	IdPs            []AssuranceIdP `json:"idps"`
	Rules           []Rule         `json:"rules"`
	PolicyPoints    []PolicyPoint  `json:"policyPoints"`
	Uncovered       []Uncovered    `json:"uncovered"`
	Options         Options        `json:"options"`
}

type AssuranceIdP struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"displayName,omitempty"`
	Type        string         `json:"type"`
	Enabled     bool           `json:"enabled"`
	Healthy     bool           `json:"healthy"`
	Assurance   map[string]any `json:"assurance,omitempty"`
	CheckedAt   string         `json:"checkedAt,omitempty"`
	Checks      []any          `json:"checks,omitempty"`
}

// Rule is a WorkloadProfile following the chain.
type Rule struct {
	Name            string         `json:"name"`
	ResourceVersion string         `json:"resourceVersion"`
	Spec            map[string]any `json:"spec"`
	Status          map[string]any `json:"status,omitempty"`
}

// PolicyPoint is a gateway policy that takes the broker's tokens, or asks
// the gate for one of the chain's rules.
type PolicyPoint struct {
	APIVersion      string `json:"apiVersion"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	ResourceVersion string `json:"resourceVersion"`
	Gateway         string `json:"gateway,omitempty"` // the gateway it applies at, as the graph names it
	// Rule: the rule it asks the gate for ("" the chain's default rule); null
	// when it doesn't ask the gate.
	Rule        *string `json:"rule"`
	FailureMode string  `json:"failureMode,omitempty"`
	// ExtAuth: where its extAuth goes, when that isn't a gate (the gate
	// can't be added beside it).
	ExtAuth   string `json:"extAuth,omitempty"`
	Workloads []any  `json:"workloads,omitempty"`
}

// Uncovered is something that relies on the broker that no rule covers: an
// app that signs people in through it (source sso), or a gateway policy
// that takes its tokens without asking the gate (source policyPoint).
type Uncovered struct {
	Name      string   `json:"name"` // a name for its rule
	Label     string   `json:"label"`
	Source    string   `json:"source"`
	Ref       string   `json:"ref"` // the SSO extension or the policy, namespace/name
	Hosts     []string `json:"hosts,omitempty"`
	Workloads []any    `json:"workloads,omitempty"`
	Clients   []string `json:"clients,omitempty"`
}

// Options: what a rule can name, as the cluster has it.
type Options struct {
	Workloads []string `json:"workloads"` // namespace/serviceAccount, of the mesh's identities
	Clients   []string `json:"clients"`   // broker clients seen in use
}

type GateRef struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	GRPCPort     int64  `json:"grpcPort"`
	EvaluatePort int64  `json:"evaluatePort"`
	selector     map[string]any
	grpcTarget   any
	override     string
}

// RuleSchema: the choices a rule's fields take and their defaults, as the
// installed CRDs define them.
type RuleSchema struct {
	Criticality []string `json:"criticality"`
	Levels      []string `json:"levels"`
	Sessions    []string `json:"sessions"`
	Modes       []string `json:"modes"`
	Defaults    struct {
		Mode     string `json:"mode"`
		Minimum  string `json:"minimum"`
		Sessions string `json:"sessions"`
	} `json:"defaults"`
}

func (a *Assurance) View(ctx context.Context, ic *unstructured.Unstructured) AssuranceView {
	ix := a.index()
	v := AssuranceView{Namespace: ic.GetNamespace(), Name: ic.GetName(), ResourceVersion: ic.GetResourceVersion(),
		Issuer: str(ic.Object, "status", "broker", "issuer"), Realm: str(ic.Object, "spec", "broker", "keycloak", "realm"),
		Active: str(ic.Object, "status", "active"), Policy: obj(ic.Object, "spec", "assurancePolicy"),
		IdPs: []AssuranceIdP{}, Rules: []Rule{}, PolicyPoints: []PolicyPoint{}, Uncovered: []Uncovered{},
		Options: Options{Workloads: []string{}, Clients: []string{}}}
	if v.Policy == nil {
		v.Policy = map[string]any{}
	}
	if s, err := a.schema(ctx); err != nil {
		v.SchemaError = err.Error()
	} else {
		v.Schema = s
	}
	v.Gate = a.gate(ic.GetNamespace())

	status := map[string]map[string]any{}
	for _, s := range slice(ic.Object, "status", "tiers") {
		sm, _ := s.(map[string]any)
		status[str(sm, "name")] = sm
	}
	for _, t := range slice(ic.Object, "spec", "tiers") {
		tm, _ := t.(map[string]any)
		idp := AssuranceIdP{Name: str(tm, "name"), DisplayName: str(tm, "displayName"), Type: str(tm, "type"), Enabled: tm["enabled"] != false,
			Assurance: obj(tm, "assurance")}
		if st := status[idp.Name]; st != nil {
			idp.Healthy, _ = st["healthy"].(bool)
			idp.CheckedAt = str(st, "trust", "checkedAt")
			idp.Checks = slice(st, "trust", "checks")
		}
		v.IdPs = append(v.IdPs, idp)
	}

	rules := map[string]bool{}
	covered := map[string]bool{} // namespace/serviceAccount a rule covers
	clients := map[string]bool{}
	for _, p := range a.k.List("workloadprofiles") {
		if p.GetNamespace() != ic.GetNamespace() || str(p.Object, "spec", "continuity") != ic.GetName() {
			continue
		}
		rules[p.GetName()] = true
		v.Rules = append(v.Rules, Rule{Name: p.GetName(), ResourceVersion: p.GetResourceVersion(), Spec: obj(p.Object, "spec"), Status: obj(p.Object, "status")})
		for _, w := range slice(p.Object, "spec", "workloads") {
			wm, _ := w.(map[string]any)
			covered[str(wm, "namespace")+"/"+str(wm, "serviceAccount")] = true
		}
		for _, c := range slice(p.Object, "spec", "clients") {
			clients[fmt.Sprint(c)] = true
		}
		for _, c := range slice(p.Object, "status", "clients") {
			cm, _ := c.(map[string]any)
			if cm["found"] == true {
				clients[str(cm, "clientID")] = true
			}
		}
	}
	sort.Slice(v.Rules, func(i, j int) bool { return v.Rules[i].Name < v.Rules[j].Name })

	issuer := strings.TrimSuffix(v.Issuer, "/")
	gates := a.gates()
	seen := map[string]bool{}
	uncovered := func(u Uncovered) {
		if u.Name != "" && !seen[u.Name] {
			seen[u.Name] = true
			v.Uncovered = append(v.Uncovered, u)
		}
	}
	for _, pol := range a.policies() {
		pp := PolicyPoint{APIVersion: pol.GetAPIVersion(), Kind: pol.GetKind(), Namespace: pol.GetNamespace(), Name: pol.GetName(),
			ResourceVersion: pol.GetResourceVersion()}
		gw := policyGateway(a.k, pol, ix)
		pp.Gateway, pp.Workloads = labelOf(ix, gw), workloadsOf(ix, gw)
		if rule, chain, asks, other := askedRule(pol, gates); other != "" {
			pp.ExtAuth = other
		} else if asks {
			if rule == ic.GetName()+defaultRuleSuffix && !rules[rule] {
				rule, chain = "", ic.GetName()
			}
			if (rule != "" && !rules[rule]) || (rule == "" && chain != "" && chain != ic.GetName()) {
				continue // another chain's rule
			}
			pp.Rule, pp.FailureMode = &rule, failureMode(pol)
		}
		if pp.Rule == nil && (issuer == "" || !trustsIssuer(pol, issuer)) {
			continue
		}
		v.PolicyPoints = append(v.PolicyPoints, pp)
		if pp.Rule == nil && pp.ExtAuth == "" && !anyCovered(pp.Workloads, covered) {
			uncovered(Uncovered{Name: dnsName(pol.GetName()), Label: pp.Gateway, Source: "policyPoint", Ref: key(pol), Workloads: pp.Workloads})
		}
	}
	for _, s := range ix.sso {
		if issuer == "" || strings.TrimSuffix(s.Issuer, "/") != issuer {
			continue
		}
		cs := ssoClients(a.k, s.Name)
		for _, c := range cs {
			clients[c] = true
		}
		for _, app := range s.Apps {
			ws := workloadsOf(ix, app)
			if len(ws) == 0 || anyCovered(ws, covered) {
				continue
			}
			uncovered(Uncovered{Name: dnsName(labelOf(ix, app)), Label: labelOf(ix, app), Source: "sso", Ref: s.Name, Hosts: s.Hosts, Workloads: ws, Clients: cs})
		}
	}
	for spiffe := range ix.bySA {
		if m := saRe.FindStringSubmatch(spiffe); m != nil {
			v.Options.Workloads = append(v.Options.Workloads, m[1]+"/"+m[2])
		}
	}
	sort.Strings(v.Options.Workloads)
	for c := range clients {
		v.Options.Clients = append(v.Options.Clients, c)
	}
	sort.Strings(v.Options.Clients)
	return v
}

func (a *Assurance) policies() []*unstructured.Unstructured {
	return append(a.k.List("agentgatewaypolicies"), a.k.List("enterpriseagentgatewaypolicies")...)
}

func trustsIssuer(pol *unstructured.Unstructured, issuer string) bool {
	for _, p := range slice(pol.Object, "spec", "traffic", "jwtAuthentication", "providers") {
		pm, _ := p.(map[string]any)
		if strings.TrimSuffix(str(pm, "issuer"), "/") == issuer {
			return true
		}
	}
	return false
}

func backendKey(pol *unstructured.Unstructured, ext map[string]any) string {
	ns := str(ext, "backendRef", "namespace")
	if ns == "" {
		ns = pol.GetNamespace()
	}
	return ns + "/" + str(ext, "backendRef", "name")
}

// gates: the assurance gates' Services, as namespace/name.
func (a *Assurance) gates() map[string]bool {
	out := map[string]bool{}
	for _, s := range a.k.List("services") {
		if _, ok := s.GetLabels()[gateLabel]; ok {
			out[key(s)] = true
		}
	}
	return out
}

// gate: the assurance gate serving a chain's namespace (it reads the rules
// there).
func (a *Assurance) gate(ns string) *GateRef {
	for _, s := range a.k.List("services") {
		if _, ok := s.GetLabels()[gateLabel]; !ok || s.GetNamespace() != ns {
			continue
		}
		g := &GateRef{Namespace: s.GetNamespace(), Name: s.GetName(), selector: obj(s.Object, "spec", "selector"), override: a.gateURL}
		for _, p := range slice(s.Object, "spec", "ports") {
			pm, _ := p.(map[string]any)
			port, _ := pm["port"].(int64)
			switch str(pm, "name") {
			case "grpc":
				g.GRPCPort, g.grpcTarget = port, pm["targetPort"]
			case "evaluate":
				g.EvaluatePort = port
			}
		}
		return g
	}
	return nil
}

// An Enterprise policy asks Solo's ext-auth service for a rule through the
// AuthConfig the continuity controller writes for it: assurance-<rule>, the
// chain's default rule assurance-<chain>-default.
const (
	authConfigPrefix  = "assurance-"
	defaultRuleSuffix = "-default"
)

// askedRule: the rule a policy asks for (and the chain, when it names one),
// through the gate (extAuth) or Solo's ext-auth service (entExtAuth); other:
// where its ext-auth goes when that's something else.
func askedRule(pol *unstructured.Unstructured, gates map[string]bool) (rule, chain string, asks bool, other string) {
	if ext := obj(pol.Object, "spec", "traffic", "extAuth"); ext != nil {
		if !asksGate(pol, gates) {
			return "", "", false, backendKey(pol, ext)
		}
		return str(ext, "grpc", "contextExtensions", "profile"), str(ext, "grpc", "contextExtensions", "continuity"), true, ""
	}
	if ent := obj(pol.Object, "spec", "traffic", "entExtAuth"); ent != nil {
		ref := str(ent, "authConfigRef", "name")
		if ref == "" || !strings.HasPrefix(ref, authConfigPrefix) {
			if ref == "" {
				return "", "", false, backendKey(pol, ent)
			}
			ns := str(ent, "authConfigRef", "namespace")
			if ns == "" {
				ns = pol.GetNamespace()
			}
			return "", "", false, "AuthConfig " + ns + "/" + ref
		}
		return strings.TrimPrefix(ref, authConfigPrefix), "", true, ""
	}
	return "", "", false, ""
}

// failureMode: what a policy asking for a rule does when nothing answers;
// Solo's ext-auth service always fails closed.
func failureMode(pol *unstructured.Unstructured) string {
	if m := str(pol.Object, "spec", "traffic", "extAuth", "failureMode"); m != "" {
		return m
	}
	return "FailClosed"
}

// asksGate: the policy's extAuth is one of the gates (a backendRef without a
// namespace is in the policy's own).
func asksGate(pol *unstructured.Unstructured, gates map[string]bool) bool {
	ext := obj(pol.Object, "spec", "traffic", "extAuth")
	if kind := str(ext, "backendRef", "kind"); ext == nil || (kind != "" && kind != "Service") {
		return false
	}
	return gates[backendKey(pol, ext)]
}

// schema: the rules' choices and defaults, from the installed CRDs.
func (a *Assurance) schema(ctx context.Context) (*RuleSchema, error) {
	if a.k.dyn == nil {
		return nil, fmt.Errorf("no API client")
	}
	get := func(name string) (map[string]any, error) {
		u, err := a.k.dyn.Resource(gvrCRD).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading the %s CRD: %w", name, err)
		}
		return u.Object, nil
	}
	wlp, err := get("workloadprofiles.continuity.lab.solo.io")
	if err != nil {
		return nil, err
	}
	icp, err := get("identitycontinuities.continuity.lab.solo.io")
	if err != nil {
		return nil, err
	}
	return schemaFrom(wlp, icp)
}

// schemaFrom: the choices and defaults in the WorkloadProfile and
// IdentityContinuity CRDs' stored versions.
func schemaFrom(wlpCRD, icCRD map[string]any) (*RuleSchema, error) {
	spec := func(crd map[string]any) map[string]any {
		for _, ver := range slice(crd, "spec", "versions") {
			vm, _ := ver.(map[string]any)
			if vm["storage"] == true {
				return obj(vm, "schema", "openAPIV3Schema", "properties", "spec", "properties")
			}
		}
		return nil
	}
	wlp, icp := spec(wlpCRD), spec(icCRD)
	if wlp == nil || icp == nil {
		return nil, fmt.Errorf("the continuity CRDs have no stored version with a schema")
	}
	enum := func(m map[string]any, path ...string) []string {
		out := []string{}
		for _, e := range slice(m, append(path, "enum")...) {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	s := &RuleSchema{Criticality: enum(wlp, "criticality"), Levels: enum(wlp, "assurance", "properties", "minimum"),
		Sessions: enum(wlp, "sessions"), Modes: enum(wlp, "mode")}
	s.Defaults.Mode = str(wlp, "mode", "default")
	s.Defaults.Minimum = str(icp, "assurancePolicy", "properties", "minimum", "default")
	s.Defaults.Sessions = str(icp, "assurancePolicy", "properties", "sessions", "default")
	return s, nil
}

// ssoClients: the OAuth client an edge SSO extension signs people in as.
func ssoClients(k *Kube, ext string) []string {
	for _, e := range k.List("gatewayextensions") {
		if key(e) == ext {
			if c := str(e.Object, "spec", "oauth2", "credentials", "clientID"); c != "" {
				return []string{c}
			}
			if c := str(e.Object, "spec", "oauth2", "clientID"); c != "" {
				return []string{c}
			}
		}
	}
	return nil
}

// policyGateway: the gateway node a policy applies at (its Gateway, or its
// HTTPRoute's parent).
func policyGateway(k *Kube, pol *unstructured.Unstructured, ix *Index) string {
	for _, t := range slice(pol.Object, "spec", "targetRefs") {
		tm, _ := t.(map[string]any)
		switch str(tm, "kind") {
		case "Gateway":
			return ix.gateway(pol.GetNamespace() + "/" + str(tm, "name"))
		case "HTTPRoute":
			for _, r := range k.List("httproutes") {
				if r.GetNamespace() != pol.GetNamespace() || r.GetName() != str(tm, "name") {
					continue
				}
				for _, pr := range slice(r.Object, "spec", "parentRefs") {
					prm, _ := pr.(map[string]any)
					ns := str(prm, "namespace")
					if ns == "" {
						ns = r.GetNamespace()
					}
					if gw := ix.gateway(ns + "/" + str(prm, "name")); gw != "" {
						return gw
					}
				}
			}
		}
	}
	return ""
}

var saRe = regexp.MustCompile(`/ns/([^/]+)/sa/([^/]+)$`)

// workloadsOf: a node's mesh identities, as workload references.
func workloadsOf(ix *Index, id string) []any {
	n := ix.nodes[id]
	if n == nil {
		return nil
	}
	var out []any
	for _, spiffe := range n.Identity {
		if m := saRe.FindStringSubmatch(spiffe); m != nil {
			out = append(out, map[string]any{"namespace": m[1], "serviceAccount": m[2]})
		}
	}
	return out
}

func anyCovered(ws []any, covered map[string]bool) bool {
	for _, w := range ws {
		wm, _ := w.(map[string]any)
		if covered[str(wm, "namespace")+"/"+str(wm, "serviceAccount")] {
			return true
		}
	}
	return false
}

func labelOf(ix *Index, id string) string {
	if n := ix.nodes[id]; n != nil {
		return n.Label
	}
	return id
}

var notDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// dnsName: a Kubernetes name from a label.
func dnsName(s string) string {
	s = strings.Trim(notDNS.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

func (a *Assurance) instance(ns, name string) *unstructured.Unstructured {
	for _, ic := range a.k.List("identitycontinuities") {
		if ic.GetNamespace() == ns && ic.GetName() == name {
			return ic
		}
	}
	return nil
}

// Get (GET /api/assurance/{ns}/{name}): one IdentityContinuity's view.
func (a *Assurance) Get(w http.ResponseWriter, r *http.Request) {
	ic := a.instance(r.PathValue("ns"), r.PathValue("name"))
	if ic == nil {
		http.Error(w, "no such IdentityContinuity", http.StatusNotFound)
		return
	}
	writeJSON(w, a.View(r.Context(), ic))
}

// Evaluate (POST /api/assurance/{ns}/{name}/evaluate): what the chain's
// rules decide, as the assurance gate answers it (its evaluate API), for
// the rules as saved or with the body's draft over them, and optionally one
// session (what if).
func (a *Assurance) Evaluate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	g := a.evaluateGate(w, r)
	if g == nil {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body == nil {
		body = map[string]any{}
	}
	body["continuity"] = name
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, g.url("/v1/evaluate"), strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		http.Error(w, "the assurance gate didn't answer: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 4<<20))
}

// Policy (GET /api/assurance/{ns}/{name}/policy): the gate's decision
// logic, the Rego it decides every request with, as text. Read-only: the
// rules are what people edit.
func (a *Assurance) Policy(w http.ResponseWriter, r *http.Request) {
	g := a.evaluateGate(w, r)
	if g == nil {
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, g.url("/v1/policy"), nil)
	resp, err := a.http.Do(req)
	if err != nil {
		http.Error(w, "the assurance gate didn't answer: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("the assurance gate's policy answer: %s %s", resp.Status, strings.TrimSpace(string(body))), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}

// evaluateGate: the gate serving the request's chain, by its evaluate port;
// nil (with the error written) when there's no such chain or gate.
func (a *Assurance) evaluateGate(w http.ResponseWriter, r *http.Request) *GateRef {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if a.instance(ns, name) == nil {
		http.Error(w, "no such IdentityContinuity", http.StatusNotFound)
		return nil
	}
	g := a.gate(ns)
	if g == nil || g.EvaluatePort == 0 {
		http.Error(w, fmt.Sprintf("no assurance gate in %s: a Service labelled %s with a port named evaluate", ns, gateLabel), http.StatusServiceUnavailable)
		return nil
	}
	return g
}

func (g *GateRef) url(path string) string {
	if g.override != "" {
		return strings.TrimSuffix(g.override, "/") + path
	}
	return fmt.Sprintf("http://%s.%s.svc:%d%s", g.Name, g.Namespace, g.EvaluatePort, path)
}

// PutProfile (PUT /api/assurance/{ns}/profiles/{profile}): a rule, as the
// signed-in admin. With the resourceVersion it was read at, an update (409
// if it changed since); without one, a new rule.
func (a *Assurance) PutProfile(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("profile")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "need namespace and rule name (DNS labels)", http.StatusBadRequest)
		return
	}
	var in struct {
		ResourceVersion string         `json:"resourceVersion"`
		Spec            map[string]any `json:"spec"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.Spec == nil {
		http.Error(w, `need {"resourceVersion": ..., "spec": {...}}`, http.StatusBadRequest)
		return
	}
	cl, err := a.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	ri := cl.Resource(gvrWLP).Namespace(ns)
	var out *unstructured.Unstructured
	if in.ResourceVersion == "" {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "continuity.lab.solo.io/v1alpha1", "kind": "WorkloadProfile",
			"metadata": map[string]any{"name": name, "namespace": ns}, "spec": in.Spec}}
		out, err = ri.Create(r.Context(), u, metav1.CreateOptions{FieldManager: fieldManager})
		if apierrors.IsAlreadyExists(err) {
			http.Error(w, "a rule named "+name+" was added since this was read: reload", http.StatusConflict)
			return
		}
	} else {
		var cur *unstructured.Unstructured
		if cur, err = ri.Get(r.Context(), name, metav1.GetOptions{}); err == nil {
			cur.SetResourceVersion(in.ResourceVersion)
			cur.Object["spec"] = in.Spec
			out, err = ri.Update(r.Context(), cur, metav1.UpdateOptions{FieldManager: fieldManager})
		}
		if apierrors.IsConflict(err) {
			http.Error(w, name+" changed since it was read: reload and edit again", http.StatusConflict)
			return
		}
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, clean(out).Object)
}

// DeleteProfile (DELETE /api/assurance/{ns}/profiles/{profile}): a rule
// removed.
func (a *Assurance) DeleteProfile(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("profile")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "need namespace and rule name (DNS labels)", http.StatusBadRequest)
		return
	}
	cl, err := a.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	if err := cl.Resource(gvrWLP).Namespace(ns).Delete(r.Context(), name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		httpErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PutPolicyPoint (PUT /api/assurance/{ns}/{name}/policy-points/{pns}/{pname}):
// a gateway policy asks the gate for one of the chain's rules ({"rule":
// "<name>"}, "" for the default rule), or no longer does ({"rule": null}),
// as the signed-in admin, at the resourceVersion it was read (409 on a
// conflict). Turning it on first lets the policy's namespace name the gate
// (a ReferenceGrant) and its gateway call it (an AuthorizationPolicy), each
// labelled assurance-gate-caller beside the gate, where nothing yet does;
// the extAuth is the gate's own (its policy-point API), failing closed.
func (a *Assurance) PutPolicyPoint(w http.ResponseWriter, r *http.Request) {
	ns, name, pns, pname := r.PathValue("ns"), r.PathValue("name"), r.PathValue("pns"), r.PathValue("pname")
	if !nameRe.MatchString(pns) || !nameRe.MatchString(pname) {
		http.Error(w, "need the policy's namespace and name", http.StatusBadRequest)
		return
	}
	var in struct {
		Kind            string  `json:"kind"`
		ResourceVersion string  `json:"resourceVersion"`
		Rule            *string `json:"rule"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || in.Kind == "" || in.ResourceVersion == "" {
		http.Error(w, `need {"kind": ..., "resourceVersion": ..., "rule": "<rule>" | "" | null}`, http.StatusBadRequest)
		return
	}
	ic := a.instance(ns, name)
	g := a.gate(ns)
	if ic == nil || g == nil || g.GRPCPort == 0 {
		http.Error(w, fmt.Sprintf("no assurance gate in %s: a Service labelled %s with a port named grpc", ns, gateLabel), http.StatusConflict)
		return
	}
	var pol *unstructured.Unstructured
	for _, p := range a.policies() {
		if p.GetKind() == in.Kind && p.GetNamespace() == pns && p.GetName() == pname {
			pol = p
		}
	}
	if pol == nil {
		http.Error(w, fmt.Sprintf("no %s %s/%s", in.Kind, pns, pname), http.StatusNotFound)
		return
	}
	gvr, ok := a.k.GVR(pol.GetAPIVersion(), pol.GetKind())
	if !ok {
		http.Error(w, "unknown kind "+in.Kind, http.StatusBadRequest)
		return
	}
	cl, err := a.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	cur, err := cl.Resource(gvr).Namespace(pns).Get(r.Context(), pname, metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	cur.SetResourceVersion(in.ResourceVersion)
	if _, _, _, other := askedRule(cur, a.gates()); other != "" {
		http.Error(w, fmt.Sprintf("%s/%s already sends ext-auth to %s: the rules can't be added beside it", pns, pname, other), http.StatusConflict)
		return
	}
	// whichever way it asks now goes; the decision service's own answer
	// says how it asks from here on
	unstructured.RemoveNestedField(cur.Object, "spec", "traffic", "extAuth")
	unstructured.RemoveNestedField(cur.Object, "spec", "traffic", "entExtAuth")
	if in.Rule == nil {
		if len(obj(cur.Object, "spec", "traffic")) == 0 {
			unstructured.RemoveNestedField(cur.Object, "spec", "traffic")
		}
	} else {
		if *in.Rule != "" && !nameRe.MatchString(*in.Rule) {
			http.Error(w, "bad rule name", http.StatusBadRequest)
			return
		}
		pp, err := a.policyPoint(r.Context(), g, *in.Rule, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if pp.NeedsBackendRef {
			pp.Spec["backendRef"] = map[string]any{"kind": "Service", "name": g.Name, "namespace": g.Namespace, "port": g.GRPCPort}
			if err := a.allowCaller(r.Context(), cl, g, cur); err != nil {
				httpErr(w, err)
				return
			}
		}
		if err := unstructured.SetNestedField(cur.Object, pp.Spec, "spec", "traffic", pp.Field); err != nil {
			httpErr(w, err)
			return
		}
	}
	out, err := cl.Resource(gvr).Namespace(pns).Update(r.Context(), cur, metav1.UpdateOptions{FieldManager: fieldManager})
	if apierrors.IsConflict(err) {
		http.Error(w, pns+"/"+pname+" changed since it was read: reload", http.StatusConflict)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"resourceVersion": out.GetResourceVersion()})
}

// pointSpec: how a policy asks for a rule, as the gate answers it: the
// policy field (extAuth to the gate itself, entExtAuth to Solo's ext-auth
// service) and its value; NeedsBackendRef: the gate's Service goes in it.
type pointSpec struct {
	Field           string         `json:"field"`
	Spec            map[string]any `json:"spec"`
	NeedsBackendRef bool           `json:"needsBackendRef"`
}

// policyPoint: how the gate says a policy asks for a rule.
func (a *Assurance) policyPoint(ctx context.Context, g *GateRef, rule, continuity string) (*pointSpec, error) {
	q := url.Values{"continuity": {continuity}}
	if rule != "" {
		q.Set("rule", rule)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.url("/v1/policy-point?"+q.Encode()), nil)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the assurance gate didn't answer: %w", err)
	}
	defer resp.Body.Close()
	var pp pointSpec
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&pp) != nil ||
		(pp.Field != "extAuth" && pp.Field != "entExtAuth") || pp.Spec == nil {
		return nil, fmt.Errorf("the assurance gate's policy-point answer: %s", resp.Status)
	}
	return &pp, nil
}

// allowCaller: the policy's namespace may name the gate, and its gateway may
// call it, adding a grant of each beside the gate where none does yet.
func (a *Assurance) allowCaller(ctx context.Context, cl dynamic.Interface, g *GateRef, pol *unstructured.Unstructured) error {
	group := strings.SplitN(pol.GetAPIVersion(), "/", 2)[0]
	grants, err := cl.Resource(gvrRefGrant).Namespace(g.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if !granted(grants.Items, g.Name, group, pol.GetKind(), pol.GetNamespace()) {
		rg := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1beta1", "kind": "ReferenceGrant",
			"metadata": map[string]any{"name": dnsName(g.Name + "-from-" + pol.GetNamespace() + "-" + strings.ToLower(pol.GetKind())), "namespace": g.Namespace,
				"labels": map[string]any{callerLabel: "true"}},
			"spec": map[string]any{
				"from": []any{map[string]any{"group": group, "kind": pol.GetKind(), "namespace": pol.GetNamespace()}},
				"to":   []any{map[string]any{"group": "", "kind": "Service", "name": g.Name}}}}}
		if _, err := cl.Resource(gvrRefGrant).Namespace(g.Namespace).Create(ctx, rg, metav1.CreateOptions{FieldManager: fieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	ix := a.index()
	gw := ix.nodes[policyGateway(a.k, pol, ix)]
	if gw == nil || len(gw.Identity) == 0 {
		return apierrors.NewConflict(schema.GroupResource{Group: group, Resource: pol.GetKind()}, pol.GetName(),
			fmt.Errorf("the Observatory sees no mesh identity for the gateway it applies at, so the gate can't be told to take its calls"))
	}
	principal := strings.TrimPrefix(gw.Identity[0], "spiffe://")
	port := a.gatePodPort(g)
	pols, err := cl.Resource(gvrAuthz).Namespace(g.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if allowsCaller(pols.Items, principal, port) {
		return nil
	}
	nm := "caller"
	if m := saRe.FindStringSubmatch(principal); m != nil {
		nm = m[1] + "-" + m[2]
	}
	ap := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "security.istio.io/v1", "kind": "AuthorizationPolicy",
		"metadata": map[string]any{"name": dnsName(g.Name + "-" + nm), "namespace": g.Namespace, "labels": map[string]any{callerLabel: "true"}},
		"spec": map[string]any{"selector": map[string]any{"matchLabels": g.selector}, "action": "ALLOW",
			"rules": []any{map[string]any{
				"from": []any{map[string]any{"source": map[string]any{"principals": []any{principal}}}},
				"to":   []any{map[string]any{"operation": map[string]any{"ports": []any{port}}}}}}}}}
	if _, err := cl.Resource(gvrAuthz).Namespace(g.Namespace).Create(ctx, ap, metav1.CreateOptions{FieldManager: fieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// granted: a ReferenceGrant lets that kind in that namespace name the gate.
func granted(grants []unstructured.Unstructured, gate, group, kind, ns string) bool {
	for _, rg := range grants {
		to := false
		for _, t := range slice(rg.Object, "spec", "to") {
			tm, _ := t.(map[string]any)
			to = to || (str(tm, "kind") == "Service" && (str(tm, "name") == "" || str(tm, "name") == gate))
		}
		for _, f := range slice(rg.Object, "spec", "from") {
			fm, _ := f.(map[string]any)
			if to && str(fm, "group") == group && str(fm, "kind") == kind && str(fm, "namespace") == ns {
				return true
			}
		}
	}
	return false
}

// allowsCaller: an ALLOW AuthorizationPolicy lets the principal reach the port.
func allowsCaller(pols []unstructured.Unstructured, principal, port string) bool {
	for _, ap := range pols {
		if act := str(ap.Object, "spec", "action"); act != "" && act != "ALLOW" {
			continue
		}
		for _, rule := range slice(ap.Object, "spec", "rules") {
			rm, _ := rule.(map[string]any)
			if hasPrincipal(rm, principal) && hasPort(rm, port) {
				return true
			}
		}
	}
	return false
}

// gatePodPort: the gate's gRPC port on its pods (what mesh policy names).
func (a *Assurance) gatePodPort(g *GateRef) string {
	switch t := g.grpcTarget.(type) {
	case int64:
		return strconv.FormatInt(t, 10)
	case string:
		for _, p := range a.k.List("pods") {
			if p.GetNamespace() != g.Namespace || !matches(p.GetLabels(), g.selector) {
				continue
			}
			for _, c := range slice(p.Object, "spec", "containers") {
				cm, _ := c.(map[string]any)
				for _, cp := range slice(cm, "ports") {
					cpm, _ := cp.(map[string]any)
					if str(cpm, "name") == t {
						return fmt.Sprint(cpm["containerPort"])
					}
				}
			}
		}
	}
	return strconv.FormatInt(g.GRPCPort, 10)
}

func matches(labels map[string]string, sel map[string]any) bool {
	if len(sel) == 0 {
		return false
	}
	for k, v := range sel {
		if labels[k] != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

func hasPrincipal(rule map[string]any, principal string) bool {
	for _, f := range slice(rule, "from") {
		fm, _ := f.(map[string]any)
		for _, p := range slice(fm, "source", "principals") {
			if p == principal {
				return true
			}
		}
	}
	return false
}

func hasPort(rule map[string]any, port string) bool {
	to := slice(rule, "to")
	if len(to) == 0 {
		return true
	}
	for _, t := range to {
		tm, _ := t.(map[string]any)
		ports := slice(tm, "operation", "ports")
		if len(ports) == 0 {
			return true
		}
		for _, p := range ports {
			if fmt.Sprint(p) == port {
				return true
			}
		}
	}
	return false
}

// CheckTrust asks the continuity controller to run an instance's trust checks
// now (its check-trust annotation), as the signed-in admin.
func (a *Assurance) CheckTrust(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "bad instance", http.StatusBadRequest)
		return
	}
	cl, err := a.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		"continuity.lab.solo.io/check-trust": fmt.Sprintf("%s %s", userFrom(r.Context()).Name, time.Now().UTC().Format(time.RFC3339Nano))}}})
	if _, err := cl.Resource(gvrIC).Namespace(ns).Patch(r.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		httpErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
