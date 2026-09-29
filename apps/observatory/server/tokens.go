package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// A JWT anywhere in a log record: header.payload.signature, base64url.
var jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)

// Credentials that aren't JWTs but ride in URLs (a logged path with its
// query, a redirect's fragment): authorization codes, opaque tokens, secrets.
var credParamRe = regexp.MustCompile(`(?i)([?&#;](?:code|access_token|refresh_token|id_token|token|subject_token|actor_token|assertion|client_secret|code_verifier|password|session_state)=)[^&#\s"';]*`)

// tokens pulls the credentials out of an access-log record:
//
//   - claims the gateway verified (agentgateway's `jwt`, logged as a map or
//     as JSON, flattened to jwt.* attributes)
//   - any raw JWT that made it into an attribute (a logged header): decoded,
//     then replaced in the record by its fingerprint, so a bearer token is
//     never stored or sent to the browser
func tokens(a map[string]string) []Token {
	var out []Token
	if claims := verifiedClaims(a); len(claims) > 0 {
		out = append(out, Token{Source: "verified by the gateway's JWT policy", Verified: true, Claims: claims})
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a[k] = jwtRe.ReplaceAllStringFunc(a[k], func(raw string) string {
			t, ok := decodeJWT(raw)
			if !ok {
				return "‹token›"
			}
			t.Source = "seen in " + k
			if !seen(out, t.Fingerprint) {
				out = append(out, t)
			}
			return "‹token " + t.Fingerprint + "›"
		})
		// then opaque credentials in URLs; a JWT there is already a fingerprint
		a[k] = credParamRe.ReplaceAllStringFunc(a[k], func(m string) string {
			name, val, _ := strings.Cut(m, "=")
			if strings.HasPrefix(val, "‹") {
				return m
			}
			return name + "=‹redacted›"
		})
	}
	return out
}

func seen(ts []Token, fp string) bool {
	for _, t := range ts {
		if t.Fingerprint == fp {
			return true
		}
	}
	return false
}

// verifiedClaims rebuilds the claims map from jwt.* attributes (or a jwt
// attribute holding JSON), and removes them from the record.
func verifiedClaims(a map[string]string) map[string]any {
	claims := map[string]any{}
	if v := a["jwt"]; strings.HasPrefix(v, "{") {
		if json.Unmarshal([]byte(v), &claims) == nil {
			delete(a, "jwt")
		}
	}
	for k, v := range a {
		if !strings.HasPrefix(k, "jwt.") {
			continue
		}
		name := strings.TrimPrefix(k, "jwt.")
		if name == "rawToken" { // redacted by agentgateway; not a claim
			delete(a, k)
			continue
		}
		setPath(claims, strings.Split(name, "."), v)
		delete(a, k)
	}
	return claims
}

func setPath(m map[string]any, path []string, v string) {
	for len(path) > 1 {
		next, ok := m[path[0]].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[path[0]] = next
		}
		m, path = next, path[1:]
	}
	m[path[0]] = v
}

func decodeJWT(raw string) (Token, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Token{}, false
	}
	var h, c map[string]any
	if !part(parts[0], &h) || !part(parts[1], &c) {
		return Token{}, false
	}
	sum := sha256.Sum256([]byte(raw))
	return Token{Fingerprint: "sha256:" + hex.EncodeToString(sum[:6]), Header: h, Claims: c}, true
}

func part(s string, v any) bool {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	return err == nil && json.Unmarshal(b, v) == nil
}
