package assurance

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/open-policy-agent/opa/v1/rego"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

// signed: an RS256 JWT over claims, as the broker signs its tokens.
func signed(t *testing.T, k *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	in := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// What Solo's ext-auth service runs: the same module, the request as input,
// the chain's state from the controller's data module.
func TestExtAuth(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())}}})
	const iss = "https://idp.sterling.lab/realms/sterling-vance"
	now := time.Now()
	state := func(generated time.Time) string {
		s, _ := json.Marshal(map[string]any{
			"issuer": iss, "jwks": string(jwks), "stale_s": 60, "generated_at": generated.Unix(),
			"chain": ChainInput([]v1.Tier{
				{Name: "keycloak", Type: "oidc", Assurance: &v1.TierAssurance{Levels: []v1.AssuranceLevel{{ACR: "aal1", Level: "AAL1"}, {ACR: "aal2", Level: "AAL2"}}}},
				{Name: "contingency", Type: "oidc"}}),
			"current": CurrentInput(Current{Active: "keycloak"}),
			"rules": map[string]any{
				"advisor-workspace":   map[string]any{"mode": "Enforce", "rules": RulesInput(Rules{Minimum: 2})},
				"ledgerline-research": map[string]any{"mode": "Enforce", "rules": RulesInput(Rules{Minimum: 2, ActiveIdPOnly: true})},
				"research-notes":      map[string]any{"mode": "ReportOnly", "rules": RulesInput(Rules{Minimum: 2})},
			},
		})
		return "package assurance_state\n\nstate := " + string(s) + "\n"
	}
	ask := func(rule, token string, generated time.Time) map[string]any {
		t.Helper()
		q, err := rego.New(rego.Query(`data.assurance.extauth["`+rule+`"]`), rego.Module("assurance.rego", Policy),
			rego.Module("state.rego", state(generated))).PrepareForEval(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rs, err := q.Eval(context.Background(), rego.EvalInput(map[string]any{"http_request": map[string]any{
			"headers": map[string]string{"authorization": "Bearer " + token}}}))
		if err != nil || len(rs) == 0 {
			t.Fatalf("%s: %v %v", rule, rs, err)
		}
		return rs[0].Expressions[0].Value.(map[string]any)
	}
	tok := func(acr string, issuer string) string {
		return signed(t, k, map[string]any{"iss": issuer, "sub": "bob", "exp": now.Add(time.Minute).Unix(), "idp": "keycloak", "idp_acr": acr, "idp_amr": "pwd otp"})
	}
	hdr := func(r map[string]any, h string) string {
		s, _ := r["response_headers_to_add"].(map[string]any)[h].(string)
		return s
	}

	if r := ask("advisor-workspace", tok("aal2", iss), now); r["allow"] != true || hdr(r, "x-continuity-decision") != "allow advisor-workspace: AAL2 via keycloak (acr aal2)" ||
		len(r["request_headers_to_remove"].([]any)) != 1 || r["request_headers_to_remove"].([]any)[0] != "authorization" {
		t.Fatalf("AAL2: %v", r)
	}
	r := ask("advisor-workspace", tok("aal1", iss), now)
	if r["allow"] != false || r["http_status"] != json.Number("401") || hdr(r, "www-authenticate") !=
		`Bearer error="insufficient_user_authentication", error_description="assurance AAL1 below AAL2: session from keycloak (acr aal1)", acr_values="aal2"` {
		t.Fatalf("AAL1: %v", r)
	}
	if r := ask("research-notes", tok("aal1", iss), now); r["allow"] != true || hdr(r, "x-continuity-decision")[:10] != "would-deny" {
		t.Fatalf("report only: %v", r)
	}
	if r := ask("ledgerline-research", tok("aal2", iss), now.Add(-5*time.Minute)); r["http_status"] != json.Number("503") {
		t.Fatalf("stale state: %v", r)
	}
	if r := ask("advisor-workspace", tok("aal2", "https://evil.example"), now); r["allow"] != false || r["http_status"] != json.Number("403") {
		t.Fatalf("another issuer: %v", r)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := signed(t, other, map[string]any{"iss": iss, "sub": "bob", "exp": now.Add(time.Minute).Unix(), "idp": "keycloak", "idp_acr": "aal2"})
	if r := ask("advisor-workspace", forged, now); r["allow"] != false {
		t.Fatalf("forged token: %v", r)
	}
}
