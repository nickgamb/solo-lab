// idtoken-exchange: agentgateway's external processor (ext_proc) for Cross App
// Access. Given the caller's access token the gateway verified, it sets
// x-id-token to an OIDC ID token for the same user from S&V's broker, which
// the gateway then trades there for an ID-JAG
// (demos/bob/manifests/xaa/ledgerline.yaml). The broker vouches for every S&V
// user, whichever upstream IdP signed them in, so a partner trusts one issuer.
//
// It exists because the agent doesn't have the ID token: kagent passes agents
// the caller's access token, not the ID token. And agentgateway's own token
// exchange can't fetch it: it requires token_type Bearer in the response,
// while RFC 8693 (2.2.1) has an IdP return N_A for a token that isn't an
// access token, as Keycloak does for an ID token.
//
// At the broker, as the requesting app (kagent): RFC 8693 for a refresh token
// in the user's session, then that refresh token for the ID token (exchange,
// below). The broker issues them only for a live session of that user, so the
// ID token belongs to the same sign-in.
//
// The IdPs in UPSTREAM_IDPS vouch for their own sign-ins instead: for a
// session one of them signed in (the access token's idp claim), the ID token
// is the one that IdP issued for it, which the broker keeps with the session
// and hands only to kagent (the continuity-upstream endpoint in
// tools/keycloak-idjag). Every other session gets the broker's.
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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

const (
	tokenExchange    = "urn:ietf:params:oauth:grant-type:token-exchange"
	typeAccessToken  = "urn:ietf:params:oauth:token-type:access_token"
	typeIDToken      = "urn:ietf:params:oauth:token-type:id_token"
	typeRefreshToken = "urn:ietf:params:oauth:token-type:refresh_token"
	// how many ID tokens are cached at most
	maxCached = 10000
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// op is S&V's broker and S&V's client there.
type op struct {
	Name, TokenURL, ClientID string
	Auth                     string // client_secret_post or client_secret_basic
	secret                   string
}

// exchanger turns access tokens into ID tokens, caching each for a while.
type exchanger struct {
	broker op // S&V's broker, as kagent
	// upstreams: the IdPs that vouch for their own sign-ins (by the broker's
	// alias, the idp claim); upstreamURL: the broker's endpoint for their ID
	// tokens, .../continuity-upstream
	upstreams   map[string]bool
	upstreamURL string
	hc          *http.Client
	now         func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]cached
}

type cached struct {
	idToken string
	exp     time.Time
}

var (
	// errRefused: the broker declined (bad or expired subject token, no live
	// session): the caller is refused.
	errRefused = errors.New("refused")
	// errUnavailable: the broker didn't answer, or answered 5xx.
	errUnavailable = errors.New("broker unavailable")
)

func (x *exchanger) idToken(ctx context.Context, accessToken string) (id, source string, err error) {
	key := sha256.Sum256([]byte(accessToken))
	now := x.now()
	voucher := x.voucher(accessToken)
	source = voucher
	if source == "" {
		source = x.broker.Name
	}
	x.mu.Lock()
	if c, ok := x.cache[key]; ok && now.Before(c.exp) {
		x.mu.Unlock()
		return c.idToken, source, nil
	}
	for k, c := range x.cache { // small and short-lived: prune on the way
		if !now.Before(c.exp) {
			delete(x.cache, k)
		}
	}
	x.mu.Unlock()

	if id, err = x.exchange(ctx, accessToken, voucher); err != nil {
		return "", source, err
	}
	// cache until the earlier of the two expiries, less a little slack
	if exp := earliest(expiry(id), expiry(accessToken)).Add(-10 * time.Second); exp.After(now) {
		x.mu.Lock()
		x.store(key, cached{idToken: id, exp: exp}, now)
		x.mu.Unlock()
	}
	return id, source, nil
}

// voucher: the IdP that signed the session in, if it vouches for its own
// sign-ins; "" for the broker. The gateway verified the access token.
func (x *exchanger) voucher(accessToken string) string {
	if idp := payload(accessToken).Idp; x.upstreams[idp] {
		return idp
	}
	return ""
}

