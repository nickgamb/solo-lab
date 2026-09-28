package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Continuity drives the IdP failover chain (IdentityContinuity, reconciled
// by the continuity controller in sv-identity). The observatory only edits
// the declared rules and cuts or restores the network; failover itself is
// the controller reacting to what its probes see.
type Continuity struct {
	k    *Kube
	res  *Resources
	seen map[string]string // instance -> last transition time reported
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

const tierLabel = "continuity.lab.solo.io/tier"

var (
	gvrIC  = schema.GroupVersionResource{Group: "continuity.lab.solo.io", Version: "v1alpha1", Resource: "identitycontinuities"}
	gvrAP  = schema.GroupVersionResource{Group: "security.istio.io", Version: "v1", Resource: "authorizationpolicies"}
	gvrSE  = schema.GroupVersionResource{Group: "networking.istio.io", Version: "v1", Resource: "serviceentries"}
	gvrSec = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
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
			v.Partitions = append(v.Partitions, Partition{Tier: t, Namespace: p.GetNamespace(), Name: p.GetName()})
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

// PutSecret stores a tier's client secret next to the controller. The value
// is write-only: it's never read back or logged.
func (c *Continuity) PutSecret(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	var in struct {
		Name, Key, Value string
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil || !nameRe.MatchString(in.Name) || in.Value == "" {
		http.Error(w, "need name (DNS label), key and value", http.StatusBadRequest)
		return
	}
	if in.Key == "" {
		in.Key = "client-secret"
	}
	cl, err := c.res.client(r)
	if err != nil {
		httpErr(w, err)
		return
	}
	patch, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": in.Name, "namespace": ns, "labels": map[string]string{"app.kubernetes.io/part-of": "identity-continuity"}},
		"stringData": map[string]string{in.Key: in.Value}})
	if _, err := cl.Resource(gvrSec).Namespace(ns).Patch(r.Context(), in.Name, types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager, Force: ptr(true)}); err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"name": in.Name, "key": in.Key})
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
	ns, target, err := c.pathOf(in.Tier)
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
		writeJSON(w, map[string]any{"tier": in.Tier, "down": false})
		return
	}
	spec := map[string]any{"action": "DENY", "rules": []any{map[string]any{}}}
	if target != nil {
		spec["targetRefs"] = []any{target}
	}
	patch, _ := json.Marshal(map[string]any{"apiVersion": "security.istio.io/v1", "kind": "AuthorizationPolicy",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]string{tierLabel: in.Tier}},
		"spec":     spec})
	if _, err := cl.Resource(gvrAP).Namespace(ns).Patch(r.Context(), name, types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager, Force: ptr(true)}); err != nil {
		httpErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"tier": in.Tier, "down": true, "policy": ns + "/" + name})
}

// pathOf finds where a tier's traffic can be cut: the ServiceEntry the
// controller keeps for an external tier, else a namespace labelled for it.
func (c *Continuity) pathOf(tier string) (string, map[string]any, error) {
	for _, se := range c.list(gvrSE) {
		if se.GetLabels()[tierLabel] == tier {
			return se.GetNamespace(), map[string]any{"group": "networking.istio.io", "kind": "ServiceEntry", "name": se.GetName()}, nil
		}
	}
	for _, ns := range c.k.List("namespaces") {
		if ns.GetLabels()[tierLabel] == tier {
			return ns.GetName(), nil, nil
		}
	}
	return "", nil, errors.New("no network path is known for tier " + tier + " (local tiers can't be partitioned)")
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
