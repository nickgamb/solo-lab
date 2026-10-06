package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Continuity drives the IdP failover chain (IdentityContinuity, reconciled
// by the continuity controller in sv-identity). The observatory only edits
// the declared rules and cuts or restores the network; failover itself is
// the controller reacting to what its probes see.
type Continuity struct {
	k       *Kube
	res     *Resources
	traffic *TrafficStore     // outages cut and restored show in the feed
	seen    map[string]string // instance -> last transition time reported
}

// Report turns new failover transitions into events in the traffic feed.
func (c *Continuity) Report(v ContinuityView, t *TrafficStore, ix *Index) {
	if c.seen == nil {
		c.seen = map[string]string{}
	}
	for _, it := range v.Items {
		md, _ := it["metadata"].(map[string]any)
		key := fmt.Sprint(md["namespace"], "/", md["name"])
		first := c.seen[key] == ""
		for _, tr := range slice(it, "status", "transitions") {
			m, _ := tr.(map[string]any)
			at := fmt.Sprint(m["time"])
			if at <= c.seen[key] {
				continue
			}
			c.seen[key] = at
			if first {
				continue // history from before we started isn't news
			}
			broker := ix.hostOf(str(it, "spec", "broker", "keycloak", "url"))
			t.Add(Traffic{Kind: "continuity", Reporter: key, Target: broker, Outcome: "info",
				Summary: fmt.Sprintf("sign-in moved %v → %v: %v", m["from"], m["to"], m["reason"]),
				Attrs:   map[string]string{"from": fmt.Sprint(m["from"]), "to": fmt.Sprint(m["to"]), "reason": fmt.Sprint(m["reason"])}})
		}
		if first && c.seen[key] == "" {
			c.seen[key] = "0"
		}
	}
}

const (
	tierLabel            = "continuity.lab.solo.io/tier"
	secretsRoleLabel     = "continuity.lab.solo.io/secrets-role"      // the controller's
	syncSecretsRoleLabel = "continuity.lab.solo.io/sync-secrets-role" // the profile sync's
)

var (
	gvrIC   = schema.GroupVersionResource{Group: "continuity.lab.solo.io", Version: "v1alpha1", Resource: "identitycontinuities"}
	gvrAP   = schema.GroupVersionResource{Group: "security.istio.io", Version: "v1", Resource: "authorizationpolicies"}
	gvrSE   = schema.GroupVersionResource{Group: "networking.istio.io", Version: "v1", Resource: "serviceentries"}
	gvrSec  = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	gvrRole = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	gvrCron = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
	gvrJob  = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	gvrPod  = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	nameRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	keyRe   = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
)

type ContinuityView struct {
	Items      []map[string]any `json:"items"`
	Partitions []Partition      `json:"partitions"`
	Paths      []SignInPath     `json:"paths"`
}

// SignInPath is derived from the edge's SSO config: an app that signs people
// in through a continuity broker, and what a signed-in session reaches.
type SignInPath struct {
	Instance string     `json:"instance"` // ns/name of the IdentityContinuity
	Name     string     `json:"name"`
	Hosts    []string   `json:"hosts"`
	Entry    string     `json:"entry,omitempty"` // the edge
	Broker   string     `json:"broker"`
	App      string     `json:"app"`
	After    [][]string `json:"after"` // node ids by hop from the app
}

type Partition struct {
	Tier      string `json:"tier"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Since     string `json:"since,omitempty"`
	By        string `json:"by,omitempty"`   // the admin who cut it
	Path      string `json:"path,omitempty"` // what was cut, in words
}

func (c *Continuity) View(ix *Index) ContinuityView {
	v := ContinuityView{Items: []map[string]any{}, Partitions: []Partition{}, Paths: []SignInPath{}}
	for _, o := range c.k.List("identitycontinuities") {
		v.Items = append(v.Items, clean(o).Object)
		broker := ix.hostOf(str(o.Object, "spec", "broker", "keycloak", "url"))
		issuer := str(o.Object, "status", "broker", "issuer")
		realm := str(o.Object, "spec", "broker", "keycloak", "realm")
		for _, s := range ix.sso {
			match := contains(s.IdP, broker) || issuer != "" && strings.TrimSuffix(s.Issuer, "/") == strings.TrimSuffix(issuer, "/") ||
				realm != "" && strings.HasSuffix(strings.TrimSuffix(s.Issuer, "/"), "/realms/"+realm) && contains(s.IdP, broker)
			if !match || broker == "" {
				continue
			}
			for _, app := range s.Apps {
				v.Paths = append(v.Paths, SignInPath{Instance: o.GetNamespace() + "/" + o.GetName(), Name: s.Name, Hosts: s.Hosts,
					Entry: ix.edgeGW, Broker: broker, App: app, After: ix.downstream(app, 5, broker)})
			}
		}
	}
	for _, p := range c.k.List("authorizationpolicies") {
		if t := p.GetLabels()[tierLabel]; t != "" {
			a := p.GetAnnotations()
			v.Partitions = append(v.Partitions, Partition{Tier: t, Namespace: p.GetNamespace(), Name: p.GetName(),
				Since: p.GetCreationTimestamp().UTC().Format(time.RFC3339), By: a[byAnno], Path: a[pathAnno]})
		}
	}
	return v
}

// PutSpec replaces the rule set (tiers, health, failback) of one instance.
func (c *Continuity) PutSpec(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var spec map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&spec); err != nil {
		http.Error(w, "invalid spec: "+err.Error(), http.StatusBadRequest)
		return
	}
	cl, err := c.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	patch, _ := json.Marshal(map[string]any{"apiVersion": "continuity.lab.solo.io/v1alpha1", "kind": "IdentityContinuity",
		"metadata": map[string]any{"name": name, "namespace": ns}, "spec": spec})
	out, err := cl.Resource(gvrIC).Namespace(ns).Patch(r.Context(), name, types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager, Force: ptr(true)})
	if err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, clean(out).Object)
}

// PutSecret stores a tier's client secret next to the controller, or (for
// "directory") a directory's client id and secret for the profile sync
// only. Values are write-only: never read back or logged. One call writes
// the Secret's whole data, so send every key it should hold.
func (c *Continuity) PutSecret(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	var in struct {
		Name, Key, Value string
		Data             map[string]string
		For              string
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil || !nameRe.MatchString(in.Name) {
		http.Error(w, "need name (DNS label)", http.StatusBadRequest)
		return
	}
	data := in.Data
	if data == nil {
		if in.Key == "" {
			in.Key = "client-secret"
		}
		data = map[string]string{in.Key: in.Value}
	}
	label := secretsRoleLabel
	switch in.For {
	case "", "tier":
	case "directory":
		label = syncSecretsRoleLabel
		for k := range data {
			if k != "client-id" && k != "client-secret" {
				http.Error(w, "a directory's Secret holds client-id and client-secret only", http.StatusBadRequest)
				return
			}
		}
	default:
		http.Error(w, `for: "tier" or "directory"`, http.StatusBadRequest)
		return
	}
	if len(data) == 0 {
		http.Error(w, "need a value", http.StatusBadRequest)
		return
	}
	for k, v := range data {
		if !keyRe.MatchString(k) || v == "" {
			http.Error(w, "every key needs a value", http.StatusBadRequest)
			return
		}
	}
	cl, err := c.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	patch, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": in.Name, "namespace": ns, "labels": map[string]string{"app.kubernetes.io/part-of": "identity-continuity"}},
		"stringData": data})
	if _, err := cl.Resource(gvrSec).Namespace(ns).Patch(r.Context(), in.Name, types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager, Force: ptr(true)}); err != nil {
		httpErr(w, err)
		return
	}
	if err := grantSecret(r.Context(), cl, ns, in.Name, label); err != nil {
		http.Error(w, "stored the secret, but could not grant read access to it: "+err.Error(), http.StatusInternalServerError)
		return
	}
	keys := slices.Sorted(maps.Keys(data))
	writeJSON(w, map[string]any{"name": in.Name, "keys": keys})
}

// grantSecret adds a Secret to the Roles labelled label=true: the controller's
// (a tier's client secret) or the profile sync's (a directory's credentials).
// Neither has list or watch on Secrets, so this is how one added at runtime
// becomes readable to it.
func grantSecret(ctx context.Context, cl dynamic.Interface, ns, name, label string) error {
	roles, err := cl.Resource(gvrRole).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: label + "=true"})
	if err != nil {
		return err
	}
	if len(roles.Items) == 0 {
		return errors.New("no Role labelled " + label + " in " + ns)
	}
	for i := range roles.Items {
		role := &roles.Items[i]
		rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
		changed := false
		for j, rule := range rules {
			m, _ := rule.(map[string]any)
			if !slices.Contains(toStrings(m["resources"]), "secrets") {
				continue
			}
			names := toStrings(m["resourceNames"])
			if !slices.Contains(names, name) {
				l := make([]any, 0, len(names)+1) // unstructured wants []any, not []string
				for _, n := range append(names, name) {
					l = append(l, n)
				}
				m["resourceNames"] = l
				rules[j], changed = m, true
			}
		}
		if !changed {
			continue
		}
		if err := unstructured.SetNestedSlice(role.Object, rules, "rules"); err != nil {
			return err
		}
		if _, err := cl.Resource(gvrRole).Namespace(ns).Update(ctx, role, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return err
		}
	}
	return nil
}

// RunSync starts the instance's profile sync now: a Job from its CronJob's
// template, created as the signed-in admin.
func (c *Continuity) RunSync(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "bad instance", http.StatusBadRequest)
		return
	}
	cl, err := c.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	ic, err := cl.Resource(gvrIC).Namespace(ns).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	if _, ok, _ := unstructured.NestedMap(ic.Object, "spec", "sync"); !ok {
		http.Error(w, "no sync is configured: set a schedule and save first", http.StatusConflict)
		return
	}
	jobName, code, err := startSyncJob(r, cl, ns, ic, "manual", nil)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, map[string]string{"job": jobName})
}

// TestDirectory checks one tier's saved directory: a Job of the sync with
// --test-tier, so it runs exactly where the sync does (its ServiceAccount,
// credentials, mesh policy and egress). The result is read with SyncJob.
func (c *Continuity) TestDirectory(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var in struct{ Tier string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || !nameRe.MatchString(in.Tier) || !nameRe.MatchString(ns) || !nameRe.MatchString(name) {
		http.Error(w, "need tier", http.StatusBadRequest)
		return
	}
	cl, err := c.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	ic, err := cl.Resource(gvrIC).Namespace(ns).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	saved := false
	for _, t := range slice(ic.Object, "spec", "tiers") {
		m, _ := t.(map[string]any)
		if m["name"] == in.Tier && m["directory"] != nil {
			saved = true
		}
	}
	if !saved {
		http.Error(w, "tier "+in.Tier+" has no saved directory: save it first", http.StatusConflict)
		return
	}
	jobName, code, err := startSyncJob(r, cl, ns, ic, "test", func(spec map[string]any) error {
		cs, _, _ := unstructured.NestedSlice(spec, "template", "spec", "containers")
		if len(cs) == 0 {
			return errors.New("the sync's job template has no container")
		}
		c0, _ := cs[0].(map[string]any)
		args, _ := c0["args"].([]any)
		c0["args"] = append(args, "--test-tier="+in.Tier)
		spec["ttlSecondsAfterFinished"] = int64(600)
		return unstructured.SetNestedSlice(spec, cs, "template", "spec", "containers")
	})
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, map[string]string{"job": jobName})
}

// SyncJob reports a sync or test Job: running, or done with the result its
// container left (a test's is JSON: ok, users, message; never user data).
func (c *Continuity) SyncJob(w http.ResponseWriter, r *http.Request) {
	ns, job := r.PathValue("ns"), r.PathValue("job")
	if !nameRe.MatchString(ns) || !nameRe.MatchString(job) {
		http.Error(w, "bad job", http.StatusBadRequest)
		return
	}
	cl, err := c.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	j, err := cl.Resource(gvrJob).Namespace(ns).Get(r.Context(), job, metav1.GetOptions{})
	if err != nil {
		httpErr(w, err)
		return
	}
	if j.GetLabels()["app"] != "continuity-sync" {
		http.Error(w, "not a sync job", http.StatusBadRequest)
		return
	}
	out := map[string]any{"job": job, "done": false}
	succeeded, _, _ := unstructured.NestedInt64(j.Object, "status", "succeeded")
	failed, _, _ := unstructured.NestedInt64(j.Object, "status", "failed")
	if succeeded == 0 && failed == 0 {
		writeJSON(w, out)
		return
	}
	out["done"], out["ok"] = true, succeeded > 0
	if pods, err := cl.Resource(gvrPod).Namespace(ns).List(r.Context(), metav1.ListOptions{LabelSelector: "job-name=" + job}); err == nil {
		for _, p := range pods.Items {
			for _, cs := range slice(p.Object, "status", "containerStatuses") {
				m, _ := cs.(map[string]any)
				var res map[string]any
				if msg := str(m, "state", "terminated", "message"); msg != "" && json.Unmarshal([]byte(msg), &res) == nil {
					out["result"] = res
				}
			}
		}
	}
	writeJSON(w, out)
}

// startSyncJob creates a Job from the instance's sync CronJob, as the
// signed-in admin; edit adjusts the Job spec.
func startSyncJob(r *http.Request, cl dynamic.Interface, ns string, ic *unstructured.Unstructured, kind string, edit func(map[string]any) error) (string, int, error) {
	cjName := str(ic.Object, "status", "sync", "cronJob")
	if cjName == "" {
		return "", http.StatusConflict, errors.New("the controller has not created the sync's CronJob yet")
	}
	cj, err := cl.Resource(gvrCron).Namespace(ns).Get(r.Context(), cjName, metav1.GetOptions{})
	if err != nil {
		return "", http.StatusBadGateway, err
	}
	spec, ok, _ := unstructured.NestedMap(cj.Object, "spec", "jobTemplate", "spec")
	if !ok {
		return "", http.StatusConflict, errors.New("CronJob " + cjName + " has no job template")
	}
	if edit != nil {
		if err := edit(spec); err != nil {
			return "", http.StatusConflict, err
		}
	}
	jobName := fmt.Sprintf("%.40s-%s-%d", cjName, kind, time.Now().Unix())
	labels, _, _ := unstructured.NestedStringMap(cj.Object, "spec", "jobTemplate", "metadata", "labels")
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": jobName, "namespace": ns,
			"annotations":     map[string]any{"cronjob.kubernetes.io/instantiate": "manual", "continuity.lab.solo.io/run-by": userFrom(r.Context()).Name},
			"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "CronJob", "name": cjName, "uid": string(cj.GetUID())}}},
		"spec": spec,
	}}
	job.SetLabels(labels)
	if _, err := cl.Resource(gvrJob).Namespace(ns).Create(r.Context(), job, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
		return "", http.StatusBadGateway, err
	}
	return jobName, 0, nil
}

func toStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// Partition cuts (down=true) or restores the network path to one tier. It
// is a real mesh DENY on that tier's path, never a flag the controller
// reads: for an external IdP, its egress ServiceEntry; for one in the lab,
// its namespace.
func (c *Continuity) Partition(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tier string `json:"tier"`
		Down bool   `json:"down"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || !nameRe.MatchString(in.Tier) {
		http.Error(w, "need tier", http.StatusBadRequest)
		return
	}
	cl, err := c.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	ns, target, what, err := c.pathOf(in.Tier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	name := "continuity-partition-" + in.Tier
	if !in.Down {
		err := cl.Resource(gvrAP).Namespace(ns).Delete(r.Context(), name, metav1.DeleteOptions{})
		if err != nil && !isNotFound(err) {
			httpErr(w, err)
			return
		}
		if err == nil {
			c.note(in.Tier, "ok", "network restored to "+what, userFrom(r.Context()).Name)
		}
		writeJSON(w, map[string]any{"tier": in.Tier, "down": false})
		return
	}
	spec := map[string]any{"action": "DENY", "rules": []any{map[string]any{}}}
	if target != nil {
		spec["targetRefs"] = []any{target}
	}
	patch, _ := json.Marshal(map[string]any{"apiVersion": "security.istio.io/v1", "kind": "AuthorizationPolicy",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]string{tierLabel: in.Tier},
			"annotations": map[string]string{byAnno: userFrom(r.Context()).Name, pathAnno: what}},
		"spec": spec})
	if _, err := cl.Resource(gvrAP).Namespace(ns).Patch(r.Context(), name, types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager, Force: ptr(true)}); err != nil {
		httpErr(w, err)
		return
	}
	c.note(in.Tier, "error", "simulated outage: network to "+what+" cut (DENY "+ns+"/"+name+")", userFrom(r.Context()).Name)
	writeJSON(w, map[string]any{"tier": in.Tier, "down": true, "policy": ns + "/" + name})
}

