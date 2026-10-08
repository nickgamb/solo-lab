// Package assurance decides what a sign-in proves and whether a workload's
// assurance rules (its profile, over its chain's assurance policy) admit it:
// the assurance gate's decision and the profile's phase. No I/O.
package assurance

import (
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

// Level is a NIST SP 800-63B authenticator assurance level; 0 is none.
type Level int

func Parse(s string) Level {
	switch s {
	case "AAL1":
		return 1
	case "AAL2":
		return 2
	case "AAL3":
		return 3
	}
	return 0
}

func (l Level) String() string {
	if l < 1 || l > 3 {
		return "none"
	}
	return fmt.Sprintf("AAL%d", int(l))
}

// Session is what a broker token says about the sign-in behind it: the IdP
// that authenticated the user (idp; none for the broker's own accounts) and
// what that IdP asserted (idp_acr, idp_amr, idp_auth_time).
type Session struct {
	IdP      string
	ACR      string
	AMR      []string
	AuthTime time.Time // zero: not asserted
}

// Proof is what a session through a tier proves.
type Proof struct {
	Level             Level
	PhishingResistant bool
	Evidence          string // what it rests on, for the decision's reason
}

// Proven: the highest level any of the session's acr or amr values maps to
// on the tier, else the tier's default.
func Proven(t v1.Tier, s Session) Proof {
	p := Proof{Level: def(t), Evidence: "no acr or amr it maps"}
	if t.Assurance == nil {
		return p
	}
	matchedAny := false
	for _, l := range t.Assurance.Levels {
		matched := ""
		switch {
		case l.ACR != "" && l.ACR == s.ACR:
			matched = "acr " + l.ACR
		case l.AMR != "" && slices.Contains(s.AMR, l.AMR):
			matched = "amr " + l.AMR
		}
		if matched == "" {
			continue
		}
		lv := Parse(l.Level)
		if lv > p.Level || (lv == p.Level && (!matchedAny || (l.PhishingResistant && !p.PhishingResistant))) {
			p.Level, p.Evidence = lv, matched
		}
		matchedAny = true
		p.PhishingResistant = p.PhishingResistant || (l.PhishingResistant && lv >= p.Level)
	}
	return p
}

// Rules are a workload's assurance rules as they apply: its profile's,
// where it sets them, else its chain's assurance policy.
type Rules struct {
	Minimum           Level
	PhishingResistant bool
	MaxAge            time.Duration // 0: any
	AllowedIdPs       []string      // empty: every upstream
	AllowBreakGlass   bool
	ActiveIdPOnly     bool
}

// Effective resolves a profile's rules over the chain's policy.
func Effective(p v1.WorkloadProfileSpec, g v1.AssurancePolicy) Rules {
	r := Rules{Minimum: Parse(g.Minimum), PhishingResistant: g.PhishingResistant, AllowedIdPs: g.AllowedIdPs,
		AllowBreakGlass: g.AllowBreakGlass, ActiveIdPOnly: g.Sessions == "ActiveIdPOnly"}
	if g.MaxAge != nil {
		r.MaxAge = g.MaxAge.Duration
	}
	a := p.Assurance
	if a.Minimum != "" {
		r.Minimum = Parse(a.Minimum)
	}
	if a.PhishingResistant != nil {
		r.PhishingResistant = *a.PhishingResistant
	}
	if a.MaxAge != nil {
		r.MaxAge = a.MaxAge.Duration
	}
	if len(p.AllowedIdPs) > 0 {
		r.AllowedIdPs = p.AllowedIdPs
	}
	if p.AllowBreakGlass != nil {
		r.AllowBreakGlass = *p.AllowBreakGlass
	}
	if p.Sessions != "" {
		r.ActiveIdPOnly = p.Sessions == "ActiveIdPOnly"
	}
	if r.Minimum == 0 {
		r.Minimum = 1
	}
	return r
}

// Capable: whether a sign-in through the tier can meet the rules' assurance
// at all (some mapped value, or its default, reaches it).
func Capable(t v1.Tier, r Rules) bool {
	if !r.PhishingResistant && def(t) >= r.Minimum {
		return true
	}
	if t.Assurance == nil {
		return false
	}
	for _, l := range t.Assurance.Levels {
		if Parse(l.Level) >= r.Minimum && (!r.PhishingResistant || l.PhishingResistant) {
			return true
		}
	}
	return false
}

// Ceiling: the most a sign-in through the tier can prove.
func Ceiling(t v1.Tier) Level {
	c := def(t)
	if t.Assurance != nil {
		for _, l := range t.Assurance.Levels {
			c = max(c, Parse(l.Level))
		}
	}
	return c
}

func def(t v1.Tier) Level {
	if t.Assurance != nil && t.Assurance.Default != "" {
		return Parse(t.Assurance.Default)
	}
	return 1
}

// Allowed: whether the rules take sessions from the tier at all (allowed
// IdPs, and break-glass).
func Allowed(r Rules, t v1.Tier) (bool, string) {
	if t.Type == "local" {
		if r.AllowBreakGlass {
			return true, ""
		}
		return false, "break-glass sessions don't reach these workloads"
	}
	if len(r.AllowedIdPs) > 0 && !slices.Contains(r.AllowedIdPs, t.Name) {
		return false, fmt.Sprintf("these workloads take sessions from %s only, not %s", strings.Join(r.AllowedIdPs, ", "), t.Name)
	}
	return true, ""
}

// Eligible: the chain's tiers, in order, whose sign-ins can meet the rules.
func Eligible(r Rules, chain []v1.Tier) []string {
	var out []string
	for _, t := range chain {
		if ok, _ := Allowed(r, t); ok && Capable(t, r) {
			out = append(out, t.Name)
		}
	}
	return out
}

// Phase of a workload's rules with the chain serving through active.
func Phase(r Rules, chain []v1.Tier, active string) string {
	e := Eligible(r, chain)
	switch {
	case active == "" || !slices.Contains(e, active):
		return "FailedClosed"
	case e[0] != active:
		return "Degraded"
	}
	return "Available"
}

// Outcome of a sign-in through one IdP, against a workload's rules.
type Outcome string

const (
	Admit       Outcome = "Admit"       // every sign-in through it meets them
	Conditional Outcome = "Conditional" // a sign-in meets them when the IdP asserts Via
	Refuse      Outcome = "Refuse"      // no sign-in through it meets them
)

// Reach is what a sign-in through one of the chain's IdPs gets.
type Reach struct {
	IdP     string  `json:"idp"`
	Outcome Outcome `json:"outcome"`
	Via     string  `json:"via,omitempty"` // Conditional: the least acr or amr value that meets them
	Reason  string  `json:"reason"`
}

// ReachOf: what a sign-in through the tier gets against the rules, with the
// chain serving through active. What the gate decides for a session through
// it, before knowing what that session asserted.
func ReachOf(r Rules, t v1.Tier, active string) Reach {
	out := Reach{IdP: t.Name, Outcome: Refuse}
	if ok, why := Allowed(r, t); !ok {
		out.Reason = why
		return out
	}
	if r.ActiveIdPOnly && t.Name != active {
		out.Reason = fmt.Sprintf("these workloads take sessions only from the IdP signing people in now (%s)", orNone(active))
		return out
	}
	within := ""
	if r.MaxAge > 0 {
		within = fmt.Sprintf(", signed in within %s", r.MaxAge)
	}
	if d := def(t); !r.PhishingResistant && d >= r.Minimum {
		out.Outcome, out.Reason = Admit, fmt.Sprintf("every sign-in through %s proves %s, which meets %s%s", t.Name, d, r.Minimum, within)
		return out
	}
	var best *v1.AssuranceLevel
	if t.Assurance != nil {
		for i, l := range t.Assurance.Levels {
			lv := Parse(l.Level)
			if lv < r.Minimum || (r.PhishingResistant && !l.PhishingResistant) {
				continue
			}
			// the least that meets it; an acr before an amr (an app can ask for an acr)
			if best == nil || lv < Parse(best.Level) || (lv == Parse(best.Level) && l.ACR != "" && best.ACR == "") {
				best = &t.Assurance.Levels[i]
			}
		}
	}
	if best != nil {
		out.Outcome, out.Via = Conditional, "acr "+best.ACR
		if best.ACR == "" {
			out.Via = "amr " + best.AMR
		}
		out.Reason = fmt.Sprintf("a sign-in through %s meets %s when it asserts %s (%s)%s", t.Name, r.Minimum, out.Via, best.Level, within)
		return out
	}
	out.Reason = fmt.Sprintf("a sign-in through %s proves at most %s", t.Name, Ceiling(t))
	if r.PhishingResistant && Ceiling(t) >= r.Minimum {
		out.Reason = fmt.Sprintf("no sign-in through %s is phishing-resistant at %s", t.Name, r.Minimum)
	}
	return out
}

// Decision is the gate's answer for one request.
type Decision struct {
	Allow bool
	// Why, for the response and the logs: never a token or a user's details.
	Reason string
	// Insufficient: the user could pass by authenticating again, more
	// strongly (RFC 9470 insufficient_user_authentication); ACRValues and
	// MaxAge are what to ask the IdP for.
	Insufficient bool
	ACRValues    string
	MaxAge       time.Duration
}

// Decide whether the rules admit the session. chain and active are the
// IdentityContinuity's tiers and active tier; now is the request's time.
func Decide(r Rules, chain []v1.Tier, active string, s Session, now time.Time) Decision {
	var t *v1.Tier
	for i := range chain {
		if (s.IdP == "" && chain[i].Type == "local") || (s.IdP != "" && chain[i].Name == s.IdP) {
			t = &chain[i]
			break
		}
	}
	switch {
	case t == nil && s.IdP == "":
		return Decision{Reason: "the session names no IdP and the chain has no break-glass tier"}
	case t == nil:
		return Decision{Reason: fmt.Sprintf("session from %s, which isn't in the chain", s.IdP)}
	}
	if ok, why := Allowed(r, *t); !ok {
		return Decision{Reason: why}
	}
	if r.ActiveIdPOnly && t.Name != active {
		return Decision{Reason: fmt.Sprintf("session from %s; these workloads take sessions only from the IdP signing people in now (%s): sign in again", t.Name, orNone(active))}
	}
	proof := Proven(*t, s)
	need := Decision{Insufficient: true, ACRValues: stepUp(r, chain, active)}
	if proof.Level < r.Minimum {
		need.Reason = fmt.Sprintf("assurance %s below %s: session from %s (%s)", proof.Level, r.Minimum, t.Name, proof.Evidence)
		return need
	}
	if r.PhishingResistant && !proof.PhishingResistant {
		need.Reason = fmt.Sprintf("these workloads need a phishing-resistant authenticator: session from %s (%s)", t.Name, proof.Evidence)
		return need
	}
	if r.MaxAge > 0 {
		age := r.MaxAge
		switch {
		case s.AuthTime.IsZero():
			need.Reason, need.MaxAge = fmt.Sprintf("these workloads need a sign-in within %s; %s didn't say when the user authenticated", age, t.Name), age
			return need
		case now.Sub(s.AuthTime) > age:
			need.Reason, need.MaxAge = fmt.Sprintf("signed in %s ago at %s; these workloads need within %s", now.Sub(s.AuthTime).Round(time.Minute), t.Name, age), age
			return need
		}
	}
	return Decision{Allow: true, Reason: fmt.Sprintf("%s via %s (%s)", proof.Level, t.Name, proof.Evidence)}
}

// stepUp: the acr to ask the active IdP for, when one of its mapped acr
// values meets the rules; empty when none can (re-authenticating there
// won't help).
func stepUp(r Rules, chain []v1.Tier, active string) string {
	for _, t := range chain {
		if t.Name != active || t.Assurance == nil {
			continue
		}
		if ok, _ := Allowed(r, t); !ok {
			return ""
		}
		best, bestLevel := "", Level(0)
		for _, l := range t.Assurance.Levels {
			lv := Parse(l.Level)
			if l.ACR == "" || lv < r.Minimum || (r.PhishingResistant && !l.PhishingResistant) {
				continue
			}
			if best == "" || lv < bestLevel { // the least that meets it
				best, bestLevel = l.ACR, lv
			}
		}
		return best
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
