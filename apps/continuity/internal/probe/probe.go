// Package probe checks an OIDC issuer the way a relying party would use it:
// discovery, then the JWKS it points at, on a fresh connection every time.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickgamb/solo-lab/apps/continuity/internal/tiers"
)

type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint,omitempty"`
	EndSessionEndpoint    string `json:"end_session_endpoint,omitempty"`
}

type Prober struct {
	client *http.Client
	tls    *tls.Config
}

// New trusts the system roots plus caFile (the lab CA bundle), if present.
func New(caFile string) (*Prober, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", caFile)
		}
	}
	tc := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	tr := &http.Transport{
		Proxy:             nil,
		TLSClientConfig:   tc,
		DisableKeepAlives: true, // every probe proves the path is up now
	}
	return &Prober{tls: tc, client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}, nil
}

// HTTPClient is a client with the same trust for API calls (connections
// kept, redirects not followed).
func (p *Prober) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, TLSClientConfig: p.tls},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// OIDC probes an upstream issuer. The discovery document is returned only
// when the whole probe succeeded.
func (p *Prober) OIDC(ctx context.Context, issuer string, timeout time.Duration) (tiers.Result, *Discovery) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	fail := func(kind, format string, a ...any) (tiers.Result, *Discovery) {
		return tiers.Result{Kind: kind, Message: fmt.Sprintf(format, a...), Latency: time.Since(start)}, nil
	}
	var d Discovery
	url := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	if kind, err := p.getJSON(ctx, url, &d); err != nil {
		return fail(kind, "discovery: %v", err)
	}
	switch {
	case d.Issuer != issuer:
		return fail(tiers.InvalidDiscovery, "discovery issuer %q, want %q", d.Issuer, issuer)
	case d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "":
		return fail(tiers.InvalidDiscovery, "discovery lacks authorization, token or jwks endpoint")
	}
	// These are written into Keycloak and used with the client secret and
	// users' codes: never over plain HTTP.
	for _, e := range []string{d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI, d.UserinfoEndpoint, d.EndSessionEndpoint} {
		if e != "" && !strings.HasPrefix(e, "https://") {
			return fail(tiers.InvalidDiscovery, "discovery endpoint %q is not https", e)
		}
	}
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if kind, err := p.getJSON(ctx, d.JWKSURI, &jwks); err != nil {
		return fail(kind, "jwks: %v", err)
	}
	if len(jwks.Keys) == 0 {
		return fail(tiers.InvalidDiscovery, "jwks has no keys")
	}
	return tiers.Result{Kind: tiers.Healthy, Message: "discovery and jwks ok", Latency: time.Since(start)}, &d
}

// Broker fetches the broker realm's discovery document at its in-cluster URL
// and returns the public issuer it advertises.
func (p *Prober) Broker(ctx context.Context, url string, timeout time.Duration) (tiers.Result, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	var d Discovery
	if kind, err := p.getJSON(ctx, url, &d); err != nil {
		return tiers.Result{Kind: kind, Message: err.Error(), Latency: time.Since(start)}, ""
	}
	if d.Issuer == "" {
		return tiers.Result{Kind: tiers.InvalidDiscovery, Message: "broker discovery has no issuer", Latency: time.Since(start)}, ""
	}
	return tiers.Result{Kind: tiers.Healthy, Message: "broker reachable", Latency: time.Since(start)}, d.Issuer
}

func (p *Prober) getJSON(ctx context.Context, url string, v any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return tiers.InvalidDiscovery, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return tiers.Unreachable, errors.New("timed out")
		}
		return tiers.Unreachable, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case err != nil:
		return tiers.Unreachable, err
	case resp.StatusCode >= 500:
		return tiers.ServerError, fmt.Errorf("HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return tiers.InvalidDiscovery, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return tiers.InvalidDiscovery, fmt.Errorf("not JSON: %v", err)
	}
	return "", nil
}
