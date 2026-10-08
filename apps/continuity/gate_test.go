package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

const broker = "https://idp.sterling.lab/realms/sterling-vance"

// testGate: the gate over a cache holding S&V's chain (active as given) and
// two profiles, last heard from at heard.
func testGate(t *testing.T, active string, now, heard time.Time) *gate {
	ic := &v1.IdentityContinuity{ObjectMeta: metav1.ObjectMeta{Name: "sterling-vance", Namespace: "sv-identity"},
		Spec: v1.IdentityContinuitySpec{Tiers: []v1.Tier{
			{Name: "keycloak", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{ACR: "aal1", Level: "AAL1"}, {ACR: "aal2", Level: "AAL2"}}}},
			{Name: "contingency", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{ACR: "aal1", Level: "AAL1"}}}},
			{Name: "break-glass", Type: "local"}}},
		Status: v1.IdentityContinuityStatus{Active: active, Broker: &v1.BrokerStatus{Issuer: broker}}}
	ps := []client.Object{ic,
		&v1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: "advisor-workspace", Namespace: "sv-identity"},
			Spec: v1.WorkloadProfileSpec{Continuity: "sterling-vance", Criticality: "Critical", Assurance: v1.ProfileAssurance{Minimum: "AAL2"}}},
		&v1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: "ledgerline-research", Namespace: "sv-identity"},
			Spec: v1.WorkloadProfileSpec{Continuity: "sterling-vance", Criticality: "High", Sessions: "ActiveIdPOnly", Assurance: v1.ProfileAssurance{Minimum: "AAL2"}}},
		&v1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: "research-notes", Namespace: "sv-identity"},
			Spec: v1.WorkloadProfileSpec{Continuity: "sterling-vance", Criticality: "Standard", Mode: "ReportOnly", Assurance: v1.ProfileAssurance{Minimum: "AAL2"}}},
		&v1.WorkloadProfile{ObjectMeta: metav1.ObjectMeta{Name: "archive", Namespace: "sv-identity"},
			Spec: v1.WorkloadProfileSpec{Continuity: "sterling-vance", Criticality: "Standard", Mode: "Off", Assurance: v1.ProfileAssurance{Minimum: "AAL3"}}},
	}
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	g := &gate{r: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ps...).Build(), ns: "sv-identity",
		stale: 30 * time.Second, now: func() time.Time { return now }}
	g.synced.Store(true)
	g.heard.Store(heard.UnixNano())
	return g
}

