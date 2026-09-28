package controller

import (
	"slices"
	"testing"

	"github.com/nickgamb/solo-lab/apps/continuity/internal/probe"
)

func TestExternalHosts(t *testing.T) {
	internal := []string{"lab", "svc", "cluster.local"}
	d := &probe.Discovery{TokenEndpoint: "https://gamb.us.auth0.com/oauth/token", JWKSURI: "https://keys.example.com/jwks", UserinfoEndpoint: "https://gamb.us.auth0.com/userinfo"}
	if got := externalHosts(internal, "https://gamb.us.auth0.com/", d); !slices.Equal(got, []string{"gamb.us.auth0.com", "keys.example.com"}) {
		t.Errorf("auth0: %v", got)
	}
	if got := externalHosts(internal, "https://idp.ledgerline.lab/realms/ledgerline", nil); len(got) != 0 {
		t.Errorf("in-lab issuer needs no ServiceEntry: %v", got)
	}
	if got := externalHosts(internal, "https://notlab.com/", nil); !slices.Equal(got, []string{"notlab.com"}) {
		t.Errorf("suffix must match on a label boundary: %v", got)
	}
}
