package keycloak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// OwnerAnnotation marks what this controller owns in the realm's user profile,
// on IdP mappers and on the profile client scope.
const OwnerAnnotation = "continuity.lab.solo.io/instance"

// Attr is one attribute of the unified profile.
type Attr struct {
	Name, DisplayName string
	Multivalued       bool
}

// Builtin are the realm's own profile attributes: never added or removed here.
var Builtin = []string{"username", "email", "firstName", "lastName"}

// IdentityKeys identify a user and link their upstream accounts: an upstream
// sets them once, at first sign-in, and nothing overwrites them later.
var IdentityKeys = []string{"username", "email"}

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

// ClaimMapper maps an upstream claim to a profile attribute at sign-in.
type ClaimMapper struct{ Claim, Attribute string }

const claimMapperPrefix = "claim-to-"

// SyncClaimMappers makes alias's IdP mappers carry exactly ms (the ones this
// controller owns, named claim-to-<attribute>). With follow, an attribute
// follows the upstream on every sign-in (FORCE: the chain's first tier);
// otherwise it is filled at first sign-in only (IMPORT), so a later tier
// never overwrites the first tier's value. An identity key is always IMPORT.
// username is never mapped: it is the verified email.
func (c *Client) SyncClaimMappers(ctx context.Context, alias string, ms []ClaimMapper, follow bool) (bool, error) {
	base := "/identity-provider/instances/" + url.PathEscape(alias) + "/mappers"
	var have []map[string]any
	if err := c.do(ctx, http.MethodGet, base, nil, &have); err != nil {
		return false, err
	}
	want := map[string]map[string]any{}
	for _, m := range ms {
		if m.Attribute == "username" || m.Claim == "" {
			continue
		}
		mode := "FORCE"
		if !follow || slices.Contains(IdentityKeys, m.Attribute) {
			mode = "IMPORT"
		}
		name := claimMapperPrefix + m.Attribute
		want[name] = map[string]any{
			"name": name, "identityProviderAlias": alias, "identityProviderMapper": "oidc-user-attribute-idp-mapper",
			"config": map[string]any{"claim": m.Claim, "user.attribute": m.Attribute, "syncMode": mode},
		}
	}
	changed := false
	for _, h := range have {
		name, _ := h["name"].(string)
		if !strings.HasPrefix(name, claimMapperPrefix) {
			continue
		}
		id, _ := h["id"].(string)
		w, ok := want[name]
		switch {
		case !ok:
			if err := c.do(ctx, http.MethodDelete, base+"/"+url.PathEscape(id), nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				return changed, err
			}
			changed = true
		case fmt.Sprint(h["config"]) != fmt.Sprint(w["config"]):
			w["id"] = id
			if err := c.do(ctx, http.MethodPut, base+"/"+url.PathEscape(id), w, nil); err != nil {
				return changed, err
			}
			changed = true
		}
		delete(want, name)
	}
	for _, w := range want {
		if err := c.do(ctx, http.MethodPost, base, w, nil); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// EnsureProfileScope keeps client scope name carrying attrs as token claims
// (ID token, access token, userinfo), and makes it a default scope of exactly
// clients among those it was given to before. Built-in attributes are carried
// by the realm's own profile and email scopes already.
func (c *Client) EnsureProfileScope(ctx context.Context, owner, name string, attrs []Attr, clients []string) error {
	var scopes []map[string]any
	if err := c.do(ctx, http.MethodGet, "/client-scopes", nil, &scopes); err != nil {
		return err
	}
	var scope map[string]any
	for _, s := range scopes {
		if s["name"] == name {
			scope = s
		}
	}
	mappers := []any{}
	for _, a := range attrs {
		if slices.Contains(Builtin, a.Name) {
			continue
		}
		mappers = append(mappers, map[string]any{
			"name": a.Name, "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-attribute-mapper",
			"config": map[string]any{"user.attribute": a.Name, "claim.name": a.Name, "jsonType.label": "String",
				"multivalued": fmt.Sprint(a.Multivalued), "id.token.claim": "true", "access.token.claim": "true",
				"userinfo.token.claim": "true", "introspection.token.claim": "true"},
		})
	}
	prev := []string{}
	if scope != nil && !ownedScope(scope, owner) {
		return fmt.Errorf("client scope %q exists and is not managed by %s", name, owner)
	}
	if scope == nil {
		scope = map[string]any{"name": name, "protocol": "openid-connect", "description": "The unified profile (" + owner + ")",
			"attributes": map[string]any{"include.in.token.scope": "false", "display.on.consent.screen": "false", OwnerAnnotation: owner}}
		if err := c.do(ctx, http.MethodPost, "/client-scopes", scope, nil); err != nil {
			return err
		}
		if err := c.do(ctx, http.MethodGet, "/client-scopes", nil, &scopes); err != nil {
			return err
		}
		for _, s := range scopes {
			if s["name"] == name {
				scope = s
			}
		}
	} else if attrsOf, ok := scope["attributes"].(map[string]any); ok {
		if v, _ := attrsOf[OwnerAnnotation+"/clients"].(string); v != "" {
			prev = strings.Split(v, ",")
		}
	}
	id, _ := scope["id"].(string)
	if id == "" {
		return fmt.Errorf("client scope %s: no id", name)
	}
	// mappers: replace the scope's set with the wanted one
	var have []map[string]any
	if err := c.do(ctx, http.MethodGet, "/client-scopes/"+id+"/protocol-mappers/models", nil, &have); err != nil {
		return err
	}
	byName := map[string]map[string]any{}
	for _, h := range have {
		n, _ := h["name"].(string)
		byName[n] = h
	}
	for _, x := range mappers {
		m := x.(map[string]any)
		n := m["name"].(string)
		h, ok := byName[n]
		delete(byName, n)
		if ok && fmt.Sprint(h["config"]) == fmt.Sprint(m["config"]) {
			continue
		}
		if ok {
			m["id"] = h["id"]
			if err := c.do(ctx, http.MethodPut, "/client-scopes/"+id+"/protocol-mappers/models/"+fmt.Sprint(h["id"]), m, nil); err != nil {
				return err
			}
		} else if err := c.do(ctx, http.MethodPost, "/client-scopes/"+id+"/protocol-mappers/models", m, nil); err != nil {
			return err
		}
	}
	for _, h := range byName {
		if err := c.do(ctx, http.MethodDelete, "/client-scopes/"+id+"/protocol-mappers/models/"+fmt.Sprint(h["id"]), nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	// default scope of exactly the listed clients
	for _, cid := range clients {
		uuid, err := c.clientUUID(ctx, cid)
		if err != nil {
			return err
		}
		var defaults []map[string]any
		if err := c.do(ctx, http.MethodGet, "/clients/"+uuid+"/default-client-scopes", nil, &defaults); err != nil {
			return err
		}
		if slices.ContainsFunc(defaults, func(d map[string]any) bool { return d["id"] == id }) {
			continue
		}
		if err := c.do(ctx, http.MethodPut, "/clients/"+uuid+"/default-client-scopes/"+id, nil, nil); err != nil {
			return err
		}
	}
	for _, cid := range prev {
		if slices.Contains(clients, cid) {
			continue
		}
		if uuid, err := c.clientUUID(ctx, cid); err == nil {
			if err := c.do(ctx, http.MethodDelete, "/clients/"+uuid+"/default-client-scopes/"+id, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	attrs2, _ := scope["attributes"].(map[string]any)
	if attrs2 == nil {
		attrs2 = map[string]any{}
	}
	if attrs2[OwnerAnnotation+"/clients"] != strings.Join(clients, ",") {
		attrs2[OwnerAnnotation+"/clients"] = strings.Join(clients, ",")
		scope["attributes"] = attrs2
		return c.do(ctx, http.MethodPut, "/client-scopes/"+id, scope, nil)
	}
	return nil
}

// DeleteProfileScope removes client scope name if owner manages it (Keycloak
// drops it from every client with it).
func (c *Client) DeleteProfileScope(ctx context.Context, owner, name string) error {
	var scopes []map[string]any
	if err := c.do(ctx, http.MethodGet, "/client-scopes", nil, &scopes); err != nil {
		return err
	}
	for _, s := range scopes {
		if s["name"] == name && ownedScope(s, owner) {
			err := c.do(ctx, http.MethodDelete, "/client-scopes/"+fmt.Sprint(s["id"]), nil, nil)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

func ownedScope(s map[string]any, owner string) bool {
	a, _ := s["attributes"].(map[string]any)
	return a != nil && a[OwnerAnnotation] == owner
}

func (c *Client) clientUUID(ctx context.Context, clientID string) (string, error) {
	var cs []map[string]any
	if err := c.do(ctx, http.MethodGet, "/clients?clientId="+url.QueryEscape(clientID), nil, &cs); err != nil {
		return "", err
	}
	if len(cs) == 0 {
		return "", fmt.Errorf("client %s: %w", clientID, ErrNotFound)
	}
	id, _ := cs[0]["id"].(string)
	return id, nil
}

// User is a user representation, kept whole so an update never drops fields.
type User map[string]any

// LinkedUsers lists the realm's users linked to IdP alias, full
// representations, a page at a time.
func (c *Client) LinkedUsers(ctx context.Context, alias string, first, max int) ([]User, error) {
	var out []User
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("/users?idpAlias=%s&briefRepresentation=false&first=%d&max=%d",
		url.QueryEscape(alias), first, max), nil, &out)
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
