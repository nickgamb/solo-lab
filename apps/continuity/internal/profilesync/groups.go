package profilesync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Entry is one user in a directory's listing.
type Entry struct {
	ID, Email string
	Verified  *bool
	// Disabled: the directory has the user but won't let them sign in
	// (blocked, disabled, inactive).
	Disabled bool
}

// Lister lists a directory's users, a page at a time: the directory sync
// gives the broker an account for each of the primary's workforce users.
type Lister interface {
	List(ctx context.Context, first, max int) ([]Entry, error)
}

// Remover deletes a user from a directory: a failover's account of someone the
// primary no longer has (spec.sync.removeMissing).
type Remover interface {
	Remove(ctx context.Context, id string) error
}

// GroupReader reads the groups (or roles) a directory has a user in, by name.
type GroupReader interface {
	Groups(ctx context.Context, id string) ([]string, error)
}

// GroupWriter puts a user in exactly want among managed, by name, leaving
// the directory's other groups as they are; a managed group the directory
// doesn't have yet is created.
type GroupWriter interface {
	SetGroups(ctx context.Context, id string, want, managed []string) error
}

// ---- Auth0: roles are the groups.

func (d *auth0) List(ctx context.Context, first, max int) ([]Entry, error) {
	var us []struct {
		ID       string `json:"user_id"`
		Email    string `json:"email"`
		Verified *bool  `json:"email_verified"`
		Blocked  bool   `json:"blocked"`
	}
	q := fmt.Sprintf("/users?per_page=%d&page=%d&fields=user_id,email,email_verified,blocked&include_fields=true", max, first/max)
	if err := d.call(ctx, http.MethodGet, q, nil, &us); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(us))
	for _, u := range us {
		out = append(out, Entry{ID: u.ID, Email: u.Email, Verified: u.Verified, Disabled: u.Blocked})
	}
	return out, nil
}

func (d *auth0) Remove(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/users/"+url.PathEscape(id), nil, nil)
}

type auth0Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (d *auth0) userRoles(ctx context.Context, id string) ([]auth0Role, error) {
	var rs []auth0Role
	return rs, d.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id)+"/roles?per_page=100", nil, &rs)
}

