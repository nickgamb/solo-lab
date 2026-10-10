package assurance

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

// S&V's chain as installed: Auth0 (amr mfa), S&V's own Keycloak (acr by
// level), the contingency IdP (password only), break-glass.
func chain() []v1.Tier {
	return []v1.Tier{
		{Name: "auth0", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{AMR: "mfa", Level: "AAL2"}}, Default: "AAL1"}},
		{Name: "keycloak", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{ACR: "aal1", Level: "AAL1"}, {ACR: "aal2", Level: "AAL2"}}}},
		{Name: "contingency", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{ACR: "aal1", Level: "AAL1"}}}},
		{Name: "break-glass", Type: "local", Assurance: &v1.TierAssurance{Default: "AAL1"}},
	}
}

func critical() v1.WorkloadProfileSpec {
	return v1.WorkloadProfileSpec{Criticality: "Critical", Assurance: v1.ProfileAssurance{Minimum: "AAL2"}}
}

// the chain's policy as installed: AAL1, any IdP, no break-glass, any session
var policy = v1.AssurancePolicy{Minimum: "AAL1", Sessions: "Any"}

func yes() *bool { b := true; return &b }

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	hour := &metav1.Duration{Duration: time.Hour}
	cases := []struct {
		name    string
		profile func(*v1.WorkloadProfileSpec)
		active  string
		s       Session
		allow   bool
		reason  string
		stepUp  string
	}{
		{"keycloak with a second factor", nil, "keycloak", Session{IdP: "keycloak", ACR: "aal2"}, true, "AAL2 via keycloak (acr aal2)", ""},
		{"keycloak, password only", nil, "keycloak", Session{IdP: "keycloak", ACR: "aal1"}, false, "assurance AAL1 below AAL2", "aal2"},
		{"failed over to contingency", nil, "contingency", Session{IdP: "contingency", ACR: "aal1"}, false, "session from contingency (acr aal1)", ""},
		{"auth0 with mfa in amr", nil, "auth0", Session{IdP: "auth0", AMR: []string{"pwd", "mfa"}}, true, "AAL2 via auth0 (amr mfa)", ""},
		{"auth0 without mfa", nil, "auth0", Session{IdP: "auth0", AMR: []string{"pwd"}}, false, "AAL1 below AAL2", ""},
		{"an acr the tier doesn't map", nil, "keycloak", Session{IdP: "keycloak", ACR: "gold"}, false, "no acr or amr it maps", "aal2"},
		{"break-glass, not allowed", nil, "break-glass", Session{}, false, "break-glass sessions don't reach", ""},
		{"break-glass, allowed at AAL1", func(p *v1.WorkloadProfileSpec) { p.AllowBreakGlass, p.Assurance.Minimum = yes(), "AAL1" }, "break-glass", Session{}, true, "AAL1 via break-glass", ""},
		{"an IdP not in the chain", nil, "keycloak", Session{IdP: "okta", ACR: "aal2"}, false, "isn't in the chain", ""},
		{"allowedIdPs", func(p *v1.WorkloadProfileSpec) { p.AllowedIdPs = []string{"keycloak"} }, "auth0", Session{IdP: "auth0", AMR: []string{"mfa"}}, false, "keycloak only, not auth0", ""},
		{"active IdP only", func(p *v1.WorkloadProfileSpec) { p.Sessions = "ActiveIdPOnly" }, "contingency", Session{IdP: "keycloak", ACR: "aal2"}, false, "sign in again", ""},
		{"phishing resistance missing", func(p *v1.WorkloadProfileSpec) { p.Assurance.PhishingResistant = yes() }, "keycloak", Session{IdP: "keycloak", ACR: "aal2"}, false, "phishing-resistant", ""},
		{"max age, fresh", func(p *v1.WorkloadProfileSpec) { p.Assurance.MaxAge = hour }, "keycloak", Session{IdP: "keycloak", ACR: "aal2", AuthTime: now.Add(-10 * time.Minute)}, true, "AAL2", ""},
		{"max age, stale", func(p *v1.WorkloadProfileSpec) { p.Assurance.MaxAge = hour }, "keycloak", Session{IdP: "keycloak", ACR: "aal2", AuthTime: now.Add(-3 * time.Hour)}, false, "signed in 3h0m0s ago", "aal2"},
		{"max age, not asserted", func(p *v1.WorkloadProfileSpec) { p.Assurance.MaxAge = hour }, "keycloak", Session{IdP: "keycloak", ACR: "aal2"}, false, "didn't say when", "aal2"},
	}
	for _, c := range cases {
		p := critical()
		if c.profile != nil {
			c.profile(&p)
		}
		d := Decide(Effective(p, policy), chain(), Current{Active: c.active}, c.s, now)
		if d.Allow != c.allow || !strings.Contains(d.Reason, c.reason) || d.ACRValues != c.stepUp {
			t.Errorf("%s: %+v, want allow=%v reason~%q acr_values=%q", c.name, d, c.allow, c.reason, c.stepUp)
		}
	}
}

