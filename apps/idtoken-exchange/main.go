// idtoken-exchange: agentgateway's external authorization step for Cross App
// Access. Given the caller's verified access token from S&V's Keycloak, it
// returns an OIDC ID token for the same user from the enterprise IdP that
// vouches for them, which the gateway then trades for an ID-JAG
// (demos/bob/manifests/40-xaa-ledgerline.yaml).
//
// It exists because the agent doesn't have the ID token: kagent passes agents
// the caller's access token, not the ID token. And agentgateway's own token
// exchange can't fetch it: it requires token_type Bearer in the response,
// while RFC 8693 (2.2.1) has an IdP return N_A for a token that isn't an
// access token, as Keycloak does for an ID token.
//
// Which IdP: the one S&V's identity continuity has active (CONTINUITY, the
// IdentityContinuity's status.active), when it is an upstream that issues
// ID-JAGs (OPS, scripts/idp.sh xaa_env); otherwise S&V's Keycloak. The
// controller alone decides failover: an upstream that isn't active is never
// called, and an active one that fails fails the call until the controller
// moves on.
//
//	upstream  S&V's Keycloak brokers the user's sign-in there and keeps their
//	          upstream tokens. They are read through Keycloak's Identity
//	          Brokering API v2 as the requesting app (kagent, allowed for
//	          that upstream only) with the user's own access token, and the
//	          ID token is renewed at the upstream with the refresh token, so
//	          the upstream decides on every renewal. A user with no account
//	          there gets S&V's Keycloak; an upstream that refuses (revoked,
//	          disabled) refuses the call.
//	keycloak  RFC 8693 at S&V's Keycloak, as the requesting app:
//	            subject_token=<access token>  subject_token_type=...:access_token
//	            requested_token_type=...:id_token  scope=openid
//	          Keycloak issues it only for a live session of that user and
//	          client, so the ID token belongs to the same sign-in.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	tokenExchange   = "urn:ietf:params:oauth:grant-type:token-exchange"
	typeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	typeIDToken     = "urn:ietf:params:oauth:token-type:id_token"
	// how long an upstream ID token is reused before the upstream is asked again
	upstreamTTL = time.Minute
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// op is an OpenID Provider and S&V's client there.
type op struct {
	Name     string `json:"name"`
	TokenURL string `json:"token_url"`
	ClientID string `json:"client_id"`
	secret   string
}

// exchanger turns access tokens into ID tokens, caching each for a while.
type exchanger struct {
	keycloak  op   // S&V's Keycloak: the floor, and the broker
	upstreams []op // ID-JAG-issuing upstreams, in failover order
	brokerURL string
	hc        *http.Client
	now       func() time.Time

	mu     sync.Mutex
	active string // the active tier; "" until known
	cache  map[[32]byte]cached
	// the newest refresh token per upstream user, for upstreams that rotate
	// them (the broker keeps only the one from sign-in)
	refresh map[string]string
}

type cached struct {
	idToken string
	exp     time.Time
}

var (
	// errRefused: an IdP declined (bad or expired subject token, no live
	// session, revoked or disabled upstream account): the caller is refused.
	errRefused = errors.New("refused")
	// errNotLinked: the user has no stored tokens at that upstream.
	errNotLinked = errors.New("no account at the upstream")
	// errUnavailable: the upstream didn't answer, or answered 5xx.
	errUnavailable = errors.New("upstream unavailable")
	// errNoActive: the active tier isn't known yet.
	errNoActive = errors.New("active tier unknown")
)

// setActive records the active tier; a change drops every cached ID token,
// so nothing from the previous IdP is handed out after failover.
func (x *exchanger) setActive(tier string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if tier == x.active {
		return
	}
	slog.Info("active tier", "from", x.active, "to", tier)
	x.active = tier
	x.cache = map[[32]byte]cached{}
}

func (x *exchanger) idToken(ctx context.Context, accessToken string) (id, source string, err error) {
	key := sha256.Sum256([]byte(accessToken))
	now := x.now()
	x.mu.Lock()
	active := x.active
	if active == "" {
		x.mu.Unlock()
		return "", "", errNoActive
	}
	if c, ok := x.cache[key]; ok && now.Before(c.exp) {
		x.mu.Unlock()
		return c.idToken, payload(c.idToken).Iss, nil
	}
	for k, c := range x.cache { // small and short-lived: prune on the way
		if !now.Before(c.exp) {
			delete(x.cache, k)
		}
	}
	x.mu.Unlock()

	source, ttl := x.keycloak.Name, time.Duration(0)
	for _, u := range x.upstreams {
		if u.Name != active {
			continue
		}
		id, err = x.fromUpstream(ctx, accessToken, u)
		switch {
		case err == nil:
			source, ttl = u.Name, upstreamTTL
		case errors.Is(err, errNotLinked): // no account there: S&V's Keycloak
		default:
			return "", u.Name, err
		}
	}
	if id == "" {
		if id, err = x.exchange(ctx, accessToken); err != nil {
			return "", source, err
		}
	}
	// cache until the earlier of the two expiries, less a little slack, and
	// an upstream's for no longer than upstreamTTL
	exp := earliest(expiry(id), expiry(accessToken)).Add(-10 * time.Second)
	if ttl > 0 {
		exp = earliest(exp, now.Add(ttl))
	}
	if exp.After(now) {
		x.mu.Lock()
		if x.active == active { // not if failover happened meanwhile
			x.cache[key] = cached{idToken: id, exp: exp}
		}
		x.mu.Unlock()
	}
	return id, source, nil
}

