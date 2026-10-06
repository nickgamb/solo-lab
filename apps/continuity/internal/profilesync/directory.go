// Package profilesync reads linked users' records from each tier's directory
// and writes the mapped profile attributes into the broker. Tiers earlier in
// the chain win. It never reads or writes passwords or credentials, and never
// writes the identity keys (username, email).
package profilesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNoUser: the directory has no record for that subject.
var ErrNoUser = errors.New("no such user in the directory")

// Directory reads one user's record, as claims (OIDC claim names where the
// directory has a standard field; its own nested fields otherwise).
type Directory interface {
	User(ctx context.Context, sub string) (map[string]any, error)
}

// Checker tests a directory: a token, then a read of its users. It returns
// how many users the directory reports, never their records.
type Checker interface {
	Check(ctx context.Context) (int, error)
}

func count(v any) int {
	n, _ := v.(float64)
	return int(n)
}

// Credentials are the client_credentials the sync uses at the tier's token
// endpoint: read-only access to the directory's users.
type Credentials struct {
	TokenURL, ClientID, ClientSecret, Audience string
	Scopes                                     []string
}

// tokenSource caches one client_credentials access token.
type tokenSource struct {
	hc  *http.Client
	c   Credentials
	mu  sync.Mutex
	tok string
	exp time.Time
}

func (t *tokenSource) token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Now().Before(t.exp) {
		return t.tok, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(t.c.Scopes) > 0 {
		form.Set("scope", strings.Join(t.c.Scopes, " "))
	}
	if t.c.Audience != "" {
		form.Set("audience", t.c.Audience)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(t.c.ClientID), url.QueryEscape(t.c.ClientSecret))
	resp, err := t.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("directory token: HTTP %d %s", resp.StatusCode, out.Error)
	}
	life := time.Duration(out.ExpiresIn) * time.Second
	if life <= 0 {
		life = time.Minute
	}
	t.tok, t.exp = out.AccessToken, time.Now().Add(life*9/10)
	return t.tok, nil
}

