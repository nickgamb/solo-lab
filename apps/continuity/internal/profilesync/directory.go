// Package profilesync is the directory sync: for each employee the broker
// knows, the primary IdP's user record is read into the broker's profile,
// then the broker's profile is written to each failover IdP (the user is
// created there if missing), through each IdP's attribute mapping. It never
// reads or writes a password or other credential, never writes the username,
// and never deletes a user.
package profilesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNoUser: the directory has no such user.
	ErrNoUser = errors.New("no such user in the directory")
	// ErrNoCreate: the directory can't create a user without a credential.
	ErrNoCreate = errors.New("this directory can't create a user without a password")
	// ErrNoEnroll: the directory has no way to have a new user set their
	// own credential.
	ErrNoEnroll = errors.New("this directory can't send a credential enrollment")
)

// Directory is one IdP's user store, by the IdP's own attribute paths.
type Directory interface {
	// User is the user's record.
	User(ctx context.Context, id string) (map[string]any, error)
	// Find is the id of the user with that email, or ErrNoUser.
	Find(ctx context.Context, email string) (string, error)
	// Update sets attributes (path -> values) on the user.
	Update(ctx context.Context, id string, set map[string][]string) error
	// Create creates the user (no credential) with those attributes and
	// returns its id, or ErrNoCreate.
	Create(ctx context.Context, email string, set map[string][]string) (string, error)
	// Enroll has the directory ask a new user to set their own credential
	// there (an email from the IdP), or ErrNoEnroll. The sync never sees it.
	Enroll(ctx context.Context, id string) error
	// Check gets a token and reads the users' count, never their records.
	Check(ctx context.Context) (int, error)
	// Schema lists the attribute paths the directory knows.
	Schema(ctx context.Context) ([]string, error)
}

// Credentials are the client_credentials the sync uses at the IdP's token
// endpoint.
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

// api is one directory's HTTP API, authenticated with the token source.
type api struct {
	base, accept, ctype string
	hc                  *http.Client
	ts                  *tokenSource
}

