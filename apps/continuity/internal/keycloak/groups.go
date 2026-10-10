package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// GroupMapperPrefix names the identity provider mappers that map an IdP's
// groups claim into the shape's groups: "<prefix><group>".
const GroupMapperPrefix = "group "

// GroupMapper sets the broker user's membership of one shape group from the
// IdP's groups claim at every sign-in (FORCE): in the group when the claim
// lists it, out of it when it doesn't. Keycloak's own Advanced Claim to Group
// mapper; a claim name's dots are escaped, so a namespaced claim
// (https://example.com/groups) is read whole rather than as a nested path.
func GroupMapper(claim, group string) Mapper {
	claims, _ := json.Marshal([]map[string]string{{"key": strings.ReplaceAll(claim, ".", `\.`), "value": group}})
	return Mapper{Name: GroupMapperPrefix + group, Type: "oidc-advanced-group-idp-mapper", Config: map[string]string{
		"claims": string(claims), "are.claim.values.regex": "false", "group": "/" + group, "syncMode": "FORCE",
	}}
}

// PruneMappers removes the IdP's mappers whose name starts with prefix and
// isn't in keep.
func (c *Client) PruneMappers(ctx context.Context, alias, prefix string, keep map[string]bool) error {
	var have []map[string]any
	base := "/identity-provider/instances/" + url.PathEscape(alias) + "/mappers"
	if err := c.do(ctx, http.MethodGet, base, nil, &have); err != nil {
		return err
	}
	var errs []error
	for _, m := range have {
		name, _ := m["name"].(string)
		id, _ := m["id"].(string)
		if strings.HasPrefix(name, prefix) && !keep[name] && id != "" {
			if err := c.do(ctx, http.MethodDelete, base+"/"+url.PathEscape(id), nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// GroupID is the id of the top-level group with that name, or ErrNotFound.
func (c *Client) GroupID(ctx context.Context, name string) (string, error) {
	var g struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/group-by-path/"+url.PathEscape(name), nil, &g); err != nil {
		return "", err
	}
	if g.ID == "" {
		return "", ErrNotFound
	}
	return g.ID, nil
}

// EnsureGroup creates the top-level group if the realm doesn't have it, and
// returns its id.
func (c *Client) EnsureGroup(ctx context.Context, name string) (string, error) {
	id, err := c.GroupID(ctx, name)
	if !errors.Is(err, ErrNotFound) {
		return id, err
	}
	// another run may create it meanwhile: a conflict means it exists now
	if err := c.do(ctx, http.MethodPost, "/groups", map[string]any{"name": name}, nil); err != nil && !strings.Contains(err.Error(), "HTTP 409") {
		return "", err
	}
	return c.GroupID(ctx, name)
}

// UserGroups are the names of the top-level groups the user is in.
func (c *Client) UserGroups(ctx context.Context, userID string) ([]string, error) {
	var gs []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/groups?briefRepresentation=true&max=1000", nil, &gs); err != nil {
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

// SetUserGroups puts the user in exactly want, among managed: groups outside
// managed are left as they are. ids maps each managed group's name to its id.
// Reports whether anything changed.
func (c *Client) SetUserGroups(ctx context.Context, userID string, want []string, managed map[string]string) (bool, error) {
	have, err := c.UserGroups(ctx, userID)
	if err != nil {
		return false, err
	}
	changed := false
	for name, gid := range managed {
		in, wanted := slices.Contains(have, name), slices.Contains(want, name)
		path := "/users/" + url.PathEscape(userID) + "/groups/" + url.PathEscape(gid)
		switch {
		case wanted && !in:
			err = c.do(ctx, http.MethodPut, path, nil, nil)
		case !wanted && in:
			err = c.do(ctx, http.MethodDelete, path, nil, nil)
		default:
			continue
		}
		if err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// FindUserByEmail is the id of the realm's user with that email (exact), or
// ErrNotFound.
func (c *Client) FindUserByEmail(ctx context.Context, email string) (string, error) {
	var us []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := c.do(ctx, http.MethodGet, "/users?exact=true&briefRepresentation=true&email="+url.QueryEscape(email), nil, &us); err != nil {
		return "", err
	}
	for _, u := range us {
		if strings.EqualFold(u.Email, email) {
			return u.ID, nil
		}
	}
	return "", ErrNotFound
}

// DeleteUser removes the realm's user (its links and sessions go with it).
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/users/"+url.PathEscape(id), nil, nil)
}

// CreateUser creates an account named by its email, with no credential: the
// user signs in through an IdP, which links to it by that verified email.
func (c *Client) CreateUser(ctx context.Context, email string, verified bool) (string, error) {
	err := c.do(ctx, http.MethodPost, "/users", map[string]any{
		"username": strings.ToLower(email), "email": email, "emailVerified": verified, "enabled": true,
	}, nil)
	if err != nil {
		return "", err
	}
	return c.FindUserByEmail(ctx, email)
}
