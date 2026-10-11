package relationships

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/openfga/language/pkg/go/transformer"
)

// Store is what a sync needs of OpenFGA (Client, or a fake in tests).
type Store interface {
	EnsureStore(ctx context.Context, name string) (string, error)
	LatestModel(ctx context.Context, storeID string) (string, json.RawMessage, error)
	WriteModel(ctx context.Context, storeID string, model json.RawMessage) (string, error)
	ReadAll(ctx context.Context, storeID string) ([]Tuple, error)
	Write(ctx context.Context, storeID, modelID string, writes, deletes []Tuple) error
}

// Declared is one Relationship: its name (for reporting) and its tuple.
type Declared struct {
	Name  string
	Tuple Tuple
}

// Result is what a sync did and found.
type Result struct {
	StoreID, ModelID string
	ModelWritten     bool
	Written, Deleted int
	Relationships    int
	// Rejected: Relationships OpenFGA refused under the model, by name, with
	// its reason. Everything else is in the store.
	Rejected map[string]string
}

// ModelJSON turns a model in OpenFGA's DSL into the JSON its API takes.
func ModelJSON(dsl string) (json.RawMessage, error) {
	s, err := transformer.TransformDSLToJSON(dsl)
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	return json.RawMessage(s), nil
}

// sameModel: the two models define the same types, relations and
// conditions. OpenFGA stores a model with fields the DSL leaves out (empty
// names, null source info, empty lists and objects), so both sides drop
// every empty value first. Nothing that tells two models apart is empty: a
// direct relation ("this": {}) always lists its user types too.
func sameModel(a, b json.RawMessage) bool {
	norm := func(m json.RawMessage) any {
		var v map[string]any
		_ = json.Unmarshal(m, &v)
		out := map[string]any{}
		for _, k := range []string{"schema_version", "type_definitions", "conditions"} {
			if c := clean(v[k]); c != nil {
				out[k] = c
			}
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// clean: v without empty values (null, "", {}, []), at any depth; nil when
// nothing is left.
func clean(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if c := clean(x); c != nil {
				out[k] = c
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			if c := clean(x); c != nil {
				out = append(out, c)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		if t == "" {
			return nil
		}
	case nil:
		return nil
	}
	return v
}

// Sync makes the named store hold the model and exactly the declared
// relationships.
func Sync(ctx context.Context, s Store, storeName, dsl string, declared []Declared) (Result, error) {
	var res Result
	model, err := ModelJSON(dsl)
	if err != nil {
		return res, err
	}
	if res.StoreID, err = s.EnsureStore(ctx, storeName); err != nil {
		return res, fmt.Errorf("store: %w", err)
	}
	id, have, err := s.LatestModel(ctx, res.StoreID)
	if err != nil {
		return res, fmt.Errorf("model: %w", err)
	}
	if id == "" || !sameModel(have, model) {
		if id, err = s.WriteModel(ctx, res.StoreID, model); err != nil {
			return res, fmt.Errorf("model: %w", err)
		}
		res.ModelWritten = true
	}
	res.ModelID = id

	want := map[Tuple]string{}
	for _, d := range declared {
		want[d.Tuple] = d.Name
	}
	writes, deletes, err := diff(ctx, s, res.StoreID, want)
	if err != nil {
		return res, err
	}
	if err := s.Write(ctx, res.StoreID, res.ModelID, writes, deletes); err != nil {
		if _, ok := err.(*APIError); !ok {
			return res, fmt.Errorf("write: %w", err)
		}
		// one refused tuple takes its whole batch with it, and earlier batches
		// may already be in: read the store again and apply what's left one
		// at a time, to keep the rest and name what OpenFGA refuses
		if writes, deletes, err = diff(ctx, s, res.StoreID, want); err != nil {
			return res, err
		}
		for _, t := range deletes {
			if s.Write(ctx, res.StoreID, res.ModelID, nil, []Tuple{t}) == nil {
				res.Deleted++
			}
		}
		for _, t := range writes {
			if err := s.Write(ctx, res.StoreID, res.ModelID, []Tuple{t}, nil); err != nil {
				if res.Rejected == nil {
					res.Rejected = map[string]string{}
				}
				res.Rejected[want[t]] = reason(err)
				continue
			}
			res.Written++
		}
	} else {
		res.Written, res.Deleted = len(writes), len(deletes)
	}
	res.Relationships = len(want) - len(res.Rejected)
	return res, nil
}

// diff: what to write and delete for the store to hold exactly want.
func diff(ctx context.Context, s Store, storeID string, want map[Tuple]string) (writes, deletes []Tuple, err error) {
	stored, err := s.ReadAll(ctx, storeID)
	if err != nil {
		return nil, nil, fmt.Errorf("read: %w", err)
	}
	have := map[Tuple]bool{}
	for _, t := range stored {
		have[t] = true
		if _, ok := want[t]; !ok {
			deletes = append(deletes, t)
		}
	}
	for t := range want {
		if !have[t] {
			writes = append(writes, t)
		}
	}
	order := func(a, b Tuple) int { return strings.Compare(a.String(), b.String()) }
	slices.SortFunc(writes, order)
	slices.SortFunc(deletes, order)
	return writes, deletes, nil
}

func reason(err error) string {
	if e, ok := err.(*APIError); ok && e.Message != "" {
		return e.Message
	}
	return err.Error()
}
