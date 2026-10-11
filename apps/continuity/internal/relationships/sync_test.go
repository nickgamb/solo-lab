package relationships

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

const dsl = `model
  schema 1.1

type user

type book
  relations
    define advisor: [user]

type client
  relations
    define book: [book]
    define can_view: advisor from book
`

// fake: one store, the model last written, its tuples. It refuses tuples
// whose relation the model doesn't define on the object's type, as OpenFGA
// does, and counts calls.
type fake struct {
	storeID, modelID string
	model            json.RawMessage
	tuples           map[Tuple]bool
	modelWrites      int
}

func (f *fake) EnsureStore(context.Context, string) (string, error) {
	if f.storeID == "" {
		f.storeID = "store-1"
	}
	return f.storeID, nil
}
func (f *fake) LatestModel(context.Context, string) (string, json.RawMessage, error) {
	return f.modelID, f.model, nil
}
func (f *fake) WriteModel(_ context.Context, _ string, m json.RawMessage) (string, error) {
	f.modelWrites++
	f.modelID, f.model = fmt.Sprintf("model-%d", f.modelWrites), m
	return f.modelID, nil
}
func (f *fake) ReadAll(context.Context, string) ([]Tuple, error) {
	var out []Tuple
	for t := range f.tuples {
		out = append(out, t)
	}
	return out, nil
}
func (f *fake) Write(_ context.Context, _, _ string, writes, deletes []Tuple) error {
	for _, t := range writes {
		if t.Relation == "nope" {
			return &APIError{Status: 400, Code: "validation_error", Message: "relation 'client#nope' not found"}
		}
		if f.tuples[t] {
			return &APIError{Status: 400, Code: "write_failed_due_to_invalid_input", Message: "tuple exists"}
		}
	}
	for _, t := range deletes {
		delete(f.tuples, t)
	}
	for _, t := range writes {
		f.tuples[t] = true
	}
	return nil
}

func TestSyncWritesTheModelOnceAndMatchesTheTuples(t *testing.T) {
	f := &fake{tuples: map[Tuple]bool{{"book:old", "book", "client:c-9"}: true}}
	decl := []Declared{
		{"bob-advises-sv-bob", Tuple{"user:bob@sterling.lab", "advisor", "book:sv-bob"}},
		{"c-1001-in-sv-bob", Tuple{"book:sv-bob", "book", "client:c-1001"}},
	}
	res, err := Sync(context.Background(), f, "sv-identity/sterling-vance", dsl, decl)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ModelWritten || res.Written != 2 || res.Deleted != 1 || res.Relationships != 2 || len(f.tuples) != 2 {
		t.Fatalf("first sync: %+v, store %v", res, f.tuples)
	}
	// the same again: nothing to do, and the model isn't rewritten
	res, err = Sync(context.Background(), f, "sv-identity/sterling-vance", dsl, decl)
	if err != nil || res.ModelWritten || res.Written != 0 || res.Deleted != 0 || f.modelWrites != 1 {
		t.Fatalf("second sync: %+v %v, model writes %d", res, err, f.modelWrites)
	}
	// a changed model is written
	if res, _ = Sync(context.Background(), f, "x", dsl+"\ntype group\n", decl); !res.ModelWritten || f.modelWrites != 2 {
		t.Fatalf("changed model: %+v", res)
	}
}

func TestSyncNamesWhatOpenFGARefusesAndKeepsTheRest(t *testing.T) {
	f := &fake{tuples: map[Tuple]bool{}}
	decl := []Declared{
		{"good", Tuple{"book:sv-bob", "book", "client:c-1001"}},
		{"bad", Tuple{"user:bob@sterling.lab", "nope", "client:c-1001"}},
	}
	res, err := Sync(context.Background(), f, "s", dsl, decl)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected["bad"] == "" || len(res.Rejected) != 1 || !f.tuples[decl[0].Tuple] || res.Relationships != 1 {
		t.Fatalf("%+v, store %v", res, f.tuples)
	}
}

// The DSL is parsed here; what it means (an undefined type, say) is OpenFGA's
// to judge when the model is written.
func TestDSLThatDoesNotParseIsAnError(t *testing.T) {
	if _, err := Sync(context.Background(), &fake{tuples: map[Tuple]bool{}}, "s", "model\n  schema 1.1\ntype x\n  relations\n    define y [user\n", nil); err == nil {
		t.Fatal("want an error")
	}
}

// OpenFGA hands a model back with fields the DSL leaves out: it's still the
// same model, and isn't written again.
func TestTheStoredFormIsTheSameModel(t *testing.T) {
	ours, err := ModelJSON(dsl)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(ours, &m)
	m["id"] = "01M4"
	m["conditions"] = map[string]any{}
	// as OpenFGA hands it back: a type with no relations has an empty map,
	// a computed relation an empty list of user types
	for _, td := range m["type_definitions"].([]any) {
		d := td.(map[string]any)
		if _, ok := d["relations"]; !ok {
			d["relations"] = map[string]any{}
		}
	}
	for _, td := range m["type_definitions"].([]any) {
		d := td.(map[string]any)
		md, _ := d["metadata"].(map[string]any)
		if md == nil {
			d["metadata"] = nil
			continue
		}
		md["module"], md["source_info"] = "", nil
		for _, r := range md["relations"].(map[string]any) {
			rel := r.(map[string]any)
			rel["module"], rel["source_info"] = "", nil
			users, _ := rel["directly_related_user_types"].([]any)
			if users == nil {
				rel["directly_related_user_types"] = []any{}
			}
			for _, u := range users {
				u.(map[string]any)["condition"] = ""
			}
		}
	}
	stored, _ := json.Marshal(m)
	if !sameModel(ours, stored) {
		t.Fatalf("the stored form differs:\n%s\n%s", ours, stored)
	}
	// a real change still shows
	other, _ := ModelJSON(dsl + "\ntype group\n")
	if sameModel(other, stored) {
		t.Fatal("a changed model compared equal")
	}
}
