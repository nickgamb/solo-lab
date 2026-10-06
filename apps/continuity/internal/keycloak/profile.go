package keycloak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

// OwnerAnnotation marks the user profile attributes this controller owns.
const OwnerAnnotation = "continuity.lab.solo.io/instance"

// Attr is one attribute of the unified profile.
type Attr struct {
	Name, DisplayName string
	Multivalued       bool
}

// Builtin are the realm's own profile attributes: never added or removed here.
var Builtin = []string{"username", "email", "firstName", "lastName"}

// NeverSynced: the broker's own name for the user, set when the account is
// made and never written by the sync.
var NeverSynced = []string{"username"}

// EnsureProfile makes the realm's user profile hold attrs (beside the built-in
// ones), owned by owner: added, updated, or removed when owner no longer lists
// them. Users can view them; only admins (and the sync) edit them.
func (c *Client) EnsureProfile(ctx context.Context, owner string, attrs []Attr) (bool, error) {
	var up map[string]any
	if err := c.do(ctx, http.MethodGet, "/users/profile", nil, &up); err != nil {
		return false, fmt.Errorf("user profile: %w", err)
	}
	have, _ := up["attributes"].([]any)
	want := map[string]Attr{}
	for _, a := range attrs {
		if !slices.Contains(Builtin, a.Name) {
			want[a.Name] = a
		}
	}
	changed := false
	var out []any
	for _, x := range have {
		m, _ := x.(map[string]any)
		name, _ := m["name"].(string)
		ann, _ := m["annotations"].(map[string]any)
		mine := ann != nil && ann[OwnerAnnotation] == owner
		a, wanted := want[name]
		switch {
		case wanted:
			n := profileAttr(owner, a, m)
			if fmt.Sprint(n) != fmt.Sprint(m) {
				changed = true
			}
			out = append(out, n)
			delete(want, name)
		case mine:
			changed = true // no longer in the spec: drop it (the values stay on users)
		default:
			out = append(out, m)
		}
	}
	for _, a := range attrs {
		if _, ok := want[a.Name]; ok {
			out = append(out, profileAttr(owner, a, map[string]any{}))
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	up["attributes"] = out
	return true, c.do(ctx, http.MethodPut, "/users/profile", up, nil)
}

func profileAttr(owner string, a Attr, base map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range base {
		m[k] = v
	}
	m["name"] = a.Name
	if a.DisplayName != "" {
		m["displayName"] = a.DisplayName
	}
	m["multivalued"] = a.Multivalued
	m["permissions"] = map[string]any{"view": []any{"admin", "user"}, "edit": []any{"admin"}}
	ann, _ := m["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
	}
	ann[OwnerAnnotation] = owner
	m["annotations"] = ann
	return m
}

// User is a user representation, kept whole so an update never drops fields.
type User map[string]any

// Users lists the realm's users, full representations, a page at a time.
func (c *Client) Users(ctx context.Context, first, max int) ([]User, error) {
	var out []User
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("/users?briefRepresentation=false&first=%d&max=%d", first, max), nil, &out)
}

// FederatedIdentity is the user's subject at an upstream.
func (c *Client) FederatedIdentity(ctx context.Context, userID, alias string) (string, error) {
	var links []struct {
		IdentityProvider string `json:"identityProvider"`
		UserID           string `json:"userId"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/federated-identity", nil, &links); err != nil {
		return "", err
	}
	for _, l := range links {
		if l.IdentityProvider == alias {
			return l.UserID, nil
		}
	}
	return "", ErrNotFound
}

// UpdateUser writes the user's whole representation back (Keycloak drops
// attributes left out of an update).
func (c *Client) UpdateUser(ctx context.Context, u User) error {
	id, _ := u["id"].(string)
	if id == "" {
		return errors.New("user without id")
	}
	return c.do(ctx, http.MethodPut, "/users/"+url.PathEscape(id), u, nil)
}

// HasRealmRole reports whether the user holds a realm role, directly or
// through a group (effective roles).
func (c *Client) HasRealmRole(ctx context.Context, userID, role string) (bool, error) {
	var rs []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/role-mappings/realm/composite", nil, &rs); err != nil {
		return false, err
	}
	for _, r := range rs {
		if r.Name == role {
			return true, nil
		}
	}
	return false, nil
}