func getJSON(ctx context.Context, hc *http.Client, ts *tokenSource, u string, accept string) (map[string]any, error) {
	tok, err := ts.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", accept)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNoUser
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("directory: HTTP %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// New returns the directory for type typ at base.
func New(typ, base string, c Credentials, hc *http.Client) (Directory, error) {
	ts := &tokenSource{hc: hc, c: c}
	base = strings.TrimSuffix(base, "/")
	switch typ {
	case "scim":
		return &scim{base: base, hc: hc, ts: ts}, nil
	case "auth0":
		return &auth0{base: base, hc: hc, ts: ts}, nil
	case "keycloak":
		return &keycloakDir{base: base, hc: hc, ts: ts}, nil
	}
	return nil, fmt.Errorf("directory type %q: scim, auth0 or keycloak", typ)
}

// scim is a SCIM 2.0 directory (RFC 7643/7644). The upstream's sub is the
// SCIM id (Gluu: the user's inum).
type scim struct {
	base string
	hc   *http.Client
	ts   *tokenSource
}

func (d *scim) User(ctx context.Context, sub string) (map[string]any, error) {
	r, err := getJSON(ctx, d.hc, d.ts, d.base+"/Users/"+url.PathEscape(sub), "application/scim+json, application/json")
	if err != nil {
		return nil, err
	}
	return fromSCIM(r), nil
}

func (d *scim) Check(ctx context.Context) (int, error) {
	r, err := getJSON(ctx, d.hc, d.ts, d.base+"/Users?count=1&attributes=id", "application/scim+json, application/json")
	if errors.Is(err, ErrNoUser) {
		return 0, fmt.Errorf("directory: HTTP 404 at %s/Users", d.base)
	}
	return count(r["totalResults"]), err
}

// fromSCIM puts SCIM core fields under their OIDC claim names; the record's
// other fields (extensions by their schema URN) stay as they are.
func fromSCIM(r map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		out[k] = v
	}
	if n, ok := r["name"].(map[string]any); ok {
		set(out, "given_name", n["givenName"])
		set(out, "family_name", n["familyName"])
		set(out, "middle_name", n["middleName"])
		if f, _ := n["formatted"].(string); f != "" {
			out["name"] = f
		} else {
			delete(out, "name")
		}
	}
	set(out, "preferred_username", r["userName"])
	set(out, "nickname", r["nickName"])
	set(out, "locale", r["locale"])
	set(out, "zoneinfo", r["timezone"])
	set(out, "website", r["profileUrl"])
	if d, _ := r["displayName"].(string); d != "" && out["name"] == nil {
		out["name"] = d
	}
	set(out, "email", primary(r["emails"]))
	set(out, "phone_number", primary(r["phoneNumbers"]))
	set(out, "picture", primary(r["photos"]))
	if gs, ok := r["groups"].([]any); ok {
		var names []any
		for _, g := range gs {
			if m, ok := g.(map[string]any); ok && m["display"] != nil {
				names = append(names, m["display"])
			}
		}
		out["groups"] = names
	}
	return out
}

func set(m map[string]any, k string, v any) {
	if v != nil && v != "" {
		m[k] = v
	}
}

// primary is the value of a SCIM multi-valued attribute's primary entry, or
// its first.
func primary(v any) any {
	l, _ := v.([]any)
	var first any
	for _, x := range l {
		m, _ := x.(map[string]any)
		if m == nil {
			continue
		}
		if first == nil {
			first = m["value"]
		}
		if p, _ := m["primary"].(bool); p {
			return m["value"]
		}
	}
	return first
}

// auth0 is the Auth0 Management API v2: a user's fields are already OIDC
// claim names (given_name, family_name, ...), plus user_metadata and
// app_metadata.
type auth0 struct {
	base string
	hc   *http.Client
	ts   *tokenSource
}

func (d *auth0) User(ctx context.Context, sub string) (map[string]any, error) {
	return getJSON(ctx, d.hc, d.ts, d.base+"/users/"+url.PathEscape(sub), "application/json")
}

func (d *auth0) Check(ctx context.Context) (int, error) {
	r, err := getJSON(ctx, d.hc, d.ts, d.base+"/users?per_page=1&include_totals=true&fields=user_id", "application/json")
	if errors.Is(err, ErrNoUser) {
		return 0, fmt.Errorf("directory: HTTP 404 at %s/users", d.base)
	}
	return count(r["total"]), err
}

// keycloakDir is one Keycloak realm's admin API (base .../admin/realms/<realm>):
// the sub is the user id.
type keycloakDir struct {
	base string
	hc   *http.Client
	ts   *tokenSource
}

func (d *keycloakDir) User(ctx context.Context, sub string) (map[string]any, error) {
	r, err := getJSON(ctx, d.hc, d.ts, d.base+"/users/"+url.PathEscape(sub), "application/json")
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	set(out, "given_name", r["firstName"])
	set(out, "family_name", r["lastName"])
	set(out, "email", r["email"])
	set(out, "preferred_username", r["username"])
	if a, ok := r["attributes"].(map[string]any); ok {
		for k, v := range a {
			if l, ok := v.([]any); ok && len(l) == 1 {
				out[k] = l[0]
			} else {
				out[k] = v
			}
		}
	}
	return out, nil
}

func (d *keycloakDir) Check(ctx context.Context) (int, error) {
	tok, err := d.ts.token(ctx)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/users/count", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("directory: HTTP %d", resp.StatusCode)
	}
	var n int
	return n, json.NewDecoder(io.LimitReader(resp.Body, 64)).Decode(&n)
}

// Lookup reads path in a record: a dot path ("user_metadata.department"), or
// a SCIM extension attribute by its schema URN
// ("urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department").
func Lookup(r map[string]any, path string) any {
	if strings.HasPrefix(path, "urn:") {
		i := strings.LastIndex(path, ":")
		if ext, ok := r[path[:i]].(map[string]any); ok {
			return Lookup(ext, path[i+1:])
		}
		return nil
	}
	var cur any = r
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// Values turns a record value into attribute values (nil when absent).
func Values(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, Values(e)...)
		}
		return out
	case map[string]any:
		return nil // an object is not an attribute value
	default:
		return []string{fmt.Sprint(x)}
	}
}
