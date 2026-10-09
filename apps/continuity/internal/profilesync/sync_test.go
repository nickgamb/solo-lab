package profilesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	store   func(keycloak.User) // what the broker does to a user it stores
	groups  map[string][]string // user id -> group names
	created []string            // emails it created accounts for
}

// Users lists copies, as the admin API does.
func (f *fakeBroker) Users(_ context.Context, first, max int) ([]keycloak.User, error) {
	if first >= len(f.users) {
		return nil, nil
	}
	var out []keycloak.User
	for _, u := range f.users[first:min(first+max, len(f.users))] {
		out = append(out, clone(u))
	}
	return out, nil
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

func (f *fakeBroker) UpdateUser(_ context.Context, id string, apply func(keycloak.User) bool) (bool, error) {
	for i, u := range f.users {
		if u["id"] != id {
			continue
		}
		c := clone(u)
		if !apply(c) {
			return false, nil
		}
		if f.store != nil {
			f.store(c)
		}
		f.users[i] = c
		f.updated = append(f.updated, c)
		return true, nil
	}
	return false, keycloak.ErrNotFound
}

func (f *fakeBroker) FindUserByEmail(_ context.Context, email string) (string, error) {
	for _, u := range f.users {
		if s, _ := u["email"].(string); strings.EqualFold(s, email) {
			id, _ := u["id"].(string)
			return id, nil
		}
	}
	return "", keycloak.ErrNotFound
}

func (f *fakeBroker) CreateUser(_ context.Context, email string, verified bool) (string, error) {
	id := "new-" + email
	f.users = append(f.users, keycloak.User{"id": id, "username": email, "email": email, "emailVerified": verified})
	f.created = append(f.created, email)
	return id, nil
}

func (f *fakeBroker) EnsureGroup(_ context.Context, name string) (string, error) {
	return "g-" + name, nil
}

func (f *fakeBroker) UserGroups(_ context.Context, id string) ([]string, error) {
	return slices.Clone(f.groups[id]), nil
}

func (f *fakeBroker) SetUserGroups(_ context.Context, id string, want []string, managed map[string]string) (bool, error) {
	if f.groups == nil {
		f.groups = map[string][]string{}
	}
	var next []string
	for _, g := range f.groups[id] {
		if _, m := managed[g]; !m {
			next = append(next, g)
		}
	}
	next = append(next, want...)
	slices.Sort(next)
	changed := !slices.Equal(next, f.groups[id])
	f.groups[id] = next
	return changed, nil
}

// groupDir is a fakeDir with users to list and groups (or roles) per user.
type groupDir struct {
	fakeDir
	entries []Entry
	groups  map[string][]string
	set     map[string][]string // id -> the want of the last SetGroups
}

func (d *groupDir) List(_ context.Context, first, max int) ([]Entry, error) {
	if first >= len(d.entries) {
		return nil, nil
	}
	return d.entries[first:min(first+max, len(d.entries))], nil
}

func (d *groupDir) Groups(_ context.Context, id string) ([]string, error) { return d.groups[id], nil }

func (d *groupDir) SetGroups(_ context.Context, id string, want, _ []string) error {
	if d.set == nil {
		d.set = map[string][]string{}
	}
	d.set[id] = want
	return nil
}

// fakeDir is a directory of flat records (path -> value), as Lookup reads them.
type fakeDir struct {
	recs     map[string]map[string]any
	err      error
	noCreate bool
	drop     string // a path it silently doesn't keep
	lower    bool   // it keeps emails lowercased
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
		if s, _ := r["email"].(string); strings.EqualFold(s, email) {
			return id, nil
		}
	}
	return "", ErrNoUser
}

func (d *fakeDir) Update(_ context.Context, id string, set map[string][]string, verified *bool) error {
	d.writes = append(d.writes, set)
	if _, ok := set["email"]; ok && verified != nil {
		d.recs[id]["email_verified"] = *verified
	}
	for p, v := range set {
		if p == "email" && d.lower {
			v = []string{strings.ToLower(v[0])}
		}
		if p != d.drop {
			d.recs[id][p] = one(v)
		}
	}
	return nil
}

