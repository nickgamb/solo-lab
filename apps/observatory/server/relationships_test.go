package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
)

func relationship(ns, name, model, user, relation, what string) map[string]any {
	return object("continuity.lab.solo.io/v1alpha1", "Relationship", ns, name,
		"spec", map[string]any{"model": model, "user": user, "relation": relation, "object": what})
}

// relationships: a model, its relationships beside another model's and
// another namespace's, and the guard, found by its label.
func relationshipObjects() map[string][]map[string]any {
	guard := object("v1", "Service", "gw", "entitlements", "spec", map[string]any{
		"ports": []any{map[string]any{"name": "grpc", "port": int64(4445)}, map[string]any{"name": "evaluate", "port": int64(9002)}}})
	guard["metadata"].(map[string]any)["labels"] = map[string]any{guardLabel: "true"}
	return map[string][]map[string]any{
		"relationshipmodels": {object("continuity.lab.solo.io/v1alpha1", "RelationshipModel", "id", "firm",
			"spec", map[string]any{"description": "who advises whom", "model": "model\n  schema 1.1\n"},
			"status", map[string]any{"storeID": "s1", "modelID": "m1", "relationships": int64(2),
				"conditions": []any{map[string]any{"type": "Synced", "status": "True"}}})},
		"relationships": {
			relationship("id", "ann-advisor-ledger", "firm", "user:ann@firm.lab", "advisor", "book:ledger"),
			relationship("id", "desk-acts-for-advisors", "firm", "group:advisors#member", "acts_for", "agent:apps.desk"),
			relationship("id", "other-model", "other", "user:x", "advisor", "book:y"),
			relationship("elsewhere", "ann-advisor-ledger", "firm", "user:ann@firm.lab", "advisor", "book:ledger"),
		},
		"services": {guard},
	}
}

func relRequest(method, body string, path ...string) *http.Request {
	r := httptest.NewRequest(method, "/", strings.NewReader(body))
	for i := 0; i+1 < len(path); i += 2 {
		r.SetPathValue(path[i], path[i+1])
	}
	return r
}

func TestRelationshipsView(t *testing.T) {
	k := storeKube(relationshipObjects())
	w := httptest.NewRecorder()
	(&Relationships{k: k}).Get(w, relRequest(http.MethodGet, "", "ns", "id", "name", "firm"))
	var v RelationshipsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got []string
	for _, r := range v.Relationships {
		got = append(got, r.Name+" "+r.User+" "+r.Relation+" "+r.Object+" "+r.ResourceVersion)
	}
	if strings.Join(got, " | ") != "ann-advisor-ledger user:ann@firm.lab advisor book:ledger 7 | desk-acts-for-advisors group:advisors#member acts_for agent:apps.desk 7" {
		t.Errorf("only this model's: %v", got)
	}
	if v.Description != "who advises whom" || !strings.HasPrefix(v.Model, "model") || str(v.Status, "storeID") != "s1" {
		t.Errorf("model %+v", v)
	}
	if v.Guard == nil || v.Guard.Namespace != "gw" || v.Guard.Name != "entitlements" {
		t.Errorf("guard %+v", v.Guard)
	}
	w = httptest.NewRecorder()
	(&Relationships{k: k}).Get(w, relRequest(http.MethodGet, "", "ns", "id", "name", "nope"))
	if w.Code != http.StatusNotFound {
		t.Errorf("no model: %d", w.Code)
	}

	cv := (&Continuity{k: k}).View(&Index{})
	if len(cv.RelationshipModels) != 1 || cv.RelationshipModels[0] != (RelationshipModelPhase{Namespace: "id", Name: "firm", Relationships: 2, Synced: "True"}) {
		t.Errorf("continuity view: %+v", cv.RelationshipModels)
	}
}

func TestRelationshipName(t *testing.T) {
	long := "user:" + strings.Repeat("a", 70)
	for _, c := range []struct {
		user, relation, object string
		taken                  []string
		want                   string
	}{
		{"user:bob@sterling.lab", "advisor", "book:sv-bob", nil, "bob-sterling-lab-advisor-sv-bob"},
		{"group:advisors#member", "acts_for", "agent:sv-agents.bob-assistant", nil, "advisors-acts-for-sv-agents-bob-assistant"},
		{"client:c-1001", "holds", "mcp_server:alice-vault", []string{"c-1001-holds-alice-vault", "c-1001-holds-alice-vault-2"}, "c-1001-holds-alice-vault-3"},
		{long, "advisor", "book:x", nil, strings.Repeat("a", 63)},
		{long, "advisor", "book:x", []string{strings.Repeat("a", 63)}, strings.Repeat("a", 61) + "-2"},
	} {
		taken := map[string]bool{}
		for _, n := range c.taken {
			taken[n] = true
		}
		if got := relationshipName(c.user, c.relation, c.object, taken); got != c.want || !nameRe.MatchString(got) {
			t.Errorf("%s %s %s: %q", c.user, c.relation, c.object, got)
		}
	}
}

