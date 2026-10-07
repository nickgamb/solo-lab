package profilesync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
)

// Broker is the slice of the broker's admin API the sync uses.
type Broker interface {
	Users(ctx context.Context, first, max int) ([]keycloak.User, error)
	FederatedIdentity(ctx context.Context, userID, alias string) (string, error)
	HasRealmRole(ctx context.Context, userID, role string) (bool, error)
	UpdateUser(ctx context.Context, u keycloak.User) error
}

// IdP is one IdP in the chain with a directory, and its attribute mapping.
type IdP struct {
	Name       string
	Attributes []v1.AttributeMapping
	Dir        Directory
}

// Result counts one run. Errors and notes name users by id and IdPs and
// attributes by name, never values.
type Result struct {
	Users, Updated, Written, Created, Failed int
	Errors                                   []string
	// Notes: users created without a credential enrollment sent; they can't
	// sign in at that IdP until they get one.
	Notes []string
}

const (
	pageSize = 100
	// LocalOnlyRole marks accounts never linked to, or synced with, an IdP.
	LocalOnlyRole = "local-only"
)

// Run syncs every employee the broker has: the primary's record into the
// broker's profile (nil primary: none to read), then the broker's profile out
// to each failover, creating the user there if the primary has them and the
// failover doesn't. writable are the
// broker attributes a mapping may carry; username never is.
func Run(ctx context.Context, b Broker, primary *IdP, failovers []IdP, writable, lists []string, logf func(string, ...any)) Result {
	var res Result
	for first := 0; ; first += pageSize {
		page, err := b.Users(ctx, first, pageSize)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("listing the broker's users: %v", err))
			return res
		}
		for _, u := range page {
			syncUser(ctx, b, u, primary, failovers, writable, lists, logf, &res)
		}
		if len(page) < pageSize {
			return res
		}
	}
}

func syncUser(ctx context.Context, b Broker, u keycloak.User, primary *IdP, failovers []IdP, writable, lists []string, logf func(string, ...any), res *Result) {
	// one value, unless the profile attribute holds a list
	fit := func(attr string, v []string) []string {
		if slices.Contains(lists, attr) {
			return v
		}
		return v[:1]
	}
	id, _ := u["id"].(string)
	if id == "" || u["serviceAccountClientId"] != nil {
		return
	}
	fail := func(format string, a ...any) {
		res.Errors = append(res.Errors, fmt.Sprintf("user %s: ", id)+fmt.Sprintf(format, a...))
	}
	before := len(res.Errors)
	defer func() {
		if len(res.Errors) > before {
			res.Failed++
		}
	}()
	if local, err := b.HasRealmRole(ctx, id, LocalOnlyRole); err != nil || local {
		if err != nil {
			fail("roles: %v", err)
		}
		return
	}
	res.Users++

	// 1. the primary, into the broker
	inPrimary := false
	if primary != nil {
		if pid, err := locate(ctx, b, id, email(u), *primary); err == nil {
			rec, err := primary.Dir.User(ctx, pid)
			inPrimary = err == nil
			if err != nil {
				fail("%s: %v", primary.Name, err)
			} else if values := read(rec, primary.Attributes, writable, fit); apply(u, values) {
				if err := b.UpdateUser(ctx, u); err != nil {
					fail("broker update: %v", err)
				} else {
					res.Updated++
					logf("broker profile updated", "user", id, "attributes", sortedKeys(values))
				}
			}
		} else if !errors.Is(err, ErrNoUser) {
			fail("%s: %v", primary.Name, err)
		}
	}

	// 2. the broker, out to each failover
	mail := email(u)
	for _, f := range failovers {
		want := map[string][]string{} // path -> values, from the broker's profile
		for _, m := range f.Attributes {
			if slices.Contains(writable, m.Attribute) && m.Attribute != "username" {
				if v := brokerValue(u, m.Attribute); len(v) > 0 {
					want[m.Path] = fit(m.Attribute, v)
				}
			}
		}
		fid, err := locate(ctx, b, id, mail, f)
		switch {
		case errors.Is(err, ErrNoUser) && mail != "" && inPrimary: // only users the primary has
			fid, err = f.Dir.Create(ctx, mail, want)
			if errors.Is(err, ErrNoCreate) {
				fail("%s: no account there, and it can't create one without a password", f.Name)
				continue
			}
			if err != nil {
				fail("%s: create: %v", f.Name, err)
				continue
			}
			res.Created++
			logf("created at failover", "user", id, "idp", f.Name)
			verify(ctx, f, fid, want, fail)
			if err := f.Dir.Enroll(ctx, fid); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("user %s: created at %s, no credential enrollment sent: %v", id, f.Name, err))
			}
			continue
		case errors.Is(err, ErrNoUser):
			continue // not there, and not created: not in the primary, or no email
		case err != nil:
			fail("%s: %v", f.Name, err)
			continue
		}
		rec, err := f.Dir.User(ctx, fid)
		if err != nil {
			fail("%s: %v", f.Name, err)
			continue
		}
		set := map[string][]string{}
		for p, v := range want {
			if !slices.Equal(Values(Lookup(rec, p)), v) {
				set[p] = v
			}
		}
		if len(set) == 0 {
			continue
		}
		if err := f.Dir.Update(ctx, fid, set); err != nil {
			fail("%s: update: %v", f.Name, err)
			continue
		}
		res.Written++
		logf("failover written", "user", id, "idp", f.Name, "attributes", sortedKeys(set))
		verify(ctx, f, fid, set, fail)
	}
}

