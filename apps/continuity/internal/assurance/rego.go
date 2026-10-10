package assurance

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/open-policy-agent/opa/v1/rego"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

// Policy is the assurance decision in Rego: the module the gate evaluates
// here, and Solo's ext-auth service loads as is (data.assurance.extauth).
//
//go:embed assurance.rego
var Policy string

// the module, compiled on first use (the controller imports this package
// but never decides)
var decision = sync.OnceValue(func() rego.PreparedEvalQuery {
	q, err := rego.New(rego.Query("data.assurance.decision"), rego.Module("assurance.rego", Policy)).PrepareForEval(context.Background())
	if err != nil {
		panic(fmt.Sprintf("assurance.rego: %v", err))
	}
	return q
})

// Decide whether the rules admit the session: the Rego module's answer.
// chain and cur are the IdentityContinuity's tiers and the IdPs signing
// people in now; now is the request's time. A policy that can't be
// evaluated refuses (fail closed).
func Decide(r Rules, chain []v1.Tier, cur Current, s Session, now time.Time) Decision {
	authTime := int64(0)
	if !s.AuthTime.IsZero() {
		authTime = s.AuthTime.Unix()
	}
	amr := s.AMR
	if amr == nil {
		amr = []string{}
	}
	in := map[string]any{
		"rules":   RulesInput(r),
		"chain":   ChainInput(chain),
		"current": CurrentInput(cur),
		"session": map[string]any{"idp": s.IdP, "acr": s.ACR, "amr": amr, "auth_time": authTime},
		"now":     now.Unix(),
	}
	rs, err := decision().Eval(context.Background(), rego.EvalInput(in))
	if err != nil || len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return Decision{Reason: fmt.Sprintf("the assurance policy couldn't decide: %v", err)}
	}
	raw, _ := json.Marshal(rs[0].Expressions[0].Value)
	var out struct {
		Allow        bool   `json:"allow"`
		Reason       string `json:"reason"`
		Insufficient bool   `json:"insufficient"`
		ACRValues    string `json:"acr_values"`
		MaxAgeS      int64  `json:"max_age_s"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Decision{Reason: fmt.Sprintf("the assurance policy answered %s", raw)}
	}
	return Decision{Allow: out.Allow, Reason: out.Reason, Insufficient: out.Insufficient, ACRValues: out.ACRValues,
		MaxAge: time.Duration(out.MaxAgeS) * time.Second}
}

// RulesInput: the rules as the Rego module reads them.
func RulesInput(r Rules) map[string]any {
	allowed := r.AllowedIdPs
	if allowed == nil {
		allowed = []string{}
	}
	return map[string]any{
		"minimum": int(r.Minimum), "phishing_resistant": r.PhishingResistant, "max_age_s": int64(r.MaxAge / time.Second),
		"allowed_idps": allowed, "allow_break_glass": r.AllowBreakGlass, "active_idp_only": r.ActiveIdPOnly,
	}
}

// ChainInput: the tiers as the Rego module reads them (their JSON).
func ChainInput(chain []v1.Tier) []any {
	out := make([]any, 0, len(chain))
	for _, t := range chain {
		var m map[string]any
		raw, _ := json.Marshal(t)
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

// CurrentInput: the IdPs signing people in now, as the Rego module reads them.
func CurrentInput(c Current) map[string]any {
	routed := c.Routed
	if routed == nil {
		routed = []string{}
	}
	return map[string]any{"active": c.Active, "routed": routed}
}

// EnabledTiers: the tiers that can ever be active (disabled ones never are).
func EnabledTiers(ts []v1.Tier) []v1.Tier {
	var out []v1.Tier
	for _, t := range ts {
		if t.Enabled == nil || *t.Enabled {
			out = append(out, t)
		}
	}
	return out
}

// CurrentOf: the IdPs a chain signs people in through now: its active tier,
// and those its routing rules send sign-ins to.
func CurrentOf(ic *v1.IdentityContinuity) Current {
	c := Current{Active: ic.Status.Active}
	for _, r := range ic.Status.Routing {
		if r.IdP != "" {
			c.Routed = append(c.Routed, r.IdP)
		}
	}
	return c
}
