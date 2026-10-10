package profilesync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
)

// Broker is the slice of the broker's admin API the sync uses.
type Broker interface {
	Users(ctx context.Context, first, max int) ([]keycloak.User, error)
	FederatedIdentity(ctx context.Context, userID, alias string) (string, error)
	HasRealmRole(ctx context.Context, userID, role string) (bool, error)
	// UpdateUser reads the user afresh, has apply change it, and writes it
	// back if apply reports a change.
	UpdateUser(ctx context.Context, id string, apply func(keycloak.User) bool) (bool, error)
	FindUserByEmail(ctx context.Context, email string) (string, error)
	CreateUser(ctx context.Context, email string, verified bool) (string, error)
	DeleteUser(ctx context.Context, id string) error
	EnsureGroup(ctx context.Context, name string) (string, error)
	UserGroups(ctx context.Context, userID string) ([]string, error)
	SetUserGroups(ctx context.Context, userID string, want []string, managed map[string]string) (bool, error)
}

// Shape is what the sync maps beyond attributes: the shape's groups
// (spec.profile.groups) and the workforce's email domains, which the broker
// gives an account to (spec.profile.domains); and whether an account the
// primary no longer has goes (spec.sync.removeMissing).
type Shape struct {
	Groups, Domains []string
	RemoveMissing   bool
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
	// Provisioned: broker accounts created for the primary's workforce users.
	// Removed: broker accounts removed, the primary no longer having the user.
	Users, Updated, Written, Created, Failed, Provisioned, Removed int
	Errors                                                         []string
	// Notes: users with no way to sign in at a failover yet: created there
	// without a credential enrollment sent, or not there and the directory
	// can't create them without a password.
	Notes []string
}

const (
	pageSize = 100
	// maxPages bounds one run's listing of the broker's users.
	maxPages = 1000
	// LocalOnlyRole marks accounts never linked to, or synced with, an IdP.
	LocalOnlyRole = "local-only"
)

// Run syncs every employee the broker has: the primary's record into the
// broker's profile (nil primary: none to read), then the broker's profile out
// to each failover, creating the user there if the primary has them and the
// failover doesn't. writable are the broker attributes a mapping may carry;
// keycloak.NeverSynced never are.
func Run(ctx context.Context, b Broker, primary *IdP, failovers []IdP, writable, lists []string, shape Shape, logf func(string, ...any)) Result {
	var res Result
	// the shape's groups at the broker, by name -> id
	groups := map[string]string{}
	for _, g := range shape.Groups {
		id, err := b.EnsureGroup(ctx, g)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("group %s at the broker: %v", g, err))
			return res
		}
		groups[g] = id
	}
	if primary != nil {
		present, complete := provision(ctx, b, *primary, shape.Domains, logf, &res)
		// who the primary no longer has goes, but only on a complete answer
		// from it: never while it can't be read, never on an empty listing
		if shape.RemoveMissing && complete && len(present) > 0 {
			removeMissing(ctx, b, present, failovers, shape.Domains, logf, &res)
		}
	}
	seen := map[string]bool{} // a user moved between pages is synced once
	for n, first := 0, 0; ; n, first = n+1, first+pageSize {
		if n == maxPages {
			res.Errors = append(res.Errors, fmt.Sprintf("listing the broker's users: stopped after %d pages", maxPages))
			return res
		}
		page, err := b.Users(ctx, first, pageSize)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("listing the broker's users: %v", err))
			return res
		}
		for _, u := range page {
			if id, _ := u["id"].(string); id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			syncUser(ctx, b, u, primary, failovers, writable, lists, groups, logf, &res)
		}
		if len(page) < pageSize {
			return res
		}
	}
}