// call sends method to path with body (JSON) and decodes the answer into out.
// A 404 is ErrNoUser; any other answer outside 2xx an error with its status.
func (a *api) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := a.ts.token(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", a.accept)
	if body != nil {
		req.Header.Set("Content-Type", a.ctype)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoUser
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("directory: %s %s: HTTP %d", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// New returns the directory for type typ at base.
func New(typ, base string, c Credentials, hc *http.Client) (Directory, error) {
	ts := &tokenSource{hc: hc, c: c}
	base = strings.TrimSuffix(base, "/")
	switch typ {
	case "scim":
		return &scim{api{base: base, accept: "application/scim+json, application/json", ctype: "application/scim+json", hc: hc, ts: ts}}, nil
	case "auth0":
		return &auth0{api{base: base, accept: "application/json", ctype: "application/json", hc: hc, ts: ts}}, nil
	case "keycloak":
		return &keycloakDir{api{base: base, accept: "application/json", ctype: "application/json", hc: hc, ts: ts}}, nil
	}
	return nil, fmt.Errorf("directory type %q: scim, auth0 or keycloak", typ)
}

func one(v []string) any {
	if len(v) == 1 {
		return v[0]
	}
	return v
}

// ---- SCIM 2.0 (RFC 7643/7644): Gluu, Ping, most enterprise IdPs. The id is
// the SCIM id (Gluu: the user's inum, which is also its sub).

type scim struct{ api }

const (
	scimCore  = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimPatch = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
)

func (d *scim) User(ctx context.Context, id string) (map[string]any, error) {
	var r map[string]any
	return r, d.call(ctx, http.MethodGet, "/Users/"+url.PathEscape(id), nil, &r)
}

func (d *scim) Find(ctx context.Context, email string) (string, error) {
	var r struct {
		Resources []struct {
			ID string `json:"id"`
		} `json:"Resources"`
	}
	filter := `emails.value eq ` + strconv.Quote(email)
	if err := d.call(ctx, http.MethodGet, "/Users?count=2&attributes=id&filter="+url.QueryEscape(filter), nil, &r); err != nil {
		return "", err
	}
	if len(r.Resources) != 1 {
		return "", ErrNoUser // none, or not one user: never guess
	}
	return r.Resources[0].ID, nil
}

func (d *scim) Update(ctx context.Context, id string, set map[string][]string) error {
	var ops []any
	for _, p := range sortedKeys(set) {
		ops = append(ops, map[string]any{"op": "replace", "path": p, "value": one(set[p])})
	}
	return d.call(ctx, http.MethodPatch, "/Users/"+url.PathEscape(id), map[string]any{"schemas": []string{scimPatch}, "Operations": ops}, nil)
}

func (d *scim) Create(ctx context.Context, email string, set map[string][]string) (string, error) {
	u := map[string]any{"schemas": []string{scimCore}, "userName": email, "active": true,
		"emails": []any{map[string]any{"value": email, "primary": true}}}
	for _, p := range sortedKeys(set) {
		if err := setPath(u, p, one(set[p])); err != nil {
			return "", err
		}
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := d.call(ctx, http.MethodPost, "/Users", u, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (d *scim) Enroll(context.Context, string) error { return ErrNoEnroll }

func (d *scim) Check(ctx context.Context) (int, error) {
	var r struct {
		Total int `json:"totalResults"`
	}
	err := d.call(ctx, http.MethodGet, "/Users?count=1&attributes=id", nil, &r)
	if errors.Is(err, ErrNoUser) {
		err = fmt.Errorf("directory: HTTP 404 at %s/Users", d.base)
	}
	return r.Total, err
}

// Schema lists the User schema's and its extensions' writable attributes:
// "name.givenName", and "<extension URN>:<attribute>".
func (d *scim) Schema(ctx context.Context) ([]string, error) {
	var r struct {
		Resources []struct {
			ID         string     `json:"id"`
			Attributes []scimAttr `json:"attributes"`
		} `json:"Resources"`
	}
	if err := d.call(ctx, http.MethodGet, "/Schemas", nil, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.Resources {
		prefix := ""
		switch {
		case s.ID == scimCore:
		case strings.HasPrefix(s.ID, "urn:") && strings.Contains(s.ID, ":extension:") && strings.HasSuffix(s.ID, ":User"):
			prefix = s.ID + ":"
		default:
			continue
		}
		for _, a := range s.Attributes {
			if a.Mutability == "readOnly" || a.Name == "password" || a.Name == "userName" || a.MultiValued {
				continue
			}
			if a.Type == "complex" {
				for _, sa := range a.SubAttributes {
					if sa.Mutability != "readOnly" && !sa.MultiValued {
						out = append(out, prefix+a.Name+"."+sa.Name)
					}
				}
				continue
			}
			out = append(out, prefix+a.Name)
		}
	}
	return out, nil
}

type scimAttr struct {
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	MultiValued   bool       `json:"multiValued"`
	Mutability    string     `json:"mutability"`
	SubAttributes []scimAttr `json:"subAttributes"`
}

// ---- Auth0 Management API v2. The id is the user_id (the sub).

type auth0 struct{ api }

// the root attributes of an Auth0 user the sync may write
var auth0Root = []string{"email", "given_name", "family_name", "name", "nickname", "picture"}

func (d *auth0) User(ctx context.Context, id string) (map[string]any, error) {
	var r map[string]any
	return r, d.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id), nil, &r)
}

func (d *auth0) Find(ctx context.Context, email string) (string, error) {
	var r []struct {
		ID string `json:"user_id"`
	}
	if err := d.call(ctx, http.MethodGet, "/users-by-email?fields=user_id&email="+url.QueryEscape(email), nil, &r); err != nil {
		return "", err
	}
	if len(r) != 1 {
		return "", ErrNoUser
	}
	return r[0].ID, nil
}

func (d *auth0) Update(ctx context.Context, id string, set map[string][]string) error {
	body := map[string]any{}
	for _, p := range sortedKeys(set) {
		root := strings.SplitN(p, ".", 2)[0]
		if !slices.Contains(auth0Root, p) && root != "user_metadata" && root != "app_metadata" {
			return fmt.Errorf("auth0: %s is not writable (a root profile field, user_metadata.* or app_metadata.*)", p)
		}
		if err := setPath(body, p, one(set[p])); err != nil {
			return err
		}
		if p == "email" {
			body["email_verified"] = true // the primary verified it
		}
	}
	return d.call(ctx, http.MethodPatch, "/users/"+url.PathEscape(id), body, nil)
}

func (d *auth0) Create(context.Context, string, map[string][]string) (string, error) {
	return "", ErrNoCreate // a database connection requires a password
}

func (d *auth0) Enroll(context.Context, string) error { return ErrNoEnroll }

func (d *auth0) Check(ctx context.Context) (int, error) {
	var r struct {
		Total int `json:"total"`
	}
	err := d.call(ctx, http.MethodGet, "/users?per_page=1&include_totals=true&fields=user_id", nil, &r)
	if errors.Is(err, ErrNoUser) {
		err = fmt.Errorf("directory: HTTP 404 at %s/users", d.base)
	}
	return r.Total, err
}

// Schema: Auth0 has no schema API; its root profile fields, and metadata by
// path (user_metadata.<key>, app_metadata.<key>).
func (d *auth0) Schema(context.Context) ([]string, error) {
	return slices.Clone(auth0Root), nil
}

// ---- Keycloak admin API, one realm (base .../admin/realms/<realm>). The id
// is the user id (the sub).

type keycloakDir struct{ api }

var keycloakRoot = []string{"username", "email", "firstName", "lastName"}

// User flattens the representation: the root fields and each attribute by
// name, as Lookup reads them.
func (d *keycloakDir) User(ctx context.Context, id string) (map[string]any, error) {
	var r map[string]any
	if err := d.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id), nil, &r); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range keycloakRoot {
		if v, ok := r[k]; ok {
			out[k] = v
		}
	}
	if a, ok := r["attributes"].(map[string]any); ok {
		for k, v := range a {
			if _, root := out[k]; !root {
				out[k] = v
			}
		}
	}
	return out, nil
}

func (d *keycloakDir) Find(ctx context.Context, email string) (string, error) {
	var r []struct {
		ID string `json:"id"`
	}
	if err := d.call(ctx, http.MethodGet, "/users?exact=true&briefRepresentation=true&email="+url.QueryEscape(email), nil, &r); err != nil {
		return "", err
	}
	if len(r) != 1 {
		return "", ErrNoUser
	}
	return r[0].ID, nil
}

func (d *keycloakDir) Update(ctx context.Context, id string, set map[string][]string) error {
	var r map[string]any // the whole representation goes back: Keycloak drops what's left out
	if err := d.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id), nil, &r); err != nil {
		return err
	}
	if err := keycloakApply(r, set); err != nil {
		return err
	}
	return d.call(ctx, http.MethodPut, "/users/"+url.PathEscape(id), r, nil)
}

func keycloakApply(r map[string]any, set map[string][]string) error {
	attrs, _ := r["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	for p, v := range set {
		switch p {
		case "username":
			return errors.New("keycloak: username is never written")
		case "email", "firstName", "lastName":
			r[p] = v[0]
			if p == "email" {
				r["emailVerified"] = true // the primary verified it
			}
		default:
			l := make([]any, len(v))
			for i, s := range v {
				l[i] = s
			}
			attrs[p] = l
		}
	}
	r["attributes"] = attrs
	return nil
}

func (d *keycloakDir) Create(ctx context.Context, email string, set map[string][]string) (string, error) {
	r := map[string]any{"username": email, "email": email, "emailVerified": true, "enabled": true}
	if err := keycloakApply(r, set); err != nil {
		return "", err
	}
	if err := d.call(ctx, http.MethodPost, "/users", r, nil); err != nil {
		return "", err
	}
	return d.Find(ctx, email)
}

// Enroll: the realm emails the user a link to set their password (valid a
// week). Needs the realm's SMTP settings.
func (d *keycloakDir) Enroll(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodPut, "/users/"+url.PathEscape(id)+"/execute-actions-email?lifespan=604800", []string{"UPDATE_PASSWORD"}, nil)
}

