package assurance

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

// Policy is the assurance decision in Rego as the repo ships it: the module
// the install puts in ConfigMap assurance-policy, which the gate evaluates
// here and Solo's ext-auth service loads as is (data.assurance.extauth).
// The gate decides with it until it has read that ConfigMap.
//
//go:embed assurance.rego
var Policy string

// Module is a compiled assurance module: its text and its decision query.
type Module struct {
	Text string
	q    rego.PreparedEvalQuery
}

// Compile: the module, if it parses, compiles and defines
// data.assurance.decision.
func Compile(text string) (*Module, error) {
	m, err := ast.ParseModuleWithOpts("assurance.rego", text, ast.ParserOptions{RegoVersion: ast.RegoV1})
	if err != nil {
		return nil, err
	}
	if p := m.Package.Path.String(); p != "data.assurance" {
		return nil, fmt.Errorf("package %s: the decision logic is package assurance", strings.TrimPrefix(p, "data."))
	}
	if !Defines(m, "decision") {
		return nil, errors.New("no rule decision: the gate asks data.assurance.decision")
	}
	q, err := rego.New(rego.Query("data.assurance.decision"), rego.ParsedModule(m)).PrepareForEval(context.Background())
	if err != nil {
		return nil, err
	}
	return &Module{Text: text, q: q}, nil
}

// Defines: the module has a rule by that name.
func Defines(m *ast.Module, name string) bool {
	for _, r := range m.Rules {
		if ref := r.Head.Ref(); len(ref) > 0 && ref[0].Value.Compare(ast.Var(name)) == 0 {
			return true
		}
	}
	return false
}

var (
	// Policy, compiled on first use (the controller imports this package
	// but never decides)
	builtin = sync.OnceValue(func() *Module {
		m, err := Compile(Policy)
		if err != nil {
			panic(fmt.Sprintf("assurance.rego: %v", err))
		}
		return m
	})
	live atomic.Pointer[Module]
)

// Use: decide with m from the next decision on (nil: Policy again).
func Use(m *Module) { live.Store(m) }

// Active: the module deciding now.
func Active() *Module {
	if m := live.Load(); m != nil {
		return m
	}
	return builtin()
}

// Decide whether the rules admit the session: the active Rego module's answer.
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
	rs, err := Active().q.Eval(context.Background(), rego.EvalInput(in))
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
