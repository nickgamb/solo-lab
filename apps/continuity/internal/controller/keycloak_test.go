package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
)

// Keycloak's copy of an IdP as the admin API returns it: the managed fields
// plus the secret masked.
func fromKeycloak(want keycloak.IdP) keycloak.IdP {
	cur := keycloak.IdP{}
	for k, v := range want {
		cur[k] = v
	}
	cfg := map[string]any{}
	for k, v := range want.Config() {
		cfg[k] = v
	}
	cfg["clientSecret"] = "**********"
	cur["config"] = cfg
	return cur
}

func TestSameIdP(t *testing.T) {
	ic := &v1.IdentityContinuity{}
	ic.Namespace, ic.Name = "sv-identity", "sterling-vance"
	tier := v1.Tier{Name: "auth0", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://tenant.example/"}}
	d := &probe.Discovery{AuthorizationEndpoint: "https://tenant.example/authorize", TokenEndpoint: "https://tenant.example/oauth/token", JWKSURI: "https://tenant.example/jwks"}
	want := desiredIdP(ic, tier, credential{id: "client", secret: "s3cret"}, d, "sv-identity/sterling-vance", false, true)

	if !sameIdP(fromKeycloak(want), want) {
		t.Fatal("an unchanged IdP (secret masked) must compare equal")
	}
	edited := fromKeycloak(want)
	edited.Config()["tokenUrl"] = "https://attacker.example/token"
	if sameIdP(edited, want) {
		t.Error("a token URL changed in Keycloak by hand must be repaired")
	}
	edited = fromKeycloak(want)
	edited["trustEmail"] = false
	if sameIdP(edited, want) {
		t.Error("trustEmail changed by hand must be repaired")
	}
	rotated := desiredIdP(ic, tier, credential{id: "client", secret: "rotated"}, d, "sv-identity/sterling-vance", false, true)
	if sameIdP(fromKeycloak(want), rotated) {
		t.Error("a rotated client secret must be pushed (the hash covers it)")
	}
}

func TestStoreTokens(t *testing.T) {
	ic := &v1.IdentityContinuity{}
	d := &probe.Discovery{AuthorizationEndpoint: "https://op.example/authorize", TokenEndpoint: "https://op.example/token", JWKSURI: "https://op.example/jwks"}
	tier := v1.Tier{Name: "gluu", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://op.example"}}
	off := desiredIdP(ic, tier, credential{id: "c", secret: "s"}, d, "o", false, true)
	if off["storeToken"] != false {
		t.Fatalf("storeToken = %v, want false by default", off["storeToken"])
	}
	tier.OIDC.StoreTokens = true
	on := desiredIdP(ic, tier, credential{id: "c", secret: "s"}, d, "o", false, true)
	if on["storeToken"] != true {
		t.Fatalf("storeToken = %v, want true", on["storeToken"])
	}
	if sameIdP(fromKeycloak(off), on) {
		t.Error("turning storeTokens on must update the IdP")
	}
}

// private_key_jwt: the broker signs a client assertion with the realm key for
// the algorithm; no client secret reaches Keycloak's IdP config.
func TestClientAuth(t *testing.T) {
	ic := &v1.IdentityContinuity{}
	d := &probe.Discovery{AuthorizationEndpoint: "https://op.example/authorize", TokenEndpoint: "https://op.example/token", JWKSURI: "https://op.example/jwks"}
	tier := v1.Tier{Name: "gluu", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://op.example", ClientID: "sv", ClientAuth: "private_key_jwt"}}
	cfg := desiredIdP(ic, tier, credential{id: "sv"}, d, "o", false, true).Config()
	if cfg["clientAuthMethod"] != "private_key_jwt" || cfg["clientAssertionSigningAlg"] != "PS256" {
		t.Fatalf("config %v", cfg)
	}
	if _, ok := cfg["clientSecret"]; ok {
		t.Fatal("private_key_jwt config carries a clientSecret")
	}
	tier.OIDC.ClientAuth = ""
	cfg = desiredIdP(ic, tier, credential{id: "sv", secret: "s"}, d, "o", false, true).Config()
	if cfg["clientAuthMethod"] != "client_secret_post" || cfg["clientSecret"] != "s" {
		t.Fatalf("default config %v", cfg)
	}
}

// adminAPI is a small in-memory Keycloak admin API: identity providers, their
// mappers, and one browser flow with a redirector.
type adminAPI struct {
	idps         map[string]keycloak.IdP
	deleted      []string
	cacheCleared int // clear-user-cache calls
	redirect     string
}

func (a *adminAPI) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		const pre = "/admin/realms/r"
		switch {
		case strings.HasSuffix(p, "/protocol/openid-connect/token"):
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 60})
		case p == pre+"/identity-provider/instances" && r.Method == http.MethodGet:
			out := []keycloak.IdP{}
			for _, v := range a.idps {
				out = append(out, v)
			}
			json.NewEncoder(w).Encode(out)
		case p == pre+"/identity-provider/instances" && r.Method == http.MethodPost:
			var v keycloak.IdP
			json.NewDecoder(r.Body).Decode(&v)
			a.idps[v["alias"].(string)] = v
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(p, "/mappers"):
			if r.Method == http.MethodGet {
				w.Write([]byte(`[{"id":"u","name":"username-from-email","identityProviderMapper":"oidc-username-idp-mapper","config":{"template":"${CLAIM.email}","target":"LOCAL","syncMode":"INHERIT"}},` +
					`{"id":"a","name":"continuity-assurance","identityProviderMapper":"continuity-session-claims-idp-mapper","config":{"claims":"acr,amr,auth_time","note.prefix":"continuity.","syncMode":"FORCE"}}]`))
			}
		case strings.HasPrefix(p, pre+"/identity-provider/instances/"):
			alias := strings.TrimPrefix(p, pre+"/identity-provider/instances/")
			switch r.Method {
			case http.MethodPut:
				var v keycloak.IdP
				json.NewDecoder(r.Body).Decode(&v)
				a.idps[alias] = v
			case http.MethodDelete:
				delete(a.idps, alias)
				a.deleted = append(a.deleted, alias)
			}
		case p == pre+"/clear-user-cache" && r.Method == http.MethodPost:
			a.cacheCleared++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/executions"):
			w.Write([]byte(`[{"id":"e1","providerId":"identity-provider-redirector","authenticationConfig":"c1"}]`))
		case p == pre+"/authentication/config/c1" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"id": "c1", "alias": "continuity", "config": map[string]string{"defaultProvider": a.redirect}})
		case p == pre+"/authentication/config/c1" && r.Method == http.MethodPut:
			var c struct {
				Config map[string]string `json:"config"`
			}
			json.NewDecoder(r.Body).Decode(&c)
			a.redirect = c.Config["defaultProvider"]
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// A tier whose credentials Secret goes missing keeps its IdP (hidden), so
// every user's link to it survives; only removing the tier deletes it.
func TestReconcileKeycloakMissingSecretKeepsIdP(t *testing.T) {
	owner := "sv-identity/sterling-vance"
	api := &adminAPI{idps: map[string]keycloak.IdP{
		"auth0": {"alias": "auth0", "hideOnLogin": false, "enabled": true, "config": map[string]any{cfgInstance: owner}},
	}, redirect: "auth0"}
	srv := api.serve(t)
	defer srv.Close()
	kc := keycloak.New(srv.URL, "r")
	kc.SetCredentials("id", "secret")

	ic := &v1.IdentityContinuity{Spec: v1.IdentityContinuitySpec{
		Broker: v1.Broker{Keycloak: v1.KeycloakBroker{BrowserFlow: "continuity-browser"}},
		Tiers: []v1.Tier{
			{Name: "auth0", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://tenant.example/"}},
			{Name: "keycloak", Type: "local"},
		}}}
	ic.Namespace, ic.Name = "sv-identity", "sterling-vance"
	r := &Reconciler{discovery: map[string]*probe.Discovery{}}
	creds := map[string]credential{"auth0": {missing: "secret sv-identity/upstream-auth0 not found"}}
	st := map[string]*v1.TierStatus{"auth0": {Configured: false}, "keycloak": {Configured: true, Healthy: true}}

	effective, applied, _, err := r.reconcileKeycloak(context.Background(), ic, kc, creds, st, "keycloak", nil)
	if err != nil || !applied {
		t.Fatalf("reconcile: applied=%v err=%v", applied, err)
	}
	if _, ok := api.idps["auth0"]; !ok || len(api.deleted) > 0 {
		t.Fatalf("auth0 IdP deleted with its Secret missing (deleted=%v)", api.deleted)
	}
	if api.idps["auth0"]["hideOnLogin"] != true {
		t.Error("auth0 still offered on the login page without credentials")
	}
	if api.redirect != "" || effective != "keycloak" {
		t.Errorf("redirect %q, effective %q: want the local form", api.redirect, effective)
	}

	ic.Spec.Tiers = ic.Spec.Tiers[1:] // the tier itself removed from the spec
	if _, _, _, err := r.reconcileKeycloak(context.Background(), ic, kc, map[string]credential{}, st, "keycloak", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.idps["auth0"]; ok {
		t.Error("a tier removed from the spec keeps its IdP")
	}
}

// An update puts back what the controller manages and leaves the rest of
// the IdP as Keycloak has it.
func TestReconcileKeycloakKeepsUnmanagedConfig(t *testing.T) {
	owner := "sv-identity/sterling-vance"
	api := &adminAPI{idps: map[string]keycloak.IdP{
		"auth0": {"alias": "auth0", "guiOrder": "3", "config": map[string]any{cfgInstance: owner,
			"tokenUrl": "https://attacker.example/token", "prompt": "login"}},
	}}
	srv := api.serve(t)
	defer srv.Close()
	kc := keycloak.New(srv.URL, "r")
	kc.SetCredentials("id", "secret")
	ic := &v1.IdentityContinuity{Spec: v1.IdentityContinuitySpec{
		Broker: v1.Broker{Keycloak: v1.KeycloakBroker{BrowserFlow: "continuity-browser"}},
		Tiers:  []v1.Tier{{Name: "auth0", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://tenant.example/"}}},
	}}
	ic.Namespace, ic.Name = "sv-identity", "sterling-vance"
	r := &Reconciler{discovery: map[string]*probe.Discovery{}}
	r.discovery[discoveryKey(ic, ic.Spec.Tiers[0])] = &probe.Discovery{AuthorizationEndpoint: "https://tenant.example/authorize",
		TokenEndpoint: "https://tenant.example/oauth/token", JWKSURI: "https://tenant.example/jwks"}
	st := map[string]*v1.TierStatus{"auth0": {Configured: true, Healthy: true}}
	if _, _, _, err := r.reconcileKeycloak(context.Background(), ic, kc, map[string]credential{"auth0": {id: "c", secret: "s"}}, st, "auth0", nil); err != nil {
		t.Fatal(err)
	}
	p := api.idps["auth0"]
	cfg := p.Config()
	if cfg["tokenUrl"] != "https://tenant.example/oauth/token" || p["enabled"] != true {
		t.Fatalf("managed fields not put back: %v", p)
	}
	if cfg["prompt"] != "login" || p["guiOrder"] != "3" {
		t.Fatalf("a field set in Keycloak was dropped: %v", p)
	}
}

func TestPruneDiscovery(t *testing.T) {
	ic := &v1.IdentityContinuity{Spec: v1.IdentityContinuitySpec{Tiers: []v1.Tier{
		{Name: "auth0", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://tenant.example/"}}}}}
	ic.Namespace, ic.Name = "sv-identity", "sterling-vance"
	other := &v1.IdentityContinuity{Spec: ic.Spec}
	other.Namespace, other.Name = "sv-identity", "other"
	gone := v1.Tier{Name: "gluu", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://gluu.example"}}
	moved := v1.Tier{Name: "auth0", Type: "oidc", OIDC: &v1.OIDCUpstream{Issuer: "https://old.example/"}}
	d := &probe.Discovery{}
	r := &Reconciler{discovery: map[string]*probe.Discovery{
		discoveryKey(ic, ic.Spec.Tiers[0]): d, discoveryKey(ic, gone): d, discoveryKey(ic, moved): d,
		discoveryKey(other, gone): d,
	}}
	r.pruneDiscovery(ic)
	if len(r.discovery) != 2 || r.discovery[discoveryKey(ic, ic.Spec.Tiers[0])] == nil || r.discovery[discoveryKey(other, gone)] == nil {
		t.Fatalf("cache %v: this instance's tiers and other instances' entries only", r.discovery)
	}
}