func (d *keycloakDir) Check(ctx context.Context) (int, error) {
	var n int
	return n, d.call(ctx, http.MethodGet, "/users/count", nil, &n)
}

// Schema: the realm's user profile attributes.
func (d *keycloakDir) Schema(ctx context.Context) ([]string, error) {
	var r struct {
		Attributes []struct {
			Name string `json:"name"`
		} `json:"attributes"`
	}
	if err := d.call(ctx, http.MethodGet, "/users/profile/metadata", nil, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range r.Attributes {
		if a.Name != "username" {
			out = append(out, a.Name)
		}
	}
	return out, nil
}

// ---- paths

// Lookup reads path in a record: a dot path ("user_metadata.department"), a
// SCIM extension attribute by its schema URN
// ("urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department"),
// or a SCIM filtered value ("emails[primary eq true].value").
func Lookup(r map[string]any, path string) any {
	if strings.HasPrefix(path, "urn:") {
		i := strings.LastIndex(path, ":")
		if ext, ok := r[path[:i]].(map[string]any); ok {
			return Lookup(ext, path[i+1:])
		}
		return nil
	}
	var cur any = r
	for _, seg := range strings.Split(path, ".") {
		name, key, val, filtered := filterOf(seg)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[name]
		if filtered {
			l, _ := cur.([]any)
			cur = nil
			for _, x := range l {
				if e, ok := x.(map[string]any); ok && fmt.Sprint(e[key]) == val {
					cur = e
					break
				}
			}
		}
	}
	return cur
}

// filterOf splits `emails[primary eq true]` into emails, primary, true.
func filterOf(seg string) (name, key, val string, ok bool) {
	i := strings.Index(seg, "[")
	if i < 0 || !strings.HasSuffix(seg, "]") {
		return seg, "", "", false
	}
	f := strings.Fields(seg[i+1 : len(seg)-1])
	if len(f) != 3 || f[1] != "eq" {
		return seg, "", "", false
	}
	return seg[:i], f[0], strings.Trim(f[2], `"`), true
}

// setPath writes value at path into a JSON object, creating what's missing
// (a filtered segment becomes a list entry carrying the filter's key).
func setPath(obj map[string]any, path string, value any) error {
	if strings.HasPrefix(path, "urn:") {
		i := strings.LastIndex(path, ":")
		ext, _ := obj[path[:i]].(map[string]any)
		if ext == nil {
			ext = map[string]any{}
			obj[path[:i]] = ext
		}
		return setPath(ext, path[i+1:], value)
	}
	segs := strings.Split(path, ".")
	cur := obj
	for i, seg := range segs {
		name, key, val, filtered := filterOf(seg)
		last := i == len(segs)-1
		switch {
		case filtered:
			l, _ := cur[name].([]any)
			var entry map[string]any
			for _, x := range l {
				if e, ok := x.(map[string]any); ok && fmt.Sprint(e[key]) == val {
					entry = e
				}
			}
			if entry == nil {
				entry = map[string]any{key: parseScalar(val)}
				cur[name] = append(l, entry)
			}
			if last {
				return fmt.Errorf("%s: a filtered path needs a sub-attribute", path)
			}
			cur = entry
		case last:
			cur[name] = value
		default:
			next, _ := cur[name].(map[string]any)
			if next == nil {
				next = map[string]any{}
				cur[name] = next
			}
			cur = next
		}
	}
	return nil
}

func parseScalar(s string) any {
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	return s
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

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