// provision gives the broker an account for each of the primary's users
// whose verified email is in the workforce's domains, so that who exists is
// the primary's to say. The account has no credential: its owner signs in
// through an IdP, which the broker links to it by that email.
// It answers with the workforce emails the primary has, enabled, and whether
// that listing is complete.
func provision(ctx context.Context, b Broker, primary IdP, domains []string, logf func(string, ...any), res *Result) (map[string]bool, bool) {
	l, ok := primary.Dir.(Lister)
	if !ok || len(domains) == 0 {
		return nil, false
	}
	present := map[string]bool{}
	for n, first := 0, 0; n < maxPages; n, first = n+1, first+pageSize {
		page, err := l.List(ctx, first, pageSize)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("listing %s's users: %v", primary.Name, err))
			return present, false
		}
		for _, e := range page {
			if !e.Disabled && inDomains(e.Email, domains) {
				present[normalEmail(e.Email)] = true
			}
			if e.Disabled || e.Verified == nil || !*e.Verified || !inDomains(e.Email, domains) {
				continue
			}
			mail := normalEmail(e.Email)
			_, err := b.FindUserByEmail(ctx, mail)
			if err == nil {
				continue
			}
			if !errors.Is(err, keycloak.ErrNotFound) {
				res.Errors = append(res.Errors, fmt.Sprintf("%s user %s: broker lookup: %v", primary.Name, e.ID, err))
				continue
			}
			id, err := b.CreateUser(ctx, mail, true)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s user %s: broker account: %v", primary.Name, e.ID, err))
				continue
			}
			res.Provisioned++
			logf("broker account provisioned", "user", id, "from", primary.Name)
		}
		if len(page) < pageSize {
			return present, true
		}
	}
	return present, false // stopped at maxPages: not the whole directory
}

// removeMissing removes each workforce account at the broker whose user the
// primary no longer has (or has disabled), with their accounts at the
// failovers whose directories can remove them. Accounts never linked to an
// IdP (local-only, break-glass) and service accounts stay.
func removeMissing(ctx context.Context, b Broker, present map[string]bool, failovers []IdP, domains []string, logf func(string, ...any), res *Result) {
	var gone []keycloak.User
	for n, first := 0, 0; ; n, first = n+1, first+pageSize {
		if n == maxPages {
			res.Errors = append(res.Errors, fmt.Sprintf("listing the broker's users: stopped after %d pages", maxPages))
			return
		}
		page, err := b.Users(ctx, first, pageSize)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("listing the broker's users: %v", err))
			return
		}
		for _, u := range page {
			mail := email(u)
			if u["serviceAccountClientId"] != nil || mail == "" || !inDomains(mail, domains) || present[normalEmail(mail)] {
				continue
			}
			gone = append(gone, u)
		}
		if len(page) < pageSize {
			break
		}
	}
	for _, u := range gone {
		id, _ := u["id"].(string)
		if local, err := b.HasRealmRole(ctx, id, LocalOnlyRole); err != nil || local {
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("user %s: roles: %v", id, err))
			}
			continue
		}
		for _, f := range failovers {
			r, ok := f.Dir.(Remover)
			if !ok {
				continue
			}
			fid, err := locate(ctx, b, id, email(u), f)
			if errors.Is(err, ErrNoUser) {
				continue
			}
			if err == nil {
				err = r.Remove(ctx, fid)
			}
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("user %s: %s: remove: %v", id, f.Name, err))
				continue
			}
			logf("removed at failover", "user", id, "idp", f.Name)
		}
		if err := b.DeleteUser(ctx, id); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("user %s: broker: remove: %v", id, err))
			continue
		}
		res.Removed++
		logf("broker account removed: the primary no longer has the user", "user", id)
	}
}

