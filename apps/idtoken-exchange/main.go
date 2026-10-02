// idtoken-exchange: agentgateway's external authorization step for Cross App
// Access. Given the caller's verified access token, it returns the same user's
// OIDC ID token for the requesting app, which the gateway then trades for an
// ID-JAG (demos/bob/manifests/40-xaa-ledgerline.yaml).
//
// It exists because the agent can't bring the ID token on every edition:
// kagent-enterprise 0.5.9 forwards only the access token to agents. And
// agentgateway's own token exchange can't fetch it: it requires token_type
// Bearer in the response, while RFC 8693 (2.2.1) has an IdP return N_A for a
// token that isn't an access token, as Keycloak does for an ID token.
//
// The exchange is RFC 8693 at the IdP, authenticated as the requesting app:
//
//	subject_token=<access token>  subject_token_type=...:access_token
//	requested_token_type=...:id_token  scope=openid
//
// The IdP issues it only for a live session of that user and client, so the
// ID token belongs to the same sign-in as the access token.
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
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// exchanger turns access tokens into ID tokens, caching each until it expires.
type exchanger struct {
	tokenURL, clientID, clientSecret string
	hc                               *http.Client
	now                              func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]cached
}

type cached struct {
	idToken string
	exp     time.Time
}

// errRefused is the IdP declining the exchange (bad or expired subject token,
// no live session): the caller is refused. Anything else is a failure.
var errRefused = errors.New("exchange refused")

func (x *exchanger) idToken(ctx context.Context, accessToken string) (string, error) {
	key := sha256.Sum256([]byte(accessToken))
	now := x.now()
	x.mu.Lock()
	if c, ok := x.cache[key]; ok && now.Before(c.exp) {
		x.mu.Unlock()
		return c.idToken, nil
	}
	for k, c := range x.cache { // small and short-lived: prune on the way
		if !now.Before(c.exp) {
			delete(x.cache, k)
		}
	}
	x.mu.Unlock()

	form := url.Values{
		"grant_type":           {tokenExchange},
		"subject_token":        {accessToken},
		"subject_token_type":   {typeAccessToken},
		"requested_token_type": {typeIDToken},
		"scope":                {"openid"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(x.clientID), url.QueryEscape(x.clientSecret))
	resp, err := x.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return "", fmt.Errorf("%w: %s", errRefused, e.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken     string `json:"access_token"` // RFC 8693 carries any issued token here
		IssuedTokenType string `json:"issued_token_type"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	if out.IssuedTokenType != typeIDToken || out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint issued %q, not an ID token", out.IssuedTokenType)
	}
	// cache until the earlier of the two expiries, less a little slack
	exp := earliest(expiry(out.AccessToken), expiry(accessToken)).Add(-10 * time.Second)
	if exp.After(now) {
		x.mu.Lock()
		x.cache[key] = cached{idToken: out.AccessToken, exp: exp}
		x.mu.Unlock()
	}
	return out.AccessToken, nil
}

// expiry reads a JWT's exp without verifying it: the IdP verifies the subject
// token, and the gateway verifies the ID token it passes on.
func expiry(jwt string) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

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
	id, err := x.idToken(r.Context(), tok)
	switch {
	case errors.Is(err, errRefused):
		slog.Info("exchange refused", "err", err)
		http.Error(w, "refused", http.StatusForbidden)
		return
	case err != nil:
		slog.Warn("exchange failed", "err", err)
		http.Error(w, "exchange failed", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("x-id-token", id)
	w.WriteHeader(http.StatusOK)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	secret, err := os.ReadFile(env("CLIENT_SECRET_FILE", "/var/run/secrets/client/clientSecret"))
	if err != nil {
		slog.Error("client secret", "err", err)
		os.Exit(1)
	}
	x := &exchanger{
		tokenURL:     env("TOKEN_URL", ""),
		clientID:     env("CLIENT_ID", ""),
		clientSecret: strings.TrimSpace(string(secret)),
		hc:           &http.Client{Timeout: 10 * time.Second},
		now:          time.Now,
		cache:        map[[32]byte]cached{},
	}
	if x.tokenURL == "" || x.clientID == "" {
		slog.Error("TOKEN_URL and CLIENT_ID are required")
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/", x)
	srv := &http.Server{Addr: env("LISTEN", ":8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("listening", "addr", srv.Addr, "token_url", x.tokenURL, "client_id", x.clientID)
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
