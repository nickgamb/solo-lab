// xaa-relay: S&V's egress client for the token requests agentgateway's
// crossAppAccess makes (demos/bob/manifests/40-xaa-ledgerline.yaml).
//
//	POST /op/token    ID token -> ID-JAG (RFC 8693) at the enterprise IdP
//	                  that issued the subject token (OPS, by issuer), as S&V's
//	                  client there: the gateway names the requesting app and
//	                  holds no IdP credential. The ID-JAG is checked before
//	                  the gateway uses it: typ oauth-id-jag+jwt, signature
//	                  (that IdP's JWKS), iss, aud the requested audience, sub,
//	                  client_id, exp. A failed check is a 502 to the gateway.
//	POST /ras/token   ID-JAG -> access token (RFC 7523) at Ledgerline's
//	                  authorization server (RAS_TOKEN_URL), relayed as sent:
//	                  the gateway authenticates with private_key_jwt.
//
// Each request is logged with its parameters; credential values (tokens,
// assertions, secrets) are replaced by <redacted>, with a JWT's jti kept.
// Responses are logged by their claims, never the tokens.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	typIDJAG       = "oauth-id-jag+jwt"
	tokenExchange  = "urn:ietf:params:oauth:grant-type:token-exchange"
	jwtBearer      = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	requestedIDJAG = "urn:ietf:params:oauth:token-type:id-jag"
	maxBody        = 1 << 20
)

// secret form parameters: logged as <redacted>, a JWT's jti kept
var secretParams = map[string]bool{
	"subject_token": true, "actor_token": true, "assertion": true, "client_secret": true,
	"client_assertion": true, "refresh_token": true, "code": true, "code_verifier": true, "password": true,
}

// op is an enterprise IdP and S&V's client there.
type op struct {
	Name     string `json:"name"`
	Issuer   string `json:"issuer"`
	TokenURL string `json:"token_url"`
	JWKSURL  string `json:"jwks_url"`
	ClientID string `json:"client_id"`
	secret   string
	keys     *jwks
}

type relay struct {
	client      *http.Client
	ops         []*op
	rasTokenURL string
	now         func() time.Time
}

// opFor is the IdP that issued the subject token (matched by issuer).
func (x *relay) opFor(subjectToken string) *op {
	_, c := split(subjectToken)
	iss, _ := c["iss"].(string)
	for _, o := range x.ops {
		if iss != "" && iss == o.Issuer {
			return o
		}
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (x *relay) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /op/token", x.op)
	mux.HandleFunc("POST /ras/token", x.ras)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// readForm reads the request's form body.
func readForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return nil, false
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "form", http.StatusBadRequest)
		return nil, false
	}
	return form, true
}

