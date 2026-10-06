package profilesync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
)

type fakeBroker struct {
	users   []keycloak.User
	links   map[string]map[string]string // user id -> idp -> id there
	local   map[string]bool
	updated []keycloak.User
}

func (f *fakeBroker) Users(_ context.Context, first, max int) ([]keycloak.User, error) {
	if first >= len(f.users) {
		return nil, nil
	}
	return f.users[first:min(first+max, len(f.users))], nil
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

// fakeDir is a directory of flat records (path -> value), as Lookup reads them.
type fakeDir struct {
	recs     map[string]map[string]any
	err      error
	noCreate bool
	drop     string // a path it silently doesn't keep
	writes   []map[string][]string
	created  []string
}

func (d *fakeDir) User(_ context.Context, id string) (map[string]any, error) {
	if d.err != nil {
		return nil, d.err
	}
	if r, ok := d.recs[id]; ok {
		return r, nil
	}
	return nil, ErrNoUser
}

func (d *fakeDir) Find(_ context.Context, email string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	for id, r := range d.recs {
		if r["email"] == email {
			return id, nil
		}
	}
	return "", ErrNoUser
}

func (d *fakeDir) Update(_ context.Context, id string, set map[string][]string) error {
	d.writes = append(d.writes, set)
	for p, v := range set {
		if p != d.drop {
			d.recs[id][p] = one(v)
		}
	}
	return nil
}

func (d *fakeDir) Create(_ context.Context, email string, set map[string][]string) (string, error) {
	if d.noCreate {
		return "", ErrNoCreate
	}
	id := "new-" + email
	d.recs[id] = map[string]any{"email": email}
	d.created = append(d.created, email)
	for p, v := range set {
		d.recs[id][p] = one(v)
	}
	return id, nil
}

func (d *fakeDir) Enroll(context.Context, string) error     { return nil }
func (d *fakeDir) Check(context.Context) (int, error)       { return len(d.recs), d.err }
func (d *fakeDir) Schema(context.Context) ([]string, error) { return nil, nil }

var writable = []string{"username", "email", "firstName", "lastName", "department"}

func bob() keycloak.User {
	return keycloak.User{"id": "bob", "username": "bob", "email": "bob@sterling.lab", "firstName": "Bob",
		"attributes": map[string]any{"department": []any{"old"}}}
}

var maps = []v1.AttributeMapping{{Attribute: "department", Path: "department"}, {Attribute: "firstName", Path: "given_name"},
	{Attribute: "username", Path: "preferred_username"}}

func nop(string, ...any) {}

func TestPrimaryIntoBrokerThenOutToFailovers(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{bob()}, links: map[string]map[string]string{"bob": {"auth0": "a-bob"}}}
	primary := &IdP{Name: "auth0", Attributes: maps, Dir: &fakeDir{recs: map[string]map[string]any{
		"a-bob": {"department": "Advisory", "given_name": "Robert", "preferred_username": "evil"}}}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@sterling.lab", "department": "old", "given_name": "Bob"}}}
	res := Run(context.Background(), b, primary, []IdP{{Name: "keycloak", Attributes: maps, Dir: kc}}, writable, nop)
	if res.Users != 1 || res.Updated != 1 || res.Written != 1 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	u := b.updated[0]
	if u["firstName"] != "Robert" || u["username"] != "bob" || !slices.Equal(Values(u["attributes"].(map[string]any)["department"]), []string{"Advisory"}) {
		t.Fatalf("broker %v: the primary's values, never the username", u)
	}
	r := kc.recs["k-bob"]
	if r["department"] != "Advisory" || r["given_name"] != "Robert" || r["preferred_username"] != nil {
		t.Fatalf("failover %v: found by email, written from the broker, no username", r)
	}
}

func TestCreatedAtFailoverOnlyIfInPrimary(t *testing.T) {
	carol := keycloak.User{"id": "carol", "email": "carol@sterling.lab", "attributes": map[string]any{"department": []any{"Ops"}}}
	dave := keycloak.User{"id": "dave", "email": "dave@sterling.lab"}
	b := &fakeBroker{users: []keycloak.User{carol, dave}}
	primary := &IdP{Name: "auth0", Attributes: maps, Dir: &fakeDir{recs: map[string]map[string]any{
		"a-carol": {"email": "carol@sterling.lab", "department": "Ops"}}}}
	kc := &fakeDir{recs: map[string]map[string]any{}}
	res := Run(context.Background(), b, primary, []IdP{{Name: "keycloak", Attributes: maps, Dir: kc}}, writable, nop)
	if res.Created != 1 || !slices.Equal(kc.created, []string{"carol@sterling.lab"}) || kc.recs["new-carol@sterling.lab"]["department"] != "Ops" {
		t.Fatalf("created %v (%+v): carol, who is in the primary; never dave", kc.created, res)
	}
	auth0 := &fakeDir{recs: map[string]map[string]any{}, noCreate: true}
	res = Run(context.Background(), &fakeBroker{users: []keycloak.User{carol}}, primary, []IdP{{Name: "auth0-backup", Attributes: maps, Dir: auth0}}, writable, nop)
	if res.Failed != 1 || !strings.Contains(strings.Join(res.Errors, ""), "can't create one without a password") {
		t.Fatalf("a directory that can't create without a password is reported: %+v", res)
	}
}

func TestUnreadablePrimaryLeavesBrokerAndStillWritesFailovers(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{bob()}, links: map[string]map[string]string{"bob": {"auth0": "a-bob"}}}
	primary := &IdP{Name: "auth0", Attributes: maps, Dir: &fakeDir{err: errors.New("unreachable")}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@sterling.lab", "department": "stale"}}}
	res := Run(context.Background(), b, primary, []IdP{{Name: "keycloak", Attributes: maps, Dir: kc}}, writable, nop)
	if res.Failed != 1 || len(b.updated) != 0 || kc.recs["k-bob"]["department"] != "old" {
		t.Fatalf("broker kept, failover given the broker's last value: %+v %v", res, kc.recs)
	}
	if len(kc.created) != 0 {
		t.Fatal("nobody is created while the primary can't be read")
	}
}

func TestDroppedAttributeIsReported(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{bob()}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@sterling.lab"}}, drop: "department"}
	res := Run(context.Background(), b, nil, []IdP{{Name: "keycloak", Attributes: maps, Dir: kc}}, writable, nop)
	if res.Failed != 1 || !strings.Contains(strings.Join(res.Errors, ""), "didn't keep department") {
		t.Fatalf("result %+v", res)
	}
}

func TestLocalOnlyAndServiceAccountsSkipped(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{{"id": "ops", "email": "ops@sterling.lab"}, {"id": "sa", "serviceAccountClientId": "x"}},
		local: map[string]bool{"ops": true}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-ops": {"email": "ops@sterling.lab"}}}
	if res := Run(context.Background(), b, nil, []IdP{{Name: "keycloak", Attributes: maps, Dir: kc}}, writable, nop); res.Users != 0 || len(kc.writes) != 0 {
		t.Fatalf("result %+v writes %v", res, kc.writes)
	}
}

// directories over HTTP

func tokenOK(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
}

func TestSCIMDirectory(t *testing.T) {
	var patched, created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenOK(w)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/scim/v2/Users" && r.URL.Query().Get("filter") == `emails.value eq "bob@sterling.lab"`:
			w.Write([]byte(`{"totalResults":1,"Resources":[{"id":"inum-bob"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/scim/v2/Users":
			w.Write([]byte(`{"totalResults":42,"Resources":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/scim/v2/Users/inum-bob":
			w.Write([]byte(`{"id":"inum-bob","name":{"givenName":"Bob"},"emails":[{"value":"x@y"},{"value":"bob@sterling.lab","primary":true}],
				"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Advisory"}}`))
		case r.Method == http.MethodPatch:
			json.Unmarshal(body, &patched)
		case r.Method == http.MethodPost && r.URL.Path == "/scim/v2/Users":
			json.Unmarshal(body, &created)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"inum-new"}`))
		case r.URL.Path == "/scim/v2/Schemas":
			w.Write([]byte(`{"Resources":[{"id":"urn:ietf:params:scim:schemas:core:2.0:User","attributes":[
				{"name":"userName","type":"string"},{"name":"name","type":"complex","subAttributes":[{"name":"givenName","type":"string"}]},
				{"name":"emails","type":"complex","multiValued":true},{"name":"id","type":"string","mutability":"readOnly"},{"name":"title","type":"string"}]},
				{"id":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User","attributes":[{"name":"department","type":"string"}]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d, _ := New("scim", srv.URL+"/scim/v2", Credentials{TokenURL: srv.URL + "/token", ClientID: "c", ClientSecret: "s"}, srv.Client())
	ctx := context.Background()
	id, err := d.Find(ctx, "bob@sterling.lab")
	if err != nil || id != "inum-bob" {
		t.Fatalf("find: %q %v", id, err)
	}
	rec, _ := d.User(ctx, id)
	for path, want := range map[string]string{"name.givenName": "Bob", "emails[primary eq true].value": "bob@sterling.lab",
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department": "Advisory"} {
		if got := Lookup(rec, path); got != want {
			t.Errorf("Lookup(%s) = %v, want %s", path, got, want)
		}
	}
	d.Update(ctx, id, map[string][]string{"name.givenName": {"Robert"}})
	ops := patched["Operations"].([]any)
	if op := ops[0].(map[string]any); op["op"] != "replace" || op["path"] != "name.givenName" || op["value"] != "Robert" {
		t.Fatalf("patch %v", patched)
	}
	if nid, err := d.Create(ctx, "carol@sterling.lab", map[string][]string{"name.givenName": {"Carol"},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department": {"Ops"}}); err != nil || nid != "inum-new" {
		t.Fatalf("create: %q %v", nid, err)
	}
	if created["userName"] != "carol@sterling.lab" || created["password"] != nil ||
		created["urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"].(map[string]any)["department"] != "Ops" {
		t.Fatalf("created %v", created)
	}
	schema, _ := d.Schema(ctx)
	want := []string{"name.givenName", "title", "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department"}
	if !slices.Equal(schema, want) {
		t.Fatalf("schema %v, want %v", schema, want)
	}
	if n, err := d.Check(ctx); n != 42 || err != nil {
		t.Fatalf("check %d %v", n, err)
	}
}

func TestKeycloakDirectory(t *testing.T) {
	rep := map[string]any{"id": "u1", "username": "bob", "email": "bob@sterling.lab", "firstName": "Bob", "emailVerified": false,
		"requiredActions": []any{}, "attributes": map[string]any{"department": []any{"Advisory"}}}
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			tokenOK(w)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/r/users/u1":
			json.NewEncoder(w).Encode(rep)
		case r.Method == http.MethodPut:
			json.NewDecoder(r.Body).Decode(&put)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d, _ := New("keycloak", srv.URL+"/admin/realms/r", Credentials{TokenURL: srv.URL + "/token", ClientID: "c", ClientSecret: "s"}, srv.Client())
	rec, _ := d.User(context.Background(), "u1")
	if rec["firstName"] != "Bob" || !slices.Equal(Values(rec["department"]), []string{"Advisory"}) {
		t.Fatalf("record %v", rec)
	}
	if err := d.Update(context.Background(), "u1", map[string][]string{"email": {"robert@sterling.lab"}, "costCenter": {"CC-7"}}); err != nil {
		t.Fatal(err)
	}
	attrs := put["attributes"].(map[string]any)
	if put["email"] != "robert@sterling.lab" || put["emailVerified"] != true || put["requiredActions"] == nil ||
		!slices.Equal(Values(attrs["department"]), []string{"Advisory"}) || !slices.Equal(Values(attrs["costCenter"]), []string{"CC-7"}) {
		t.Fatalf("put %v: the whole representation, email verified, attributes merged", put)
	}
	if err := d.Update(context.Background(), "u1", map[string][]string{"username": {"x"}}); err == nil {
		t.Fatal("username must never be written")
	}
}

func TestAuth0WritesOnlyProfileAndMetadata(t *testing.T) {
	var patched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			r.ParseForm()
			if _, _, basic := r.BasicAuth(); basic || r.PostForm.Get("client_id") != "m2m" || r.PostForm.Get("client_secret") != "s" ||
				r.PostForm.Get("audience") != "https://t/api/v2/" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			tokenOK(w)
			return
		}
		json.NewDecoder(r.Body).Decode(&patched)
	}))
	defer srv.Close()
	d, _ := New("auth0", srv.URL+"/api/v2", Credentials{TokenURL: srv.URL + "/token", ClientID: "m2m", ClientSecret: "s", Audience: "https://t/api/v2/"}, srv.Client())
	if err := d.Update(context.Background(), "auth0|1", map[string][]string{"user_metadata.department": {"Ops"}, "email": {"b@s"}}); err != nil {
		t.Fatal(err)
	}
	if patched["user_metadata"].(map[string]any)["department"] != "Ops" || patched["email_verified"] != true {
		t.Fatalf("patch %v", patched)
	}
	if err := d.Update(context.Background(), "auth0|1", map[string][]string{"password": {"x"}}); err == nil {
		t.Fatal("anything outside the profile and metadata is refused")
	}
	if _, err := d.Create(context.Background(), "c@s", nil); !errors.Is(err, ErrNoCreate) {
		t.Fatalf("create: %v", err)
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
