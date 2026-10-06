package profilesync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
)

type fakeBroker struct {
	users   map[string]keycloak.User     // by id
	links   map[string]map[string]string // user id -> alias -> sub
	local   map[string]bool
	updated []keycloak.User
}

func (f *fakeBroker) LinkedUsers(_ context.Context, alias string, first, max int) ([]keycloak.User, error) {
	var out []keycloak.User
	for id, l := range f.links {
		if _, ok := l[alias]; ok {
			out = append(out, f.users[id])
		}
	}
	slices.SortFunc(out, func(a, b keycloak.User) int { return strings.Compare(a["id"].(string), b["id"].(string)) })
	if first >= len(out) {
		return nil, nil
	}
	return out[first:min(first+max, len(out))], nil
}

func (f *fakeBroker) FederatedIdentity(_ context.Context, id, alias string) (string, error) {
	if s, ok := f.links[id][alias]; ok {
		return s, nil
	}
	return "", keycloak.ErrNotFound
}

func (f *fakeBroker) HasRealmRole(_ context.Context, id, role string) (bool, error) {
	return f.local[id] && role == LocalOnlyRole, nil
}

func (f *fakeBroker) UpdateUser(_ context.Context, u keycloak.User) error {
	f.updated = append(f.updated, u)
	return nil
}

type fakeDir struct {
	recs map[string]map[string]any
	err  error
}

func (d fakeDir) User(_ context.Context, sub string) (map[string]any, error) {
	if d.err != nil {
		return nil, d.err
	}
	if r, ok := d.recs[sub]; ok {
		return r, nil
	}
	return nil, ErrNoUser
}

var writable = []string{"firstName", "lastName", "email", "department", "costCenter"}

func bob() keycloak.User {
	return keycloak.User{"id": "bob", "username": "bob@sterling.lab", "email": "bob@sterling.lab", "firstName": "Bob",
		"attributes": map[string]any{"department": []any{"old"}}}
}

func TestPrimaryWinsAndIdentityKeysAreNeverWritten(t *testing.T) {
	b := &fakeBroker{users: map[string]keycloak.User{"bob": bob()},
		links: map[string]map[string]string{"bob": {"gluu": "g-bob", "auth0": "a-bob"}}}
	tiers := []Tier{
		{Name: "gluu", Dir: fakeDir{recs: map[string]map[string]any{"g-bob": {"given_name": "Robert", "department": "Advisory", "email": "evil@x"}}},
			Claims: []v1.ClaimMapping{{Claim: "given_name", Attribute: "firstName"}, {Claim: "department", Attribute: "department"}, {Claim: "email", Attribute: "email"}}},
		{Name: "auth0", Dir: fakeDir{recs: map[string]map[string]any{"a-bob": {"given_name": "Bobby", "family_name": "Smith",
			"app_metadata": map[string]any{"cost": "CC-7"}}}},
			Claims: []v1.ClaimMapping{{Claim: "given_name", Attribute: "firstName"}, {Claim: "family_name", Attribute: "lastName"},
				{Claim: "https://sv/cost", Attribute: "costCenter", DirectoryPath: "app_metadata.cost"}}},
	}
	res := Run(context.Background(), b, tiers, writable, func(string, ...any) {})
	if res.Users != 1 || res.Updated != 1 || res.Failed != 0 || len(b.updated) != 1 {
		t.Fatalf("result %+v", res)
	}
	u := b.updated[0]
	attrs := u["attributes"].(map[string]any)
	if u["firstName"] != "Robert" || u["lastName"] != "Smith" || u["email"] != "bob@sterling.lab" {
		t.Fatalf("user %v: primary first name, secondary last name, email untouched", u)
	}
	if !sameValues(attrs["department"], []string{"Advisory"}) || !sameValues(attrs["costCenter"], []string{"CC-7"}) {
		t.Fatalf("attributes %v", attrs)
	}
}

func TestUnreadablePrimaryKeepsItsAttributes(t *testing.T) {
	b := &fakeBroker{users: map[string]keycloak.User{"bob": bob()},
		links: map[string]map[string]string{"bob": {"gluu": "g-bob", "auth0": "a-bob"}}}
	tiers := []Tier{
		{Name: "gluu", Dir: fakeDir{err: errors.New("unreachable")}, Claims: []v1.ClaimMapping{{Claim: "department", Attribute: "department"}}},
		{Name: "auth0", Dir: fakeDir{recs: map[string]map[string]any{"a-bob": {"department": "Retail", "family_name": "Smith"}}},
			Claims: []v1.ClaimMapping{{Claim: "department", Attribute: "department"}, {Claim: "family_name", Attribute: "lastName"}}},
	}
	res := Run(context.Background(), b, tiers, writable, func(string, ...any) {})
	if res.Failed != 1 || len(b.updated) != 1 {
		t.Fatalf("result %+v", res)
	}
	attrs := b.updated[0]["attributes"].(map[string]any)
	if !sameValues(attrs["department"], []string{"old"}) || b.updated[0]["lastName"] != "Smith" {
		t.Fatalf("the secondary overwrote the primary's attribute: %v", b.updated[0])
	}
}