// vouchers: the IdPs that vouch for their own sign-ins, sorted.
func (x *exchanger) vouchers() []string {
	out := make([]string, 0, len(x.upstreams))
	for n := range x.upstreams {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// store caches c under key, keeping at most maxCached entries: expired ones
// go first, then the one expiring soonest. x.mu is held.
func (x *exchanger) store(key [32]byte, c cached, now time.Time) {
	if _, ok := x.cache[key]; !ok && len(x.cache) >= maxCached {
		for k, e := range x.cache {
			if !now.Before(e.exp) {
				delete(x.cache, k)
			}
		}
		for len(x.cache) >= maxCached {
			var oldest [32]byte
			first := true
			for k, e := range x.cache {
				if first || e.exp.Before(x.cache[oldest].exp) {
					oldest, first = k, false
				}
			}
			delete(x.cache, oldest)
		}
	}
	x.cache[key] = c
}

// exchange gets the ID token from S&V's broker in two steps. RFC 8693 for a
// refresh token in the user's own session (the kagent client's
// enableRefreshRequestedTokenType SAME_SESSION), which gives kagent a lasting
// client session in it, then that refresh token for the ID token. An ID token
// exchanged for directly carries only a transient client session, and the
// broker issues an ID-JAG only for a session the requesting app (kagent) is
// in: a user who signed in through another of S&V's apps (the Solo UI's
// kagent-ui) would be refused. The refresh token never leaves this request.
func (x *exchanger) exchange(ctx context.Context, accessToken, voucher string) (string, error) {
	var rt struct {
		RefreshToken    string `json:"refresh_token"`
		IssuedTokenType string `json:"issued_token_type"`
	}
	if err := x.tokenRequest(ctx, x.broker, url.Values{
		"grant_type":           {tokenExchange},
		"subject_token":        {accessToken},
		"subject_token_type":   {typeAccessToken},
		"requested_token_type": {typeRefreshToken},
		"scope":                {"openid"},
	}, &rt); err != nil {
		return "", err
	}
	if rt.IssuedTokenType != typeRefreshToken || rt.RefreshToken == "" {
		return "", fmt.Errorf("token endpoint issued %q, not a refresh token", rt.IssuedTokenType)
	}
	var out struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := x.tokenRequest(ctx, x.broker, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt.RefreshToken},
		"scope":         {"openid"},
	}, &out); err != nil {
		return "", err
	}
	if voucher != "" {
		return x.upstreamIDToken(ctx, voucher, out.AccessToken)
	}
	if out.IDToken == "" {
		return "", errors.New("token endpoint returned no ID token")
	}
	return out.IDToken, nil
}

// upstreamIDToken: the ID token the IdP (by its alias) issued for the sign-in
// behind kagent's access token (in the same session), from the broker as kagent.
func (x *exchanger) upstreamIDToken(ctx context.Context, idp, accessToken string) (string, error) {
	if accessToken == "" {
		return "", errors.New("token endpoint returned no access token")
	}
	at := x.broker
	at.Name, at.TokenURL = idp, strings.TrimSuffix(x.upstreamURL, "/")+"/"+url.PathEscape(idp)+"/id-token"
	var out struct {
		IDToken string `json:"id_token"`
	}
	if err := x.tokenRequest(ctx, at, url.Values{"token": {accessToken}}, &out); err != nil {
		return "", err
	}
	if out.IDToken == "" {
		return "", fmt.Errorf("%s: no ID token", idp)
	}
	return out.IDToken, nil
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
	Iss string `json:"iss"`
	Aud any    `json:"aud"`
	Jti string `json:"jti"`
	Idp string `json:"idp"`
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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	broker := op{Name: env("BROKER_NAME", "sterling-vance"),
		TokenURL: env("TOKEN_URL", "http://keycloak.sv-identity/realms/sterling-vance/protocol/openid-connect/token"),
		ClientID: env("CLIENT_ID", "kagent"), Auth: env("CLIENT_AUTH", "client_secret_basic")}
	if err := broker.loadCredential(env("SECRETS_DIR", "/var/run/secrets/op")); err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	hc, err := caClient(os.Getenv("CA_FILE"))
	if err != nil {
		slog.Error("CA_FILE", "err", err)
		os.Exit(1)
	}
	x := &exchanger{broker: broker, hc: hc, now: time.Now, cache: map[[32]byte]cached{},
		upstreams: map[string]bool{}, upstreamURL: env("UPSTREAM_URL", "http://keycloak.sv-identity/realms/sterling-vance/continuity-upstream")}
	for _, n := range strings.Split(os.Getenv("UPSTREAM_IDPS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			x.upstreams[n] = true
		}
	}

	health := &http.Server{Addr: env("HEALTH_LISTEN", ":8081"), ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
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
		slog.Info("listening (ext_proc)", "addr", lis.Addr().String(), "broker", broker.TokenURL, "client_id", broker.ClientID, "vouchers", x.vouchers())
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
