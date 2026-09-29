// Package keycloak is the slice of the Keycloak Admin REST API the controller
// needs: identity providers and the browser flow's IdP redirector. It signs in
// as a realm service-account client (client credentials), never as an admin.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const redirectorID = "identity-provider-redirector"

// ErrNotFound is returned for a 404 from the admin API.
var ErrNotFound = errors.New("not found")

type Client struct {
	base, realm string
	http        *http.Client

	mu               sync.Mutex
	clientID, secret string
	token            string
	expires          time.Time
}

func New(base, realm string) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), realm: realm, http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) Base() string  { return c.base }
func (c *Client) Realm() string { return c.realm }

// SetCredentials swaps the service-account credentials (a rotated Secret).
func (c *Client) SetCredentials(id, secret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != c.clientID || secret != c.secret {
		c.clientID, c.secret, c.token = id, secret, ""
	}
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.clientID}, "client_secret": {c.secret}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/realms/"+url.PathEscape(c.realm)+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		return "", fmt.Errorf("service-account token: HTTP %d %s", resp.StatusCode, t.Error)
	}
	c.token, c.expires = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn)*time.Second-30*time.Second)
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		var body io.Reader
		if in != nil {
			b, _ := json.Marshal(in)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, c.base+"/admin/realms/"+url.PathEscape(c.realm)+path, body)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			c.mu.Lock()
			c.token = "" // Keycloak restarted, or the session ended
			c.mu.Unlock()
			continue
		case resp.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case resp.StatusCode >= 300:
			return fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
		}
		if out != nil && len(b) > 0 {
			return json.Unmarshal(b, out)
		}
		return nil
	}
}

// IdP is an identity provider representation, kept as a map so a read-modify-
// write never drops fields this controller does not manage.
type IdP map[string]any

func (p IdP) Config() map[string]any {
	if c, ok := p["config"].(map[string]any); ok {
		return c
	}
	c := map[string]any{}
	p["config"] = c
	return c
}

func (c *Client) IdPs(ctx context.Context) ([]IdP, error) {
	var out []IdP
	return out, c.do(ctx, http.MethodGet, "/identity-provider/instances", nil, &out)
}

func (c *Client) CreateIdP(ctx context.Context, p IdP) error {
	return c.do(ctx, http.MethodPost, "/identity-provider/instances", p, nil)
}

func (c *Client) UpdateIdP(ctx context.Context, alias string, p IdP) error {
	return c.do(ctx, http.MethodPut, "/identity-provider/instances/"+url.PathEscape(alias), p, nil)
}

// UsernameMapper is the mapper EnsureUsernameFromEmail keeps on an IdP.
const UsernameMapper = "username-from-email"

// EnsureUsernameFromEmail makes a brokered user's username their email. The
// first-broker-login flow finds an existing user by email OR username, so
// without it an upstream account whose username is "bob" would be linked to
// the local bob whatever its email. With it, both lookups use the email the
// upstream verified.
func (c *Client) EnsureUsernameFromEmail(ctx context.Context, alias string) error {
	var have []map[string]any
	base := "/identity-provider/instances/" + url.PathEscape(alias) + "/mappers"
	if err := c.do(ctx, http.MethodGet, base, nil, &have); err != nil {
		return err
	}
	for _, m := range have {
		if m["name"] == UsernameMapper {
			return nil
		}
	}
	return c.do(ctx, http.MethodPost, base, map[string]any{
		"name": UsernameMapper, "identityProviderAlias": alias, "identityProviderMapper": "oidc-username-idp-mapper",
		"config": map[string]any{"template": "${CLAIM.email}", "target": "LOCAL", "syncMode": "INHERIT"},
	}, nil)
}

func (c *Client) DeleteIdP(ctx context.Context, alias string) error {
	err := c.do(ctx, http.MethodDelete, "/identity-provider/instances/"+url.PathEscape(alias), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// SetRedirector points the flow's Identity Provider Redirector at an IdP
// alias, or clears it ("") so the flow falls through to the login form.
// Reports whether anything changed.
func (c *Client) SetRedirector(ctx context.Context, flow, alias string) (bool, error) {
	var execs []struct {
		ID                   string `json:"id"`
		ProviderID           string `json:"providerId"`
		AuthenticationConfig string `json:"authenticationConfig"`
	}
	if err := c.do(ctx, http.MethodGet, "/authentication/flows/"+url.PathEscape(flow)+"/executions", nil, &execs); err != nil {
		return false, fmt.Errorf("flow %s: %w", flow, err)
	}
	want := map[string]string{}
	if alias != "" {
		want["defaultProvider"] = alias
	}
	for _, e := range execs {
		if e.ProviderID != redirectorID {
			continue
		}
		if e.AuthenticationConfig == "" {
			return true, c.do(ctx, http.MethodPost, "/authentication/executions/"+e.ID+"/config",
				map[string]any{"alias": "continuity", "config": want}, nil)
		}
		var cfg struct {
			ID     string            `json:"id"`
			Alias  string            `json:"alias"`
			Config map[string]string `json:"config"`
		}
		if err := c.do(ctx, http.MethodGet, "/authentication/config/"+e.AuthenticationConfig, nil, &cfg); err != nil {
			return false, err
		}
		if cfg.Config["defaultProvider"] == alias {
			return false, nil
		}
		delete(cfg.Config, "defaultProvider")
		if cfg.Config == nil {
			cfg.Config = map[string]string{}
		}
		for k, v := range want {
			cfg.Config[k] = v
		}
		return true, c.do(ctx, http.MethodPut, "/authentication/config/"+cfg.ID, cfg, nil)
	}
	return false, fmt.Errorf("flow %s has no %s execution", flow, redirectorID)
}