func (d *fakeDir) Create(_ context.Context, email string, set map[string][]string, _ *bool) (string, error) {
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

var mappings = []v1.AttributeMapping{{Attribute: "department", Path: "department"}, {Attribute: "firstName", Path: "given_name"},
	{Attribute: "username", Path: "preferred_username"}}

func nop(string, ...any) {}

func TestPrimaryIntoBrokerThenOutToFailovers(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{bob()}, links: map[string]map[string]string{"bob": {"auth0": "a-bob"}}}
	primary := &IdP{Name: "auth0", Attributes: mappings, Dir: &fakeDir{recs: map[string]map[string]any{
		"a-bob": {"department": "Advisory", "given_name": "Robert", "preferred_username": "evil"}}}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@sterling.lab", "department": "old", "given_name": "Bob"}}}
	res := Run(context.Background(), b, primary, []IdP{{Name: "keycloak", Attributes: mappings, Dir: kc}}, writable, nil, Shape{}, nop)
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
	primary := &IdP{Name: "auth0", Attributes: mappings, Dir: &fakeDir{recs: map[string]map[string]any{
		"a-carol": {"email": "carol@sterling.lab", "department": "Ops"}}}}
	kc := &fakeDir{recs: map[string]map[string]any{}}
	res := Run(context.Background(), b, primary, []IdP{{Name: "keycloak", Attributes: mappings, Dir: kc}}, writable, nil, Shape{}, nop)
	if res.Created != 1 || !slices.Equal(kc.created, []string{"carol@sterling.lab"}) || kc.recs["new-carol@sterling.lab"]["department"] != "Ops" {
		t.Fatalf("created %v (%+v): carol, who is in the primary; never dave", kc.created, res)
	}
	auth0 := &fakeDir{recs: map[string]map[string]any{}, noCreate: true}
	res = Run(context.Background(), &fakeBroker{users: []keycloak.User{carol}}, primary, []IdP{{Name: "auth0-backup", Attributes: mappings, Dir: auth0}}, writable, nil, Shape{}, nop)
	if res.Failed != 0 || !strings.Contains(strings.Join(res.Notes, ""), "can't create one without a password") {
		t.Fatalf("a directory that can't create without a password is noted, not a failed run: %+v", res)
	}
}

func TestUnreadablePrimaryLeavesBrokerAndStillWritesFailovers(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{bob()}, links: map[string]map[string]string{"bob": {"auth0": "a-bob"}}}
	primary := &IdP{Name: "auth0", Attributes: mappings, Dir: &fakeDir{err: errors.New("unreachable")}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@sterling.lab", "department": "stale"}}}
	res := Run(context.Background(), b, primary, []IdP{{Name: "keycloak", Attributes: mappings, Dir: kc}}, writable, nil, Shape{}, nop)
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
	res := Run(context.Background(), b, nil, []IdP{{Name: "keycloak", Attributes: mappings, Dir: kc}}, writable, nil, Shape{}, nop)
	if res.Failed != 1 || !strings.Contains(strings.Join(res.Errors, ""), "didn't keep department") {
		t.Fatalf("result %+v", res)
	}
}

func TestLocalOnlyAndServiceAccountsSkipped(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{{"id": "ops", "email": "ops@sterling.lab"}, {"id": "sa", "serviceAccountClientId": "x"}},
		local: map[string]bool{"ops": true}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-ops": {"email": "ops@sterling.lab"}}}
	if res := Run(context.Background(), b, nil, []IdP{{Name: "keycloak", Attributes: mappings, Dir: kc}}, writable, nil, Shape{}, nop); res.Users != 0 || len(kc.writes) != 0 {
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
	d.Update(ctx, id, map[string][]string{"name.givenName": {"Robert"}}, nil)
	ops := patched["Operations"].([]any)
	if op := ops[0].(map[string]any); op["op"] != "replace" || op["path"] != "name.givenName" || op["value"] != "Robert" {
		t.Fatalf("patch %v", patched)
	}
	// a filtered path is added as the complex value it names
	d.Update(ctx, id, map[string][]string{`phoneNumbers[type eq "work"].value`: {"+1 555 0100"}, "emails[primary eq true].value": {"bob@sterling.lab"}}, nil)
	ops = patched["Operations"].([]any)
	want0 := `map[op:add path:emails value:[map[primary:true value:bob@sterling.lab]]]`
	want1 := `map[op:add path:phoneNumbers value:[map[type:work value:+1 555 0100]]]`
	if len(ops) != 2 || fmt.Sprint(ops[0]) != want0 || fmt.Sprint(ops[1]) != want1 {
		t.Fatalf("patch %v", ops)
	}
	if nid, err := d.Create(ctx, "carol@sterling.lab", map[string][]string{"name.givenName": {"Carol"},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department": {"Ops"}}, nil); err != nil || nid != "inum-new" {
		t.Fatalf("create: %q %v", nid, err)
	}
	if created["userName"] != "carol@sterling.lab" || created["password"] != nil ||
		created["urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"].(map[string]any)["department"] != "Ops" {
		t.Fatalf("created %v", created)
	}
	if s := fmt.Sprint(created["schemas"]); s != "[urn:ietf:params:scim:schemas:core:2.0:User urn:ietf:params:scim:schemas:extension:enterprise:2.0:User]" {
		t.Fatalf("schemas %s: the extension in use is declared", s)
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
	yes := true
	if err := d.Update(context.Background(), "u1", map[string][]string{"email": {"robert@sterling.lab"}, "costCenter": {"CC-7"}}, &yes); err != nil {
		t.Fatal(err)
	}
	attrs := put["attributes"].(map[string]any)
	if put["email"] != "robert@sterling.lab" || put["emailVerified"] != true || put["requiredActions"] == nil ||
		!slices.Equal(Values(attrs["department"]), []string{"Advisory"}) || !slices.Equal(Values(attrs["costCenter"]), []string{"CC-7"}) {
		t.Fatalf("put %v: the whole representation, email verified as the source has it, attributes merged", put)
	}
	if err := d.Update(context.Background(), "u1", map[string][]string{"email": {"robert@sterling.lab"}}, nil); err != nil || put["emailVerified"] != false {
		t.Fatalf("put %v: verification unknown, so left as Keycloak has it (err %v)", put, err)
	}
	if err := d.Update(context.Background(), "u1", map[string][]string{"username": {"x"}}, nil); err == nil {
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
	no := false
	if err := d.Update(context.Background(), "auth0|1", map[string][]string{"user_metadata.department": {"Ops"}, "email": {"b@s"}}, &no); err != nil {
		t.Fatal(err)
	}
	if patched["user_metadata"].(map[string]any)["department"] != "Ops" || patched["email_verified"] != false {
		t.Fatalf("patch %v: email_verified copied from the source", patched)
	}
	if err := d.Update(context.Background(), "auth0|1", map[string][]string{"password": {"x"}}, nil); err == nil {
		t.Fatal("anything outside the profile and metadata is refused")
	}
	if _, err := d.Create(context.Background(), "c@s", nil, nil); !errors.Is(err, ErrNoCreate) {
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

func TestListsCarryEveryValueSinglesOne(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{{"id": "bob", "email": "bob@sterling.lab"}}, links: map[string]map[string]string{"bob": {"auth0": "a-bob"}}}
	ms := []v1.AttributeMapping{{Attribute: "groups", Path: "groups"}, {Attribute: "department", Path: "department"}}
	primary := &IdP{Name: "auth0", Attributes: ms, Dir: &fakeDir{recs: map[string]map[string]any{
		"a-bob": {"groups": []any{"advisors", "research"}, "department": []any{"Advisory", "Old"}}}}}
	Run(context.Background(), b, primary, nil, []string{"groups", "department"}, []string{"groups"}, Shape{}, nop)
	attrs := b.updated[0]["attributes"].(map[string]any)
	if !slices.Equal(Values(attrs["groups"]), []string{"advisors", "research"}) || !slices.Equal(Values(attrs["department"]), []string{"Advisory"}) {
		t.Fatalf("attributes %v: a list keeps every value, a single attribute its first", attrs)
	}
}

func TestMixedCaseEmailSettles(t *testing.T) {
	// the primary's email in mixed case; the broker and the failover keep emails lowercased
	ms := []v1.AttributeMapping{{Attribute: "email", Path: "email"}}
	b := &fakeBroker{users: []keycloak.User{{"id": "bob", "username": "bob", "email": "bob@old.lab"}},
		links: map[string]map[string]string{"bob": {"gluu": "g-bob", "kc2": "k-bob"}},
		store: func(u keycloak.User) { u["email"] = strings.ToLower(u["email"].(string)) }}
	primary := &IdP{Name: "gluu", Attributes: ms, Dir: &fakeDir{recs: map[string]map[string]any{"g-bob": {"email": " Bob@Sterling.LAB"}}}}
	fo := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@old.lab"}}, lower: true}
	res := Run(context.Background(), b, primary, []IdP{{Name: "kc2", Attributes: ms, Dir: fo}}, writable, nil, Shape{}, nop)
	if res.Updated != 1 || res.Written != 1 || res.Failed != 0 || b.users[0]["email"] != "bob@sterling.lab" || fo.recs["k-bob"]["email"] != "bob@sterling.lab" {
		t.Fatalf("first run %+v: broker %v, failover %v", res, b.users[0], fo.recs["k-bob"])
	}
	fo.recs["k-bob"]["email"] = "Bob@Sterling.lab" // as the failover may show it
	res = Run(context.Background(), b, primary, []IdP{{Name: "kc2", Attributes: ms, Dir: fo}}, writable, nil, Shape{}, nop)
	if res.Updated != 0 || res.Written != 0 || res.Failed != 0 {
		t.Fatalf("second run %+v: nothing to do when only the case differs", res)
	}
}

func TestUserOnTwoPagesSyncedOnce(t *testing.T) {
	var users []keycloak.User
	for i := range pageSize {
		users = append(users, keycloak.User{"id": fmt.Sprintf("u%03d", i), "email": fmt.Sprintf("u%03d@sterling.lab", i)})
	}
	users = append(users, users[pageSize-1]) // shifted onto the next page meanwhile
	b := &fakeBroker{users: users}
	res := Run(context.Background(), b, nil, []IdP{{Name: "keycloak", Attributes: mappings, Dir: &fakeDir{recs: map[string]map[string]any{}}}}, writable, nil, Shape{}, nop)
	if res.Users != pageSize {
		t.Fatalf("users %d, want %d", res.Users, pageSize)
	}
}

func TestUsernameNeverSyncedEitherWay(t *testing.T) {
	ms := []v1.AttributeMapping{{Attribute: "username", Path: "preferred_username"}}
	b := &fakeBroker{users: []keycloak.User{bob()}, links: map[string]map[string]string{"bob": {"auth0": "a-bob", "kc": "k-bob"}}}
	primary := &IdP{Name: "auth0", Attributes: ms, Dir: &fakeDir{recs: map[string]map[string]any{"a-bob": {"preferred_username": "evil"}}}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@sterling.lab"}}}
	res := Run(context.Background(), b, primary, []IdP{{Name: "kc", Attributes: ms, Dir: kc}}, writable, nil, Shape{}, nop)
	if res.Updated != 0 || res.Written != 0 || len(kc.writes) != 0 {
		t.Fatalf("result %+v writes %v", res, kc.writes)
	}
}

func TestEmailVerifiedCopiedFromTheSource(t *testing.T) {
	ms := []v1.AttributeMapping{{Attribute: "email", Path: "email"}}
	b := &fakeBroker{users: []keycloak.User{{"id": "bob", "email": "bob@old.lab", "emailVerified": true}},
		links: map[string]map[string]string{"bob": {"auth0": "a-bob", "kc": "k-bob"}}}
	primary := &IdP{Name: "auth0", Attributes: ms, Dir: &fakeDir{recs: map[string]map[string]any{
		"a-bob": {"email": "bob@sterling.lab", "email_verified": false}}}}
	kc := &fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@old.lab"}}}
	Run(context.Background(), b, primary, []IdP{{Name: "kc", Attributes: ms, Dir: kc}}, writable, nil, Shape{}, nop)
	if b.users[0]["emailVerified"] != false || kc.recs["k-bob"]["email_verified"] != false {
		t.Fatalf("broker %v, failover %v: unverified at the primary stays unverified", b.users[0], kc.recs["k-bob"])
	}
	// a primary that doesn't say (SCIM): a changed address is not marked verified
	b.users[0]["emailVerified"] = true
	primary.Dir = &fakeDir{recs: map[string]map[string]any{"a-bob": {"email": "robert@sterling.lab"}}}
	Run(context.Background(), b, primary, nil, writable, nil, Shape{}, nop)
	if b.users[0]["email"] != "robert@sterling.lab" || b.users[0]["emailVerified"] != false {
		t.Fatalf("broker %v", b.users[0])
	}
}

func yes() *bool { t := true; return &t }

func TestProvisionsTheWorkforceFromThePrimary(t *testing.T) {
	no := false
	b := &fakeBroker{users: []keycloak.User{bob()}}
	dir := &groupDir{fakeDir: fakeDir{recs: map[string]map[string]any{}}, entries: []Entry{
		{ID: "a-bob", Email: "bob@sterling.lab", Verified: yes()},   // has an account
		{ID: "a-dana", Email: "Dana@Sterling.lab", Verified: yes()}, // new: provisioned
		{ID: "a-eve", Email: "eve@gmail.com", Verified: yes()},      // not the workforce
		{ID: "a-fay", Email: "fay@sterling.lab", Verified: &no},     // email not verified
		{ID: "a-gus", Email: "gus@sterling.lab"},                    // verification unknown
	}}
	res := Run(context.Background(), b, &IdP{Name: "auth0", Attributes: mappings, Dir: dir}, nil, writable, nil,
		Shape{Domains: []string{"sterling.lab"}}, nop)
	if res.Provisioned != 1 || !slices.Equal(b.created, []string{"dana@sterling.lab"}) {
		t.Fatalf("provisioned %d: %v (errors %v)", res.Provisioned, b.created, res.Errors)
	}
	// no domains: no accounts
	b = &fakeBroker{}
	if res := Run(context.Background(), b, &IdP{Name: "auth0", Attributes: mappings, Dir: dir}, nil, writable, nil, Shape{}, nop); res.Provisioned != 0 {
		t.Fatalf("provisioned %d without domains", res.Provisioned)
	}
}

func TestGroupsFromThePrimaryIntoTheBrokerAndOut(t *testing.T) {
	b := &fakeBroker{users: []keycloak.User{bob()}, links: map[string]map[string]string{"bob": {"auth0": "a-bob", "kc": "k-bob"}},
		groups: map[string][]string{"bob": {"compliance", "local-team"}}}
	primary := &groupDir{fakeDir: fakeDir{recs: map[string]map[string]any{"a-bob": {"department": "Advisory"}}},
		groups: map[string][]string{"a-bob": {"Admin", "advisors"}}}
	failover := &groupDir{fakeDir: fakeDir{recs: map[string]map[string]any{"k-bob": {"email": "bob@sterling.lab"}}}}
	res := Run(context.Background(), b, &IdP{Name: "auth0", Attributes: mappings, Dir: primary},
		[]IdP{{Name: "kc", Attributes: mappings, Dir: failover}}, writable, nil,
		Shape{Groups: []string{"advisors", "compliance"}}, nop)
	if len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	// the shape's groups follow the primary; a group outside the shape stays
	if got := b.groups["bob"]; !slices.Equal(got, []string{"advisors", "local-team"}) {
		t.Fatalf("broker groups %v", got)
	}
	// the primary's roles outside the shape never reach the failover
	if got := failover.set["k-bob"]; !slices.Equal(got, []string{"advisors"}) {
		t.Fatalf("failover groups %v", got)
	}
}

func TestSCIMList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenOK(w)
			return
		}
		if r.URL.Path != "/scim/v2/Users" || r.URL.Query().Get("startIndex") != "1" || r.URL.Query().Get("count") != "2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"Resources":[
			{"id":"inum-bob","emails":[{"value":"x@y"},{"value":"bob@sterling.lab","primary":true}],
			 "urn:ietf:params:scim:schemas:extension:gluu:2.0:User":{"emailVerified":true}},
			{"id":"inum-eve","emails":[{"value":"eve@sterling.lab"}]}]}`))
	}))
	defer srv.Close()
	d, _ := New("scim", srv.URL+"/scim/v2", Credentials{TokenURL: srv.URL + "/token", ClientID: "c", ClientSecret: "s"}, srv.Client())
	page, err := d.(Lister).List(context.Background(), 0, 2)
	if err != nil || len(page) != 2 {
		t.Fatalf("list: %v %v", page, err)
	}
	if b := page[0]; b.ID != "inum-bob" || b.Email != "bob@sterling.lab" || b.Verified == nil || !*b.Verified {
		t.Fatalf("bob: %+v (want his primary email, verified by the extension)", b)
	}
	if e := page[1]; e.Email != "eve@sterling.lab" || e.Verified != nil {
		t.Fatalf("eve: %+v (want her only email, verification unknown)", e)
	}
}