// forward POSTs form to upstream. With o, it authenticates as S&V's client at
// o (client_secret_basic) in place of whatever the caller sent; without, the
// caller's client authentication passes through. It returns the response
// status and body.
func (x *relay) forward(w http.ResponseWriter, r *http.Request, upstream string, form url.Values, o *op) (int, []byte, bool) {
	send := url.Values{}
	for k, v := range form {
		send[k] = v
	}
	if o != nil {
		for _, k := range []string{"client_id", "client_secret", "client_assertion", "client_assertion_type"} {
			send.Del(k)
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, strings.NewReader(send.Encode()))
	if err != nil {
		http.Error(w, "upstream", http.StatusInternalServerError)
		return 0, nil, false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, h := range []string{"Accept", "DPoP"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if o != nil {
		req.SetBasicAuth(url.QueryEscape(o.ClientID), url.QueryEscape(o.secret))
	} else if v := r.Header.Get("Authorization"); v != "" {
		req.Header.Set("Authorization", v)
	}
	resp, err := x.client.Do(req)
	if err != nil {
		slog.Warn("upstream unreachable", "leg", leg(r), "url", upstream, "err", err)
		writeErr(w, http.StatusBadGateway, "temporarily_unavailable", "token endpoint unreachable")
		return 0, nil, false
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, out, true
}

func leg(r *http.Request) string {
	return strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/token")
}

// op relays the ID-JAG request and checks the ID-JAG before passing it on.
func (x *relay) op(w http.ResponseWriter, r *http.Request) {
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	o := x.opFor(form.Get("subject_token"))
	if o == nil {
		slog.Warn("token request refused", "leg", "op", "params", redact(form), "reason", "subject token not from an enterprise IdP")
		writeErr(w, http.StatusBadRequest, "invalid_request", "subject token not issued by an enterprise IdP")
		return
	}
	status, body, ok := x.forward(w, r, o.TokenURL, form, o)
	if !ok {
		return
	}
	attrs := []any{"leg", "op", "idp", o.Name, "url", o.TokenURL, "client", map[string]string{"client_id": o.ClientID, "method": "client_secret_basic"},
		"params", redact(form), "status", status}
	if status != http.StatusOK {
		slog.Warn("token request refused", append(attrs, "error", oauthError(body))...)
		pass(w, status, body)
		return
	}
	var tr struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
		TokenType       string `json:"token_type"`
		ExpiresIn       int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		slog.Warn("id-jag rejected", append(attrs, "reason", "no access_token in the response")...)
		writeErr(w, http.StatusBadGateway, "invalid_response", "no ID-JAG in the response")
		return
	}
	attrs = append(attrs, "issued_token_type", tr.IssuedTokenType, "token_type", tr.TokenType, "expires_in", tr.ExpiresIn)
	if form.Get("grant_type") == tokenExchange && form.Get("requested_token_type") == requestedIDJAG {
		h, c, err := x.checkIDJAG(r.Context(), o, tr.AccessToken, form.Get("audience"))
		attrs = append(attrs, "header", h, "claims", c)
		if err != nil {
			slog.Warn("id-jag rejected", append(attrs, "reason", err.Error())...)
			writeErr(w, http.StatusBadGateway, "invalid_response", "ID-JAG failed verification")
			return
		}
		slog.Info("id-jag accepted", attrs...)
	} else {
		slog.Info("token issued", attrs...)
	}
	pass(w, status, body)
}

// ras relays the JWT authorization grant and records the access token issued.
func (x *relay) ras(w http.ResponseWriter, r *http.Request) {
	form, ok := readForm(w, r)
	if !ok {
		return
	}
	status, body, ok := x.forward(w, r, x.rasTokenURL, form, nil)
	if !ok {
		return
	}
	attrs := []any{"leg", "ras", "url", x.rasTokenURL, "client", clientAuth(r, form), "params", redact(form), "status", status}
	if form.Get("grant_type") == jwtBearer {
		_, c := split(form.Get("assertion"))
		attrs = append(attrs, "assertion", pick(c, "iss", "sub", "aud", "client_id", "jti", "exp"))
	}
	if status != http.StatusOK {
		slog.Warn("token request refused", append(attrs, "error", oauthError(body))...)
		pass(w, status, body)
		return
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	_ = json.Unmarshal(body, &tr)
	_, c := split(tr.AccessToken)
	slog.Info("access token issued", append(attrs, "token_type", tr.TokenType, "expires_in", tr.ExpiresIn, "scope", tr.Scope,
		"claims", pick(c, "iss", "sub", "aud", "client_id", "azp", "scope", "jti", "iat", "exp", "cnf"))...)
	pass(w, status, body)
}

// checkIDJAG verifies an ID-JAG from o as the requesting side (ID-JAG
// draft): typ, signature, iss, aud, sub, client_id, exp. It returns the header and claims
// for the log either way.
func (x *relay) checkIDJAG(ctx context.Context, o *op, tok, audience string) (map[string]any, map[string]any, error) {
	h, c := split(tok)
	hc := pick(h, "typ", "alg", "kid")
	cc := pick(c, "iss", "sub", "aud", "client_id", "jti", "iat", "exp", "scope", "resource", "auth_time", "acr", "amr")
	if h == nil || c == nil {
		return hc, cc, errors.New("not a JWT")
	}
	if typ, _ := h["typ"].(string); !strings.EqualFold(typ, typIDJAG) && !strings.EqualFold(typ, "application/"+typIDJAG) {
		return hc, cc, fmt.Errorf("typ %q, want %s", typ, typIDJAG)
	}
	if err := o.keys.verify(ctx, tok, h); err != nil {
		return hc, cc, fmt.Errorf("signature: %w", err)
	}
	if iss, _ := c["iss"].(string); iss != o.Issuer {
		return hc, cc, fmt.Errorf("iss %q, want %q", iss, o.Issuer)
	}
	if audience != "" && !hasAud(c["aud"], audience) {
		return hc, cc, fmt.Errorf("aud %v, want %q", c["aud"], audience)
	}
	if sub, _ := c["sub"].(string); sub == "" {
		return hc, cc, errors.New("no sub")
	}
	if cid, _ := c["client_id"].(string); cid == "" {
		return hc, cc, errors.New("no client_id")
	}
	exp, _ := c["exp"].(float64)
	if exp == 0 || x.now().After(time.Unix(int64(exp), 0).Add(5*time.Second)) {
		return hc, cc, errors.New("expired")
	}
	return hc, cc, nil
}

func hasAud(aud any, want string) bool {
	switch a := aud.(type) {
	case string:
		return a == want
	case []any:
		for _, v := range a {
			if s, _ := v.(string); s == want {
				return true
			}
		}
	}
	return false
}

// split decodes a JWT's header and claims without verifying them (nil, nil
// if it isn't one).
func split(tok string) (h, c map[string]any) {
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return nil, nil
	}
	dec := func(s string, v *map[string]any) {
		if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
			_ = json.Unmarshal(b, v)
		}
	}
	dec(p[0], &h)
	dec(p[1], &c)
	return h, c
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// redact returns the form for the log: credential values replaced by
// <redacted> (with a JWT's jti, which correlates it with the issuer's log).
func redact(f url.Values) map[string]string {
	out := map[string]string{}
	for k, vs := range f {
		v := strings.Join(vs, " ")
		if secretParams[k] {
			v = "<redacted>"
			if _, c := split(vs[0]); c != nil {
				if jti, _ := c["jti"].(string); jti != "" {
					v = "<redacted jwt jti=" + jti + ">"
				}
			}
		}
		out[k] = v
	}
	return out
}

// clientAuth names the client and how it authenticated, never the secret.
func clientAuth(r *http.Request, f url.Values) map[string]string {
	if id, _, ok := r.BasicAuth(); ok {
		if u, err := url.QueryUnescape(id); err == nil {
			id = u
		}
		return map[string]string{"client_id": id, "method": "client_secret_basic"}
	}
	switch {
	case f.Get("client_assertion") != "":
		_, c := split(f.Get("client_assertion"))
		id, _ := c["sub"].(string)
		return map[string]string{"client_id": id, "method": "private_key_jwt"}
	case f.Get("client_secret") != "":
		return map[string]string{"client_id": f.Get("client_id"), "method": "client_secret_post"}
	}
	return map[string]string{"client_id": f.Get("client_id"), "method": "none"}
}

func oauthError(body []byte) map[string]any {
	var e map[string]any
	if json.Unmarshal(body, &e) != nil {
		return map[string]any{"body_bytes": len(body)}
	}
	return pick(e, "error", "error_description")
}

func pass(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeErr(w http.ResponseWriter, status int, code, desc string) {
	b, _ := json.Marshal(map[string]string{"error": code, "error_description": desc})
	pass(w, status, b)
}

// ---- JWKS ------------------------------------------------------------------

type jwks struct {
	client *http.Client
	url    string
	mu     sync.Mutex
	keys   map[string]crypto.PublicKey
	at     time.Time
}

func (k *jwks) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if pk, ok := k.keys[kid]; ok && time.Since(k.at) < 5*time.Minute {
		return pk, nil
	}
	// unknown kid or stale: refetch, at most every 10 s
	if time.Since(k.at) > 10*time.Second || k.keys == nil {
		if err := k.fetch(ctx); err != nil {
			return nil, err
		}
	}
	if pk, ok := k.keys[kid]; ok {
		return pk, nil
	}
	if kid == "" && len(k.keys) == 1 {
		for _, pk := range k.keys {
			return pk, nil
		}
	}
	return nil, fmt.Errorf("no key %q at %s", kid, k.url)
}

func (k *jwks) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty, Kid, Use, Crv, N, E, X, Y string
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&set); err != nil {
		return err
	}
	keys := map[string]crypto.PublicKey{}
	for _, j := range set.Keys {
		if j.Use != "" && j.Use != "sig" {
			continue
		}
		switch j.Kty {
		case "RSA":
			n, e := b64int(j.N), b64int(j.E)
			if n != nil && e != nil {
				keys[j.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
			}
		case "EC":
			var c elliptic.Curve
			switch j.Crv {
			case "P-256":
				c = elliptic.P256()
			case "P-384":
				c = elliptic.P384()
			case "P-521":
				c = elliptic.P521()
			}
			x, y := b64int(j.X), b64int(j.Y)
			if c != nil && x != nil && y != nil {
				keys[j.Kid] = &ecdsa.PublicKey{Curve: c, X: x, Y: y}
			}
		}
	}
	k.keys, k.at = keys, time.Now()
	return nil
}

func b64int(s string) *big.Int {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil
	}
	return new(big.Int).SetBytes(b)
}

// verify checks a JWS compact token's signature: RS*, PS*, ES* only.
func (k *jwks) verify(ctx context.Context, tok string, h map[string]any) error {
	alg, _ := h["alg"].(string)
	kid, _ := h["kid"].(string)
	if len(alg) != 5 {
		return fmt.Errorf("alg %q", alg)
	}
	var hf func() hash.Hash
	var ch crypto.Hash
	switch alg[2:] {
	case "256":
		hf, ch = sha256.New, crypto.SHA256
	case "384":
		hf, ch = sha512.New384, crypto.SHA384
	case "512":
		hf, ch = sha512.New, crypto.SHA512
	default:
		return fmt.Errorf("alg %q", alg)
	}
	p := strings.Split(tok, ".")
	sig, err := base64.RawURLEncoding.DecodeString(p[2])
	if err != nil {
		return err
	}
	hh := hf()
	hh.Write([]byte(p[0] + "." + p[1]))
	digest := hh.Sum(nil)
	pk, err := k.key(ctx, kid)
	if err != nil {
		return err
	}
	switch alg[:2] {
	case "RS":
		if rk, ok := pk.(*rsa.PublicKey); ok {
			return rsa.VerifyPKCS1v15(rk, ch, digest, sig)
		}
	case "PS":
		if rk, ok := pk.(*rsa.PublicKey); ok {
			return rsa.VerifyPSS(rk, ch, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		}
	case "ES":
		if ek, ok := pk.(*ecdsa.PublicKey); ok {
			n := (ek.Curve.Params().BitSize + 7) / 8
			if len(sig) != 2*n {
				return errors.New("bad ES signature length")
			}
			if ecdsa.Verify(ek, digest, new(big.Int).SetBytes(sig[:n]), new(big.Int).SetBytes(sig[n:])) {
				return nil
			}
			return errors.New("bad signature")
		}
	default:
		return fmt.Errorf("alg %q", alg)
	}
	return fmt.Errorf("key %q doesn't fit alg %s", kid, alg)
}

// loadOPs reads OPS (JSON) and each client secret from dir/<name>.
func loadOPs(raw, dir string, client *http.Client) ([]*op, error) {
	var ops []*op
	if err := json.Unmarshal([]byte(raw), &ops); err != nil || len(ops) == 0 {
		return nil, fmt.Errorf("OPS: a JSON list of enterprise IdPs: %v", err)
	}
	for i, o := range ops {
		if o.Name == "" || o.Issuer == "" || o.TokenURL == "" || o.JWKSURL == "" || o.ClientID == "" {
			return nil, fmt.Errorf("OPS[%d]: name, issuer, token_url, jwks_url and client_id are required", i)
		}
		s, err := os.ReadFile(dir + "/" + o.Name)
		if err != nil {
			return nil, fmt.Errorf("client secret for %s: %w", o.Name, err)
		}
		o.secret = strings.TrimSpace(string(s))
		o.keys = &jwks{client: client, url: o.JWKSURL}
	}
	return ops, nil
}

// httpClient trusts the system roots plus CA_FILE (the lab CA, for parties
// reached through the edge).
func httpClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", caFile)
		}
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Timeout: 15 * time.Second, Transport: t}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	client, err := httpClient(os.Getenv("CA_FILE"))
	if err != nil {
		slog.Error("CA_FILE", "err", err)
		os.Exit(2)
	}
	ops, err := loadOPs(os.Getenv("OPS"), env("SECRETS_DIR", "/var/run/secrets/op"), client)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(2)
	}
	x := &relay{client: client, ops: ops, rasTokenURL: os.Getenv("RAS_TOKEN_URL"), now: time.Now}
	if x.rasTokenURL == "" {
		slog.Error("RAS_TOKEN_URL is required")
		os.Exit(2)
	}
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: x.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		names := []string{}
		for _, o := range ops {
			names = append(names, o.Name+"="+o.Issuer)
		}
		slog.Info("listening", "addr", srv.Addr, "ops", names, "ras_token_url", x.rasTokenURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}
