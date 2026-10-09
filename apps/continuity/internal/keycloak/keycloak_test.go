package keycloak

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeKeycloak serves a token endpoint and one IdP's mapper list.
func fakeKeycloak(t *testing.T, mappers *[]map[string]any, writes *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/identity-provider/instances/auth0/mappers"
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 60})
		case strings.HasSuffix(r.URL.Path, base) && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(*mappers)
		case strings.HasSuffix(r.URL.Path, base) && r.Method == http.MethodPost:
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			m["id"] = fmt.Sprintf("m%d", len(*mappers))
			*mappers = append(*mappers, m)
			*writes++
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, base+"/") && r.Method == http.MethodPut:
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			for i := range *mappers {
				if (*mappers)[i]["id"] == strings.TrimPrefix(r.URL.Path[strings.Index(r.URL.Path, base):], base+"/") {
					(*mappers)[i] = m
				}
			}
			*writes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestEnsureMapper(t *testing.T) {
	var mappers []map[string]any
	writes := 0
	srv := fakeKeycloak(t, &mappers, &writes)
	defer srv.Close()
	c := New(srv.URL, "r")
	c.SetCredentials("id", "secret")
	for i := 0; i < 2; i++ { // idempotent: the second call finds it
		if err := c.EnsureMapper(context.Background(), "auth0", UsernameMapper); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatalf("want one mapper created, got %d writes", writes)
	}
	m := mappers[0]
	cfg, _ := m["config"].(map[string]any)
	if m["identityProviderMapper"] != "oidc-username-idp-mapper" || cfg["template"] != "${CLAIM.email}" {
		t.Errorf("mapper: %+v", m)
	}

	// changed in Keycloak: put back, keeping config keys it doesn't set
	cfg["template"], cfg["extra"] = "${CLAIM.sub}", "kept"
	if err := c.EnsureMapper(context.Background(), "auth0", UsernameMapper); err != nil {
		t.Fatal(err)
	}
	cfg, _ = mappers[0]["config"].(map[string]any)
	if writes != 2 || cfg["template"] != "${CLAIM.email}" || cfg["extra"] != "kept" {
		t.Errorf("after a hand edit: %d writes, config %v", writes, cfg)
	}
}

func TestAssuranceMapperOnEverySignIn(t *testing.T) {
	// FORCE: the mapper runs on every sign-in, so each session gets the notes
	// of the authentication that made it, not of the user's first sign-in.
	if AssuranceMapper.Config["syncMode"] != "FORCE" || AssuranceMapper.Config["claims"] != "acr,amr,auth_time" {
		t.Fatalf("assurance mapper %+v", AssuranceMapper)
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

func TestGroupMapperReadsANamespacedClaimWhole(t *testing.T) {
	m := GroupMapper("https://sterling.lab/groups", "advisors")
	if m.Name != "group advisors" || m.Type != "oidc-advanced-group-idp-mapper" {
		t.Fatalf("mapper %s %s", m.Name, m.Type)
	}
	if want := `[{"key":"https://sterling\\.lab/groups","value":"advisors"}]`; m.Config["claims"] != want {
		t.Fatalf("claims %s, want %s", m.Config["claims"], want)
	}
	if m.Config["group"] != "/advisors" || m.Config["syncMode"] != "FORCE" || m.Config["are.claim.values.regex"] != "false" {
		t.Fatalf("config %v", m.Config)
	}
}
