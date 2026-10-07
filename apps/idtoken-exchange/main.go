// idtoken-exchange: agentgateway's external processor (ext_proc) for Cross App
// Access. Given the caller's access token the gateway verified, it sets
// x-id-token to an OIDC ID token for the same user from the enterprise IdP
// that vouches for them, which the gateway then trades for an ID-JAG
// (demos/bob/manifests/40-xaa-ledgerline.yaml).
//
// It exists because the agent doesn't have the ID token: kagent passes agents
// the caller's access token, not the ID token. And agentgateway's own token
// exchange can't fetch it: it requires token_type Bearer in the response,
// while RFC 8693 (2.2.1) has an IdP return N_A for a token that isn't an
// access token, as Keycloak does for an ID token.
//
// Which IdP: the one the user's session came from (the access token's idp
// claim, Keycloak's identity_provider session note), when it is an upstream
// that issues ID-JAGs (OPS, scripts/idp.sh xaa_env); otherwise S&V's Keycloak,
// for its own sessions and for upstreams that don't issue ID-JAGs. A session
// from an upstream that S&V's identity continuity no longer has active
// (CONTINUITY, the IdentityContinuity's status.active) is refused: the user
// signs in again. The controller alone decides failover, and an upstream that
// isn't active is never called.
//
//	upstream  S&V's broker brokers the user's sign-in there and keeps their
//	          upstream tokens. Its Identity Brokering API v2 gives the
//	          user's upstream access token, renewed by the broker with the
//	          stored refresh token (which never leaves it), to client
//	          xaa-egress alone (its own key, for those upstreams only) with
//	          the user's own access token. The upstream then exchanges that
//	          access token for the user's ID token (RFC 8693), as S&V's
//	          client there (private_key_jwt), so the upstream decides on
//	          every call. S&V is a confidential client there, so the refresh
//	          token is not rotated (RFC 9700 4.14.2) and nothing is kept
//	          between replicas. A user with no account there gets S&V's
//	          Keycloak; an upstream that refuses (revoked, disabled) refuses
//	          the call.
//	keycloak  RFC 8693 at S&V's Keycloak, as the requesting app (kagent):
//	            subject_token=<access token>  subject_token_type=...:access_token
//	            requested_token_type=...:id_token  scope=openid
//	          Keycloak issues it only for a live session of that user and
//	          client, so the ID token belongs to the same sign-in.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
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
	Issuer   string `json:"issuer"`
	TokenURL string `json:"token_url"`
	ClientID string `json:"client_id"`
	Auth     string `json:"auth"` // private_key_jwt, client_secret_post or client_secret_basic
	secret   string
	key      *clientKey
}

// exchanger turns access tokens into ID tokens, caching each for a while.
type exchanger struct {
	keycloak  op   // S&V's Keycloak, as kagent: the ID token, the floor
	broker    op   // S&V's Keycloak, as the client that may read stored upstream tokens
	upstreams []op // ID-JAG-issuing upstreams, in failover order
	brokerURL string
	hc        *http.Client
	now       func() time.Time

	mu     sync.Mutex
	active string // the active tier; "" until known
	cache  map[[32]byte]cached
}

type cached struct {
	idToken string
	exp     time.Time
}

