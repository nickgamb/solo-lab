// Package relationships keeps an OpenFGA store matching a RelationshipModel
// and its Relationships: the model written when it changes, and exactly the
// declared tuples, no more and no fewer. It speaks OpenFGA's HTTP API.
package relationships

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tuple is one relationship as OpenFGA stores it.
type Tuple struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

func (t Tuple) String() string { return t.User + " " + t.Relation + " " + t.Object }

// maxPerWrite is OpenFGA's default limit on tuples in one write.
const maxPerWrite = 100

// Client is an OpenFGA server, authenticated with a pre-shared key.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func New(rawURL, token string) *Client {
	return &Client{URL: strings.TrimSuffix(rawURL, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// APIError is OpenFGA's answer to a request it refused.
type APIError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openfga %d %s: %s", e.Status, e.Code, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// EnsureStore returns the ID of the store with this name, creating it.
func (c *Client) EnsureStore(ctx context.Context, name string) (string, error) {
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
		if err := c.do(ctx, http.MethodGet, "/stores?"+q.Encode(), nil, &page); err != nil {
			return "", err
		}
		for _, s := range page.Stores {
			if s.Name == name {
				return s.ID, nil
			}
		}
		if page.Continuation == "" {
			break
		}
		token = page.Continuation
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/stores", map[string]string{"name": name}, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// DeleteStore removes a store and everything in it.
func (c *Client) DeleteStore(ctx context.Context, storeID string) error {
	err := c.do(ctx, http.MethodDelete, "/stores/"+storeID, nil, nil)
	if e, ok := err.(*APIError); ok && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// LatestModel returns the store's newest authorization model ("" when it has
// none) and its type definitions and conditions, for comparing.
func (c *Client) LatestModel(ctx context.Context, storeID string) (string, json.RawMessage, error) {
	var page struct {
		Models []json.RawMessage `json:"authorization_models"`
	}
	if err := c.do(ctx, http.MethodGet, "/stores/"+storeID+"/authorization-models?page_size=1", nil, &page); err != nil {
		return "", nil, err
	}
	if len(page.Models) == 0 {
		return "", nil, nil
	}
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(page.Models[0], &m); err != nil {
		return "", nil, err
	}
	return m.ID, page.Models[0], nil
}

// WriteModel writes a new authorization model (OpenFGA's JSON form).
func (c *Client) WriteModel(ctx context.Context, storeID string, model json.RawMessage) (string, error) {
	var out struct {
		ID string `json:"authorization_model_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/stores/"+storeID+"/authorization-models", model, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ReadAll returns every tuple in the store.
func (c *Client) ReadAll(ctx context.Context, storeID string) ([]Tuple, error) {
	var all []Tuple
	token := ""
	for {
		in := map[string]any{"page_size": 100}
		if token != "" {
			in["continuation_token"] = token
		}
		var page struct {
			Tuples []struct {
				Key Tuple `json:"key"`
			} `json:"tuples"`
			Continuation string `json:"continuation_token"`
		}
		if err := c.do(ctx, http.MethodPost, "/stores/"+storeID+"/read", in, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Tuples {
			all = append(all, t.Key)
		}
		if page.Continuation == "" {
			return all, nil
		}
		token = page.Continuation
	}
}

// Write adds and removes tuples, against the given model, at most
// maxPerWrite to a call.
func (c *Client) Write(ctx context.Context, storeID, modelID string, writes, deletes []Tuple) error {
	for len(writes) > 0 || len(deletes) > 0 {
		in := map[string]any{"authorization_model_id": modelID}
		n := 0
		if len(deletes) > 0 {
			k := min(len(deletes), maxPerWrite)
			in["deletes"] = map[string]any{"tuple_keys": deletes[:k]}
			deletes, n = deletes[k:], k
		}
		if len(writes) > 0 && n < maxPerWrite {
			k := min(len(writes), maxPerWrite-n)
			in["writes"] = map[string]any{"tuple_keys": writes[:k]}
			writes = writes[k:]
		}
		if err := c.do(ctx, http.MethodPost, "/stores/"+storeID+"/write", in, nil); err != nil {
			return err
		}
	}
	return nil
}
