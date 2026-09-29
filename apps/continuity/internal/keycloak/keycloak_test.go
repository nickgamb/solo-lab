package keycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeKeycloak serves a token endpoint and one IdP's mapper list.
func fakeKeycloak(t *testing.T, mappers *[]map[string]any, posts *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 60})
		case strings.HasSuffix(r.URL.Path, "/identity-provider/instances/auth0/mappers") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(*mappers)
		case strings.HasSuffix(r.URL.Path, "/identity-provider/instances/auth0/mappers") && r.Method == http.MethodPost:
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			*mappers = append(*mappers, m)
			*posts++
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestEnsureUsernameFromEmail(t *testing.T) {
	var mappers []map[string]any
	posts := 0
	srv := fakeKeycloak(t, &mappers, &posts)
	defer srv.Close()
	c := New(srv.URL, "r")
	c.SetCredentials("id", "secret")
	for i := 0; i < 2; i++ { // idempotent: the second call finds it
		if err := c.EnsureUsernameFromEmail(context.Background(), "auth0"); err != nil {
			t.Fatal(err)
		}
	}
	if posts != 1 {
		t.Fatalf("want one mapper created, got %d", posts)
	}
	m := mappers[0]
	cfg, _ := m["config"].(map[string]any)
	if m["identityProviderMapper"] != "oidc-username-idp-mapper" || cfg["template"] != "${CLAIM.email}" {
		t.Errorf("mapper: %+v", m)
	}
}