var (
	// errRefused: an IdP declined (bad or expired subject token, no live
	// session, revoked or disabled upstream account): the caller is refused.
	errRefused = errors.New("refused")
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
	if sess := payload(accessToken).IdP; sess != "" {
		if sess != active {
			return "", sess, fmt.Errorf("%w: the session came from %s, which is no longer the active IdP (%s): sign in again", errRefused, sess, active)
		}
		for _, u := range x.upstreams {
			if u.Name != sess {
				continue
			}
			if id, err = x.fromUpstream(ctx, accessToken, u); err != nil {
				return "", u.Name, err
			}
			source, ttl = u.Name, upstreamTTL
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
	user, pass, err := x.broker.authenticate(form, x.now()) // as xaa-egress, the one client allowed
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.brokerURL+"/"+url.PathEscape(u.Name)+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := x.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("broker: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode != http.StatusOK: // a session from u always has its tokens: anything else fails closed
		return "", fmt.Errorf("broker: HTTP %d: %s", resp.StatusCode, oauthError(body))
	}
	var stored struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &stored); err != nil {
		return "", fmt.Errorf("broker: %w", err)
	}
	if stored.AccessToken == "" {
		return "", fmt.Errorf("%w: %s: no upstream token stored for this session", errRefused, u.Name)
	}
	return x.idTokenAt(ctx, u, stored.AccessToken)
}

// idTokenAt has upstream u exchange the user's access token there for their
// ID token there (RFC 8693), as S&V's client at u.
func (x *exchanger) idTokenAt(ctx context.Context, u op, accessToken string) (string, error) {
	var out struct {
		AccessToken     string `json:"access_token"` // RFC 8693 carries any issued token here
		IDToken         string `json:"id_token"`
		IssuedTokenType string `json:"issued_token_type"`
	}
	if err := x.tokenRequest(ctx, u, url.Values{
		"grant_type":           {tokenExchange},
		"subject_token":        {accessToken},
		"subject_token_type":   {typeAccessToken},
		"requested_token_type": {typeIDToken},
		"scope":                {"openid"},
	}, &out); err != nil {
		return "", err
	}
	id := out.AccessToken
	if out.IssuedTokenType != typeIDToken {
		id = out.IDToken
	}
	if id == "" {
		return "", fmt.Errorf("%s: token exchange returned no ID token", u.Name)
	}
	return id, nil
}

// tokenRequest POSTs a form to o's token endpoint as S&V's client there. A
// 400, 401 or 403 is the IdP refusing (errRefused); no answer or a 5xx is
// errUnavailable.
func (x *exchanger) tokenRequest(ctx context.Context, o op, form url.Values, out any) error {
	user, pass, err := o.authenticate(form, x.now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
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
	IdP string `json:"idp"`
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

// loadOPs reads OPS (JSON, the last entry S&V's Keycloak) and S&V's
// credential at each from dir.
func loadOPs(raw, dir string) (keycloak op, upstreams []op, err error) {
	var ops []op
	if err := json.Unmarshal([]byte(raw), &ops); err != nil || len(ops) == 0 {
		return op{}, nil, fmt.Errorf("OPS: a JSON list, S&V's Keycloak last: %v", err)
	}
	for i := range ops {
		if ops[i].Name == "" || ops[i].TokenURL == "" || ops[i].ClientID == "" {
			return op{}, nil, fmt.Errorf("OPS[%d]: name, token_url and client_id are required", i)
		}
		if err := ops[i].loadCredential(dir); err != nil {
			return op{}, nil, err
		}
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
	broker := op{Name: "broker", Issuer: keycloak.Issuer, TokenURL: keycloak.TokenURL,
		ClientID: env("BROKER_CLIENT_ID", "xaa-egress"), Auth: "private_key_jwt"}
	if len(upstreams) > 0 {
		if err := broker.loadCredential(env("SECRETS_DIR", "/var/run/secrets/op")); err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
	}
	hc, err := caClient(os.Getenv("CA_FILE"))
	if err != nil {
		slog.Error("CA_FILE", "err", err)
		os.Exit(1)
	}
	x := &exchanger{
		keycloak:  keycloak,
		broker:    broker,
		upstreams: upstreams,
		brokerURL: env("BROKER_URL", "http://keycloak.sv-identity/realms/sterling-vance/broker"),
		hc:        hc,
		now:       time.Now,
		cache:     map[[32]byte]cached{},
	}
	names := []string{}
	for _, u := range upstreams {
		names = append(names, u.Name+" ("+u.Auth+")")
	}
	sa := env("SA_DIR", "/var/run/secrets/kubernetes.io/serviceaccount")
	api, err := kubeClient(sa + "/ca.crt")
	if err != nil {
		slog.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	base := "https://" + os.Getenv("KUBERNETES_SERVICE_HOST") + ":" + env("KUBERNETES_SERVICE_PORT", "443")
	go x.watchActive(ctx, api, base, sa+"/token", env("CONTINUITY", "sv-identity/sterling-vance"), 2*time.Second)

	// readiness: not ready until it knows which IdP vouches
	health := &http.Server{Addr: env("HEALTH_LISTEN", ":8081"), ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			x.mu.Lock()
			known := x.active != ""
			x.mu.Unlock()
			if !known {
				http.Error(w, "active tier unknown", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte("ok"))
		})}
	go func() {
		if err := health.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health", "err", err)
			os.Exit(1)
		}
	}()

	lis, err := net.Listen("tcp", env("LISTEN", ":8080"))
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	srv := grpc.NewServer()
	extproc.RegisterExternalProcessorServer(srv, &processor{x: x})
	go func() {
		slog.Info("listening (ext_proc)", "addr", lis.Addr().String(), "upstreams", names, "keycloak", keycloak.TokenURL, "client_id", keycloak.ClientID)
		if err := srv.Serve(lis); err != nil {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	srv.GracefulStop()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	health.Shutdown(sctx)
}