func check(t *testing.T, g *gate, profile string, claims map[string]any) *authv3.CheckResponse {
	t.Helper()
	attrs := &authv3.AttributeContext{ContextExtensions: map[string]string{gateProfileKey: profile},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Id: "r1"}}}
	if claims != nil {
		s, err := structpb.NewStruct(claims)
		if err != nil {
			t.Fatal(err)
		}
		attrs.MetadataContext = &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{gateMetaKey: s}}
	}
	resp, err := g.Check(context.Background(), &authv3.CheckRequest{Attributes: attrs})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestGate(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	g := testGate(t, "keycloak", now, now)
	strong := map[string]any{"iss": broker, "sub": "u", "idp": "keycloak", "acr": "aal2", "amr": "pwd otp", "auth_time": float64(now.Add(-time.Minute).Unix())}

	if r := check(t, g, "advisor-workspace", strong); r.GetStatus().GetCode() != int32(codes.OK) ||
		r.GetOkResponse().GetResponseHeadersToAdd()[0].GetHeader().GetValue() != "allow advisor-workspace: AAL2 via keycloak (acr aal2)" {
		t.Fatalf("AAL2 session: %v", r)
	}

	weak := map[string]any{"iss": broker, "sub": "u", "idp": "keycloak", "acr": "aal1"}
	r := check(t, g, "advisor-workspace", weak)
	d := r.GetDeniedResponse()
	if d.GetStatus().GetCode() != 401 {
		t.Fatalf("AAL1 session: %v", r)
	}
	var www string
	for _, h := range d.GetHeaders() {
		if h.GetHeader().GetKey() == "www-authenticate" {
			www = h.GetHeader().GetValue()
		}
	}
	if !strings.Contains(www, `error="insufficient_user_authentication"`) || !strings.Contains(www, `acr_values="aal2"`) {
		t.Fatalf("challenge %q: RFC 9470, asking the active IdP for its aal2", www)
	}
	if v := headerOf(d, decisionHeader); v != "deny advisor-workspace: assurance AAL1 below AAL2: session from keycloak (acr aal1)" {
		t.Fatalf("decision header %q", v)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(d.GetBody()), &body)
	if body["workload_profile"] != "advisor-workspace" || !strings.Contains(body["error_description"], "AAL1 below AAL2") {
		t.Fatalf("body %v", body)
	}

	// failed over to the password-only IdP: refused, and no acr_values to
	// ask for (re-authenticating there can't help)
	g = testGate(t, "contingency", now, now)
	r = check(t, g, "advisor-workspace", map[string]any{"iss": broker, "idp": "contingency", "acr": "aal1"})
	if d := r.GetDeniedResponse(); d.GetStatus().GetCode() != 401 || strings.Contains(headerOf(d, "www-authenticate"), "acr_values") {
		t.Fatalf("on contingency: %v", r)
	}
	// a keycloak session after failover, where only the active IdP's count
	if d := check(t, g, "ledgerline-research", strong).GetDeniedResponse(); d.GetStatus().GetCode() != 403 || !strings.Contains(d.GetBody(), "sign in again") {
		t.Fatalf("active IdP only: %v", d)
	}

	for name, c := range map[string]struct {
		profile string
		claims  map[string]any
		code    int32
	}{
		"no claims":        {"advisor-workspace", nil, 403},
		"unknown profile":  {"nope", strong, 403},
		"another issuer":   {"advisor-workspace", map[string]any{"iss": "https://evil.example", "idp": "keycloak", "acr": "aal2"}, 403},
		"break-glass":      {"advisor-workspace", map[string]any{"iss": broker, "sub": "ops"}, 403},
		"unknown upstream": {"advisor-workspace", map[string]any{"iss": broker, "idp": "okta", "acr": "aal2"}, 403},
	} {
		if r := check(t, g, c.profile, c.claims); r.GetDeniedResponse().GetStatus().GetCode() != typeCode(c.code) {
			t.Errorf("%s: %v", name, r)
		}
	}

	// no word from the chain for longer than stale (the API server gone, or
	// the controller): decisions continue from what the gate last saw, except
	// where the active IdP must be known now
	g = testGate(t, "keycloak", now, now.Add(-time.Minute))
	if r := check(t, g, "advisor-workspace", strong); r.GetStatus().GetCode() != int32(codes.OK) {
		t.Errorf("stale, any session: %v", r)
	}
	if r := check(t, g, "ledgerline-research", strong); r.GetDeniedResponse().GetStatus().GetCode() != 503 {
		t.Errorf("stale, active IdP only: %v", r)
	}
	g.synced.Store(false)
	if r := check(t, g, "advisor-workspace", strong); r.GetDeniedResponse().GetStatus().GetCode() != 503 {
		t.Errorf("nothing read yet: %v", r)
	}
}

func typeCode(c int32) typev3.StatusCode { return typev3.StatusCode(c) }

func headerOf(d *authv3.DeniedHttpResponse, k string) string {
	for _, h := range d.GetHeaders() {
		if h.GetHeader().GetKey() == k {
			return h.GetHeader().GetValue()
		}
	}
	return ""
}

func okHeader(r *authv3.CheckResponse) string {
	for _, h := range r.GetOkResponse().GetResponseHeadersToAdd() {
		if h.GetHeader().GetKey() == decisionHeader {
			return h.GetHeader().GetValue()
		}
	}
	return ""
}

