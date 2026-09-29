package main

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// The edge does the OIDC dance and forwards the admin's access token; the
// server verifies it again (never trust a header just because of where it
// came from) and acts in the cluster as that person.
type Auth struct {
	verifier *oidc.IDTokenVerifier
	audience string
	client   string
	group    string
}

type User struct {
	Name   string    `json:"name"`
	Email  string    `json:"email,omitempty"`
	Groups []string  `json:"groups"`
	Expiry time.Time `json:"-"` // when the token this request carried stops being valid
}

type userKey struct{}

// client is the edge's OIDC client: the token must have been issued to it
// (azp), not to some other app of the same realm that shares the audience.
func NewAuth(ctx context.Context, issuer, jwksURL, audience, client, group string) *Auth {
	ks := oidc.NewRemoteKeySet(ctx, jwksURL)
	v := oidc.NewVerifier(issuer, ks, &oidc.Config{SkipClientIDCheck: true})
	return &Auth{verifier: v, audience: audience, client: client, group: group}
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.user(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if !slices.Contains(u.Groups, a.group) {
			http.Error(w, "observatory access requires group "+a.group, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

func (a *Auth) user(r *http.Request) (User, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return User{}, errors.New("no bearer token")
	}
	tok, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		return User{}, err
	}
	if !slices.Contains(tok.Audience, a.audience) {
		return User{}, errors.New("token not issued for " + a.audience)
	}
	var c struct {
		Type   string   `json:"typ"`
		Azp    string   `json:"azp"`
		Name   string   `json:"preferred_username"`
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := tok.Claims(&c); err != nil {
		return User{}, err
	}
	// an access token, not an ID or refresh token of the same realm
	if c.Type != "Bearer" {
		return User{}, errors.New("not an access token")
	}
	if c.Azp != a.client {
		return User{}, errors.New("token not issued to " + a.client)
	}
	for i, g := range c.Groups {
		c.Groups[i] = strings.TrimPrefix(g, "/")
	}
	return User{Name: c.Name, Email: c.Email, Groups: c.Groups, Expiry: tok.Expiry}, nil
}

// writeGuard refuses a state-changing request another site could have sent
// with the admin's edge session cookie: the browser marks cross-origin
// requests (Sec-Fetch-Site, Origin), and a JSON or YAML body can't come from
// a plain HTML form.
func writeGuard(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	return cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mt != "application/json" && mt != "application/yaml" {
				http.Error(w, "a JSON or YAML body is required", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
}

// securityHeaders: the UI loads only its own code (Monaco's worker is a blob),
// can't be framed, and sends no referrer.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; font-src 'self' data:; worker-src 'self' blob:; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func userFrom(ctx context.Context) User {
	u, _ := ctx.Value(userKey{}).(User)
	return u
}

func devUser(name, group string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, User{Name: name, Groups: []string{group}})))
	})
}
