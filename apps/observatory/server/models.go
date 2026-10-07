package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Models drives the AI gateway's model chain: the AgentgatewayBackend behind
// the model route, its providers in priority order, and the failover rules
// on the two policies around it. The gateway does the failover; the
// Observatory edits the chain and cuts or restores a provider. Reads and
// writes run as the signed-in admin.
type Models struct {
	k       *Kube
	res     *Resources
	traffic *TrafficStore // outages cut and restored show in the feed
	index   func() *Index

	mu   sync.Mutex
	kind map[string]servedKind // apiVersion/kind -> what discovery said, briefly
}

type servedKind struct {
	gvr schema.GroupVersionResource
	ok  bool
	at  time.Time
}

const (
	modelNS        = "agentgateway-system"
	modelRoute     = "llm"
	externalRoute  = "llm-external"        // callers outside the mesh, with an API key
	backendPolicy  = "llm-backend"         // backend.health: when a provider is taken out
	callersPolicy  = "llm-callers"         // traffic.retry: a call retried on the next provider
	outageAnno     = "lab.solo.io/outage-" // + provider: its own host and port while it's cut
	modelSecretKey = "Authorization"
	unroutable     = "127.0.0.1" // with port 1: refused at once, wherever the gateway runs
	modelWindow    = 5 * time.Minute
	// modelCredsLabel marks the provider keys the Observatory may write;
	// admission refuses it any other Secret in the namespace
	modelCredsLabel = "lab.solo.io/model-credentials"
)