func TestPhase(t *testing.T) {
	r := Effective(critical(), policy)
	cases := map[string]string{"auth0": "Available", "keycloak": "Degraded", "contingency": "FailedClosed", "break-glass": "FailedClosed", "": "FailedClosed"}
	for active, want := range cases {
		if got := Phase(r, chain(), active); got != want {
			t.Errorf("active %q: %s, want %s", active, got, want)
		}
	}
	if e := Eligible(r, chain()); strings.Join(e, ",") != "auth0,keycloak" {
		t.Errorf("eligible %v", e)
	}
	std := Effective(v1.WorkloadProfileSpec{Criticality: "Standard", AllowBreakGlass: yes()}, policy)
	if got := Phase(std, chain(), "break-glass"); got != "Degraded" {
		t.Errorf("standard on break-glass: %s", got)
	}
}

// A profile's rules override the policy's where it sets them; elsewhere the
// policy applies, so changing the policy moves every profile that doesn't
// say otherwise.
func TestEffective(t *testing.T) {
	strict := v1.AssurancePolicy{Minimum: "AAL2", Sessions: "ActiveIdPOnly", AllowedIdPs: []string{"keycloak"}, MaxAge: &metav1.Duration{Duration: time.Hour}}
	r := Effective(v1.WorkloadProfileSpec{}, strict)
	if r.Minimum != 2 || !r.ActiveIdPOnly || r.AllowedIdPs[0] != "keycloak" || r.MaxAge != time.Hour || r.AllowBreakGlass {
		t.Fatalf("inherits the policy: %+v", r)
	}
	no := false
	r = Effective(v1.WorkloadProfileSpec{Sessions: "Any", AllowBreakGlass: yes(), AllowedIdPs: []string{"auth0"},
		Assurance: v1.ProfileAssurance{Minimum: "AAL1", PhishingResistant: &no, MaxAge: &metav1.Duration{}}}, strict)
	if r.Minimum != 1 || r.ActiveIdPOnly || r.AllowedIdPs[0] != "auth0" || r.MaxAge != 0 || !r.AllowBreakGlass {
		t.Fatalf("overrides it: %+v", r)
	}
	if r := Effective(v1.WorkloadProfileSpec{}, v1.AssurancePolicy{}); r.Minimum != 1 {
		t.Fatalf("no minimum anywhere is AAL1: %+v", r)
	}
}

func TestProvenTakesTheHighestMatch(t *testing.T) {
	tier := v1.Tier{Name: "okta", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{
		{AMR: "mfa", Level: "AAL2"}, {AMR: "hwk", Level: "AAL3", PhishingResistant: true}}}}
	r := Rules{Minimum: 3, PhishingResistant: true}
	d := Decide(r, []v1.Tier{tier}, Current{Active: "okta"}, Session{IdP: "okta", AMR: []string{"pwd", "mfa", "hwk"}}, time.Now())
	if !d.Allow || d.Reason != "AAL3 via okta (amr hwk)" {
		t.Fatalf("%+v", d)
	}
	if c := Ceiling(tier); c != 3 {
		t.Fatalf("ceiling %s", c)
	}
}

