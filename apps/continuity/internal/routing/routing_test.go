package routing

import (
	"strings"
	"testing"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

func spec(rules ...v1.RoutingRule) v1.IdentityContinuitySpec {
	return v1.IdentityContinuitySpec{
		Tiers: []v1.Tier{
			{Name: "gluu", Type: "oidc"}, {Name: "keycloak", Type: "oidc"},
			{Name: "contingency", Type: "oidc"}, {Name: "break-glass", Type: "local"},
		},
		Routing: &v1.Routing{Rules: rules},
	}
}

func healthy(names ...string) map[string]*v1.TierStatus {
	st := map[string]*v1.TierStatus{}
	for _, n := range []string{"gluu", "keycloak", "contingency", "break-glass"} {
		st[n] = &v1.TierStatus{Name: n, Configured: true}
	}
	for _, n := range names {
		st[n].Healthy = true
	}
	return st
}

func TestResolve(t *testing.T) {
	s := spec(
		v1.RoutingRule{Name: "ledgerline", When: "true", IdPs: []string{"keycloak", "gluu"}},
		v1.RoutingRule{Name: "ops", When: "true", IdPs: []string{"break-glass", "nope"}},
	)
	cases := []struct {
		name    string
		st      map[string]*v1.TierStatus
		ready   func(string) bool
		want    []string
		because []string
	}{
		{"first IdP", healthy("gluu", "keycloak"), nil, []string{"keycloak", ""}, []string{"its first IdP", "none of its IdPs"}},
		{"falls back in its own list", healthy("gluu"), nil, []string{"gluu", ""}, []string{"fallback: keycloak can't sign people in now", ""}},
		{"none healthy: the active tier", healthy(), nil, []string{"", ""}, []string{"none of its IdPs can", ""}},
		{"not at the broker yet", healthy("gluu", "keycloak"), func(n string) bool { return n != "keycloak" }, []string{"gluu", ""}, []string{"not set up at the broker", ""}},
	}
	for _, c := range cases {
		got := Resolve(s, c.st, c.ready)
		for i, r := range got {
			if r.IdP != c.want[i] || !strings.Contains(r.Reason, c.because[i]) {
				t.Errorf("%s: rule %s -> %q (%s), want %q (%s)", c.name, r.Name, r.IdP, r.Reason, c.want[i], c.because[i])
			}
		}
	}
	// a disabled or drained tier is never routed to
	off := false
	s.Tiers[1].Enabled = &off
	if r := Resolve(s, healthy("gluu", "keycloak"), nil); r[0].IdP != "gluu" {
		t.Errorf("disabled keycloak: routed to %q, want gluu", r[0].IdP)
	}
	s.Tiers[1].Enabled, s.Tiers[1].Drain = nil, true
	if r := Resolve(s, healthy("gluu", "keycloak"), nil); r[0].IdP != "gluu" {
		t.Errorf("drained keycloak: routed to %q, want gluu", r[0].IdP)
	}
}

func TestPath(t *testing.T) {
	s := spec(v1.RoutingRule{Name: "ledgerline", When: `request.uri.contains("ledgerline")`, IdPs: []string{"keycloak"}},
		v1.RoutingRule{Name: "down", When: `request.uri.contains("x")`, IdPs: []string{"contingency"}})
	routes := []v1.RouteStatus{{Name: "ledgerline", IdP: "keycloak"}, {Name: "down"}}
	got := Path(s, routes, "gluu")
	for _, want := range []string{
		`(request.uri.contains("ledgerline")) ? "kc_idp_hint=keycloak&"`,
		`(request.uri.contains("x")) ? "kc_idp_hint=gluu&"`, // its IdPs can't: the active tier
		`: "kc_idp_hint=gluu&")`,
		`request.uri.substring(request.uri.indexOf("?") + 1)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("path CEL lacks %s:\n%s", want, got)
		}
	}
	// the broker's own login active: no hint for unmatched sign-ins
	if got := Path(spec(), nil, ""); !strings.Contains(got, `+ ("") +`) {
		t.Errorf("no fallback hint expected:\n%s", got)
	}
	if Signing("gluu", routes)["keycloak"] != true || Signing("gluu", routes)["contingency"] {
		t.Error("signing set: active and routed IdPs only")
	}
}
