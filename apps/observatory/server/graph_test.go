package main

import (
	"slices"
	"testing"
)

// The broker's browser sign-in clients are drawn as the apps they redirect to;
// a partner's IdP federating to the broker is an identity provider, not an app.
func TestBrokerSignInsAreApps(t *testing.T) {
	ic := object("continuity.lab.solo.io/v1alpha1", "IdentityContinuity", "id", "firm",
		"spec", map[string]any{"broker": map[string]any{"keycloak": map[string]any{"url": "http://keycloak.id.svc"}}},
		"status", map[string]any{"broker": map[string]any{"issuer": "https://idp.firm.lab/realms/firm", "signIn": []any{
			map[string]any{"clientID": "console", "redirectURIs": []any{"https://console.firm.lab/callback"}},
			map[string]any{"clientID": "partner", "redirectURIs": []any{"https://idp.partner.lab/realms/p/broker/firm/endpoint"}},
		}}})
	broker, app, partner := "wl:id/keycloak", "wl:ui/console", "wl:partner/keycloak"
	b := &builder{
		k:     storeKube(map[string][]map[string]any{"identitycontinuities": {ic}}),
		svcWL: map[string][]string{"id/keycloak": {broker}},
		nodes: map[string]*Node{broker: {ID: broker, Kind: "idp", Summary: map[string]any{}},
			app:     {ID: app, Kind: "ui", Summary: map[string]any{}},
			partner: {ID: partner, Kind: "idp", Summary: map[string]any{}}},
		hosts: map[string][]string{"console.firm.lab": {app}, "idp.partner.lab": {partner}},
	}
	b.brokerSignIns()
	var apps []string
	for _, s := range b.sso {
		apps = append(apps, s.Apps...)
	}
	if !slices.Equal(apps, []string{app}) {
		t.Fatalf("sign-in apps %v, want only the console", apps)
	}
}
