package keycloak

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAdmin is the slice of Keycloak's admin API the profile code uses.
type fakeAdmin struct {
	mu      sync.Mutex
	profile map[string]any
	mappers []map[string]any
	scopes  []map[string]any
	puts    int
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/token") {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/admin/realms/r")
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	switch {
	case p == "/users/profile" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(f.profile)
	case p == "/users/profile" && r.Method == http.MethodPut:
		f.profile, f.puts = body, f.puts+1
	case strings.HasSuffix(p, "/mappers") && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(f.mappers)
	case strings.HasSuffix(p, "/mappers") && r.Method == http.MethodPost:
		body["id"] = body["name"]
		f.mappers = append(f.mappers, body)
	case strings.Contains(p, "/mappers/") && r.Method == http.MethodDelete:
		id := p[strings.LastIndex(p, "/")+1:]
		for i, m := range f.mappers {
			if m["id"] == id {
				f.mappers = append(f.mappers[:i], f.mappers[i+1:]...)
				break
			}
		}
	case p == "/client-scopes" && r.Method == http.MethodGet:
		json.NewEncoder(w).Encode(f.scopes)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func newFake(t *testing.T, f *fakeAdmin) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "r")
	c.SetCredentials("c", "s")
	return c
}

func TestEnsureProfileOwnsOnlyItsAttributes(t *testing.T) {
	f := &fakeAdmin{profile: map[string]any{"attributes": []any{
		map[string]any{"name": "username"}, map[string]any{"name": "email"},
		map[string]any{"name": "badge"}, // someone else's
		map[string]any{"name": "old", "annotations": map[string]any{OwnerAnnotation: "ns/a"}},
	}}}
	c := newFake(t, f)
	changed, err := c.EnsureProfile(context.Background(), "ns/a", []Attr{{Name: "department", DisplayName: "Department"}, {Name: "email"}})
	if err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	names := map[string]map[string]any{}
	for _, x := range f.profile["attributes"].([]any) {
		m := x.(map[string]any)
		names[m["name"].(string)] = m
	}
	if _, ok := names["old"]; ok {
		t.Error("an attribute this owner dropped from the spec must go")
	}
	if _, ok := names["badge"]; !ok {
		t.Error("another owner's attribute must stay")
	}
	d := names["department"]
	if d == nil || d["annotations"].(map[string]any)[OwnerAnnotation] != "ns/a" {
		t.Fatalf("department: %v", d)
	}
	if edit := d["permissions"].(map[string]any)["edit"].([]any); len(edit) != 1 || edit[0] != "admin" {
		t.Errorf("users must not edit upstream-sourced attributes: %v", edit)
	}
	if again, _ := c.EnsureProfile(context.Background(), "ns/a", []Attr{{Name: "department", DisplayName: "Department"}}); again || f.puts != 1 {
		t.Errorf("an unchanged profile must not be written again (puts %d)", f.puts)
	}
}

func TestProfileAttributeTypes(t *testing.T) {
	f := &fakeAdmin{profile: map[string]any{"attributes": []any{map[string]any{"name": "username"}}}}
	c := newFake(t, f)
	if _, err := c.EnsureProfile(context.Background(), "ns/a", []Attr{{Name: "employeeNumber", Type: "integer"},
		{Name: "groups", Multivalued: true}, {Name: "department", Type: "string"}}); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, x := range f.profile["attributes"].([]any) {
		m := x.(map[string]any)
		got[m["name"].(string)] = m
	}
	if v := got["employeeNumber"]["validations"].(map[string]any); v["integer"] == nil || got["employeeNumber"]["annotations"].(map[string]any)["inputType"] != "html5-number" {
		t.Errorf("integer: %v", got["employeeNumber"])
	}
	if got["groups"]["multivalued"] != true || len(got["department"]["validations"].(map[string]any)) != 0 || got["department"]["multivalued"] != false {
		t.Errorf("groups %v, department %v", got["groups"], got["department"])
	}
}
