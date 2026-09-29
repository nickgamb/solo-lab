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

// upstream serves a discovery document (edited by mutate) and a JWKS.
func upstream(t *testing.T, mutate func(d map[string]any), status int) (*httptest.Server, *Prober) {
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
	}{
		{"healthy", nil, 0, tiers.Healthy, "discovery and jwks ok"},
		{"5xx", nil, http.StatusBadGateway, tiers.ServerError, "HTTP 502"},
		{"4xx is invalid discovery", nil, http.StatusNotFound, tiers.InvalidDiscovery, "HTTP 404"},
		{"issuer mismatch", func(d map[string]any) { d["issuer"] = "https://evil.example/" }, 0, tiers.InvalidDiscovery, "want"},
		{"missing jwks", func(d map[string]any) { delete(d, "jwks_uri") }, 0, tiers.InvalidDiscovery, "lacks"},
		{"plain-http token endpoint", func(d map[string]any) {
			d["token_endpoint"] = strings.Replace(d["token_endpoint"].(string), "https://", "http://", 1)
		}, 0, tiers.InvalidDiscovery, "not https"},
		{"plain-http userinfo", func(d map[string]any) { d["userinfo_endpoint"] = "http://idp.example/userinfo" }, 0, tiers.InvalidDiscovery, "not https"},
	}
	for _, c := range cases {
		srv, p := upstream(t, c.mutate, c.status)
		res, d := p.OIDC(context.Background(), srv.URL, 2*time.Second)
		if res.Kind != c.want || !strings.Contains(res.Message, c.msg) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, res.Kind, res.Message, c.want, c.msg)
		}
		if (d != nil) != (c.want == tiers.Healthy) {
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
