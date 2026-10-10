// Package routing is the identity fabric's routing policy: which IdP each
// sign-in goes to, resolved against the chain's health, and the CEL the
// firm's gateway runs on every sign-in request to apply it. No I/O.
package routing

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/tiers"
)

// HintParam is the broker's IdP hint (Keycloak's kc_idp_hint).
const HintParam = "kc_idp_hint"

// Resolve each rule against the chain: the first IdP in its list that is
// an oidc tier able to sign people in now (tiers.Eligible) and, when ready
// is given, set up at the broker. None: no IdP (the active tier's
// sign-ins), with the reason.
func Resolve(spec v1.IdentityContinuitySpec, st map[string]*v1.TierStatus, ready func(string) bool) []v1.RouteStatus {
	if spec.Routing == nil {
		return nil
	}
	byName := map[string]v1.Tier{}
	for _, t := range spec.Tiers {
		byName[t.Name] = t
	}
	out := make([]v1.RouteStatus, 0, len(spec.Routing.Rules))
	for _, r := range spec.Routing.Rules {
		rs := v1.RouteStatus{Name: r.Name}
		var skipped []string
		for i, name := range r.IdPs {
			t, ok := byName[name]
			why := ""
			switch {
			case !ok:
				why = "not in the chain"
			case t.Type != "oidc":
				why = "not an oidc tier"
			case !tiers.Eligible(t, st[name]):
				why = "can't sign people in now"
			case ready != nil && !ready(name):
				why = "not set up at the broker"
			}
			if why == "" {
				rs.IdP = name
				if i == 0 {
					rs.Reason = "its first IdP"
				} else {
					rs.Reason = fmt.Sprintf("fallback: %s", strings.Join(skipped, "; "))
				}
				break
			}
			skipped = append(skipped, name+" "+why)
		}
		if rs.IdP == "" {
			rs.Reason = fmt.Sprintf("none of its IdPs can (%s): the active tier", strings.Join(skipped, "; "))
		}
		out = append(out, rs)
	}
	return out
}

// Signing is the set of IdPs signing people in: the active tier and those
// the rules send sign-ins to now.
func Signing(active string, routes []v1.RouteStatus) map[string]bool {
	out := map[string]bool{}
	if active != "" {
		out[active] = true
	}
	for _, r := range routes {
		if r.IdP != "" {
			out[r.IdP] = true
		}
	}
	return out
}

// Path is the CEL for the sign-in request's :path at the gateway: the same
// request, with the broker's IdP hint put first. The broker reads the first
// hint, and refuses a request where the client sent one too (a duplicated
// parameter), so the gateway's choice is the only one that takes effect.
// fallback is the hint for sign-ins no rule matches: the active tier when
// it is an upstream IdP, empty when it is the broker's own login.
func Path(spec v1.IdentityContinuitySpec, routes []v1.RouteStatus, fallback string) string {
	idp := map[string]string{}
	for _, r := range routes {
		idp[r.Name] = r.IdP
	}
	var hint strings.Builder
	if spec.Routing != nil {
		for _, r := range spec.Routing.Rules {
			to := idp[r.Name]
			if to == "" {
				to = fallback
			}
			fmt.Fprintf(&hint, "(%s) ? %s : ", r.When, strconv.Quote(param(to)))
		}
	}
	hint.WriteString(strconv.Quote(param(fallback)))
	return `request.path + "?" + (` + hint.String() + `) + ` +
		`(request.uri.contains("?") ? request.uri.substring(request.uri.indexOf("?") + 1) : "")`
}

func param(idp string) string {
	if idp == "" {
		return ""
	}
	return HintParam + "=" + url.QueryEscape(idp) + "&"
}
