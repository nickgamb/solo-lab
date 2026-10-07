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

func TestDeleteIdPClearsTheUserCache(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 60})
			return
		}
		calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/admin/realms/r"))
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := New(srv.URL, "r")
	c.SetCredentials("id", "secret")
	if err := c.DeleteIdP(context.Background(), "keycloak"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteIdP(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /identity-provider/instances/keycloak", "POST /clear-user-cache", "DELETE /identity-provider/instances/gone"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q: the cache is cleared after a delete, not when there was nothing to delete", calls, want)
	}
}

func TestErrorsCarryKeycloaksMessageOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 60})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"errorMessage": strings.Repeat("x", 1000), "params": []string{"bob@sterling.lab"}})
	}))
	defer srv.Close()
	c := New(srv.URL, "r")
	c.SetCredentials("id", "secret")
	_, err := c.Users(context.Background(), 0, 10)
	if err == nil || strings.Contains(err.Error(), "bob@") || strings.Contains(err.Error(), "first=") || len(err.Error()) > 400 {
		t.Fatalf("error %q: Keycloak's message, bounded, never params or the query", err)
	}
}

func TestRedirectsNotFollowed(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect: %s %s", r.Method, r.URL.Path)
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := New(srv.URL, "r")
	c.SetCredentials("id", "secret")
	if _, err := c.IdPs(context.Background()); err == nil {
		t.Fatal("a redirected token request succeeded")
	}
}
