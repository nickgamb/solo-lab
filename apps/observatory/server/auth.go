package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// The edge does the OIDC dance and forwards the admin's access token; the
// server verifies it again (never trust a header just because of where it
// came from) and acts in the cluster as that person.
type Auth struct {
	verifier *oidc.IDTokenVerifier
	audience string
	group    string
}

type User struct {
	Name   string   `json:"name"`
	Email  string   `json:"email,omitempty"`
	Groups []string `json:"groups"`
}

type userKey struct{}

func NewAuth(ctx context.Context, issuer, jwksURL, audience, group string) *Auth {
	ks := oidc.NewRemoteKeySet(ctx, jwksURL)
	v := oidc.NewVerifier(issuer, ks, &oidc.Config{SkipClientIDCheck: true})
	return &Auth{verifier: v, audience: audience, group: group}
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
		Name   string   `json:"preferred_username"`
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := tok.Claims(&c); err != nil {
		return User{}, err
	}
	for i, g := range c.Groups {
		c.Groups[i] = strings.TrimPrefix(g, "/")
	}
	return User{Name: c.Name, Email: c.Email, Groups: c.Groups}, nil
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
