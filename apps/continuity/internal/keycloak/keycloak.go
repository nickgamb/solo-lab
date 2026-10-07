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
	return &Client{base: strings.TrimSuffix(base, "/"), realm: realm, http: &http.Client{Timeout: 10 * time.Second,
		// a redirect would re-POST the client secret elsewhere
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
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
	life := time.Duration(t.ExpiresIn) * time.Second
	if life <= 0 {
		life = time.Minute
	}
	c.token, c.expires = t.AccessToken, time.Now().Add(life*9/10)
	return c.token, nil
}

// maxErrorDetail bounds what an error carries of Keycloak's answer.
const maxErrorDetail = 256

// apiError is a failed admin API call: the method, the path without its
// query, the status, and Keycloak's own error message (never the request).
func apiError(method, path string, status int, body []byte) error {
	var e struct {
		ErrorMessage string `json:"errorMessage"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	detail := e.ErrorMessage
	if detail == "" {
		detail = e.Error
	}
	if len(detail) > maxErrorDetail {
		detail = strings.ToValidUTF8(detail[:maxErrorDetail], "") + "..."
	}
	path, _, _ = strings.Cut(path, "?")
	msg := fmt.Sprintf("%s %s: HTTP %d", method, path, status)
	if detail != "" {
		msg += ": " + detail
	}
	return errors.New(msg)
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
			return apiError(method, path, resp.StatusCode, b)
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

// Mapper is an identity provider mapper the controller keeps on an IdP, by
// name: its type and config.
type Mapper struct {
	Name, Type string
	Config     map[string]string
}

// UsernameMapper makes a brokered user's username their email. The
// first-broker-login flow finds an existing user by email OR username, so
// without it an upstream account whose username is "bob" would be linked to
// the local bob whatever its email. With it, both lookups use the email the
// upstream verified.
var UsernameMapper = Mapper{Name: "username-from-email", Type: "oidc-username-idp-mapper",
	Config: map[string]string{"template": "${CLAIM.email}", "target": "LOCAL", "syncMode": "INHERIT"}}

// AssuranceMapper puts how the upstream authenticated each sign-in (its acr,
// amr and auth_time) on that sign-in's broker session, as notes
// continuity.<claim>, on every sign-in (FORCE), for the broker's
// continuity-assurance client scope. Its type is the broker image's provider
// (tools/keycloak-idjag/session-claims).
var AssuranceMapper = Mapper{Name: "continuity-assurance", Type: "continuity-session-claims-idp-mapper",
	Config: map[string]string{"claims": "acr,amr,auth_time", "note.prefix": "continuity.", "syncMode": "FORCE"}}

// EnsureMapper creates the mapper on the IdP, or puts its type and config
// back if they were changed in Keycloak. Config keys it doesn't set stay.
func (c *Client) EnsureMapper(ctx context.Context, alias string, m Mapper) error {
	var have []map[string]any
	base := "/identity-provider/instances/" + url.PathEscape(alias) + "/mappers"
	if err := c.do(ctx, http.MethodGet, base, nil, &have); err != nil {
		return err
	}
	for _, cur := range have {
		if cur["name"] != m.Name {
			continue
		}
		cfg, _ := cur["config"].(map[string]any)
		if cfg == nil {
			cfg = map[string]any{}
		}
		same := cur["identityProviderMapper"] == m.Type
		for k, v := range m.Config {
			same = same && cfg[k] == v
			cfg[k] = v
		}
		if same {
			return nil
		}
		id, _ := cur["id"].(string)
		cur["identityProviderMapper"], cur["config"] = m.Type, cfg
		return c.do(ctx, http.MethodPut, base+"/"+url.PathEscape(id), cur, nil)
	}
	return c.do(ctx, http.MethodPost, base, map[string]any{
		"name": m.Name, "identityProviderAlias": alias, "identityProviderMapper": m.Type, "config": m.Config,
	}, nil)
}

// DeleteIdP removes an upstream IdP, and with it every user's link to it.
// Keycloak drops the links from its store but not from its user cache, and
// a later sign-in through an IdP of the same alias then fails on the stale
// link; so the realm's user cache is cleared after a delete.
func (c *Client) DeleteIdP(ctx context.Context, alias string) error {
	err := c.do(ctx, http.MethodDelete, "/identity-provider/instances/"+url.PathEscape(alias), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/clear-user-cache", nil, nil)
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

// Registration is a client as the broker has it registered: where it sends
// users back to, the audiences its own mappers add, and its default scopes.
type Registration struct {
	Found         bool
	RedirectURIs  []string
	Audiences     []string
	DefaultScopes []string
}

// ClientRegistration reads one client by its client ID (realm-management
// view-clients).
func (c *Client) ClientRegistration(ctx context.Context, clientID string) (Registration, error) {
	var list []struct {
		ClientID            string   `json:"clientId"`
		RedirectURIs        []string `json:"redirectUris"`
		DefaultClientScopes []string `json:"defaultClientScopes"`
		ProtocolMappers     []struct {
			ProtocolMapper string            `json:"protocolMapper"`
			Config         map[string]string `json:"config"`
		} `json:"protocolMappers"`
	}
	if err := c.do(ctx, http.MethodGet, "/clients?clientId="+url.QueryEscape(clientID), nil, &list); err != nil {
		return Registration{}, err
	}
	for _, cl := range list {
		if cl.ClientID != clientID {
			continue
		}
		r := Registration{Found: true, RedirectURIs: cl.RedirectURIs, DefaultScopes: cl.DefaultClientScopes}
		for _, m := range cl.ProtocolMappers {
			if a := m.Config["included.client.audience"]; m.ProtocolMapper == "oidc-audience-mapper" && a != "" {
				r.Audiences = append(r.Audiences, a)
			}
		}
		return r, nil
	}
	return Registration{}, nil
}
