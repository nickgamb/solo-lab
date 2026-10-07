package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nickgamb/solo-lab/apps/continuity/internal/tiers"
)

// upstream serves a discovery document (edited by mutate) and a JWKS
// (failing with jwksStatus, if set).
func upstream(t *testing.T, mutate func(d map[string]any), status int, jwksStatus ...int) (*httptest.Server, *Prober) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			d := map[string]any{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
				"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks"}
			if mutate != nil {
				mutate(d)
			}
			json.NewEncoder(w).Encode(d)
		case "/jwks":
			if len(jwksStatus) > 0 {
				w.WriteHeader(jwksStatus[0])
				return
			}
			w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &Prober{client: srv.Client()}
}

func TestOIDC(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		status int
		want   string
		msg    string
		jwks   []int // the JWKS answers with this status
	}{
		{"healthy", nil, 0, tiers.Healthy, "discovery and jwks ok", nil},
		{"jwks 5xx", nil, 0, tiers.ServerError, "jwks: HTTP 503", []int{http.StatusServiceUnavailable}},
		{"5xx", nil, http.StatusBadGateway, tiers.ServerError, "HTTP 502", nil},
		{"4xx is invalid discovery", nil, http.StatusNotFound, tiers.InvalidDiscovery, "HTTP 404", nil},
		{"issuer mismatch", func(d map[string]any) { d["issuer"] = "https://evil.example/" }, 0, tiers.InvalidDiscovery, "want", nil},
		{"missing jwks", func(d map[string]any) { delete(d, "jwks_uri") }, 0, tiers.InvalidDiscovery, "lacks", nil},
		{"plain-http token endpoint", func(d map[string]any) {
			d["token_endpoint"] = strings.Replace(d["token_endpoint"].(string), "https://", "http://", 1)
		}, 0, tiers.InvalidDiscovery, "not https", nil},
		{"plain-http userinfo", func(d map[string]any) { d["userinfo_endpoint"] = "http://idp.example/userinfo" }, 0, tiers.InvalidDiscovery, "not https", nil},
	}
	for _, c := range cases {
		srv, p := upstream(t, c.mutate, c.status, c.jwks...)
		res, d := p.OIDC(context.Background(), srv.URL, 2*time.Second)
		if res.Kind != c.want || !strings.Contains(res.Message, c.msg) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, res.Kind, res.Message, c.want, c.msg)
		}
		// a valid discovery document comes back even when its JWKS fails
		if valid := c.want == tiers.Healthy || c.jwks != nil; (d != nil) != valid {
			t.Errorf("%s: discovery returned=%v", c.name, d != nil)
		}
	}
}

func TestOIDCUnreachable(t *testing.T) {
	srv, p := upstream(t, nil, 0)
	url := srv.URL
	srv.Close()
	if res, _ := p.OIDC(context.Background(), url, time.Second); res.Kind != tiers.Unreachable {
		t.Errorf("closed server: %s %q", res.Kind, res.Message)
	}
}

// The callback check: a registered callback is where the IdP answers a
// prompt=none request; anything else isn't, and a 5xx says nothing either way.
func TestCallback(t *testing.T) {
	const cb = "https://idp.sterling.lab/realms/sterling-vance/broker/keycloak/endpoint"
	cases := []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
		want   string
	}{
		{"registered", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("prompt") != "none" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != cb {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			http.Redirect(w, r, cb+"?error=login_required&state="+q.Get("state"), http.StatusFound)
		}, CallbackRegistered},
		{"refused with 400", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }, CallbackRefused},
		{"refused to an error page", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/error?error=invalid_request", http.StatusFound)
		}, CallbackRefused},
		{"login page shown", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<form>")) }, CallbackUnknown},
		{"5xx", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }, CallbackUnknown},
	}
	for _, c := range cases {
		srv := httptest.NewTLSServer(http.HandlerFunc(c.answer))
		p := &Prober{client: srv.Client()}
		p.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		got, msg := p.Callback(context.Background(), srv.URL+"/authorize", "sterling-vance-broker", cb, time.Second)
		if got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, msg, c.want)
		}
		srv.Close()
	}
}
