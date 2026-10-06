package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	issuer   = "https://op.example"
	rasIssue = "https://ras.example"
)

var now = time.Unix(1_800_000_000, 0)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signRS(t *testing.T, k *rsa.PrivateKey, h, c map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(c)
	in := b64(hb) + "." + b64(cb)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func signES(t *testing.T, k *ecdsa.PrivateKey, h, c map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(c)
	in := b64(hb) + "." + b64(cb)
	d := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, k, d[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + b64(sig)
}

func idjagClaims() map[string]any {
	return map[string]any{"iss": issuer, "sub": "bob-sub", "aud": rasIssue, "client_id": "sv-at-ras",
		"jti": "jag-1", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "scope": "research:read"}
}

// fakeOP serves a JWKS (an RSA and an EC key) and a token endpoint that
// returns whatever token mint produces, recording the request.
type fakeOP struct {
	srv    *httptest.Server
	rsa    *rsa.PrivateKey
	ec     *ecdsa.PrivateKey
	mint   func() string
	form   url.Values
	user   string // client_secret_basic user and password received
	pass   string
	refuse bool
}

func newFakeOP(t *testing.T) *fakeOP {
	t.Helper()
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f := &fakeOP{rsa: rk, ec: ek}
	f.mint = func() string {
		return signRS(t, rk, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, idjagClaims())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": "r1", "use": "sig", "n": b64(rk.N.Bytes()), "e": b64(big.NewInt(int64(rk.E)).Bytes())},
			{"kty": "EC", "kid": "e1", "crv": "P-256", "x": b64(ek.X.FillBytes(make([]byte, 32))), "y": b64(ek.Y.FillBytes(make([]byte, 32)))},
		}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.form = r.PostForm
		f.user, f.pass, _ = r.BasicAuth()
		if f.refuse {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"subject token expired"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.mint(), "issued_token_type": requestedIDJAG, "token_type": "N_A", "expires_in": 300})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newRelay(ops []*op, ras string) *relay {
	for _, o := range ops {
		o.keys = &jwks{client: http.DefaultClient, url: o.JWKSURL}
	}
	return &relay{client: http.DefaultClient, ops: ops, rasTokenURL: ras, audience: rasIssue, rasClientID: "sv-at-ras",
		scopes: map[string]bool{"openid": true, "research:read": true}, now: func() time.Time { return now }}
}

// opOf is S&V's client at the fake OP f, with issuer iss
func opOf(f *fakeOP, name, iss string) *op {
	return &op{Name: name, Issuer: iss, TokenURL: f.srv.URL + "/token", JWKSURL: f.srv.URL + "/jwks", ClientID: "sv-at-" + name, secret: name + "-secret"}
}

// idToken is a subject token from issuer iss (its signature isn't the
// relay's to check: the OP verifies it)
func idToken(iss, jti string) string {
	hb, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	cb, _ := json.Marshal(map[string]any{"iss": iss, "sub": "bob-sub", "jti": jti})
	return b64(hb) + "." + b64(cb) + ".c2ln"
}

func idjagRequest(subject string) url.Values {
	return url.Values{"grant_type": {tokenExchange}, "requested_token_type": {requestedIDJAG},
		"subject_token": {subject}, "subject_token_type": {"urn:ietf:params:oauth:token-type:id_token"},
		"audience": {rasIssue}, "scope": {"openid research:read"}, "client_id": {"kagent"}}
}

func post(h http.Handler, path string, form url.Values, basic bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		r.SetBasicAuth("sv-client", "s3cret")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// capture the JSON log lines of one call
func logs(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	f()
	return buf.String()
}

func TestValidIDJAGPassesThroughAndIsLogged(t *testing.T) {
	f := newFakeOP(t)
	x := newRelay([]*op{opOf(f, "gluu", issuer)}, "http://unused")
	subject := idToken(issuer, "id-1")
	var w *httptest.ResponseRecorder
	out := logs(t, func() { w = post(x.routes(), "/op/token", idjagRequest(subject), false) })
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	// S&V's client at that IdP authenticates; the gateway's client_id is not sent on
	if f.user != "sv-at-gluu" || f.pass != "gluu-secret" || f.form.Has("client_id") || f.form.Get("subject_token") != subject {
		t.Fatalf("relayed as %q/%q, client_id %v, subject intact %v", f.user, f.pass, f.form["client_id"], f.form.Get("subject_token") == subject)
	}
	if !strings.Contains(out, `"msg":"id-jag accepted"`) || !strings.Contains(out, `"jti":"jag-1"`) || !strings.Contains(out, `"idp":"gluu"`) {
		t.Fatalf("log: %s", out)
	}
	for _, leak := range []string{strings.Split(subject, ".")[1], "gluu-secret", strings.Split(f.mint(), ".")[2]} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaks %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"subject_token":"<redacted jwt jti=id-1>"`) || !strings.Contains(out, `"audience":"https://ras.example"`) {
		t.Fatalf("log misses params: %s", out)
	}
}

func TestRoutesByTheSubjectTokensIssuer(t *testing.T) {
	gluu, keycloak := newFakeOP(t), newFakeOP(t)
	keycloak.mint = func() string {
		c := idjagClaims()
		c["iss"] = "https://kc.example"
		return signRS(t, keycloak.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, c)
	}
	x := newRelay([]*op{opOf(gluu, "gluu", issuer), opOf(keycloak, "keycloak", "https://kc.example")}, "")
	if w := post(x.routes(), "/op/token", idjagRequest(idToken("https://kc.example", "k1")), false); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if keycloak.user != "sv-at-keycloak" || gluu.form != nil {
		t.Fatalf("keycloak got %q, gluu called %v", keycloak.user, gluu.form != nil)
	}
	// a subject token from anyone else never leaves
	w := post(x.routes(), "/op/token", idjagRequest(idToken("https://evil.example", "e1")), false)
	if w.Code != http.StatusBadRequest || gluu.form != nil {
		t.Fatalf("unknown issuer: %d, gluu called %v", w.Code, gluu.form != nil)
	}
	// an ID-JAG signed by another IdP's key is rejected, even if it claims the right iss
	gluu.mint = func() string {
		return signRS(t, keycloak.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, idjagClaims())
	}
	if w := post(x.routes(), "/op/token", idjagRequest(idToken(issuer, "g1")), false); w.Code != http.StatusBadGateway {
		t.Fatalf("cross-signed ID-JAG: %d", w.Code)
	}
}

func TestES256IDJAG(t *testing.T) {
	f := newFakeOP(t)
	f.mint = func() string {
		return signES(t, f.ec, map[string]any{"alg": "ES256", "kid": "e1", "typ": typIDJAG}, idjagClaims())
	}
	if w := post(newRelay([]*op{opOf(f, "gluu", issuer)}, "").routes(), "/op/token", idjagRequest(idToken(issuer, "t")), false); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestInvalidIDJAGRejected(t *testing.T) {
	cases := map[string]func(f *fakeOP) string{
		"typ": func(f *fakeOP) string {
			return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": "JWT"}, idjagClaims())
		},
		"iss": func(f *fakeOP) string {
			c := idjagClaims()
			c["iss"] = "https://evil.example"
			return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, c)
		},
		"aud": func(f *fakeOP) string {
			c := idjagClaims()
			c["aud"] = "https://other-ras.example"
			return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, c)
		},
		"client_id": func(f *fakeOP) string {
			c := idjagClaims()
			delete(c, "client_id")
			return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, c)
		},
		"sub": func(f *fakeOP) string {
			c := idjagClaims()
			delete(c, "sub")
			return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, c)
		},
		"expired": func(f *fakeOP) string {
			c := idjagClaims()
			c["exp"] = now.Add(-time.Minute).Unix()
			return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, c)
		},
		"signature": func(f *fakeOP) string {
			other, _ := rsa.GenerateKey(rand.Reader, 2048)
			return signRS(t, other, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, idjagClaims())
		},
		"alg none": func(f *fakeOP) string {
			hb, _ := json.Marshal(map[string]any{"alg": "none", "typ": typIDJAG})
			cb, _ := json.Marshal(idjagClaims())
			return b64(hb) + "." + b64(cb) + "."
		},
		"unknown kid": func(f *fakeOP) string {
			return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "nope", "typ": typIDJAG}, idjagClaims())
		},
	}
	for name, mint := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeOP(t)
			f.mint = func() string { return mint(f) }
			var w *httptest.ResponseRecorder
			out := logs(t, func() {
				w = post(newRelay([]*op{opOf(f, "gluu", issuer)}, "").routes(), "/op/token", idjagRequest(idToken(issuer, "t")), false)
			})
			if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "access_token") {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if !strings.Contains(out, `"msg":"id-jag rejected"`) {
				t.Fatalf("log: %s", out)
			}
		})
	}
}

func TestOPRefusalPassedOn(t *testing.T) {
	f := newFakeOP(t)
	f.refuse = true
	var w *httptest.ResponseRecorder
	out := logs(t, func() {
		w = post(newRelay([]*op{opOf(f, "gluu", issuer)}, "").routes(), "/op/token", idjagRequest(idToken(issuer, "t")), false)
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(out, `"error":"invalid_grant"`) {
		t.Fatalf("log: %s", out)
	}
}

func TestRASLegRelaysClientAuthAndLogs(t *testing.T) {
	f := newFakeOP(t)
	jag := f.mint()
	at := signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": "at+jwt"},
		map[string]any{"iss": rasIssue, "sub": "bob-at-ras", "aud": "ledgerline-research", "client_id": "sv-at-ras", "jti": "at-1", "scope": "research:read"})
	assertion := signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "c1"}, map[string]any{"iss": "sv-at-ras", "sub": "sv-at-ras", "aud": "https://ras.example/token", "jti": "ca-1"})
	var got url.Values
	ras := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 300, "scope": "research:read"})
	}))
	defer ras.Close()
	var w *httptest.ResponseRecorder
	out := logs(t, func() {
		w = post(newRelay(nil, ras.URL).routes(), "/ras/token", url.Values{"grant_type": {jwtBearer}, "assertion": {jag},
			"client_id": {"sv-at-ras"}, "client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			"client_assertion": {assertion}, "resource": {"https://mcp.example/mcp"}}, false)
	})
	if w.Code != http.StatusOK || got.Get("assertion") != jag || got.Get("client_assertion") != assertion {
		t.Fatalf("status %d; assertion and client assertion relayed as sent: %v %v", w.Code, got.Get("assertion") == jag, got.Get("client_assertion") == assertion)
	}
	for _, want := range []string{`"msg":"access token issued"`, `"assertion":"<redacted jwt jti=jag-1>"`, `"jti":"at-1"`,
		`"resource":"https://mcp.example/mcp"`, `"method":"private_key_jwt"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log misses %s: %s", want, out)
		}
	}
	for _, tok := range []string{at, jag, assertion} {
		if strings.Contains(out, strings.Split(tok, ".")[2]) {
			t.Fatalf("log leaks a token: %s", out)
		}
	}
}

func TestOPLegWithPrivateKeyJWT(t *testing.T) {
	f := newFakeOP(t)
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	o := opOf(f, "gluu", issuer)
	o.secret, o.Auth, o.key = "", "private_key_jwt", &clientKey{key: k, kid: thumbprint(&k.PublicKey)}
	if w := post(newRelay([]*op{o}, "").routes(), "/op/token", idjagRequest(idToken(issuer, "t")), false); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	a := f.form.Get("client_assertion")
	if f.user != "" || f.form.Get("client_id") != "sv-at-gluu" || f.form.Get("client_assertion_type") != assertionType || a == "" {
		t.Fatalf("basic %q, form %v", f.user, f.form)
	}
	h, c := split(a)
	if h["kid"] != o.key.kid || c["iss"] != "sv-at-gluu" || c["aud"] != o.Issuer {
		t.Fatalf("assertion header %v claims %v", h, c)
	}
}

func TestOnlyThisConnectionsRequestIsRelayed(t *testing.T) {
	cases := map[string]func(url.Values){
		"another grant":        func(f url.Values) { f.Set("grant_type", "refresh_token") },
		"another token type":   func(f url.Values) { f.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token") },
		"access token subject": func(f url.Values) { f.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token") },
		"another audience":     func(f url.Values) { f.Set("audience", "https://elsewhere.example") },
		"no audience":          func(f url.Values) { f.Del("audience") },
		"a scope not allowed":  func(f url.Values) { f.Set("scope", "openid admin") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeOP(t)
			req := idjagRequest(idToken(issuer, "t"))
			change(req)
			if w := post(newRelay([]*op{opOf(f, "gluu", issuer)}, "").routes(), "/op/token", req, false); w.Code != http.StatusBadRequest || f.form != nil {
				t.Fatalf("status %d, reached the IdP %v", w.Code, f.form != nil)
			}
		})
	}
}

func TestIDJAGMustMatchSubjectClientAndLifetime(t *testing.T) {
	cases := map[string]func(map[string]any){
		"another subject":   func(c map[string]any) { c["sub"] = "mallory" },
		"another client_id": func(c map[string]any) { c["client_id"] = "someone-else" },
		"too long-lived":    func(c map[string]any) { c["exp"] = now.Add(time.Hour).Unix() },
		"no iat":            func(c map[string]any) { delete(c, "iat") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeOP(t)
			f.mint = func() string {
				c := idjagClaims()
				change(c)
				return signRS(t, f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": typIDJAG}, c)
			}
			if w := post(newRelay([]*op{opOf(f, "gluu", issuer)}, "").routes(), "/op/token", idjagRequest(idToken(issuer, "t")), false); w.Code != http.StatusBadGateway {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
		})
	}
}

func TestRASLegOnlyJWTGrant(t *testing.T) {
	called := false
	ras := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer ras.Close()
	w := post(newRelay(nil, ras.URL).routes(), "/ras/token", url.Values{"grant_type": {"client_credentials"}}, false)
	if w.Code != http.StatusBadRequest || called {
		t.Fatalf("status %d, forwarded %v", w.Code, called)
	}
}
