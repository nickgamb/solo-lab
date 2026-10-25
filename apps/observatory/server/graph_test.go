package main

import (
	"slices"
	"testing"
)

// The broker's browser sign-in clients are drawn as the apps they redirect to;
// an upstream IdP federating to the broker is an identity provider, not an app,
// and the broker itself is the identity fabric.
func TestBrokerSignInsAreApps(t *testing.T) {
	ic := object("continuity.lab.solo.io/v1alpha1", "IdentityContinuity", "id", "firm",
		"spec", map[string]any{"broker": map[string]any{"keycloak": map[string]any{"url": "http://keycloak.id.svc"}}},
		"status", map[string]any{"broker": map[string]any{"issuer": "https://idp.firm.lab/realms/firm", "signIn": []any{
			map[string]any{"clientID": "console", "redirectURIs": []any{"https://console.firm.lab/callback"}},
			map[string]any{"clientID": "upstream", "redirectURIs": []any{"https://idp.upstream.lab/realms/p/broker/firm/endpoint"}},
		}}})
	broker, app, upstream := "wl:id/keycloak", "wl:ui/console", "wl:upstream/keycloak"
	b := &builder{
		k:     storeKube(map[string][]map[string]any{"identitycontinuities": {ic}}),
		svcWL: map[string][]string{"id/keycloak": {broker}},
		nodes: map[string]*Node{broker: {ID: broker, Kind: "idp", Summary: map[string]any{}},
			app:      {ID: app, Kind: "ui", Summary: map[string]any{}},
			upstream: {ID: upstream, Kind: "idp", Summary: map[string]any{}}},
		hosts: map[string][]string{"console.firm.lab": {app}, "idp.upstream.lab": {upstream}},
	}
	b.brokerSignIns()
	var apps []string
	for _, s := range b.sso {
		apps = append(apps, s.Apps...)
	}
	if !slices.Equal(apps, []string{app}) {
		t.Fatalf("sign-in apps %v, want only the console", apps)
	}
	// the broker is drawn as the identity fabric, never as another IdP
	if n := b.nodes[broker]; n.Kind != "fabric" || n.Label != "Identity fabric" {
		t.Fatalf("broker drawn as %s %q, want the identity fabric", n.Kind, n.Label)
	}
	if b.nodes[upstream].Kind != "idp" {
		t.Fatalf("upstream IdP drawn as %s", b.nodes[upstream].Kind)
	}
}

// A sign-in redirect lands on what serves its path, past the gateways it
// passes through: the edge sends /oauth-issuer/ to ai-gateway, which sends
// it to the issuer. ai-gateway's MCP resources on the same host are no app.
func TestBrokerSignInAtThePath(t *testing.T) {
	ic := object("continuity.lab.solo.io/v1alpha1", "IdentityContinuity", "id", "firm",
		"spec", map[string]any{"broker": map[string]any{"keycloak": map[string]any{"url": "http://keycloak.id.svc"}}},
		"status", map[string]any{"broker": map[string]any{"issuer": "https://idp.firm.lab/realms/firm", "signIn": []any{
			map[string]any{"clientID": "issuer", "redirectURIs": []any{"https://mcp.firm.lab/oauth-issuer/callback/downstream"}},
		}}})
	broker, gw, issuer, tool := "wl:id/keycloak", "gw:ai-gateway", "wl:gw/issuer", "wl:mcp/tool"
	b := &builder{
		k:     storeKube(map[string][]map[string]any{"identitycontinuities": {ic}}),
		svcWL: map[string][]string{"id/keycloak": {broker}},
		nodes: map[string]*Node{broker: {ID: broker, Kind: "idp", Summary: map[string]any{}},
			gw:     {ID: gw, Kind: "gateway", Summary: map[string]any{}},
			issuer: {ID: issuer, Kind: "workload", Summary: map[string]any{}},
			tool:   {ID: tool, Kind: "mcp", Summary: map[string]any{}}},
		hosts: map[string][]string{"mcp.firm.lab": {gw, issuer, tool}},
		hostPaths: map[string][]hostRule{"mcp.firm.lab": {
			{paths: []string{"/mcp/", "/oauth-issuer/"}, exact: []bool{false, false}, targets: []string{gw}},
			{paths: []string{"/oauth-issuer"}, exact: []bool{false}, targets: []string{issuer}},
			{paths: []string{"/mcp/tool"}, exact: []bool{false}, targets: []string{tool}},
		}},
	}
	b.brokerSignIns()
	var apps []string
	for _, s := range b.sso {
		apps = append(apps, s.Apps...)
	}
	if !slices.Equal(apps, []string{issuer}) {
		t.Fatalf("sign-in apps %v, want only the issuer", apps)
	}
}

// A backend on a .invalid host takes no calls: no node for it.
func TestInvalidHostResolvesToNothing(t *testing.T) {
	b := &builder{nodes: map[string]*Node{}, hosts: map[string][]string{}}
	if ids := b.resolveHost("issuer-resource.invalid", "ns"); len(ids) != 0 {
		t.Fatalf("issuer-resource.invalid resolved to %v", ids)
	}
}
