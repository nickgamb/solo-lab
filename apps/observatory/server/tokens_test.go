package main

import (
	"strings"
	"testing"
)

func TestTokensScrubsAndDecodes(t *testing.T) {
	// {"alg":"RS256","kid":"k1"} . {"sub":"bob","aud":"mcp-waypoint","groups":["advisors"]} . sig
	raw := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJib2IiLCJhdWQiOiJtY3Atd2F5cG9pbnQiLCJncm91cHMiOlsiYWR2aXNvcnMiXX0.c2ln"
	a := map[string]string{
		"request.headers.authorization": "Bearer " + raw,
		"jwt.sub":                       "bob",
		"jwt.act.sub":                   "kagent",
		"jwt.rawToken":                  "<redacted>",
	}
	ts := tokens(a)
	if len(ts) != 2 {
		t.Fatalf("want 2 tokens, got %d: %+v", len(ts), ts)
	}
	if !ts[0].Verified || ts[0].Claims["sub"] != "bob" || ts[0].Claims["act"].(map[string]any)["sub"] != "kagent" {
		t.Errorf("verified claims: %+v", ts[0])
	}
	if ts[1].Claims["aud"] != "mcp-waypoint" || ts[1].Header["kid"] != "k1" || ts[1].Fingerprint == "" {
		t.Errorf("decoded token: %+v", ts[1])
	}
	for k, v := range a {
		if strings.Contains(v, "eyJ") || strings.HasPrefix(k, "jwt.") {
			t.Errorf("left in the record: %s=%s", k, v)
		}
	}
}
