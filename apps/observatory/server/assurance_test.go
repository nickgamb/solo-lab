package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// storeKube is a Kube whose informers hold the given objects (never started).
func storeKube(objs map[string][]map[string]any) *Kube {
	k := &Kube{informer: map[schema.GroupVersionResource]cache.SharedIndexInformer{}}
	for res, list := range objs {
		inf := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
		for _, o := range list {
			_ = inf.GetStore().Add(&unstructured.Unstructured{Object: o})
		}
		k.informer[schema.GroupVersionResource{Resource: res}] = inf
	}
	return k
}

func object(apiVersion, kind, ns, name string, kv ...any) map[string]any {
	o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"namespace": ns, "name": name, "resourceVersion": "7"}}
	for i := 0; i < len(kv); i += 2 {
		o[kv[i].(string)] = kv[i+1]
	}
	return o
}

// The window's view of one chain: its default rule and IdPs as the spec
// names them, its rules, every gateway policy that takes the broker's tokens
// with the rule it asks the gate for (found by the gate's label, whatever
// it's called), and what relies on the broker with no rule.
func TestAssuranceView(t *testing.T) {
	const issuer = "https://idp.example.lab/realms/firm"
	jwt := map[string]any{"providers": []any{map[string]any{"issuer": issuer}}}
	ic := object("continuity.lab.solo.io/v1alpha1", "IdentityContinuity", "id", "firm",
		"spec", map[string]any{"broker": map[string]any{"keycloak": map[string]any{"realm": "firm"}},
			"assurancePolicy": map[string]any{"minimum": "AAL1", "sessions": "Any"},
			"tiers": []any{map[string]any{"name": "primary", "displayName": "Renamed IdP", "type": "oidc",
				"assurance": map[string]any{"levels": []any{map[string]any{"acr": "gold", "level": "AAL2"}}}},
				map[string]any{"name": "spare", "type": "oidc", "enabled": false}}},
		"status", map[string]any{"active": "primary", "broker": map[string]any{"issuer": issuer},
			"tiers": []any{map[string]any{"name": "primary", "healthy": true,
				"trust": map[string]any{"checkedAt": "2026-10-07T12:00:00Z", "checks": []any{map[string]any{"name": "Callback", "result": "Pass"}}}}}})
	gate := object("v1", "Service", "id", "decisions", "spec", map[string]any{"selector": map[string]any{"app": "d"},
		"ports": []any{map[string]any{"name": "grpc", "port": int64(9001)}, map[string]any{"name": "evaluate", "port": int64(9002)}}})
	gate["metadata"].(map[string]any)["labels"] = map[string]any{gateLabel: "true"}
	asks := func(rule string) map[string]any {
		return map[string]any{"backendRef": map[string]any{"name": "decisions", "namespace": "id"}, "grpc": map[string]any{"contextExtensions": map[string]any{"profile": rule}}}
	}
	k := storeKube(map[string][]map[string]any{
		"identitycontinuities": {ic},
		"services":             {gate},
		"workloadprofiles": {object("continuity.lab.solo.io/v1alpha1", "WorkloadProfile", "id", "ledger",
			"spec", map[string]any{"continuity": "firm", "criticality": "Critical", "assurance": map[string]any{"minimum": "AAL2"},
				"workloads": []any{map[string]any{"namespace": "apps", "serviceAccount": "ledger"}}, "clients": []any{"ledger-app"}},
			"status", map[string]any{"phase": "Available"}),
			object("continuity.lab.solo.io/v1alpha1", "WorkloadProfile", "id", "elsewhere", "spec", map[string]any{"continuity": "other"})},
		"agentgatewaypolicies": {
			object("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "apps", "ledger-caller", "spec", map[string]any{
				"targetRefs": []any{map[string]any{"kind": "Gateway", "name": "waypoint"}},
				"traffic":    map[string]any{"jwtAuthentication": jwt, "extAuth": asks("ledger")}}),
			object("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "apps", "notes-caller", "spec", map[string]any{
				"targetRefs": []any{map[string]any{"kind": "Gateway", "name": "notes-gw"}},
				"traffic":    map[string]any{"jwtAuthentication": jwt}}),
			object("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "apps", "other-chain", "spec", map[string]any{
				"traffic": map[string]any{"extAuth": asks("elsewhere")}}),
			object("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "apps", "not-ours", "spec", map[string]any{
				"traffic": map[string]any{"jwtAuthentication": map[string]any{"providers": []any{map[string]any{"issuer": "https://other"}}}}}),
		},
		"gatewayextensions": {object("gateway.kgateway.dev/v1alpha1", "GatewayExtension", "ui", "ui-sso",
			"spec", map[string]any{"oauth2": map[string]any{"credentials": map[string]any{"clientID": "console"}}})},
	})
	wp, notes, ui := "wl:apps/waypoint", "wl:apps/notes-gw", "wl:ui/console"
	ix := &Index{
		byOwner: map[string]string{"apps/Gateway/waypoint": wp, "apps/Gateway/notes-gw": notes},
		bySA:    map[string][]string{"spiffe://cluster.local/ns/ui/sa/console": {ui}, "spiffe://cluster.local/ns/apps/sa/notes-gw": {notes}},
		nodes: map[string]*Node{wp: {ID: wp, Label: "waypoint"}, notes: {ID: notes, Label: "notes-gw", Identity: []string{"spiffe://cluster.local/ns/apps/sa/notes-gw"}},
			ui: {ID: ui, Label: "Console UI", Identity: []string{"spiffe://cluster.local/ns/ui/sa/console"}}},
		sso: []SSO{{Name: "ui/ui-sso", Issuer: issuer, Apps: []string{ui}, Hosts: []string{"console.example.lab"}}},
	}
	a := &Assurance{k: k, index: func() *Index { return ix }}
	v := a.View(context.Background(), k.List("identitycontinuities")[0])

	if v.Realm != "firm" || v.Policy["minimum"] != "AAL1" || len(v.IdPs) != 2 || v.IdPs[0].DisplayName != "Renamed IdP" ||
		len(v.IdPs[0].Checks) != 1 || !v.IdPs[0].Enabled || v.IdPs[1].Enabled {
		t.Fatalf("chain %+v", v)
	}
	if v.Gate == nil || v.Gate.Name != "decisions" || v.Gate.GRPCPort != 9001 || v.Gate.EvaluatePort != 9002 {
		t.Fatalf("gate %+v", v.Gate)
	}
	if len(v.Rules) != 1 || v.Rules[0].Name != "ledger" {
		t.Fatalf("rules: only this chain's: %+v", v.Rules)
	}
	pps := map[string]PolicyPoint{}
	for _, p := range v.PolicyPoints {
		pps[p.Name] = p
	}
	if p := pps["ledger-caller"]; p.Rule == nil || *p.Rule != "ledger" || p.Gateway != "waypoint" || p.FailureMode != "FailClosed" {
		t.Errorf("a policy asking the gate: %+v", p)
	}
	if p, ok := pps["notes-caller"]; !ok || p.Rule != nil {
		t.Errorf("a policy taking the broker's tokens without the gate: %+v", p)
	}
	if _, ok := pps["other-chain"]; ok {
		t.Error("a policy asking for another chain's rule")
	}
	if _, ok := pps["not-ours"]; ok {
		t.Error("a policy that doesn't take the broker's tokens")
	}
	var un []string
	for _, u := range v.Uncovered {
		un = append(un, u.Source+" "+u.Name+" "+u.Ref+" "+strings.Join(u.Clients, ","))
	}
	if got := strings.Join(un, " | "); got != "policyPoint notes-caller apps/notes-caller  | sso console-ui ui/ui-sso console" {
		t.Errorf("uncovered: %s", got)
	}
	if strings.Join(v.Options.Workloads, ",") != "apps/notes-gw,ui/console" || strings.Join(v.Options.Clients, ",") != "console,ledger-app" {
		t.Errorf("options %+v", v.Options)
	}
}

