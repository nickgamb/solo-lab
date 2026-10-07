package trust

import (
	"testing"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
)

func keycloakTier() v1.Tier {
	return v1.Tier{Name: "keycloak", Type: "oidc",
		OIDC:      &v1.OIDCUpstream{Issuer: "https://login.sterling.lab/realms/workforce", ClientAuth: "private_key_jwt"},
		Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{ACR: "aal1", Level: "AAL1"}, {ACR: "aal2", Level: "AAL2"}}, Default: "AAL1"}}
}

// what a Keycloak realm publishes
func keycloakDiscovery() *probe.Discovery {
	return &probe.Discovery{
		ScopesSupported:                   []string{"openid", "email", "profile", "acr"},
		ClaimsSupported:                   []string{"sub", "iss", "email", "email_verified", "acr"},
		ACRValuesSupported:                []string{"0", "1", "aal1", "aal2"},
		TokenEndpointAuthMethodsSupported: []string{"client_secret_post", "private_key_jwt"},
		TokenEndpointAuthSigningAlgs:      []string{"RS256", "PS256"},
		CodeChallengeMethodsSupported:     []string{"plain", "S256"},
	}
}

func results(cs []v1.TrustCheck) map[string]string {
	out := map[string]string{}
	for _, c := range cs {
		out[c.Name] = c.Result
	}
	return out
}

func TestRegistrationThatHolds(t *testing.T) {
	cs := Checks(keycloakTier(), keycloakDiscovery(), probe.CallbackRegistered, "ok")
	for name, r := range results(cs) {
		if r != Pass {
			t.Errorf("%s: %s", name, r)
		}
	}
	if len(cs) != 6 || len(Failed(cs)) != 0 {
		t.Fatalf("checks %+v", cs)
	}
}

func TestEachMismatchFails(t *testing.T) {
	cases := []struct {
		check  string
		tier   func(*v1.Tier)
		disc   func(*probe.Discovery)
		refuse bool
	}{
		{Callback, nil, nil, true},
		{ClientAuth, nil, func(d *probe.Discovery) { d.TokenEndpointAuthMethodsSupported = []string{"client_secret_basic"} }, false},
		{ClientAuth, nil, func(d *probe.Discovery) { d.TokenEndpointAuthSigningAlgs = []string{"RS256"} }, false},
		{PKCE, nil, func(d *probe.Discovery) { d.CodeChallengeMethodsSupported = []string{"plain"} }, false},
		{Scopes, func(t *v1.Tier) { t.OIDC.Scopes = []string{"openid", "email", "offline_access"} }, nil, false},
		{Claims, nil, func(d *probe.Discovery) { d.ClaimsSupported = []string{"sub", "name"} }, false},
		{Assurance, nil, func(d *probe.Discovery) { d.ACRValuesSupported = []string{"0", "1"} }, false},
	}
	for _, c := range cases {
		tier, d := keycloakTier(), keycloakDiscovery()
		if c.tier != nil {
			c.tier(&tier)
		}
		if c.disc != nil {
			c.disc(d)
		}
		cb := probe.CallbackRegistered
		if c.refuse {
			cb = probe.CallbackRefused
		}
		got := Failed(Checks(tier, d, cb, "msg"))
		if len(got) != 1 || got[0] != c.check {
			t.Errorf("%s: failed %v", c.check, got)
		}
	}
}

// Many IdPs publish only part of discovery: what isn't published is Unknown,
// never a failure.
func TestUnpublishedIsUnknown(t *testing.T) {
	tier := keycloakTier()
	cs := Checks(tier, &probe.Discovery{}, probe.CallbackUnknown, "HTTP 200")
	if f := Failed(cs); len(f) != 0 {
		t.Fatalf("failed %v with nothing published", f)
	}
	for name, r := range results(cs) {
		if r != Unknown {
			t.Errorf("%s: %s, want Unknown", name, r)
		}
	}
}

func TestAssuranceByAmrOrDefaultOnly(t *testing.T) {
	tier := keycloakTier()
	tier.Assurance = &v1.TierAssurance{Levels: []v1.AssuranceLevel{{AMR: "mfa", Level: "AAL2"}}}
	d := keycloakDiscovery()
	d.ClaimsSupported = append(d.ClaimsSupported, "amr")
	if r := results(Checks(tier, d, probe.CallbackRegistered, ""))[Assurance]; r != Pass {
		t.Errorf("amr mapped and issued: %s", r)
	}
	tier.Assurance = nil
	if r := results(Checks(tier, d, probe.CallbackRegistered, ""))[Assurance]; r != Pass {
		t.Errorf("nothing mapped: %s", r)
	}
	// acr and amr mapped (Auth0): an acr the IdP doesn't list isn't a
	// failure while amr can still match
	tier.Assurance = &v1.TierAssurance{Levels: []v1.AssuranceLevel{{AMR: "mfa", Level: "AAL2"}, {ACR: "multi-factor", Level: "AAL2"}}}
	if r := results(Checks(tier, d, probe.CallbackRegistered, ""))[Assurance]; r != Pass {
		t.Errorf("acr unlisted, amr issued: %s", r)
	}
	d.ClaimsSupported = []string{"sub", "email", "email_verified"}
	if r := results(Checks(tier, d, probe.CallbackRegistered, ""))[Assurance]; r != Unknown {
		t.Errorf("acr unlisted, amr not listed: %s", r)
	}
}
