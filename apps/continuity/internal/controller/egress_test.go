package controller

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
)

func TestExternalHosts(t *testing.T) {
	internal := []string{"lab", "svc", "cluster.local"}
	d := &probe.Discovery{TokenEndpoint: "https://gamb.us.auth0.com/oauth/token", JWKSURI: "https://keys.example.com/jwks", UserinfoEndpoint: "https://gamb.us.auth0.com/userinfo"}
	if got := externalHosts(internal, "https://gamb.us.auth0.com/", d); !slices.Equal(got, []string{"gamb.us.auth0.com", "keys.example.com"}) {
		t.Errorf("auth0: %v", got)
	}
	if got := externalHosts(internal, "http://keycloak.sv-workforce.svc/admin/realms/workforce", nil); len(got) != 0 {
		t.Errorf("a cluster Service needs no ServiceEntry: %v", got)
	}
	if got := externalHosts(internal, "https://idp.ledgerline.lab/realms/ledgerline", nil); len(got) != 0 {
		t.Errorf("in-lab issuer needs no ServiceEntry: %v", got)
	}
	if got := externalHosts(internal, "https://notlab.com/", nil); !slices.Equal(got, []string{"notlab.com"}) {
		t.Errorf("suffix must match on a label boundary: %v", got)
	}
}

// The ServiceEntry reaches the broker's namespace and every spec.egress.exportTo
// namespace, so their calls to the upstream leave through the same waypoint.
func TestServiceEntryExportTo(t *testing.T) {
	e := &v1.Egress{Namespace: "sv-egress", Waypoint: "egress-waypoint", ExportTo: []string{"agentgateway-system"}}
	se := serviceEntry(e, "sv-identity", "sv-identity.sterling-vance", "gluu", []string{"gluu.example"})
	got, _, _ := unstructured.NestedSlice(se.Object, "spec", "exportTo")
	want := []any{".", "sv-identity", "agentgateway-system"}
	if len(got) != len(want) {
		t.Fatalf("exportTo = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exportTo = %v, want %v", got, want)
		}
	}
}