// The rules' choices come from the CRDs as installed.
func TestSchemaFrom(t *testing.T) {
	ver := func(props map[string]any) map[string]any {
		return map[string]any{"spec": map[string]any{"versions": []any{map[string]any{"storage": true, "schema": map[string]any{
			"openAPIV3Schema": map[string]any{"properties": map[string]any{"spec": map[string]any{"properties": props}}}}}}}}
	}
	wlp := ver(map[string]any{"criticality": map[string]any{"enum": []any{"Gold", "Silver"}},
		"mode":      map[string]any{"enum": []any{"Enforce", "ReportOnly"}, "default": "Enforce"},
		"sessions":  map[string]any{"enum": []any{"Any", "ActiveIdPOnly"}},
		"assurance": map[string]any{"properties": map[string]any{"minimum": map[string]any{"enum": []any{"AAL1", "AAL2"}}}}})
	icp := ver(map[string]any{"assurancePolicy": map[string]any{"properties": map[string]any{
		"minimum": map[string]any{"default": "AAL1"}, "sessions": map[string]any{"default": "Any"}}}})
	s, err := schemaFrom(wlp, icp)
	if err != nil || strings.Join(s.Criticality, ",") != "Gold,Silver" || strings.Join(s.Modes, ",") != "Enforce,ReportOnly" ||
		s.Defaults.Mode != "Enforce" || s.Defaults.Minimum != "AAL1" || len(s.Levels) != 2 {
		t.Fatalf("%+v %v", s, err)
	}
}