// exchange gets the ID token from S&V's Keycloak (RFC 8693).
func (x *exchanger) exchange(ctx context.Context, accessToken string) (string, error) {
	var out struct {
		AccessToken     string `json:"access_token"` // RFC 8693 carries any issued token here
		IssuedTokenType string `json:"issued_token_type"`
	}
	if err := x.tokenRequest(ctx, x.keycloak, url.Values{
		"grant_type":           {tokenExchange},
		"subject_token":        {accessToken},
		"subject_token_type":   {typeAccessToken},
		"requested_token_type": {typeIDToken},
		"scope":                {"openid"},
	}, &out); err != nil {
		return "", err
	}
	if out.IssuedTokenType != typeIDToken || out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint issued %q, not an ID token", out.IssuedTokenType)
	}
	return out.AccessToken, nil
}

// fromUpstream reads the user's tokens from upstream u out of S&V's Keycloak
// and renews the ID token there.
func (x *exchanger) fromUpstream(ctx context.Context, accessToken string, u op) (string, error) {
	form := url.Values{"token": {accessToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.brokerURL+"/"+url.PathEscape(u.Name)+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(x.keycloak.ClientID), url.QueryEscape(x.keycloak.secret))
	resp, err := x.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("broker: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return "", fmt.Errorf("%w: %s: %s", errNotLinked, u.Name, oauthError(body))
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("broker: HTTP %d: %s", resp.StatusCode, oauthError(body))
	}
	var stored struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &stored); err != nil {
		return "", fmt.Errorf("broker: %w", err)
	}
	if stored.RefreshToken == "" {
		return "", fmt.Errorf("%w: %s: no refresh token stored (offline_access)", errRefused, u.Name)
	}
	key := u.Name + "|" + subject(stored.IDToken)
	x.mu.Lock()
	rt := x.refresh[key]
	x.mu.Unlock()
	if rt == "" {
		rt = stored.RefreshToken
	}
	id, next, err := x.renew(ctx, u, rt)
	if errors.Is(err, errRefused) && rt != stored.RefreshToken {
		id, next, err = x.renew(ctx, u, stored.RefreshToken) // a newer sign-in replaced it
	}
	if err != nil {
		return "", err
	}
	if next != "" {
		x.mu.Lock()
		x.refresh[key] = next
		x.mu.Unlock()
	}
	return id, nil
}

// renew trades a refresh token for a fresh ID token at upstream u.
func (x *exchanger) renew(ctx context.Context, u op, refreshToken string) (idToken, next string, err error) {
	var out struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := x.tokenRequest(ctx, u, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid"},
	}, &out); err != nil {
		return "", "", err
	}
	if out.IDToken == "" {
		return "", "", fmt.Errorf("%s: refresh returned no ID token", u.Name)
	}
	return out.IDToken, out.RefreshToken, nil
}

// tokenRequest POSTs a form to o's token endpoint as S&V's client there. A
// 400, 401 or 403 is the IdP refusing (errRefused); no answer or a 5xx is
// errUnavailable.
func (x *exchanger) tokenRequest(ctx context.Context, o op, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(o.ClientID), url.QueryEscape(o.secret))
	resp, err := x.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", errUnavailable, o.Name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s: %s", errRefused, o.Name, oauthError(body))
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s: HTTP %d", errUnavailable, o.Name, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s: HTTP %d", o.Name, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: %w", o.Name, err)
	}
	return nil
}

func oauthError(body []byte) string {
	var e struct {
		Error string `json:"error"`
		Desc  string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &e)
	return strings.TrimSpace(e.Error + " " + e.Desc)
}

// payload reads a JWT's claims without verifying them: the IdP verifies the
// subject token, and the gateway verifies the ID token it passes on.
func payload(jwt string) (c struct {
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
	Iss string `json:"iss"`
	Aud any    `json:"aud"`
	Jti string `json:"jti"`
}) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return c
	}
	if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func expiry(jwt string) time.Time {
	if e := payload(jwt).Exp; e != 0 {
		return time.Unix(e, 0)
	}
	return time.Time{}
}