const (
	byAnno   = "continuity.lab.solo.io/cut-by"
	pathAnno = "continuity.lab.solo.io/path"
)

// note puts an outage being cut or restored in the traffic feed, next to
// the failover it causes.
func (c *Continuity) note(tier, outcome, what, by string) {
	if c.traffic == nil {
		return
	}
	c.traffic.Add(Traffic{Kind: "continuity", Reporter: "observatory", Outcome: outcome, User: by,
		Summary: what, Attrs: map[string]string{"tier": tier, "by": by}})
}

// pathOf finds where a tier's traffic can be cut: the ServiceEntry the
// controller keeps for an external tier, else a namespace labelled for it.
func (c *Continuity) pathOf(tier string) (string, map[string]any, string, error) {
	for _, se := range c.list(gvrSE) {
		if se.GetLabels()[tierLabel] == tier {
			var hosts []string
			for _, h := range slice(se.Object, "spec", "hosts") {
				hosts = append(hosts, fmt.Sprint(h))
			}
			what := strings.Join(hosts, ", ") + " at the " + se.GetNamespace() + " egress"
			return se.GetNamespace(), map[string]any{"group": "networking.istio.io", "kind": "ServiceEntry", "name": se.GetName()}, what, nil
		}
	}
	for _, ns := range c.k.List("namespaces") {
		if ns.GetLabels()[tierLabel] == tier {
			return ns.GetName(), nil, "namespace " + ns.GetName(), nil
		}
	}
	return "", nil, "", errors.New("no network path is known for tier " + tier + " (local tiers can't be partitioned)")
}

func (c *Continuity) list(gvr schema.GroupVersionResource) []*unstructured.Unstructured {
	l, err := c.k.dyn.Resource(gvr).List(bg(), metav1.ListOptions{LabelSelector: tierLabel})
	if err != nil {
		return nil
	}
	out := make([]*unstructured.Unstructured, len(l.Items))
	for i := range l.Items {
		out[i] = &l.Items[i]
	}
	return out
}

// hostOf resolves a URL's host to its (single) node.
func (ix *Index) hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return ix.host(u.Hostname())
}

// downstream walks the request path from a node, breadth first.
func (ix *Index) downstream(from string, depth int, skip string) [][]string {
	seen := map[string]bool{from: true, skip: true}
	out := [][]string{}
	level := []string{from}
	for d := 0; d < depth && len(level) > 0; d++ {
		var next []string
		for _, id := range level {
			for _, t := range ix.next[id] {
				if !seen[t] && ix.nodes[t] != nil && ix.nodes[t].Kind != "external" {
					seen[t] = true
					next = append(next, t)
				}
			}
		}
		if len(next) > 0 {
			out = append(out, next)
		}
		level = next
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