// Turning enforcement on adds a grant only where none already lets the
// policy name the gate and its gateway call it.
func TestCallerGrants(t *testing.T) {
	rg := unstructured.Unstructured{Object: object("gateway.networking.k8s.io/v1beta1", "ReferenceGrant", "id", "g", "spec", map[string]any{
		"from": []any{map[string]any{"group": "agentgateway.dev", "kind": "AgentgatewayPolicy", "namespace": "apps"}},
		"to":   []any{map[string]any{"group": "", "kind": "Service", "name": "decisions"}}})}
	if !granted([]unstructured.Unstructured{rg}, "decisions", "agentgateway.dev", "AgentgatewayPolicy", "apps") ||
		granted([]unstructured.Unstructured{rg}, "decisions", "agentgateway.dev", "AgentgatewayPolicy", "other") {
		t.Error("ReferenceGrant coverage")
	}
	ap := unstructured.Unstructured{Object: object("security.istio.io/v1", "AuthorizationPolicy", "id", "a", "spec", map[string]any{
		"rules": []any{map[string]any{"from": []any{map[string]any{"source": map[string]any{"principals": []any{"cluster.local/ns/apps/sa/gw"}}}},
			"to": []any{map[string]any{"operation": map[string]any{"ports": []any{"9001"}}}}}}})}
	if !allowsCaller([]unstructured.Unstructured{ap}, "cluster.local/ns/apps/sa/gw", "9001") ||
		allowsCaller([]unstructured.Unstructured{ap}, "cluster.local/ns/apps/sa/gw", "9002") ||
		allowsCaller([]unstructured.Unstructured{ap}, "cluster.local/ns/x/sa/y", "9001") {
		t.Error("AuthorizationPolicy coverage")
	}
}

