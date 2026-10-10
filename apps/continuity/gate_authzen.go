package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nickgamb/solo-lab/apps/continuity/internal/assurance"
)

// The gate as an AuthZEN policy decision point (OpenID AuthZEN Authorization
// API 1.0, access evaluation), on the evaluate port: any enforcement point
// that speaks AuthZEN can ask what the assurance rules decide, with the same
// answer the gate gives agentgateway.
//
//	GET  /.well-known/authzen-configuration  PDP metadata
//	POST /access/v1/evaluation               one decision
//	POST /access/v1/evaluations              several, in order
//
// The subject is a verified session: its properties are the claims of a
// broker token the enforcement point has verified (iss, idp, acr, amr,
// auth_time), never a token. The resource is an assurance rule (type
// "assurance_rule", id = the WorkloadProfile, "" or "default" = the chain's
// default rule; property continuity = the chain, optional). The action is
// whatever the point enforces; the rules don't depend on it.

type azSubject struct {
	Type       string         `json:"type"`
	ID         string         `json:"id"`
	Properties map[string]any `json:"properties,omitempty"`
}

type azResource struct {
	Type       string         `json:"type"`
	ID         string         `json:"id"`
	Properties map[string]any `json:"properties,omitempty"`
}

type azRequest struct {
	Subject  *azSubject      `json:"subject"`
	Resource *azResource     `json:"resource"`
	Action   json.RawMessage `json:"action"`
	Context  map[string]any  `json:"context,omitempty"`
}

type azResponse struct {
	Decision bool           `json:"decision"`
	Context  map[string]any `json:"context,omitempty"`
}

// azRuleType: the AuthZEN resource type an assurance rule is evaluated as.
const azRuleType = "assurance_rule"

func (g *gate) authzenRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/authzen-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		writeJSON(w, map[string]string{
			"policy_decision_point":       base,
			"access_evaluation_endpoint":  base + "/access/v1/evaluation",
			"access_evaluations_endpoint": base + "/access/v1/evaluations",
		})
	})
	mux.HandleFunc("POST /access/v1/evaluation", func(w http.ResponseWriter, r *http.Request) {
		var req azRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		res, msg := g.azEvaluate(r, req)
		if res == nil {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("POST /access/v1/evaluations", func(w http.ResponseWriter, r *http.Request) {
		// AuthZEN batch: defaults at the top level, each evaluation overriding them
		var req struct {
			azRequest
			Evaluations []azRequest `json:"evaluations"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		out := []*azResponse{}
		for _, e := range req.Evaluations {
			if e.Subject == nil {
				e.Subject = req.Subject
			}
			if e.Resource == nil {
				e.Resource = req.Resource
			}
			res, msg := g.azEvaluate(r, e)
			if res == nil {
				res = &azResponse{Context: map[string]any{"error": msg}}
			}
			out = append(out, res)
		}
		writeJSON(w, map[string]any{"evaluations": out})
	})
}

// azEvaluate: one AuthZEN evaluation; nil and why for a malformed request.
func (g *gate) azEvaluate(r *http.Request, req azRequest) (*azResponse, string) {
	if req.Subject == nil || req.Resource == nil {
		return nil, "subject and resource are required"
	}
	if req.Resource.Type != azRuleType {
		return nil, `resource.type must be "` + azRuleType + `"`
	}
	name := req.Resource.ID
	if name == "default" {
		name = ""
	}
	continuity, _ := req.Resource.Properties["continuity"].(string)
	p := req.Subject.Properties
	str := func(k string) string { v, _ := p[k].(string); return v }
	s := &assurance.Session{IdP: str("idp"), ACR: str("acr")}
	switch amr := p["amr"].(type) {
	case string:
		s.AMR = strings.Fields(amr)
	case []any:
		for _, a := range amr {
			if v, ok := a.(string); ok {
				s.AMR = append(s.AMR, v)
			}
		}
	}
	if t, ok := p["auth_time"].(float64); ok && t > 0 {
		s.AuthTime = time.Unix(int64(t), 0)
	}
	verdict, d := g.judge(r.Context(), name, continuity, str("iss"), s)
	ctx := map[string]any{"verdict": verdict, "reason_admin": map[string]string{"en": d.Reason}}
	if d.Insufficient {
		ctx["error"] = "insufficient_user_authentication"
		if d.ACRValues != "" {
			ctx["acr_values"] = d.ACRValues
		}
		if d.MaxAge > 0 {
			ctx["max_age"] = int(d.MaxAge.Seconds())
		}
	}
	// allow, or not enforced (off, would-deny): the request goes on
	return &azResponse{Decision: verdict == "allow" || verdict == "off" || verdict == "would-deny", Context: ctx}, ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