// A rule in report-only lets the request through and says what it would
// have decided; one that's off lets everything through. A policy point that
// names no rule gets the chain's default rule (its assurance policy).
func TestGateModesAndDefault(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	g := testGate(t, "keycloak", now, now)
	weak := map[string]any{"iss": broker, "sub": "u", "idp": "keycloak", "acr": "aal1"}
	if r := check(t, g, "research-notes", weak); r.GetStatus().GetCode() != int32(codes.OK) ||
		!strings.HasPrefix(okHeader(r), "would-deny research-notes: assurance AAL1 below AAL2") {
		t.Fatalf("report-only: %v", r)
	}
	if r := check(t, g, "archive", nil); r.GetStatus().GetCode() != int32(codes.OK) || !strings.HasPrefix(okHeader(r), "off archive") {
		t.Fatalf("off: %v", r)
	}
	if r := check(t, g, "", weak); r.GetStatus().GetCode() != int32(codes.OK) || !strings.HasPrefix(okHeader(r), "allow default: AAL1 via keycloak") {
		t.Fatalf("default rule: %v", r)
	}
	if r := check(t, g, "", map[string]any{"iss": broker, "sub": "ops"}); r.GetDeniedResponse().GetStatus().GetCode() != 403 {
		t.Fatalf("default rule, break-glass: %v", r)
	}
}

// The evaluate API: each IdP's outcome per rule, with a draft over what the
// gate has, and one session's decision.
func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	g := testGate(t, "contingency", now, now)
	srv := g.evaluateHandler()
	post := func(body string) EvalResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/evaluate", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		var out EvalResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	rule := func(out EvalResponse, name string) EvalRule {
		for _, r := range out.Rules {
			if r.Name == name {
				return r
			}
		}
		t.Fatalf("no rule %q in %+v", name, out.Rules)
		return EvalRule{}
	}

	out := post(`{"continuity": "sterling-vance"}`)
	if out.Active != "contingency" || len(out.IdPs) != 3 || out.Rules[0].Name != "" {
		t.Fatalf("view: %+v", out)
	}
	aw := rule(out, "advisor-workspace")
	if aw.Phase != "FailedClosed" || aw.IdPs[0].Outcome != "Conditional" || aw.IdPs[1].Outcome != "Refuse" || aw.Effective.Minimum != "AAL2" {
		t.Fatalf("advisor-workspace: %+v", aw)
	}

	// the same, with the minimum lowered and a new rule, as drafted
	out = post(`{"continuity": "sterling-vance", "draft": {"rules": {
		"advisor-workspace": {"continuity": "sterling-vance", "criticality": "Critical", "assurance": {"minimum": "AAL1"}},
		"new-one": {"continuity": "sterling-vance", "criticality": "High", "allowedIdPs": ["keycloak"]},
		"archive": null}},
		"session": {"idp": "contingency", "acr": "aal1"}}`)
	if aw := rule(out, "advisor-workspace"); aw.Phase != "Degraded" || aw.Session.Decision != "allow" {
		t.Fatalf("drafted advisor-workspace: %+v", aw)
	}
	if n := rule(out, "new-one"); n.Session.Decision != "deny" || n.Session.Status != 403 {
		t.Fatalf("drafted new rule: %+v", n.Session)
	}
	if rn := rule(out, "research-notes"); rn.Session.Decision != "would-deny" {
		t.Fatalf("report-only what-if: %+v", rn.Session)
	}
	for _, r := range out.Rules {
		if r.Name == "archive" {
			t.Fatal("a rule the draft removes is still there")
		}
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/policy-point?rule=advisor-workspace&continuity=sterling-vance", nil))
	var pp struct {
		FailureMode string `json:"failureMode"`
		GRPC        struct {
			ContextExtensions map[string]string `json:"contextExtensions"`
			RequestMetadata   map[string]string `json:"requestMetadata"`
		} `json:"grpc"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pp)
	if pp.FailureMode != "FailClosed" || pp.GRPC.ContextExtensions[gateProfileKey] != "advisor-workspace" || !strings.Contains(pp.GRPC.RequestMetadata[gateMetaKey], "jwt.idp_acr") {
		t.Fatalf("policy point: %s", rec.Body)
	}
}