func subject(jwt string) string { return payload(jwt).Sub }

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// ServeHTTP is agentgateway's HTTP ext-auth check: 200 with x-id-token, which
// the gateway copies onto the request (replacing any the caller sent), or 403.
func (x *exchanger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.Header.Get("x-subject-token"))
	if tok == "" {
		http.Error(w, "no subject token", http.StatusForbidden)
		return
	}
	id, source, err := x.idToken(r.Context(), tok)
	switch {
	case errors.Is(err, errNoActive):
		slog.Warn("failed", "err", err)
		http.Error(w, "exchange failed", http.StatusServiceUnavailable)
		return
	case errors.Is(err, errRefused):
		slog.Info("refused", "idp", source, "err", err)
		http.Error(w, "refused", http.StatusForbidden)
		return
	case err != nil:
		slog.Warn("failed", "idp", source, "err", err)
		http.Error(w, "exchange failed", http.StatusServiceUnavailable)
		return
	}
	// the ID token's claims, never the token: the trail for the ID-JAG request
	c := payload(id)
	slog.Info("id token", "iss", c.Iss, "sub", c.Sub, "aud", c.Aud, "jti", c.Jti,
		"exp", time.Unix(c.Exp, 0).UTC().Format(time.RFC3339), "request_id", r.Header.Get("x-request-id"))
	w.Header().Set("x-id-token", id)
	w.WriteHeader(http.StatusOK)
}

// watchActive polls the IdentityContinuity (CONTINUITY, "<namespace>/<name>")
// through the Kubernetes API with the pod's service account, and records
// status.active. Polling keeps it to one read-only GET.
func (x *exchanger) watchActive(ctx context.Context, api *http.Client, base, token, ic string, every time.Duration) {
	ns, name, _ := strings.Cut(ic, "/")
	u := base + "/apis/continuity.lab.solo.io/v1alpha1/namespaces/" + url.PathEscape(ns) + "/identitycontinuities/" + url.PathEscape(name)
	for {
		if tier, err := readActive(ctx, api, u, token); err != nil {
			slog.Warn("identity continuity", "err", err)
		} else if tier != "" {
			x.setActive(tier)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func readActive(ctx context.Context, api *http.Client, u, tokenFile string) (string, error) {
	tok, err := os.ReadFile(tokenFile) // re-read: projected tokens rotate
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	resp, err := api.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET identitycontinuity: HTTP %d", resp.StatusCode)
	}
	var ic struct {
		Status struct {
			Active string `json:"active"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ic); err != nil {
		return "", err
	}
	return ic.Status.Active, nil
}

// kubeClient trusts the cluster CA from the pod's service account.
func kubeClient(caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates in %s", caFile)
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

// loadOPs reads OPS (JSON, the last entry S&V's Keycloak) and each client
// secret from SECRETS_DIR/<name>.
func loadOPs(raw, dir string) (keycloak op, upstreams []op, err error) {
	var ops []op
	if err := json.Unmarshal([]byte(raw), &ops); err != nil || len(ops) == 0 {
		return op{}, nil, fmt.Errorf("OPS: a JSON list, S&V's Keycloak last: %v", err)
	}
	for i := range ops {
		if ops[i].Name == "" || ops[i].TokenURL == "" || ops[i].ClientID == "" {
			return op{}, nil, fmt.Errorf("OPS[%d]: name, token_url and client_id are required", i)
		}
		s, err := os.ReadFile(dir + "/" + ops[i].Name)
		if err != nil {
			return op{}, nil, fmt.Errorf("client secret for %s: %w", ops[i].Name, err)
		}
		ops[i].secret = strings.TrimSpace(string(s))
	}
	return ops[len(ops)-1], ops[:len(ops)-1], nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	keycloak, upstreams, err := loadOPs(os.Getenv("OPS"), env("SECRETS_DIR", "/var/run/secrets/op"))
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	x := &exchanger{
		keycloak:  keycloak,
		upstreams: upstreams,
		brokerURL: env("BROKER_URL", "http://keycloak.sv-identity/realms/sterling-vance/broker"),
		hc:        &http.Client{Timeout: 10 * time.Second},
		now:       time.Now,
		cache:     map[[32]byte]cached{},
		refresh:   map[string]string{},
	}
	names := []string{}
	for _, u := range upstreams {
		names = append(names, u.Name)
	}
	sa := env("SA_DIR", "/var/run/secrets/kubernetes.io/serviceaccount")
	api, err := kubeClient(sa + "/ca.crt")
	if err != nil {
		slog.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	base := "https://" + os.Getenv("KUBERNETES_SERVICE_HOST") + ":" + env("KUBERNETES_SERVICE_PORT", "443")
	go x.watchActive(ctx, api, base, sa+"/token", env("CONTINUITY", "sv-identity/sterling-vance"), 2*time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		x.mu.Lock()
		known := x.active != ""
		x.mu.Unlock()
		if !known { // not ready until it knows which IdP vouches
			http.Error(w, "active tier unknown", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.Handle("/", x)
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("listening", "addr", srv.Addr, "upstreams", names, "keycloak", keycloak.TokenURL, "client_id", keycloak.ClientID)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
}