// What a sign-in through each IdP gets: the matrix the Observatory shows and
// the gate's evaluate endpoint answers.
func TestReachOf(t *testing.T) {
	c := chain()
	cases := []struct {
		name    string
		profile func(*v1.WorkloadProfileSpec)
		active  string
		want    map[string]Outcome
		via     map[string]string
	}{
		{name: "AAL2 rules: the IdPs that can assert it, the one that can't, no break-glass",
			active: "auth0",
			want:   map[string]Outcome{"auth0": Conditional, "keycloak": Conditional, "contingency": Refuse, "break-glass": Refuse},
			via:    map[string]string{"auth0": "amr mfa", "keycloak": "acr aal2"}},
		{name: "AAL1 and break-glass: every sign-in",
			profile: func(p *v1.WorkloadProfileSpec) { p.Assurance.Minimum = "AAL1"; p.AllowBreakGlass = yes() },
			want:    map[string]Outcome{"auth0": Admit, "keycloak": Admit, "contingency": Admit, "break-glass": Admit}},
		{name: "active IdP only: the others refused",
			profile: func(p *v1.WorkloadProfileSpec) { p.Sessions = "ActiveIdPOnly" },
			active:  "keycloak",
			want:    map[string]Outcome{"auth0": Refuse, "keycloak": Conditional, "contingency": Refuse}},
		{name: "phishing resistance no IdP maps",
			profile: func(p *v1.WorkloadProfileSpec) { p.Assurance.PhishingResistant = yes() },
			want:    map[string]Outcome{"auth0": Refuse, "keycloak": Refuse}},
		{name: "allowed IdPs",
			profile: func(p *v1.WorkloadProfileSpec) { p.AllowedIdPs = []string{"keycloak"} },
			want:    map[string]Outcome{"auth0": Refuse, "keycloak": Conditional}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := critical()
			if tc.profile != nil {
				tc.profile(&p)
			}
			r := Effective(p, policy)
			for _, tier := range c {
				want, ok := tc.want[tier.Name]
				if !ok {
					continue
				}
				got := ReachOf(r, tier, Current{Active: tc.active})
				if got.Outcome != want || got.Reason == "" {
					t.Errorf("%s: %s (%s), want %s", tier.Name, got.Outcome, got.Reason, want)
				}
				if v := tc.via[tier.Name]; v != "" && got.Via != v {
					t.Errorf("%s: via %q, want %q", tier.Name, got.Via, v)
				}
			}
		})
	}
	if r := ReachOf(Effective(critical(), policy), c[2], Current{}); !strings.Contains(r.Reason, "at most AAL1") {
		t.Errorf("contingency's reason: %s", r.Reason)
	}
}

// A session from an IdP the fabric's routing rules send sign-ins to is a
// session from an IdP signing people in now.
func TestDecideRoutedIdP(t *testing.T) {
	now := time.Now()
	p := v1.WorkloadProfileSpec{Sessions: "ActiveIdPOnly", Assurance: v1.ProfileAssurance{Minimum: "AAL2"}}
	s := Session{IdP: "keycloak", ACR: "aal2"}
	if d := Decide(Effective(p, v1.AssurancePolicy{}), chain(), Current{Active: "contingency", Routed: []string{"keycloak"}}, s, now); !d.Allow {
		t.Fatalf("routed keycloak session refused: %s", d.Reason)
	}
	if d := Decide(Effective(p, v1.AssurancePolicy{}), chain(), Current{Active: "contingency"}, s, now); d.Allow {
		t.Fatal("keycloak session allowed with keycloak neither active nor routed")
	}
}