var (
	gvrAGWBackend = schema.GroupVersionResource{Group: "agentgateway.dev", Version: "v1alpha1", Resource: "agentgatewaybackends"}
	gvrRoute      = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	gvrNS         = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	// the failover policies' kind by edition: the enterprise one where served
	policyKinds = [][2]string{
		{"enterpriseagentgateway.solo.io/v1alpha1", "EnterpriseAgentgatewayPolicy"},
		{"agentgateway.dev/v1alpha1", "AgentgatewayPolicy"},
	}
	budgetKind    = [2]string{"enterpriseagentgateway.solo.io/v1alpha1", "EnterpriseAgentgatewayBudget"}
	rateLimitKind = [2]string{"ratelimit.solo.io/v1alpha1", "RateLimitConfig"}
	// a duration as the gateway's CRDs take it (GEP-2257)
	durationRe = regexp.MustCompile(`^([0-9]{1,5}(h|m|s|ms)){1,4}$`)
	hostRe     = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9.]{0,251}[A-Za-z0-9])?$`)
	callerRe   = regexp.MustCompile(`source\.identity\.namespace\s*==\s*"([^"]+)"\s*&&\s*source\.identity\.serviceAccount\s*(?:==\s*"([^"]+)"|in\s*\[([^\]]*)\])`)
)

type ModelView struct {
	Edition      string            `json:"edition"` // enterprise oss
	Backend      *ModelBackend     `json:"backend"` // nil: no model chain yet (make llm)
	Gateway      string            `json:"gateway,omitempty"`
	External     *ModelRoute       `json:"external,omitempty"` // the route for callers outside the mesh, if any
	Providers    []ModelProvider   `json:"providers"`
	Rules        FailoverRules     `json:"rules"`
	Callers      []ModelCaller     `json:"callers,omitempty"`  // left out: not a simple list
	Declared     map[string]string `json:"declared,omitempty"` // what make llm last set
	Unattributed ModelStats        `json:"unattributed"`       // calls no provider's model matched
	Enterprise   EnterpriseModels  `json:"enterprise"`
}

type ModelBackend struct {
	Namespace       string   `json:"namespace"`
	Name            string   `json:"name"`
	ResourceVersion string   `json:"resourceVersion"`
	Policies        []string `json:"policies"` // "Kind name" of the failover policies found
}

type ModelRoute struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
}

type ModelProvider struct {
	Group  int          `json:"group"` // priority: the first group with a healthy provider serves
	Name   string       `json:"name"`
	Kind   string       `json:"kind"` // openai anthropic, a custom provider's override (ollama), other
	Model  string       `json:"model"`
	Host   string       `json:"host,omitempty"` // while cut: its own, not the closed port
	Port   int64        `json:"port,omitempty"`
	Secret string       `json:"secret,omitempty"` // the Secret its key is in; never the key
	Outage *ModelOutage `json:"outage,omitempty"`
	Stats  ModelStats   `json:"stats"`
}

// ModelOutage is a simulated outage, kept on the backend while it lasts: the
// provider's own host and port, and who cut it when.
type ModelOutage struct {
	Host  string `json:"host,omitempty"`
	Port  int64  `json:"port,omitempty"`
	By    string `json:"by,omitempty"`
	Since string `json:"since,omitempty"`
}

type ModelStats struct {
	Calls      int    `json:"calls"`
	Errors     int    `json:"errors"`
	LastServed string `json:"lastServed,omitempty"`
	LastStatus int    `json:"lastStatus,omitempty"`
}

type FailoverRules struct {
	On5xx               bool    `json:"on5xx"`
	On429               bool    `json:"on429"`
	Condition           string  `json:"condition"`
	Custom              bool    `json:"custom"` // the condition says more than these two: saving replaces it
	ConsecutiveFailures int64   `json:"consecutiveFailures"`
	Duration            string  `json:"duration"`
	RetryAttempts       int64   `json:"retryAttempts"`
	RetryCodes          []int64 `json:"retryCodes"`
}

type ModelCaller struct {
	Namespace      string   `json:"namespace"`
	ServiceAccount string   `json:"serviceAccount"`
	Nodes          []string `json:"nodes,omitempty"`
}

type EnterpriseModels struct {
	Budgets    []string `json:"budgets"`
	RateLimits []string `json:"rateLimits"`
}

// served resolves a kind through discovery, remembered for a minute: the
// tab polls, and an edition doesn't change under it.
func (m *Models) served(apiVersion, kind string) (schema.GroupVersionResource, bool) {
	key := apiVersion + "/" + kind
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.kind[key]; ok && time.Since(s.at) < time.Minute {
		return s.gvr, s.ok
	}
	gvr, ok := m.k.GVR(apiVersion, kind)
	if m.kind == nil {
		m.kind = map[string]servedKind{}
	}
	m.kind[key] = servedKind{gvr, ok, time.Now()}
	return gvr, ok
}

func (m *Models) edition() string {
	if _, ok := m.served(policyKinds[0][0], policyKinds[0][1]); ok {
		return "enterprise"
	}
	return "oss"
}

// chain finds the backend the model route sends to, and the route's gateway.
func (m *Models) chain(ctx context.Context, cl dynamic.Interface) (ns, name, gw string) {
	ns, name = modelNS, "llm"
	r, err := cl.Resource(gvrRoute).Namespace(modelNS).Get(ctx, modelRoute, metav1.GetOptions{})
	if err != nil {
		return
	}
	for _, rule := range slice(r.Object, "spec", "rules") {
		for _, br := range slice(rule.(map[string]any), "backendRefs") {
			bm, _ := br.(map[string]any)
			if str(bm, "kind") == "AgentgatewayBackend" {
				name = str(bm, "name")
				if v := str(bm, "namespace"); v != "" {
					ns = v
				}
			}
		}
	}
	if ps := slice(r.Object, "spec", "parentRefs"); len(ps) > 0 && m.index != nil {
		pm, _ := ps[0].(map[string]any)
		pns := str(pm, "namespace")
		if pns == "" {
			pns = modelNS
		}
		gw = m.index().gateway(pns + "/" + str(pm, "name"))
	}
	return
}

// external is the route for callers outside the mesh (API keys), when it
// sends to the same backend.
func external(ctx context.Context, cl dynamic.Interface, ns, name string) *ModelRoute {
	r, err := cl.Resource(gvrRoute).Namespace(modelNS).Get(ctx, externalRoute, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	for _, rule := range slice(r.Object, "spec", "rules") {
		for _, br := range slice(rule.(map[string]any), "backendRefs") {
			bm, _ := br.(map[string]any)
			bns := str(bm, "namespace")
			if bns == "" {
				bns = r.GetNamespace()
			}
			if str(bm, "kind") == "AgentgatewayBackend" && bns == ns && str(bm, "name") == name {
				return &ModelRoute{Name: r.GetName(), Hosts: toStrings(r.Object["spec"].(map[string]any)["hostnames"])}
			}
		}
	}
	return nil
}

// policy finds one of the chain's policies: the enterprise kind where it is
// served and holds it, else the OSS one. Absent is (nil, nil).
func (m *Models) policy(ctx context.Context, cl dynamic.Interface, ns, name string) (*unstructured.Unstructured, schema.GroupVersionResource, error) {
	for _, pk := range policyKinds {
		gvr, ok := m.served(pk[0], pk[1])
		if !ok {
			continue
		}
		p, err := cl.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if isNotFound(err) {
			continue
		}
		return p, gvr, err
	}
	return nil, schema.GroupVersionResource{}, nil
}

func (m *Models) Get(w http.ResponseWriter, r *http.Request) {
	cl, err := m.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	v, err := m.view(r.Context(), cl)
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, v)
}

func (m *Models) view(ctx context.Context, cl dynamic.Interface) (ModelView, error) {
	v := ModelView{Edition: m.edition(), Providers: []ModelProvider{}, Enterprise: EnterpriseModels{Budgets: []string{}, RateLimits: []string{}}}
	ns, name, gw := m.chain(ctx, cl)
	v.Gateway = gw
	v.External = external(ctx, cl, ns, name)
	if n, err := cl.Resource(gvrNS).Get(ctx, modelNS, metav1.GetOptions{}); err == nil {
		a := n.GetAnnotations()
		for _, k := range []string{"provider", "model", "fallback"} {
			if x := a["lab.solo.io/llm-"+k]; x != "" {
				if v.Declared == nil {
					v.Declared = map[string]string{}
				}
				v.Declared[k] = x
			}
		}
	}
	if v.Edition == "enterprise" {
		v.Enterprise.Budgets, v.Enterprise.RateLimits = m.names(ctx, cl, budgetKind), m.names(ctx, cl, rateLimitKind)
	}
	be, err := cl.Resource(gvrAGWBackend).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if isNotFound(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	v.Backend = &ModelBackend{Namespace: ns, Name: name, ResourceVersion: be.GetResourceVersion(), Policies: []string{}}
	v.Providers = providersOf(be)
	bp, _, err := m.policy(ctx, cl, ns, backendPolicy)
	if err != nil {
		return v, err
	}
	cp, _, err := m.policy(ctx, cl, ns, callersPolicy)
	if err != nil {
		return v, err
	}
	var bo, co map[string]any
	for _, p := range []*unstructured.Unstructured{bp, cp} {
		if p != nil {
			v.Backend.Policies = append(v.Backend.Policies, p.GetKind()+" "+p.GetName())
		}
	}
	if bp != nil {
		bo = bp.Object
	}
	if cp != nil {
		co = cp.Object
		v.Callers = parseCallers(cp.Object)
		if m.index != nil {
			ix := m.index()
			for i, c := range v.Callers {
				v.Callers[i].Nodes = ix.bySA["spiffe://"+trustDomain+"/ns/"+c.Namespace+"/sa/"+c.ServiceAccount]
			}
		}
	}
	v.Rules = parseRules(bo, co)
	if m.traffic != nil {
		v.Unattributed = modelStats(m.traffic.Recent("", 5000), v.Providers, time.Now().Add(-modelWindow))
	}
	return v, nil
}

// names lists a kind's objects as ns/name, everywhere; none if not served.
func (m *Models) names(ctx context.Context, cl dynamic.Interface, k [2]string) []string {
	out := []string{}
	gvr, ok := m.served(k[0], k[1])
	if !ok {
		return out
	}
	l, err := cl.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, o := range l.Items {
		out = append(out, o.GetNamespace()+"/"+o.GetName())
	}
	slices.Sort(out)
	return out
}

// providersOf reads the backend's providers in priority order: one
// (spec.ai.provider, named primary) or one per group (spec.ai.groups).
func providersOf(be *unstructured.Unstructured) []ModelProvider {
	out := []ModelProvider{}
	if p := obj(be.Object, "spec", "ai", "provider"); p != nil {
		mp := parseProvider(p, 0, "primary")
		mp.Secret = str(be.Object, "spec", "policies", "auth", "secretRef", "name")
		out = append(out, mp)
	}
	for g, gr := range slice(be.Object, "spec", "ai", "groups") {
		gm, _ := gr.(map[string]any)
		for i, p := range slice(gm, "providers") {
			pm, _ := p.(map[string]any)
			name := str(pm, "name")
			if name == "" {
				name = fmt.Sprintf("provider-%d-%d", g, i)
			}
			mp := parseProvider(pm, g, name)
			mp.Secret = str(pm, "policies", "auth", "secretRef", "name")
			out = append(out, mp)
		}
	}
	a := be.GetAnnotations()
	for i, p := range out {
		var o ModelOutage
		if raw := a[outageAnno+p.Name]; raw != "" && json.Unmarshal([]byte(raw), &o) == nil && p.Port == 1 {
			out[i].Host, out[i].Port, out[i].Outage = o.Host, o.Port, &o
		}
	}
	return out
}

func parseProvider(p map[string]any, group int, name string) ModelProvider {
	mp := ModelProvider{Group: group, Name: name, Kind: "other", Host: str(p, "host"), Port: i64(p, "port")}
	for _, k := range []string{"openai", "anthropic"} {
		if m := obj(p, k); m != nil {
			mp.Kind, mp.Model = k, str(m, "model")
			return mp
		}
	}
	if m := obj(p, "custom"); m != nil { // shown as what it stands in for
		mp.Kind, mp.Model = str(m, "providerOverride"), str(m, "model")
		if mp.Kind == "" || mp.Kind == "openai" || mp.Kind == "anthropic" {
			mp.Kind = "custom"
		}
		return mp
	}
	for _, k := range []string{"gemini", "vertexai", "bedrock", "azureopenai"} {
		if m := obj(p, k); m != nil {
			mp.Model = str(m, "model")
		}
	}
	return mp
}

// providerSpec is a provider as the backend takes it. Ollama is a custom
// provider speaking both the OpenAI and the Anthropic API.
func providerSpec(p modelProviderIn) map[string]any {
	if p.Kind != "ollama" {
		return map[string]any{p.Kind: map[string]any{"model": p.Model}}
	}
	return map[string]any{"custom": map[string]any{"model": p.Model, "providerOverride": "ollama", "formats": []any{
		map[string]any{"type": "Completions", "path": "/v1/chat/completions"},
		map[string]any{"type": "Messages", "path": "/v1/messages"},
	}}}
}

// parseRules reads the failover rules: when a provider counts as failed and
// for how long it's out (llm-backend), and whether a call is retried
// (llm-callers).
func parseRules(backend, callers map[string]any) FailoverRules {
	r := FailoverRules{
		Condition:           str(backend, "spec", "backend", "health", "unhealthyCondition"),
		ConsecutiveFailures: i64(backend, "spec", "backend", "health", "eviction", "consecutiveFailures"),
		Duration:            str(backend, "spec", "backend", "health", "eviction", "duration"),
		RetryAttempts:       i64(callers, "spec", "traffic", "retry", "attempts"),
		RetryCodes:          []int64{},
	}
	for _, c := range slice(callers, "spec", "traffic", "retry", "codes") {
		if n, ok := c.(int64); ok {
			r.RetryCodes = append(r.RetryCodes, n)
		}
	}
	for _, term := range strings.Split(r.Condition, "||") {
		switch strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(term), "()")), " ") {
		case "response.code >= 500":
			r.On5xx = true
		case "response.code == 429":
			r.On429 = true
		case "":
		default:
			r.Custom = true
		}
	}
	return r
}

// unhealthyWhen is the CEL for the checked conditions.
func unhealthyWhen(on5xx, on429 bool) string {
	var c []string
	if on5xx {
		c = append(c, "response.code >= 500")
	}
	if on429 {
		c = append(c, "response.code == 429")
	}
	return strings.Join(c, " || ")
}

// parseCallers reads who may call the model route, when the rule is a plain
// list of namespace and ServiceAccount pairs; anything else is nil.
func parseCallers(pol map[string]any) []ModelCaller {
	if a := str(pol, "spec", "traffic", "authorization", "action"); a != "" && a != "Allow" {
		return nil
	}
	var out []ModelCaller
	for _, e := range slice(pol, "spec", "traffic", "authorization", "policy", "matchExpressions") {
		expr := fmt.Sprint(e)
		if strings.Trim(callerRe.ReplaceAllString(expr, ""), " \t\n()|") != "" {
			return nil
		}
		for _, mt := range callerRe.FindAllStringSubmatch(expr, -1) {
			sas := []string{mt[2]}
			if mt[2] == "" {
				sas = nil
				for _, s := range strings.Split(mt[3], ",") {
					s = strings.TrimSpace(s)
					if len(s) < 3 || s[0] != '"' || s[len(s)-1] != '"' {
						return nil
					}
					sas = append(sas, s[1:len(s)-1])
				}
			}
			for _, sa := range sas {
				out = append(out, ModelCaller{Namespace: mt[1], ServiceAccount: sa})
			}
		}
	}
	return out
}

// servedBy is the model and provider that answered a call, as the gateway
// logged them: the response's model where it says, else the request's.
func servedBy(t Traffic) (model, provider string) {
	a := t.Attrs
	for _, k := range []string{"gen_ai.response.model", "gen_ai.request.model", "llm.request.model"} {
		if model = a[k]; model != "" {
			break
		}
	}
	return model, a["gen_ai.provider.name"]
}

// modelStats counts recent model calls per provider (into ps) by the model
// that served them; what matches no provider is returned. A provider's
// dated model (gpt-5-mini-2025-08-07) is its model.
func modelStats(ts []Traffic, ps []ModelProvider, since time.Time) ModelStats {
	var other ModelStats
	match := func(model, prov string) int {
		ok := func(p ModelProvider) bool {
			return prov == "" || p.Kind == "other" || p.Kind == prov || prov == "custom" && p.Kind != "openai" && p.Kind != "anthropic"
		}
		for i, p := range ps {
			if p.Model == model && ok(p) {
				return i
			}
		}
		for i, p := range ps {
			if p.Model != "" && strings.HasPrefix(model, p.Model+"-") && ok(p) {
				return i
			}
		}
		return -1
	}
	for _, t := range ts { // newest first
		if t.Kind != "llm" {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, t.Time); err == nil && at.Before(since) {
			continue
		}
		s := &other
		if i := match(servedBy(t)); i >= 0 {
			s = &ps[i].Stats
		}
		s.Calls++
		if t.Outcome == "error" || t.Status == http.StatusTooManyRequests {
			s.Errors++
		}
		if s.LastServed == "" {
			s.LastServed, s.LastStatus = t.Time, t.Status
		}
	}
	return other
}

type modelProviderIn struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Model  string `json:"model"`
	Host   string `json:"host,omitempty"`
	Port   int64  `json:"port,omitempty"`
	Secret string `json:"secret,omitempty"`
}

type modelRulesIn struct {
	On5xx               bool    `json:"on5xx"`
	On429               bool    `json:"on429"`
	ConsecutiveFailures int64   `json:"consecutiveFailures"`
	Duration            string  `json:"duration"`
	RetryAttempts       int64   `json:"retryAttempts"`
	RetryCodes          []int64 `json:"retryCodes,omitempty"`
}

var defaultRetryCodes = []int64{429, 500, 502, 503, 504}

func validChain(ps []modelProviderIn, r modelRulesIn) error {
	if len(ps) < 1 || len(ps) > 4 {
		return errors.New("1 to 4 model connections")
	}
	seen := map[string]bool{}
	for _, p := range ps {
		switch {
		case !nameRe.MatchString(p.Name) || len(p.Name) > 40:
			return fmt.Errorf("connection name %q: a DNS label of up to 40 characters", p.Name)
		case seen[p.Name]:
			return fmt.Errorf("connection name %q is used twice", p.Name)
		case p.Kind != "openai" && p.Kind != "anthropic" && p.Kind != "ollama":
			return fmt.Errorf("%s: kind ollama, openai (or OpenAI-compatible) or anthropic", p.Name)
		case p.Kind == "ollama" && p.Host == "":
			return fmt.Errorf("%s: Ollama needs a host and port", p.Name)
		case strings.TrimSpace(p.Model) == "" || len(p.Model) > 200 || strings.ContainsAny(p.Model, "\n\r\t\"'"):
			return fmt.Errorf("%s: needs a model", p.Name)
		case (p.Host == "") != (p.Port == 0):
			return fmt.Errorf("%s: a host needs a port, and a port a host", p.Name)
		case p.Host != "" && !hostRe.MatchString(p.Host) && net.ParseIP(p.Host) == nil:
			return fmt.Errorf("%s: host %q is not a hostname or IP", p.Name, p.Host)
		case p.Port < 0 || p.Port > 65535:
			return fmt.Errorf("%s: port 1 to 65535", p.Name)
		case p.Secret != "" && !nameRe.MatchString(p.Secret):
			return fmt.Errorf("%s: Secret name %q is not a DNS label", p.Name, p.Secret)
		}
		seen[p.Name] = true
	}
	d, err := time.ParseDuration(r.Duration)
	switch {
	case !r.On5xx && !r.On429:
		return errors.New("fail over on 5xx, 429, or both")
	case r.ConsecutiveFailures < 1 || r.ConsecutiveFailures > 10:
		return errors.New("failures before a provider is taken out: 1 to 10")
	case err != nil || !durationRe.MatchString(r.Duration) || d < time.Second || d > time.Hour:
		return errors.New("time out of rotation: 1s to 1h (e.g. 30s)")
	case r.RetryAttempts < 0 || r.RetryAttempts > 3:
		return errors.New("retries: 0 to 3")
	}
	for _, c := range r.RetryCodes {
		if c < 400 || c > 599 {
			return fmt.Errorf("retry code %d: 400 to 599", c)
		}
	}
	return nil
}

// Put rewrites the model chain (PUT /api/models/{ns}/{name}): the backend's
// providers in priority order, then the failover rules on the two policies.
// One provider is written as spec.ai.provider, two or more as one group each.
// It is an update at the resourceVersion the chain was read at: a change
// made since is a conflict (409). A provider under a simulated outage stays
// cut, at its edited host and port.
func (m *Models) Put(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "bad backend", http.StatusBadRequest)
		return
	}
	var in struct {
		ResourceVersion string            `json:"resourceVersion"`
		Providers       []modelProviderIn `json:"providers"`
		Rules           *modelRulesIn     `json:"rules"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil || in.Rules == nil {
		http.Error(w, `need {"resourceVersion": ..., "providers": [...], "rules": {...}}`, http.StatusBadRequest)
		return
	}
	for i := range in.Providers {
		in.Providers[i].Model = strings.TrimSpace(in.Providers[i].Model)
	}
	if err := validChain(in.Providers, *in.Rules); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cl, err := m.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	ctx := r.Context()
	if cns, cname, _ := m.chain(ctx, cl); cns != ns || cname != name {
		http.Error(w, ns+"/"+name+" is not the model route's backend", http.StatusNotFound)
		return
	}
	// both policies first: a missing one refuses the save before anything is written
	bp, bgvr, err := m.policy(ctx, cl, ns, backendPolicy)
	if err != nil {
		httpErr(w, err)
		return
	}
	cp, cgvr, err := m.policy(ctx, cl, ns, callersPolicy)
	if err != nil {
		httpErr(w, err)
		return
	}
	if bp == nil || cp == nil {
		http.Error(w, "policies "+backendPolicy+" and "+callersPolicy+" must exist in "+ns+": make llm writes them", http.StatusConflict)
		return
	}
	ri := cl.Resource(gvrAGWBackend).Namespace(ns)
	be, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	if in.ResourceVersion != "" {
		be.SetResourceVersion(in.ResourceVersion)
	}
	rewriteChain(be, in.Providers)
	if _, err := ri.Update(ctx, be, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		if apierrors.IsConflict(err) {
			http.Error(w, "changed since it was read: reload and edit again", http.StatusConflict)
			return
		}
		httpErr(w, err)
		return
	}
	rl := *in.Rules
	health, _ := json.Marshal(map[string]any{"spec": map[string]any{"backend": map[string]any{"health": map[string]any{
		"unhealthyCondition": unhealthyWhen(rl.On5xx, rl.On429),
		"eviction":           map[string]any{"consecutiveFailures": rl.ConsecutiveFailures, "duration": rl.Duration}}}}})
	var retry any // null: no retry
	if rl.RetryAttempts > 0 {
		codes := rl.RetryCodes
		if len(codes) == 0 {
			codes = toInt64s(slice(cp.Object, "spec", "traffic", "retry", "codes"))
		}
		if len(codes) == 0 {
			codes = defaultRetryCodes
		}
		retry = map[string]any{"attempts": rl.RetryAttempts, "codes": codes}
	}
	retryPatch, _ := json.Marshal(map[string]any{"spec": map[string]any{"traffic": map[string]any{"retry": retry}}})
	for _, p := range []struct {
		gvr   schema.GroupVersionResource
		name  string
		patch []byte
	}{{bgvr, backendPolicy, health}, {cgvr, callersPolicy, retryPatch}} {
		if _, err := cl.Resource(p.gvr).Namespace(ns).Patch(ctx, p.name, types.MergePatchType, p.patch,
			metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
			http.Error(w, "saved the model connections, but not the failover rules on "+p.name+": "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	v, err := m.view(ctx, cl)
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, v)
}

func toInt64s(l []any) []int64 {
	var out []int64
	for _, x := range l {
		if n, ok := x.(int64); ok {
			out = append(out, n)
		}
	}
	return out
}

// rewriteChain sets the backend's providers. Fields the editor doesn't know
// (a provider's other policies, the backend's) are kept.
func rewriteChain(be *unstructured.Unstructured, ps []modelProviderIn) {
	old := map[string]map[string]any{} // a group's provider name -> its policies
	for _, g := range slice(be.Object, "spec", "ai", "groups") {
		for _, p := range slice(g.(map[string]any), "providers") {
			pm, _ := p.(map[string]any)
			old[str(pm, "name")] = obj(pm, "policies")
		}
	}
	single := len(ps) == 1
	annos := be.GetAnnotations()
	if annos == nil {
		annos = map[string]string{}
	}
	cut := map[string]ModelOutage{} // providers cut now; a stale note just goes
	for _, p := range providersOf(be) {
		if p.Outage != nil {
			cut[p.Name] = *p.Outage
		}
	}
	for k := range annos {
		if strings.HasPrefix(k, outageAnno) {
			delete(annos, k)
		}
	}
	spec, _ := be.Object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		be.Object["spec"] = spec
	}
	ai, _ := spec["ai"].(map[string]any)
	if ai == nil {
		ai = map[string]any{}
		spec["ai"] = ai
	}
	delete(ai, "provider")
	delete(ai, "groups")
	pol, _ := spec["policies"].(map[string]any)
	delete(pol, "auth")
	var groups []any
	for _, p := range ps {
		final := p.Name
		if single {
			final = "primary" // the one provider has no name of its own
		}
		host, port := p.Host, p.Port
		if o, ok := cut[p.Name]; ok { // stays cut, now hiding its edited target
			o.Host, o.Port = host, port
			b, _ := json.Marshal(o)
			annos[outageAnno+final] = string(b)
			host, port = cutTarget(host)
		}
		po := providerSpec(p)
		if host != "" {
			po["host"], po["port"] = host, port
		}
		if single {
			ai["provider"] = po
			if p.Secret != "" {
				if pol == nil {
					pol = map[string]any{}
				}
				pol["auth"] = map[string]any{"secretRef": map[string]any{"name": p.Secret}}
			}
			continue
		}
		po["name"] = p.Name
		pp := old[p.Name]
		delete(pp, "auth")
		if p.Secret != "" {
			if pp == nil {
				pp = map[string]any{}
			}
			pp["auth"] = map[string]any{"secretRef": map[string]any{"name": p.Secret}}
		}
		if len(pp) > 0 {
			po["policies"] = pp
		}
		groups = append(groups, map[string]any{"providers": []any{po}})
	}
	if groups != nil {
		ai["groups"] = groups
	}
	if len(pol) > 0 {
		spec["policies"] = pol
	} else {
		delete(spec, "policies")
	}
	if len(annos) == 0 {
		annos = nil
	}
	be.SetAnnotations(annos)
}

