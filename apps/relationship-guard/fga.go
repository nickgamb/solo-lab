package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// fga is the OpenFGA store the continuity controller keeps (by name), asked
// with OpenFGA's own API: Check and ListObjects, against its latest model.
type fga struct {
	url, token, store string
	http              *http.Client

	mu      sync.Mutex
	storeID string
	at      time.Time
}

// tuple is one relationship, as OpenFGA reads it.
type tuple struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

type fgaError struct {
	status  int
	message string
}

func (e *fgaError) Error() string { return fmt.Sprintf("openfga %d: %s", e.status, e.message) }

// tryTimeout bounds one attempt; the caller's deadline bounds both.
const tryTimeout = 1500 * time.Millisecond

// do: one request to OpenFGA. Every request the guard makes is a read
// (Check, ListObjects, the store list), so one that fails in transit or gets
// a 5xx is tried once more: a replica gone, a connection to it gone stale, a
// database failover under way. The broken connection isn't reused.
func (f *fga) do(ctx context.Context, method, path string, in, out any) error {
	var b []byte
	if in != nil {
		b, _ = json.Marshal(in)
	}
	var err error
	for try := 0; try < 2; try++ {
		var again bool
		if again, err = f.once(ctx, method, path, b, out); !again || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func (f *fga) once(ctx context.Context, method, path string, b []byte, out any) (again bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, tryTimeout)
	defer cancel()
	var body io.Reader
	if b != nil {
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.url+path, body)
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode >= 500, &fgaError{resp.StatusCode, e.Message}
	}
	return false, json.Unmarshal(raw, out)
}

// storeIDFor: the store's ID, looked up by name and kept for a few minutes
// (the controller creates it; a deleted one is looked up again).
func (f *fga) storeIDFor(ctx context.Context) (string, error) {
	f.mu.Lock()
	id, at := f.storeID, f.at
	f.mu.Unlock()
	if id != "" && time.Since(at) < 5*time.Minute {
		return id, nil
	}
	token := ""
	for {
		var page struct {
			Stores []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"stores"`
			Continuation string `json:"continuation_token"`
		}
		q := url.Values{"page_size": {"100"}}
		if token != "" {
			q.Set("continuation_token", token)
		}
		if err := f.do(ctx, http.MethodGet, "/stores?"+q.Encode(), nil, &page); err != nil {
			return "", err
		}
		for _, s := range page.Stores {
			if s.Name == f.store {
				f.mu.Lock()
				f.storeID, f.at = s.ID, time.Now()
				f.mu.Unlock()
				return s.ID, nil
			}
		}
		if page.Continuation == "" {
			return "", fmt.Errorf("no OpenFGA store %q (its RelationshipModel synced?)", f.store)
		}
		token = page.Continuation
	}
}

func (f *fga) forget() {
	f.mu.Lock()
	f.storeID = ""
	f.mu.Unlock()
}

// call: a request against the store, looking it up again once if it's gone.
func (f *fga) call(ctx context.Context, op string, in, out any) error {
	for attempt := 0; ; attempt++ {
		id, err := f.storeIDFor(ctx)
		if err != nil {
			return err
		}
		err = f.do(ctx, http.MethodPost, "/stores/"+id+"/"+op, in, out)
		if e, ok := err.(*fgaError); ok && e.status == http.StatusNotFound && attempt == 0 {
			f.forget()
			continue
		}
		return err
	}
}

func contextual(ts []tuple) map[string]any {
	if len(ts) == 0 {
		return nil
	}
	return map[string]any{"tuple_keys": ts}
}

// check: whether user has relation to object, with the given contextual
// tuples (what the verified token says, held for this call only).
func (f *fga) check(ctx context.Context, user, relation, object string, ctxTuples []tuple) (bool, error) {
	var out struct {
		Allowed bool `json:"allowed"`
	}
	in := map[string]any{"tuple_key": tuple{user, relation, object}}
	if c := contextual(ctxTuples); c != nil {
		in["contextual_tuples"] = c
	}
	err := f.call(ctx, "check", in, &out)
	return out.Allowed, err
}

// listObjects: the objects of a type user has relation to (their IDs, without
// the type).
func (f *fga) listObjects(ctx context.Context, user, relation, typ string, ctxTuples []tuple) ([]string, error) {
	var out struct {
		Objects []string `json:"objects"`
	}
	in := map[string]any{"user": user, "relation": relation, "type": typ}
	if c := contextual(ctxTuples); c != nil {
		in["contextual_tuples"] = c
	}
	if err := f.call(ctx, "list-objects", in, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Objects))
	for _, o := range out.Objects {
		ids = append(ids, strings.TrimPrefix(o, typ+":"))
	}
	return ids, nil
}
