package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/assurance"
)

// The gate's evaluate API, on a port of its own (the Observatory's, never a
// gateway's): what the assurance rules decide, from the same code that
// enforces them, for rules as saved or as an admin is editing them. Read
// only; it changes nothing and holds nothing.
//
//	POST /v1/evaluate      a chain's rules: each IdP's outcome per rule, and
//	                       optionally one session's decision (what if)
//	GET  /v1/policy-point  the extAuth a gateway policy needs to ask the gate
//	                       for a rule
func (g *gate) evaluateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/evaluate", g.serveEvaluate)
	mux.HandleFunc("GET /v1/policy-point", servePolicyPoint)
	return mux
}

// EvalRequest: the chain to evaluate, with a draft laid over what the gate
// has (a rule set to null is removed), and a session for what if.
type EvalRequest struct {
	Continuity string     `json:"continuity"`
	Draft      *EvalDraft `json:"draft,omitempty"`
	Session    *struct {
		IdP      string   `json:"idp"`
		ACR      string   `json:"acr,omitempty"`
		AMR      []string `json:"amr,omitempty"`
		AuthTime int64    `json:"authTime,omitempty"` // unix seconds; 0: not asserted
	} `json:"session,omitempty"`
}

type EvalDraft struct {
	Policy *v1.AssurancePolicy                `json:"policy,omitempty"`
	Tiers  map[string]*v1.TierAssurance       `json:"tiers,omitempty"`
	Rules  map[string]*v1.WorkloadProfileSpec `json:"rules,omitempty"`
}

type EvalResponse struct {
	Continuity string     `json:"continuity"`
	Active     string     `json:"active,omitempty"`
	IdPs       []EvalIdP  `json:"idps"`
	Rules      []EvalRule `json:"rules"`
}