// Adding writes a Relationship named after it, as the signed-in admin;
// one the model already has, or a malformed one, is refused. Removing
// takes one of the model's away, never another model's.
func TestRelationshipsAddRemove(t *testing.T) {
	objs := relationshipObjects()
	var seed []runtime.Object
	for _, o := range objs["relationships"] {
		seed = append(seed, &unstructured.Unstructured{Object: o})
	}
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvrRel: "RelationshipList"}, seed...)
	rs := &Relationships{k: storeKube(objs), writer: func(*http.Request) (dynamic.Interface, error) { return cl, nil }}
	add := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		rs.Add(w, relRequest(http.MethodPost, body, "ns", "id", "name", "firm"))
		return w
	}
	w := add(`{"user": "book:ledger", "relation": "book", "object": "client:c-9"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	got, err := cl.Resource(gvrRel).Namespace("id").Get(ctx, "ledger-book-c-9", metav1.GetOptions{})
	if err != nil || str(got.Object, "spec", "model") != "firm" || str(got.Object, "spec", "object") != "client:c-9" {
		t.Fatalf("created %v %v", got, err)
	}
	if w := add(`{"user": "user:ann@firm.lab", "relation": "advisor", "object": "book:ledger"}`); w.Code != http.StatusConflict {
		t.Errorf("one the model has: %d", w.Code)
	}
	for _, bad := range []string{`{"user": "ann", "relation": "advisor", "object": "book:ledger"}`, `{"user": "user:ann", "relation": "Advisor", "object": "book:ledger"}`,
		`{"user": "user:ann", "relation": "advisor", "object": "book:ledger#x"}`, `{"user": "user:a b", "relation": "advisor", "object": "book:ledger"}`} {
		if w := add(bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	w = httptest.NewRecorder()
	rs.Add(w, relRequest(http.MethodPost, `{"user": "user:a", "relation": "advisor", "object": "book:b"}`, "ns", "id", "name", "nope"))
	if w.Code != http.StatusNotFound {
		t.Errorf("no model: %d", w.Code)
	}

	remove := func(name string) int {
		w := httptest.NewRecorder()
		rs.Remove(w, relRequest(http.MethodDelete, `{"resourceVersion": "7"}`, "ns", "id", "name", "firm", "rel", name))
		return w.Code
	}
	if remove("other-model") != http.StatusNotFound {
		t.Error("another model's relationship")
	}
	if remove("ann-advisor-ledger") != http.StatusNoContent {
		t.Fatal("remove")
	}
	if _, err := cl.Resource(gvrRel).Namespace("id").Get(ctx, "ann-advisor-ledger", metav1.GetOptions{}); err == nil {
		t.Error("still there")
	}
	if _, err := cl.Resource(gvrRel).Namespace("elsewhere").Get(ctx, "ann-advisor-ledger", metav1.GetOptions{}); err != nil {
		t.Error("another namespace's went too")
	}
}

// The what-if is the guard's own answer, passed through; without a guard
// it says so.
func TestRelationshipsEvaluate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			User   string   `json:"user"`
			Groups []string `json:"groups"`
			Agent  string   `json:"agent"`
			Server string   `json:"server"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/evaluate" || json.NewDecoder(r.Body).Decode(&in) != nil ||
			in.User != "ann@firm.lab" || strings.Join(in.Groups, ",") != "advisors" || in.Agent != "agent:apps.desk" || in.Server != "ledger" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agent": "agent:apps.desk", "decision": {"allow": true, "actsFor": true, "canCall": true, "clients": ["c-9"], "reason": "ok"}}`))
	}))
	defer srv.Close()
	const body = `{"user": "ann@firm.lab", "groups": ["advisors"], "agent": "agent:apps.desk", "server": "ledger"}`
	eval := func(objs map[string][]map[string]any, name string) *httptest.ResponseRecorder {
		rs := &Relationships{k: storeKube(objs), http: srv.Client(), guardURL: srv.URL}
		w := httptest.NewRecorder()
		rs.Evaluate(w, relRequest(http.MethodPost, body, "ns", "id", "name", name))
		return w
	}
	objs := relationshipObjects()
	if w := eval(objs, "firm"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"clients": ["c-9"]`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := eval(objs, "nope"); w.Code != http.StatusNotFound {
		t.Errorf("no model: %d", w.Code)
	}
	objs["services"] = nil
	if w := eval(objs, "firm"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no guard: %d", w.Code)
	}
}

func TestRelationshipModelSave(t *testing.T) {
	objs := relationshipObjects()
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvrRelModel: "RelationshipModelList"},
		&unstructured.Unstructured{Object: objs["relationshipmodels"][0]})
	rs := &Relationships{k: storeKube(objs), writer: func(*http.Request) (dynamic.Interface, error) { return cl, nil }}
	save := func(model string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"model": model})
		w := httptest.NewRecorder()
		rs.SaveModel(w, relRequest(http.MethodPut, string(b), "ns", "id", "name", "firm"))
		return w
	}

	// OpenFGA's parser refuses it: nothing is written, and the editor is told where
	w := save("model\n  schema 1.1\n\ntype user\n\ntype book\n  relations\n    define advisor: [user\n")
	var bad struct{ Errors []CodeMarker }
	if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &bad) != nil || len(bad.Errors) == 0 || bad.Errors[0].Line < 8 {
		t.Fatalf("an unparseable model: %d %s", w.Code, w.Body)
	}
	if w := save("  "); w.Code != http.StatusBadRequest {
		t.Fatalf("an empty model: %d", w.Code)
	}

	good := "model\n  schema 1.1\n\ntype user\n\ntype book\n  relations\n    define advisor: [user]\n"
	if w := save(good); w.Code != http.StatusOK {
		t.Fatalf("a model that parses: %d %s", w.Code, w.Body)
	}
	got, err := cl.Resource(gvrRelModel).Namespace("id").Get(context.Background(), "firm", metav1.GetOptions{})
	if err != nil || str(got.Object, "spec", "model") != good || str(got.Object, "spec", "description") != "who advises whom" {
		t.Fatalf("stored: %v %v", got, err)
	}

	w = httptest.NewRecorder()
	rs.SaveModel(w, relRequest(http.MethodPut, `{"model": "x"}`, "ns", "id", "name", "nope"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("no such model: %d", w.Code)
	}
}