// locate is the user's id at an IdP: the broker's link to it, else the one
// user there with the same email.
func locate(ctx context.Context, b Broker, userID, mail string, p IdP) (string, error) {
	sub, err := b.FederatedIdentity(ctx, userID, p.Name)
	if err == nil {
		return sub, nil
	}
	if !errors.Is(err, keycloak.ErrNotFound) {
		return "", err
	}
	if mail == "" {
		return "", ErrNoUser
	}
	return p.Dir.Find(ctx, mail)
}

// verify reads the record back: a directory that drops an attribute it
// doesn't know (a Keycloak realm without it in its user profile) is reported,
// not assumed written.
func verify(ctx context.Context, f IdP, id string, set map[string][]string, fail func(string, ...any)) {
	rec, err := f.Dir.User(ctx, id)
	if err != nil {
		fail("%s: reading back: %v", f.Name, err)
		return
	}
	var lost []string
	for _, p := range sortedKeys(set) {
		if !slices.Equal(Values(Lookup(rec, p)), set[p]) {
			lost = append(lost, p)
		}
	}
	if len(lost) > 0 {
		fail("%s: didn't keep %s (not in its user schema?)", f.Name, strings.Join(lost, ", "))
	}
}

// read maps the primary's record onto broker attributes.
func read(rec map[string]any, ms []v1.AttributeMapping, writable []string, fit func(string, []string) []string) map[string][]string {
	values := map[string][]string{}
	for _, m := range ms {
		if !slices.Contains(writable, m.Attribute) || m.Attribute == "username" {
			continue
		}
		if v := Values(Lookup(rec, m.Path)); len(v) > 0 {
			values[m.Attribute] = fit(m.Attribute, v)
		}
	}
	return values
}

// apply writes values into the broker user's representation; email,
// firstName and lastName are top-level fields, everything else an attribute.
// Reports whether anything changed.
func apply(u keycloak.User, values map[string][]string) bool {
	changed := false
	attrs, _ := u["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	for a, v := range values {
		switch a {
		case "email", "firstName", "lastName":
			if cur, _ := u[a].(string); cur != v[0] {
				u[a], changed = v[0], true
				if a == "email" {
					u["emailVerified"] = true // the primary verified it
				}
			}
		default:
			if !slices.Equal(Values(attrs[a]), v) {
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

func brokerValue(u keycloak.User, attr string) []string {
	switch attr {
	case "email", "firstName", "lastName", "username":
		return Values(u[attr])
	}
	attrs, _ := u["attributes"].(map[string]any)
	return Values(attrs[attr])
}

func email(u keycloak.User) string { s, _ := u["email"].(string); return s }
