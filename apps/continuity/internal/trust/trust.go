// Package trust checks S&V's registration at an upstream IdP against what the
// IdP publishes: that failing over to it keeps the trust and token model the
// broker relies on. No I/O: the discovery document and the callback probe's
// outcome come in.
package trust

import (
	"fmt"
	"slices"
	"strings"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
)

const (
	Pass    = "Pass"
	Fail    = "Fail"
	Unknown = "Unknown"
)

// Check names, in the order they are reported.
const (
	Callback   = "Callback"
	ClientAuth = "ClientAuth"
	PKCE       = "PKCE"
	Scopes     = "Scopes"
	Claims     = "Claims"
	Assurance  = "Assurance"
)

// identityClaims are what the broker needs from every IdP's ID token beyond
// sub, which every OpenID provider issues (OIDC Core 2) whether or not it
// lists it in claims_supported (Gluu's doesn't): the email it links the S&V
// user by. It also requires email_verified at sign-in, which IdPs often issue
// without listing it (Keycloak's doesn't), so its absence there is a note.
var identityClaims = []string{"email"}

// Checks are an oidc tier's checks: its spec, the IdP's discovery and the
// callback probe's outcome (probe.Callback*).
func Checks(t v1.Tier, d *probe.Discovery, callback, callbackMsg string) []v1.TrustCheck {
	o := t.OIDC
	var out []v1.TrustCheck
	add := func(name, result, format string, a ...any) {
		out = append(out, v1.TrustCheck{Name: name, Result: result, Message: fmt.Sprintf(format, a...)})
	}
	switch callback {
	case probe.CallbackRegistered:
		add(Callback, Pass, "%s", callbackMsg)
	case probe.CallbackRefused:
		add(Callback, Fail, "%s", callbackMsg)
	default:
		add(Callback, Unknown, "%s", callbackMsg)
	}

	method := o.ClientAuth
	if method == "" {
		method = "client_secret_post"
	}
	switch {
	case len(d.TokenEndpointAuthMethodsSupported) == 0:
		add(ClientAuth, Unknown, "the IdP doesn't publish its client authentication methods")
	case !slices.Contains(d.TokenEndpointAuthMethodsSupported, method):
		add(ClientAuth, Fail, "the broker authenticates with %s; the IdP takes %s", method, strings.Join(d.TokenEndpointAuthMethodsSupported, ", "))
	case method == "private_key_jwt" && len(d.TokenEndpointAuthSigningAlgs) > 0 && !slices.Contains(d.TokenEndpointAuthSigningAlgs, alg(o)):
		add(ClientAuth, Fail, "the broker signs its client assertion with %s; the IdP takes %s", alg(o), strings.Join(d.TokenEndpointAuthSigningAlgs, ", "))
	default:
		add(ClientAuth, Pass, "%s", method)
	}

	switch {
	case len(d.CodeChallengeMethodsSupported) == 0:
		add(PKCE, Unknown, "the IdP doesn't publish its PKCE methods")
	case slices.Contains(d.CodeChallengeMethodsSupported, "S256"):
		add(PKCE, Pass, "S256")
	default:
		add(PKCE, Fail, "the broker sends PKCE S256; the IdP takes %s", strings.Join(d.CodeChallengeMethodsSupported, ", "))
	}

	scopes := o.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	if len(d.ScopesSupported) == 0 {
		add(Scopes, Unknown, "the IdP doesn't publish its scopes")
	} else if missing := without(scopes, d.ScopesSupported); len(missing) > 0 {
		add(Scopes, Fail, "the broker asks for %s, which the IdP doesn't offer", strings.Join(missing, ", "))
	} else {
		add(Scopes, Pass, "%s", strings.Join(scopes, " "))
	}

	if len(d.ClaimsSupported) == 0 {
		add(Claims, Unknown, "the IdP doesn't publish its claims")
	} else if missing := without(identityClaims, d.ClaimsSupported); len(missing) > 0 {
		add(Claims, Fail, "the broker links users by sub, %s; the IdP doesn't issue %s", strings.Join(identityClaims, ", "), strings.Join(missing, ", "))
	} else if !slices.Contains(d.ClaimsSupported, "email_verified") {
		add(Claims, Pass, "sub, email (email_verified isn't listed; the broker requires it at sign-in)")
	} else {
		add(Claims, Pass, "sub, email, email_verified")
	}

	out = append(out, assurance(t, d))
	return out
}

// assurance: what the tier maps is something the IdP asserts. Fails only
// when nothing mapped can ever match: acr values it publishes it never
// asserts, and no amr mapping that could match instead.
func assurance(t v1.Tier, d *probe.Discovery) v1.TrustCheck {
	c := v1.TrustCheck{Name: Assurance}
	var acrs []string
	amr := false
	if t.Assurance != nil {
		for _, l := range t.Assurance.Levels {
			if l.ACR != "" {
				acrs = append(acrs, l.ACR)
			}
			amr = amr || l.AMR != ""
		}
	}
	published := len(d.ACRValuesSupported) > 0
	asserted := len(acrs) - len(without(acrs, d.ACRValuesSupported))
	amrIssued := slices.Contains(d.ClaimsSupported, "amr")
	switch {
	case len(acrs) == 0 && !amr:
		c.Result, c.Message = Pass, "no acr or amr mapped: every sign-in counts as "+def(t)
	case published && asserted > 0:
		c.Result, c.Message = Pass, "the IdP asserts the mapped acr values"
		if missing := without(acrs, d.ACRValuesSupported); len(missing) > 0 {
			c.Message = "the IdP asserts some of the mapped acr values; not " + strings.Join(missing, ", ")
		}
	case amr && amrIssued:
		c.Result, c.Message = Pass, "the IdP issues amr, which the mapping reads"
	case published && len(acrs) > 0 && !amr:
		c.Result, c.Message = Fail, fmt.Sprintf("none of the mapped acr values (%s) are ones the IdP asserts (%s): every sign-in counts as %s",
			strings.Join(acrs, ", "), strings.Join(d.ACRValuesSupported, ", "), def(t))
	default:
		c.Result, c.Message = Unknown, "the IdP doesn't publish the acr values or amr the mapping reads"
	}
	return c
}

// Failed names the checks that failed.
func Failed(checks []v1.TrustCheck) []string {
	var out []string
	for _, c := range checks {
		if c.Result == Fail {
			out = append(out, c.Name)
		}
	}
	return out
}

func without(want, have []string) []string {
	var out []string
	for _, w := range want {
		if !slices.Contains(have, w) {
			out = append(out, w)
		}
	}
	return out
}

func alg(o *v1.OIDCUpstream) string {
	if o.ClientAssertionSigningAlg != "" {
		return o.ClientAssertionSigningAlg
	}
	return "PS256"
}

func def(t v1.Tier) string {
	if t.Assurance != nil && t.Assurance.Default != "" {
		return t.Assurance.Default
	}
	return "AAL1"
}
