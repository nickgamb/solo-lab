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
}

// Lister lists a directory's users, a page at a time: the directory sync
// gives the broker an account for each of the primary's workforce users.
type Lister interface {
	List(ctx context.Context, first, max int) ([]Entry, error)
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
	}
	q := fmt.Sprintf("/users?per_page=%d&page=%d&fields=user_id,email,email_verified&include_fields=true", max, first/max)
	if err := d.call(ctx, http.MethodGet, q, nil, &us); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(us))
	for _, u := range us {
		out = append(out, Entry{ID: u.ID, Email: u.Email, Verified: u.Verified})
	}
	return out, nil
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
	}
	if err := d.call(ctx, http.MethodGet, fmt.Sprintf("/users?briefRepresentation=true&first=%d&max=%d", first, max), nil, &us); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(us))
	for _, u := range us {
		out = append(out, Entry{ID: u.ID, Email: u.Email, Verified: u.Verified})
	}
	return out, nil
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

// ---- SCIM: the record's groups (read-only here; membership is written on
// the Group resources, which the sync doesn't manage).

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