// cutTarget is where a cut provider points: its own host on a closed port,
// or, for a cloud provider with no host, a closed port here.
func cutTarget(host string) (string, int64) {
	if host == "" {
		host = unroutable
	}
	return host, 1
}

// providerAt is the provider's object in the backend itself (not a copy),
// by the name the view gives it.
func providerAt(be map[string]any, name string) map[string]any {
	spec, _ := be["spec"].(map[string]any)
	ai, _ := spec["ai"].(map[string]any)
	if p, ok := ai["provider"].(map[string]any); ok && name == "primary" {
		return p
	}
	groups, _ := ai["groups"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		ps, _ := gm["providers"].([]any)
		for _, p := range ps {
			if pm, _ := p.(map[string]any); pm != nil && pm["name"] == name {
				return pm
			}
		}
	}
	return nil
}

// Outage cuts (cut=true) or restores one provider (POST
// /api/models/{ns}/{name}/outage). A cut points the provider at a closed
// port and keeps its own host and port in an annotation; a restore puts them
// back. Calls to it then fail as they would in a real outage, and the
// gateway's failover rules take over. Both are idempotent.
func (m *Models) Outage(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var in struct {
		Provider string `json:"provider"`
		Cut      bool   `json:"cut"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || !nameRe.MatchString(in.Provider) ||
		!nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "need the provider's name", http.StatusBadRequest)
		return
	}
	cl, err := m.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	ctx := r.Context()
	if cns, cname, _ := m.chain(ctx, cl); cns != ns || cname != name {
		http.Error(w, ns+"/"+name+" is not the model route's backend", http.StatusNotFound)
		return
	}
	ri := cl.Resource(gvrAGWBackend).Namespace(ns)
	be, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	p := providerAt(be.Object, in.Provider)
	if p == nil {
		http.Error(w, "no provider "+in.Provider+" in "+ns+"/"+name, http.StatusNotFound)
		return
	}
	annos := be.GetAnnotations()
	if annos == nil {
		annos = map[string]string{}
	}
	key := outageAnno + in.Provider
	var o ModelOutage
	raw, noted := annos[key]
	// cut: noted, and still at the closed port (a rewrite since may have moved it)
	isCut := noted && i64(p, "port") == 1 && json.Unmarshal([]byte(raw), &o) == nil
	out := map[string]any{"provider": in.Provider, "cut": in.Cut}
	if in.Cut && isCut || !in.Cut && !noted {
		writeJSON(w, out)
		return
	}
	user := userFrom(ctx).Name
	what := providerWhat(p)
	if in.Cut {
		o = ModelOutage{Host: str(p, "host"), Port: i64(p, "port"), By: user, Since: time.Now().UTC().Format(time.RFC3339)}
		b, _ := json.Marshal(o)
		annos[key] = string(b)
		p["host"], p["port"] = cutTarget(o.Host)
	} else {
		if isCut { // else the note is stale: only it goes
			if o.Host != "" {
				p["host"], p["port"] = o.Host, o.Port
			} else {
				delete(p, "host")
				delete(p, "port")
			}
		}
		delete(annos, key)
	}
	if len(annos) == 0 {
		annos = nil
	}
	be.SetAnnotations(annos)
	if _, err := ri.Update(ctx, be, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		if apierrors.IsConflict(err) {
			http.Error(w, "the model chain changed meanwhile: try again", http.StatusConflict)
			return
		}
		httpErr(w, err)
		return
	}
	switch {
	case in.Cut:
		h, pt := cutTarget(o.Host)
		m.note(in.Provider, "error", fmt.Sprintf("simulated outage: model provider %s (%s) pointed at %s:%d", in.Provider, what, h, pt), user)
	case isCut:
		to := "its provider's own endpoint"
		if o.Host != "" {
			to = fmt.Sprintf("%s:%d", o.Host, o.Port)
		}
		m.note(in.Provider, "ok", fmt.Sprintf("model provider %s (%s) restored to %s", in.Provider, what, to), user)
	}
	writeJSON(w, out)
}

func providerWhat(p map[string]any) string {
	mp := parseProvider(p, 0, "")
	return mp.Kind + "/" + mp.Model
}

// note puts a provider being cut or restored in the traffic feed, next to
// the failover it causes.
func (m *Models) note(provider, outcome, what, by string) {
	if m.traffic == nil {
		return
	}
	m.traffic.Add(Traffic{Kind: "model", Reporter: "observatory", Outcome: outcome, User: by,
		Summary: what, Attrs: map[string]string{"provider": provider, "by": by}})
}

// PutSecret (PUT /api/models/{ns}/secrets/{name}) stores a provider's API
// key, as key Authorization of Secret name. The name must be one a provider
// of the model chain refers to. Write-only: the value is never read back or
// logged.
func (m *Models) PutSecret(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "need namespace and name (DNS labels)", http.StatusBadRequest)
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Value) == "" {
		http.Error(w, `need {"value": ...}`, http.StatusBadRequest)
		return
	}
	cl, err := m.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	ctx := r.Context()
	bns, bname, _ := m.chain(ctx, cl)
	if bns != ns {
		http.Error(w, "the model chain is in "+bns, http.StatusNotFound)
		return
	}
	be, err := cl.Resource(gvrAGWBackend).Namespace(ns).Get(ctx, bname, metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	if !slices.ContainsFunc(providersOf(be), func(p ModelProvider) bool { return p.Secret == name }) {
		http.Error(w, "no provider of "+ns+"/"+bname+" refers to Secret "+name+": save the reference first", http.StatusConflict)
		return
	}
	patch, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]string{
			"app.kubernetes.io/part-of": "model-continuity",
			modelCredsLabel:             "true", // the only Secrets here admission lets the Observatory write
		}},
		"stringData": map[string]string{modelSecretKey: strings.TrimSpace(in.Value)}})
	if _, err := cl.Resource(gvrSec).Namespace(ns).Patch(ctx, name, types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager, Force: ptr(true)}); err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"name": name, "keys": []string{modelSecretKey}})
}
