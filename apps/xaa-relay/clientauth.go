package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"
)

const assertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// clientKey signs client assertions (private_key_jwt, RFC 7523 section 2.2):
// iss and sub the client ID, aud the authorization server's issuer, a fresh
// jti, a minute's life. kid is the key's RFC 7638 thumbprint, as published in its JWKS.
type clientKey struct {
	key *rsa.PrivateKey
	kid string
}

func loadClientKey(path string) (*clientKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	var k *rsa.PrivateKey
	if p, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		var ok bool
		if k, ok = p.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("%s: not an RSA key", path)
		}
	} else if k, err = x509.ParsePKCS1PrivateKey(blk.Bytes); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &clientKey{key: k, kid: thumbprint(&k.PublicKey)}, nil
}

func thumbprint(pub *rsa.PublicKey) string {
	b64 := base64.RawURLEncoding.EncodeToString
	j := fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, b64(big.NewInt(int64(pub.E)).Bytes()), b64(pub.N.Bytes()))
	sum := sha256.Sum256([]byte(j))
	return b64(sum[:])
}

func (k *clientKey) assertion(clientID, audience string, now time.Time) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.kid})
	c, _ := json.Marshal(map[string]any{"iss": clientID, "sub": clientID, "aud": audience,
		"jti": hex.EncodeToString(jti), "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, d[:])
	if err != nil {
		return "", err
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// authenticate adds S&V's client authentication at o to the request: a
// client assertion (private_key_jwt), or the client secret in the form
// (client_secret_post) or as Basic credentials, which it returns.
func (o *op) authenticate(form url.Values, now time.Time) (user, pass string, err error) {
	switch {
	case o.key != nil:
		aud := o.Issuer // the AS's identifier (RFC 7523bis), not a URL it may be reached by
		if aud == "" {
			aud = o.TokenURL
		}
		a, err := o.key.assertion(o.ClientID, aud, now)
		if err != nil {
			return "", "", err
		}
		form.Set("client_id", o.ClientID)
		form.Set("client_assertion_type", assertionType)
		form.Set("client_assertion", a)
		return "", "", nil
	case o.secret != "" && o.Auth == "client_secret_post":
		form.Set("client_id", o.ClientID)
		form.Set("client_secret", o.secret)
		return "", "", nil
	case o.secret != "":
		return url.QueryEscape(o.ClientID), url.QueryEscape(o.secret), nil
	}
	return "", "", errors.New(o.Name + ": no client credential")
}

// loadCredential reads S&V's credential at o from dir: <name>.key for
// private_key_jwt, else the secret in <name>.
func (o *op) loadCredential(dir string) error {
	switch o.Auth {
	case "private_key_jwt":
		k, err := loadClientKey(dir + "/" + o.Name + ".key")
		if err != nil {
			return err
		}
		o.key = k
	case "", "client_secret_basic", "client_secret_post":
		s, err := os.ReadFile(dir + "/" + o.Name)
		if err != nil {
			return fmt.Errorf("client secret for %s: %w", o.Name, err)
		}
		o.secret = strings.TrimSpace(string(s))
	default:
		return fmt.Errorf("%s: auth %q: private_key_jwt, client_secret_post or client_secret_basic", o.Name, o.Auth)
	}
	return nil
}