func syncUser(ctx context.Context, b Broker, u keycloak.User, primary *IdP, failovers []IdP, writable, lists []string, groups map[string]string, logf func(string, ...any), res *Result) {
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
			} else if values, verified := read(rec, primary.Attributes, writable, fit), emailVerified(rec); apply(clone(u), values, verified) {
				var fresh keycloak.User
				changed, err := b.UpdateUser(ctx, id, func(cur keycloak.User) bool {
					fresh = cur
					return apply(cur, values, verified)
				})
				switch {
				case err != nil:
					fail("broker update: %v", err)
				case changed:
					res.Updated++
					logf("broker profile updated", "user", id, "attributes", sortedKeys(values))
				}
				if err == nil && fresh != nil {
					u = fresh // what the broker has now goes out to the failovers
				}
			}
		} else if !errors.Is(err, ErrNoUser) {
			fail("%s: %v", primary.Name, err)
		}
	}

	// the primary's groups (or roles), into the broker's shape groups. Only
	// what the primary said this run goes out to the failovers: a primary
	// with no directory, or none that lists groups, says nothing about
	// them, and the failovers keep their own.
	shapeNames := slices.Sorted(maps.Keys(groups))
	known := false
	if primary != nil && inPrimary && len(groups) > 0 {
		if gr, ok := primary.Dir.(GroupReader); ok {
			if pid, err := locate(ctx, b, id, email(u), *primary); err == nil {
				have, err := gr.Groups(ctx, pid)
				if err != nil {
					fail("%s: groups: %v", primary.Name, err)
				} else if changed, err := b.SetUserGroups(ctx, id, intersect(have, shapeNames), groups); err != nil {
					fail("broker groups: %v", err)
				} else {
					known = true
					if changed {
						res.Updated++
						logf("broker groups updated", "user", id)
					}
				}
			}
		}
	}
	var brokerGroups []string
	if known {
		have, err := b.UserGroups(ctx, id)
		if err != nil {
			fail("broker groups: %v", err)
		}
		brokerGroups = intersect(have, shapeNames)
	}

	// 2. the broker, out to each failover
	mail, verified := email(u), emailVerified(u)
	for _, f := range failovers {
		want := map[string][]string{} // path -> values, from the broker's profile
		for _, m := range f.Attributes {
			if slices.Contains(writable, m.Attribute) && !slices.Contains(keycloak.NeverSynced, m.Attribute) {
				if v := brokerValue(u, m.Attribute); len(v) > 0 {
					want[m.Path] = fit(m.Attribute, v)
				}
			}
		}
		folded := emailPaths(f.Attributes)
		fid, err := locate(ctx, b, id, mail, f)
		switch {
		case errors.Is(err, ErrNoUser) && mail != "" && inPrimary: // only users the primary has
			fid, err = f.Dir.Create(ctx, mail, want, verified)
			if errors.Is(err, ErrNoCreate) { // a limit of that directory, not a failed run
				res.Notes = append(res.Notes, fmt.Sprintf("user %s: no account at %s, which can't create one without a password", id, f.Name))
				continue
			}
			if err != nil {
				fail("%s: create: %v", f.Name, err)
				continue
			}
			res.Created++
			logf("created at failover", "user", id, "idp", f.Name)
			verify(ctx, f, fid, want, fail)
			if known {
				writeGroups(ctx, f, fid, brokerGroups, shapeNames, fail)
			}
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
		if known {
			writeGroups(ctx, f, fid, brokerGroups, shapeNames, fail)
		}
		rec, err := f.Dir.User(ctx, fid)
		if err != nil {
			fail("%s: %v", f.Name, err)
			continue
		}
		set := map[string][]string{}
		for p, v := range want {
			if !same(Values(Lookup(rec, p)), v, folded[p]) {
				set[p] = v
			}
		}
		if len(set) == 0 {
			continue
		}
		if err := f.Dir.Update(ctx, fid, set, verified); err != nil {
			fail("%s: update: %v", f.Name, err)
			continue
		}
		res.Written++
		logf("failover written", "user", id, "idp", f.Name, "attributes", sortedKeys(set))
		verify(ctx, f, fid, set, fail)
	}
}