func TestDNSName(t *testing.T) {
	for in, want := range map[string]string{"Console UI": "console-ui", "ns/route,x": "ns-route-x", "--a--": "a"} {
		if got := dnsName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// Turning enforcement on at a gateway policy writes the gate's own extAuth
// into it (failing closed, the gate's Service as found by its label), and
// lets the policy's namespace name the gate and its gateway call it; off
// takes the extAuth out again.
func TestPutPolicyPoint(t *testing.T) {
	gvrAGP := schema.GroupVersionResource{Group: "agentgateway.dev", Version: "v1alpha1", Resource: "agentgatewaypolicies"}
	pol := object("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", "apps", "notes-caller", "spec", map[string]any{
		"targetRefs": []any{map[string]any{"kind": "Gateway", "name": "notes-gw"}},
		"traffic":    map[string]any{"jwtAuthentication": map[string]any{"mode": "Strict"}}})
	gate := object("v1", "Service", "id", "decisions", "spec", map[string]any{"selector": map[string]any{"app": "d"},
		"ports": []any{map[string]any{"name": "grpc", "port": int64(9001), "targetPort": int64(9001)}, map[string]any{"name": "evaluate", "port": int64(9002)}}})
	gate["metadata"].(map[string]any)["labels"] = map[string]any{gateLabel: "true"}
	k := storeKube(map[string][]map[string]any{
		"identitycontinuities": {object("continuity.lab.solo.io/v1alpha1", "IdentityContinuity", "id", "firm")},
		"services":             {gate},
		"agentgatewaypolicies": {pol},
	})
	k.kinds = map[schema.GroupVersionResource]string{gvrAGP: "AgentgatewayPolicy"}
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvrAGP: "AgentgatewayPolicyList", gvrRefGrant: "ReferenceGrantList", gvrAuthz: "AuthorizationPolicyList"},
		&unstructured.Unstructured{Object: pol})
	gw := "wl:apps/notes-gw"
	ix := &Index{byOwner: map[string]string{"apps/Gateway/notes-gw": gw},
		nodes: map[string]*Node{gw: {ID: gw, Label: "notes-gw", Identity: []string{"spiffe://cluster.local/ns/apps/sa/notes-gw"}}}}
	contract := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/policy-point" || r.URL.Query().Get("rule") != "notes" || r.URL.Query().Get("continuity") != "firm" {
			http.Error(w, "unexpected "+r.URL.String(), http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"failureMode": "FailClosed", "grpc": {"contextExtensions": {"profile": "notes"}, "requestMetadata": {"continuity": "{}"}}}`))
	}))
	defer contract.Close()
	a := &Assurance{k: k, index: func() *Index { return ix }, http: contract.Client(), gateURL: contract.URL,
		writer: func(*http.Request) (dynamic.Interface, error) { return cl, nil }}
	put := func(body string) int {
		r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		for k, v := range map[string]string{"ns": "id", "name": "firm", "pns": "apps", "pname": "notes-caller"} {
			r.SetPathValue(k, v)
		}
		w := httptest.NewRecorder()
		a.PutPolicyPoint(w, r)
		if w.Code != http.StatusOK {
			t.Logf("%d %s", w.Code, w.Body)
		}
		return w.Code
	}
	ctx := context.Background()
	if put(`{"kind": "AgentgatewayPolicy", "resourceVersion": "7", "rule": "notes"}`) != http.StatusOK {
		t.Fatal("turning it on")
	}
	got, _ := cl.Resource(gvrAGP).Namespace("apps").Get(ctx, "notes-caller", metav1.GetOptions{})
	ext := obj(got.Object, "spec", "traffic", "extAuth")
	if str(ext, "grpc", "contextExtensions", "profile") != "notes" || str(ext, "failureMode") != "FailClosed" ||
		str(ext, "backendRef", "name") != "decisions" || str(ext, "backendRef", "namespace") != "id" || obj(got.Object, "spec", "traffic", "jwtAuthentication") == nil {
		t.Fatalf("extAuth %v", got.Object["spec"])
	}
	rgs, _ := cl.Resource(gvrRefGrant).Namespace("id").List(ctx, metav1.ListOptions{})
	aps, _ := cl.Resource(gvrAuthz).Namespace("id").List(ctx, metav1.ListOptions{})
	if !granted(rgs.Items, "decisions", "agentgateway.dev", "AgentgatewayPolicy", "apps") || !allowsCaller(aps.Items, "cluster.local/ns/apps/sa/notes-gw", "9001") {
		t.Fatalf("grants %v %v", rgs.Items, aps.Items)
	}
	if put(`{"kind": "AgentgatewayPolicy", "resourceVersion": "`+got.GetResourceVersion()+`", "rule": null}`) != http.StatusOK {
		t.Fatal("turning it off")
	}
	got, _ = cl.Resource(gvrAGP).Namespace("apps").Get(ctx, "notes-caller", metav1.GetOptions{})
	if obj(got.Object, "spec", "traffic", "extAuth") != nil || obj(got.Object, "spec", "traffic", "jwtAuthentication") == nil {
		t.Fatalf("off: %v", got.Object["spec"])
	}
}
