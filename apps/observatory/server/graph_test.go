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
			app:     {ID: app, Kind: "ui", Summary: map[string]any{}},
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