type EvalIdP struct {
	Name     string `json:"name"`
	Ceiling  string `json:"ceiling"`
	Active   bool   `json:"active,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// EvalRule: one rule (Name "" is the chain's default rule).
type EvalRule struct {
	Name      string            `json:"name"`
	Mode      string            `json:"mode"`
	Phase     string            `json:"phase"`
	Eligible  []string          `json:"eligible"`
	Effective EvalRules         `json:"effective"`
	IdPs      []assurance.Reach `json:"idps"`
	Session   *EvalSession      `json:"session,omitempty"`
}

type EvalRules struct {
	Minimum           string   `json:"minimum"`
	PhishingResistant bool     `json:"phishingResistant"`
	MaxAge            string   `json:"maxAge,omitempty"`
	AllowedIdPs       []string `json:"allowedIdPs"`
	AllowBreakGlass   bool     `json:"allowBreakGlass"`
	Sessions          string   `json:"sessions"`
}

// EvalSession: what the gate answers that session (status as a policy point
// returns it) and why.
type EvalSession struct {
	Decision  string `json:"decision"` // allow, deny, would-deny, off
	Status    int    `json:"status"`
	Reason    string `json:"reason"`
	ACRValues string `json:"acrValues,omitempty"`
	MaxAge    string `json:"maxAge,omitempty"`
}

func (g *gate) serveEvaluate(w http.ResponseWriter, r *http.Request) {
	if !g.synced.Load() {
		http.Error(w, "the gate hasn't read the assurance rules yet", http.StatusServiceUnavailable)
		return
	}
	var req EvalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ic, why := g.continuity(r.Context(), req.Continuity)
	if why != "" {
		http.Error(w, why, http.StatusNotFound)
		return
	}
	var ps v1.WorkloadProfileList
	if err := g.r.List(r.Context(), &ps, client.InNamespace(g.ns)); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(evaluate(ic, ps.Items, req, g.now()))
}

// evaluate: every rule following the chain (and its default rule), with the
// draft laid over them, against each of the chain's IdPs.
func evaluate(ic v1.IdentityContinuity, profiles []v1.WorkloadProfile, req EvalRequest, now time.Time) EvalResponse {
	spec := ic.Spec
	rules := map[string]v1.WorkloadProfileSpec{}
	for _, p := range profiles {
		if p.Spec.Continuity == ic.Name {
			rules[p.Name] = p.Spec
		}
	}
	if d := req.Draft; d != nil {
		if d.Policy != nil {
			spec.AssurancePolicy = *d.Policy
		}
		tiers := make([]v1.Tier, len(spec.Tiers))
		copy(tiers, spec.Tiers)
		for i, t := range tiers {
			if a, ok := d.Tiers[t.Name]; ok {
				tiers[i].Assurance = a
			}
		}
		spec.Tiers = tiers
		for name, r := range d.Rules {
			if r == nil {
				delete(rules, name)
			} else {
				rules[name] = *r
			}
		}
	}
	chain, active, cur := activeChain(spec.Tiers), ic.Status.Active, current(ic)
	out := EvalResponse{Continuity: ic.Name, Active: active, IdPs: []EvalIdP{}, Rules: []EvalRule{}}
	for _, t := range spec.Tiers {
		out.IdPs = append(out.IdPs, EvalIdP{Name: t.Name, Ceiling: assurance.Ceiling(t).String(), Active: t.Name == active,
			Disabled: t.Enabled != nil && !*t.Enabled})
	}
	names := make([]string, 0, len(rules)) // a draft's new rules among them
	for name := range rules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range append([]string{""}, names...) {
		ps := rules[name] // the default rule: no profile, the policy alone
		mode := ps.Mode
		if mode == "" || name == "" {
			mode = modeEnforce
		}
		rs := assurance.Effective(ps, spec.AssurancePolicy)
		er := EvalRule{Name: name, Mode: mode, Phase: assurance.Phase(rs, chain, active), Eligible: assurance.Eligible(rs, chain),
			Effective: evalRules(rs), IdPs: []assurance.Reach{}}
		if er.Eligible == nil {
			er.Eligible = []string{}
		}
		for _, t := range chain {
			er.IdPs = append(er.IdPs, assurance.ReachOf(rs, t, cur))
		}
		if s := req.Session; s != nil {
			sess := assurance.Session{IdP: s.IdP, ACR: s.ACR, AMR: s.AMR}
			if s.AuthTime > 0 {
				sess.AuthTime = time.Unix(s.AuthTime, 0)
			}
			er.Session = sessionDecision(assurance.Decide(rs, chain, cur, sess, now), mode)
		}
		out.Rules = append(out.Rules, er)
	}
	return out
}

func evalRules(r assurance.Rules) EvalRules {
	out := EvalRules{Minimum: r.Minimum.String(), PhishingResistant: r.PhishingResistant, AllowedIdPs: r.AllowedIdPs,
		AllowBreakGlass: r.AllowBreakGlass, Sessions: "Any"}
	if out.AllowedIdPs == nil {
		out.AllowedIdPs = []string{}
	}
	if r.MaxAge > 0 {
		out.MaxAge = r.MaxAge.String()
	}
	if r.ActiveIdPOnly {
		out.Sessions = "ActiveIdPOnly"
	}
	return out
}

// sessionDecision: the decision as the gate returns it, for the rule's mode.
func sessionDecision(d assurance.Decision, mode string) *EvalSession {
	s := &EvalSession{Decision: "allow", Status: http.StatusOK, Reason: d.Reason}
	switch {
	case mode == modeOff:
		s.Decision, s.Reason = "off", "the rule is off"
	case d.Allow:
	case mode == modeReportOnly:
		s.Decision = "would-deny"
	case d.Insufficient:
		s.Decision, s.Status, s.ACRValues = "deny", http.StatusUnauthorized, d.ACRValues
		if d.MaxAge > 0 {
			s.MaxAge = d.MaxAge.String()
		}
	default:
		s.Decision, s.Status = "deny", http.StatusForbidden
	}
	return s
}

// gateClaims is the requestMetadata a policy point sends: the verified
// token's claims about the sign-in, never a header the caller could set.
const gateClaims = `{"iss": jwt.iss, "sub": jwt.sub, "idp": has(jwt.idp) ? jwt.idp : "", ` +
	`"acr": has(jwt.idp_acr) ? jwt.idp_acr : "", "amr": has(jwt.idp_amr) ? jwt.idp_amr : "", ` +
	`"auth_time": has(jwt.idp_auth_time) ? jwt.idp_auth_time : 0}`

// servePolicyPoint: the extAuth a gateway policy needs to ask the gate for a
// rule (?rule=; none for the default rule) of a chain (?continuity=), less
// the backendRef, which is wherever this gate's Service is.
func servePolicyPoint(w http.ResponseWriter, r *http.Request) {
	ext := map[string]string{}
	if v := r.URL.Query().Get("rule"); v != "" {
		ext[gateProfileKey] = v
	}
	if v := r.URL.Query().Get("continuity"); v != "" {
		ext[gateContinuityKey] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"failureMode": "FailClosed",
		"grpc":        map[string]any{"contextExtensions": ext, "requestMetadata": map[string]string{gateMetaKey: gateClaims}},
	})
}