func TestLocalOnlyAndUnchangedUsersAreNotWritten(t *testing.T) {
	ops := keycloak.User{"id": "ops", "firstName": "Ops"}
	b := &fakeBroker{users: map[string]keycloak.User{"bob": bob(), "ops": ops},
		links: map[string]map[string]string{"bob": {"gluu": "g-bob"}, "ops": {"gluu": "g-ops"}}, local: map[string]bool{"ops": true}}
	tiers := []Tier{{Name: "gluu", Dir: fakeDir{recs: map[string]map[string]any{
		"g-bob": {"given_name": "Bob", "department": "old"}, "g-ops": {"given_name": "Mallory"}}},
		Claims: []v1.ClaimMapping{{Claim: "given_name", Attribute: "firstName"}, {Claim: "department", Attribute: "department"}}}}
	res := Run(context.Background(), b, tiers, writable, func(string, ...any) {})
	if res.Users != 1 || res.Updated != 0 || len(b.updated) != 0 {
		t.Fatalf("result %+v, updated %v", res, b.updated)
	}
}

func TestSCIMDirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if id, sec, _ := r.BasicAuth(); id != "sync" || sec != "s" || r.PostForm.Get("grant_type") != "client_credentials" ||
				r.PostForm.Get("scope") != "https://jans.io/scim/users.read" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		case "/scim/v2/Users":
			if r.Header.Get("Authorization") != "Bearer t" || r.URL.Query().Get("count") != "1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"totalResults":42,"Resources":[{"id":"inum-bob"}]}`))
		case "/scim/v2/Users/inum-bob":
			if r.Header.Get("Authorization") != "Bearer t" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"id":"inum-bob","userName":"bob","name":{"givenName":"Bob","familyName":"Smith"},
				"emails":[{"value":"other@x"},{"value":"bob@sterling.lab","primary":true}],
				"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Advisory"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d, _ := New("scim", srv.URL+"/scim/v2", Credentials{TokenURL: srv.URL + "/token", ClientID: "sync", ClientSecret: "s",
		Scopes: []string{"https://jans.io/scim/users.read"}}, srv.Client())
	r, err := d.User(context.Background(), "inum-bob")
	if err != nil {
		t.Fatal(err)
	}
	if r["given_name"] != "Bob" || r["family_name"] != "Smith" || r["email"] != "bob@sterling.lab" || r["preferred_username"] != "bob" {
		t.Fatalf("record %v", r)
	}
	if v := Lookup(r, "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department"); v != "Advisory" {
		t.Fatalf("extension lookup %v", v)
	}
	if n, err := d.(Checker).Check(context.Background()); err != nil || n != 42 {
		t.Fatalf("check: %d %v", n, err)
	}
	if _, err := d.User(context.Background(), "nobody"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("missing user: %v", err)
	}
}

func TestKeycloakDirectoryNormalises(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
			return
		}
		w.Write([]byte(`{"id":"u1","username":"bob","firstName":"Bob","lastName":"Smith","attributes":{"department":["Advisory"],"roles":["a","b"]}}`))
	}))
	defer srv.Close()
	d, _ := New("keycloak", srv.URL+"/admin/realms/r", Credentials{TokenURL: srv.URL + "/token", ClientID: "c", ClientSecret: "s"}, srv.Client())
	r, err := d.User(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r["given_name"] != "Bob" || r["department"] != "Advisory" || len(Values(r["roles"])) != 2 {
		t.Fatalf("record %v", r)
	}
}

func TestValues(t *testing.T) {
	for _, c := range []struct {
		in   any
		want []string
	}{{nil, nil}, {"", nil}, {"a", []string{"a"}}, {[]any{"a", "b"}, []string{"a", "b"}}, {true, []string{"true"}}, {map[string]any{"x": 1}, nil}} {
		if got := Values(c.in); !slices.Equal(got, c.want) {
			t.Errorf("Values(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
