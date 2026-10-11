package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// The relations the gateway asks about, as S&V's RelationshipModel defines
// them (platform/48-relationships/model.yaml).
const (
	relActsFor    = "acts_for" // agent: the people it may act for
	relCanCall    = "can_call" // mcp_server: who may call it
	relCanView    = "can_view" // client: who may see the client's records
	typeClient    = "client"
	relMember     = "member" // group: from the token, never stored
	headerClients = "x-relationship-clients"
)

// caller is who is on a tool call, as the gateway verified it.
type caller struct {
	// the user's email in the token (jwt.email)
	User string
	// the token's groups: membership comes from the IdP, so each call holds
	// it as contextual tuples instead of a copy in OpenFGA
	Groups []string
	// the calling workload's mesh identity, namespace/serviceaccount, when
	// it is a workload in the mesh
	Workload string
	// the token's OAuth client (jwt.azp): the agent when no workload is (an
	// AI client outside the mesh)
	Client string
}

func (c caller) user() string { return "user:" + strings.ToLower(c.User) }

// agent: the OpenFGA object for whoever is acting: a workload as
// agent:<namespace>.<serviceaccount>, else the OAuth client as
// agent:client.<client_id>.
func (c caller) agent() string {
	if ns, sa, ok := strings.Cut(c.Workload, "/"); ok && ns != "" && sa != "" {
		return "agent:" + ns + "." + sa
	}
	if c.Client != "" {
		return "agent:client." + c.Client
	}
	return ""
}

func (c caller) memberships() []tuple {
	out := make([]tuple, 0, len(c.Groups))
	for _, g := range c.Groups {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, tuple{c.user(), relMember, "group:" + g})
		}
	}
	return out
}

// decision is what the gateway is told about one tool call.
type decision struct {
	Allow   bool     `json:"allow"`
	Reason  string   `json:"reason"`
	ActsFor bool     `json:"actsFor"`
	CanCall bool     `json:"canCall"`
	Clients []string `json:"clients"`
}

// decide: the agent acts for the user, the user may call the server, and the
// clients the user may see (handed to the server).
func decide(ctx context.Context, f *fga, c caller, server string) (decision, error) {
	var d decision
	if c.User == "" {
		d.Reason = "no verified user on this call"
		return d, nil
	}
	agent := c.agent()
	if agent == "" {
		d.Reason = "no workload identity or OAuth client to act as"
		return d, nil
	}
	ctxt := c.memberships()
	var err error
	if d.ActsFor, err = f.check(ctx, c.user(), relActsFor, agent, ctxt); err != nil {
		return d, err
	}
	if !d.ActsFor {
		d.Reason = fmt.Sprintf("%s does not act for %s", agent, c.user())
		return d, nil
	}
	if d.CanCall, err = f.check(ctx, c.user(), relCanCall, "mcp_server:"+server, ctxt); err != nil {
		return d, err
	}
	if !d.CanCall {
		d.Reason = fmt.Sprintf("%s may not call %s", c.user(), server)
		return d, nil
	}
	if d.Clients, err = f.listObjects(ctx, c.user(), relCanView, typeClient, ctxt); err != nil {
		return d, err
	}
	slices.Sort(d.Clients)
	d.Allow = true
	d.Reason = fmt.Sprintf("%s acts for %s, who may call %s and see %d clients", agent, c.user(), server, len(d.Clients))
	return d, nil
}
