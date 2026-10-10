package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// authenticate adds S&V's client authentication at o to the request: the
// client secret in the form (client_secret_post) or as Basic credentials,
// which it returns.
func (o op) authenticate(form url.Values, _ time.Time) (user, pass string, err error) {
	switch {
	case o.secret != "" && o.Auth == "client_secret_post":
		form.Set("client_id", o.ClientID)
		form.Set("client_secret", o.secret)
		return "", "", nil
	case o.secret != "":
		return url.QueryEscape(o.ClientID), url.QueryEscape(o.secret), nil
	}
	return "", "", errors.New(o.Name + ": no client credential")
}

// loadCredential reads S&V's client secret at o from dir/<name>.
func (o *op) loadCredential(dir string) error {
	switch o.Auth {
	case "", "client_secret_basic", "client_secret_post":
		s, err := os.ReadFile(dir + "/" + o.Name)
		if err != nil {
			return fmt.Errorf("client secret for %s: %w", o.Name, err)
		}
		o.secret = strings.TrimSpace(string(s))
	default:
		return fmt.Errorf("%s: auth %q: client_secret_post or client_secret_basic", o.Name, o.Auth)
	}
	return nil
}