func (d *auth0) Groups(ctx context.Context, id string) ([]string, error) {
	rs, err := d.userRoles(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out, nil
}

func (d *auth0) SetGroups(ctx context.Context, id string, want, managed []string) error {
	have, err := d.userRoles(ctx, id)
	if err != nil {
		return err
	}
	var all []auth0Role
	if err := d.call(ctx, http.MethodGet, "/roles?per_page=100", nil, &all); err != nil {
		return err
	}
	ids := map[string]string{}
	for _, r := range all {
		ids[r.Name] = r.ID
	}
	var add, remove []string
	for _, g := range managed {
		in := slices.ContainsFunc(have, func(r auth0Role) bool { return r.Name == g })
		switch wanted := slices.Contains(want, g); {
		case wanted && !in:
			if ids[g] == "" {
				var r auth0Role
				if err := d.call(ctx, http.MethodPost, "/roles", map[string]any{"name": g}, &r); err != nil {
					return err
				}
				ids[g] = r.ID
			}
			add = append(add, ids[g])
		case !wanted && in:
			remove = append(remove, ids[g])
		}
	}
	path := "/users/" + url.PathEscape(id) + "/roles"
	if len(add) > 0 {
		if err := d.call(ctx, http.MethodPost, path, map[string]any{"roles": add}, nil); err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		return d.call(ctx, http.MethodDelete, path, map[string]any{"roles": remove}, nil)
	}
	return nil
}

// ---- Keycloak: the realm's top-level groups.

func (d *keycloakDir) List(ctx context.Context, first, max int) ([]Entry, error) {
	var us []struct {
		ID       string `json:"id"`
		Email    string `json:"email"`
		Verified *bool  `json:"emailVerified"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := d.call(ctx, http.MethodGet, fmt.Sprintf("/users?briefRepresentation=true&first=%d&max=%d", first, max), nil, &us); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(us))
	for _, u := range us {
		out = append(out, Entry{ID: u.ID, Email: u.Email, Verified: u.Verified, Disabled: u.Enabled != nil && !*u.Enabled})
	}
	return out, nil
}

func (d *keycloakDir) Remove(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/users/"+url.PathEscape(id), nil, nil)
}

func (d *keycloakDir) Groups(ctx context.Context, id string) ([]string, error) {
	var gs []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := d.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id)+"/groups?briefRepresentation=true&max=1000", nil, &gs); err != nil {
		return nil, err
	}
	var out []string
	for _, g := range gs {
		if g.Path == "/"+g.Name {
			out = append(out, g.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (d *keycloakDir) groupID(ctx context.Context, name string, create bool) (string, error) {
	var g struct {
		ID string `json:"id"`
	}
	err := d.call(ctx, http.MethodGet, "/group-by-path/"+url.PathEscape(name), nil, &g)
	if errors.Is(err, ErrNoUser) && create { // a 404: no such group
		if err := d.call(ctx, http.MethodPost, "/groups", map[string]any{"name": name}, nil); err != nil {
			return "", err
		}
		return d.groupID(ctx, name, false)
	}
	return g.ID, err
}

func (d *keycloakDir) SetGroups(ctx context.Context, id string, want, managed []string) error {
	have, err := d.Groups(ctx, id)
	if err != nil {
		return err
	}
	for _, g := range managed {
		in, wanted := slices.Contains(have, g), slices.Contains(want, g)
		if in == wanted {
			continue
		}
		gid, err := d.groupID(ctx, g, wanted)
		if err != nil {
			return fmt.Errorf("group %s: %w", g, err)
		}
		method := http.MethodPut
		if !wanted {
			method = http.MethodDelete
		}
		if err := d.call(ctx, method, "/users/"+url.PathEscape(id)+"/groups/"+url.PathEscape(gid), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// ---- SCIM: users a page at a time; the record's groups (read-only here;
// membership is written on the Group resources, which the sync doesn't
// manage).

func (d *scim) List(ctx context.Context, first, max int) ([]Entry, error) {
	out := make([]Entry, 0, max)
	for len(out) < max {
		var r struct {
			Total     int              `json:"totalResults"`
			Resources []map[string]any `json:"Resources"`
		}
		start := first + len(out) // from 0; SCIM counts from 1
		q := fmt.Sprintf("/Users?startIndex=%d&count=%d", start+1, max-len(out))
		if err := d.call(ctx, http.MethodGet, q, nil, &r); err != nil {
			return nil, err
		}
		for _, u := range r.Resources {
			id, _ := u["id"].(string)
			email, verified := scimEmail(u)
			active, ok := u["active"].(bool)
			out = append(out, Entry{ID: id, Email: email, Verified: verified, Disabled: ok && !active})
		}
		// a server may answer with fewer than asked for (its own page size):
		// ask again from where it stopped, while it says there are more
		if len(r.Resources) == 0 || start+len(r.Resources) >= r.Total {
			break
		}
	}
	return out, nil
}

// scimEmail is the user's primary email (else the first), and whether the
// directory verified that one: SCIM has no core attribute for it, so it is
// the email's own "verified", else an extension's emailVerified (Gluu's),
// else unknown.
func scimEmail(u map[string]any) (string, *bool) {
	emails, _ := u["emails"].([]any)
	pick := -1
	for i, e := range emails {
		if m, _ := e.(map[string]any); m != nil {
			if p, _ := m["primary"].(bool); p {
				pick = i
				break
			}
		}
	}
	if pick < 0 && len(emails) > 0 {
		pick = 0
	}
	var email string
	var verified *bool
	if pick >= 0 {
		m, _ := emails[pick].(map[string]any)
		email, _ = m["value"].(string)
		if b, ok := m["verified"].(bool); ok {
			verified = &b
		}
	}
	if verified == nil {
		exts := make([]string, 0, len(u))
		for k := range u {
			if strings.HasPrefix(k, "urn:") {
				exts = append(exts, k)
			}
		}
		slices.Sort(exts) // the same answer every time
		for _, k := range exts {
			if m, ok := u[k].(map[string]any); ok {
				if b, ok := m["emailVerified"].(bool); ok {
					verified = &b
					break
				}
			}
		}
	}
	return email, verified
}

func (d *scim) Remove(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/Users/"+url.PathEscape(id), nil, nil)
}

func (d *scim) Groups(ctx context.Context, id string) ([]string, error) {
	rec, err := d.User(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range Values(Lookup(rec, "groups.display")) {
		out = append(out, v)
	}
	slices.Sort(out)
	return out, nil
}

// inDomains is whether the email is under one of the domains.
func inDomains(email string, domains []string) bool {
	_, d, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	return ok && slices.ContainsFunc(domains, func(x string) bool { return strings.EqualFold(strings.TrimPrefix(x, "@"), d) })
}
