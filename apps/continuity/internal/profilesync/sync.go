package profilesync

import (
	"context"
	"errors"
	"fmt"
	"slices"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
)

// Broker is the slice of the broker's admin API the sync uses.
type Broker interface {
	LinkedUsers(ctx context.Context, alias string, first, max int) ([]keycloak.User, error)
	FederatedIdentity(ctx context.Context, userID, alias string) (string, error)
	HasRealmRole(ctx context.Context, userID, role string) (bool, error)
	UpdateUser(ctx context.Context, u keycloak.User) error
}

// Tier is one upstream in chain order, with its directory and claim mappings.
type Tier struct {
	Name   string
	Claims []v1.ClaimMapping
	Dir    Directory
}

// Result counts one run. Errors hold attribute and tier names, never values.
type Result struct {
	Users, Updated, Failed int
	Errors                 []string
}

const (
	pageSize = 100
	// LocalOnlyRole marks accounts never linked to, or filled from, an upstream.
	LocalOnlyRole = "local-only"
)

// Run syncs every user linked to a tier with a directory. writable are the
// attributes the sync may write (the profile's and the built-in names);
// identity keys never are.
func Run(ctx context.Context, b Broker, tiers []Tier, writable []string, logf func(string, ...any)) Result {
	var res Result
	order := []string{}
	users := map[string]keycloak.User{}
	for _, t := range tiers {
		if t.Dir == nil {
			continue
		}
		for first := 0; ; first += pageSize {
			page, err := b.LinkedUsers(ctx, t.Name, first, pageSize)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("listing users linked to %s: %v", t.Name, err))
				break
			}
			for _, u := range page {
				id, _ := u["id"].(string)
				if _, seen := users[id]; !seen && id != "" {
					users[id], order = u, append(order, id)
				}
			}
			if len(page) < pageSize {
				break
			}
		}
	}
	for _, id := range order {
		u := users[id]
		if local, err := b.HasRealmRole(ctx, id, LocalOnlyRole); err != nil || local {
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("user %s: roles: %v", id, err))
			}
			continue
		}
		res.Users++
		values, failed := merge(ctx, b, id, tiers, writable, &res)
		if failed {
			res.Failed++
		}
		if changed := apply(u, values); changed {
			if err := b.UpdateUser(ctx, u); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("user %s: update: %v", id, err))
				continue
			}
			res.Updated++
			logf("updated", "user", id, "attributes", keys(values))
		}
	}
	return res
}

// merge reads the user's record from each tier in chain order: the first tier
// with a value for an attribute wins. A tier that can't be read decides its
// attributes as "keep": a lower tier never overwrites the primary's value
// because the primary was briefly unreachable.
func merge(ctx context.Context, b Broker, userID string, tiers []Tier, writable []string, res *Result) (map[string][]string, bool) {
	values := map[string][]string{}
	decided := map[string]bool{}
	failed := false
	for _, t := range tiers {
		if t.Dir == nil {
			continue
		}
		sub, err := b.FederatedIdentity(ctx, userID, t.Name)
		if errors.Is(err, keycloak.ErrNotFound) {
			continue // no account there
		}
		var rec map[string]any
		if err == nil {
			rec, err = t.Dir.User(ctx, sub)
		}
		if errors.Is(err, ErrNoUser) {
			continue
		}
		if err != nil {
			failed = true
			res.Errors = append(res.Errors, fmt.Sprintf("user %s: %s: %v", userID, t.Name, err))
			for _, m := range t.Claims {
				decided[m.Attribute] = true
			}
			continue
		}
		for _, m := range t.Claims {
			a := m.Attribute
			if decided[a] || !slices.Contains(writable, a) || slices.Contains(keycloak.IdentityKeys, a) {
				continue
			}
			path := m.DirectoryPath
			if path == "" {
				path = m.Claim
			}
			if v := Values(Lookup(rec, path)); len(v) > 0 {
				values[a], decided[a] = v, true
			}
		}
	}
	return values, failed
}

// apply writes values into the user's representation; firstName and
// lastName are top-level fields, everything else an attribute. Reports
// whether anything changed.
func apply(u keycloak.User, values map[string][]string) bool {
	changed := false
	attrs, _ := u["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	for a, v := range values {
		switch a {
		case "firstName", "lastName":
			if cur, _ := u[a].(string); cur != v[0] {
				u[a], changed = v[0], true
			}
		default:
			if !sameValues(attrs[a], v) {
				l := make([]any, len(v))
				for i, s := range v {
					l[i] = s
				}
				attrs[a], changed = l, true
			}
		}
	}
	if changed {
		u["attributes"] = attrs
	}
	return changed
}

func sameValues(cur any, v []string) bool {
	l, _ := cur.([]any)
	if len(l) != len(v) {
		return false
	}
	for i := range l {
		if fmt.Sprint(l[i]) != v[i] {
			return false
		}
	}
	return true
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