// writeGroups puts the failover's user in the broker's shape groups, where
// the failover's directory can write them.
func writeGroups(ctx context.Context, f IdP, fid string, want, managed []string, fail func(string, ...any)) {
	gw, ok := f.Dir.(GroupWriter)
	if !ok || len(managed) == 0 {
		return
	}
	if err := gw.SetGroups(ctx, fid, want, managed); err != nil {
		fail("%s: groups: %v", f.Name, err)
	}
}

// intersect is the names of have that are in shape.
func intersect(have, shape []string) []string {
	var out []string
	for _, g := range have {
		if slices.Contains(shape, g) {
			out = append(out, g)
		}
	}
	return out
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
	folded := emailPaths(f.Attributes)
	var lost []string
	for _, p := range sortedKeys(set) {
		if !same(Values(Lookup(rec, p)), set[p], folded[p]) {
			lost = append(lost, p)
		}
	}
	if len(lost) > 0 {
		fail("%s: didn't keep %s (not in its user schema?)", f.Name, strings.Join(lost, ", "))
	}
}

// read maps the primary's record onto broker attributes. Emails are
// lowercased, as the broker keeps them.
func read(rec map[string]any, ms []v1.AttributeMapping, writable []string, fit func(string, []string) []string) map[string][]string {
	values := map[string][]string{}
	for _, m := range ms {
		if !slices.Contains(writable, m.Attribute) || slices.Contains(keycloak.NeverSynced, m.Attribute) {
			continue
		}
		v := Values(Lookup(rec, m.Path))
		if m.Attribute == "email" {
			for i := range v {
				v[i] = normalEmail(v[i])
			}
		}
		if len(v) > 0 {
			values[m.Attribute] = fit(m.Attribute, v)
		}
	}
	return values
}

func normalEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// emailPaths are the IdP paths mapped from the broker's email: compared
// without regard to case.
func emailPaths(ms []v1.AttributeMapping) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		if m.Attribute == "email" {
			out[m.Path] = true
		}
	}
	return out
}

// same compares attribute values, ignoring case when fold is set.
func same(a, b []string, fold bool) bool {
	if !fold {
		return slices.Equal(a, b)
	}
	return slices.EqualFunc(a, b, func(x, y string) bool { return strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(y)) })
}

// clone copies a user deep enough for apply to change without touching u.
func clone(u keycloak.User) keycloak.User {
	c := maps.Clone(u)
	if a, ok := u["attributes"].(map[string]any); ok {
		c["attributes"] = maps.Clone(a)
	}
	return c
}

// apply writes values into the broker user's representation; email,
// firstName and lastName are top-level fields, everything else an attribute.
// With the email goes whether the primary verified it (verified); a new
// address the primary doesn't say it verified is not marked verified.
// Reports whether anything changed.
func apply(u keycloak.User, values map[string][]string, verified *bool) bool {
	changed := false
	attrs, _ := u["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	for a, v := range values {
		switch a {
		case "email", "firstName", "lastName":
			if cur, _ := u[a].(string); cur != v[0] && (a != "email" || !strings.EqualFold(cur, v[0])) {
				u[a], changed = v[0], true
				if a == "email" && verified == nil {
					u["emailVerified"] = false
				}
			}
			if a == "email" && verified != nil && u["emailVerified"] != *verified {
				u["emailVerified"], changed = *verified, true
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

// emailVerified is whether a record's email is verified, when it says:
// email_verified (OIDC, Auth0) or emailVerified (Keycloak).
func emailVerified(rec map[string]any) *bool {
	if _, ok := rec["emails"].([]any); ok { // a SCIM record
		_, v := scimEmail(rec)
		return v
	}
	for _, k := range []string{"email_verified", "emailVerified"} {
		switch v := rec[k].(type) {
		case bool:
			return &v
		case string:
			if b, err := strconv.ParseBool(v); err == nil {
				return &b
			}
		}
	}
	return nil
}
