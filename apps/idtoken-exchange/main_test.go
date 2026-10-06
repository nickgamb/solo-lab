package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func jwtWithExp(exp time.Time) string {
	p, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(p) + ".s"
}

func jwtFor(iss, sub string, exp time.Time) string {
	p, _ := json.Marshal(map[string]any{"iss": iss, "sub": sub, "exp": exp.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(p) + ".s"
}

// idp fakes S&V's Keycloak token endpoint: it checks the exchange request
// and answers with status and body.
func idp(t *testing.T, calls *int32, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if id, secret, ok := r.BasicAuth(); !ok || id != "kagent" || secret != "s3cret" {
			t.Errorf("client auth = %q/%q/%v", id, secret, ok)
		}
		r.ParseForm()
		for k, want := range map[string]string{
			"grant_type": tokenExchange, "subject_token_type": typeAccessToken,
			"requested_token_type": typeIDToken, "scope": "openid",
		} {
			if got := r.PostForm.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// keycloakIDToken is what the fake Keycloak issues for the exchange
var keycloakIDToken = jwtFor("https://idp.sterling.lab/realms/sterling-vance", "bob-kc", time.Now().Add(5*time.Minute))

func keycloakOK(t *testing.T, calls *int32) *httptest.Server {
	return idp(t, calls, 200, fmt.Sprintf(`{"access_token":%q,"issued_token_type":%q,"token_type":"N_A"}`, keycloakIDToken, typeIDToken))
}

func newExchanger(keycloakURL string, upstreams ...op) *exchanger {
	return &exchanger{keycloak: op{Name: "keycloak", TokenURL: keycloakURL, ClientID: "kagent", secret: "s3cret"},
		upstreams: upstreams, hc: http.DefaultClient, now: time.Now, cache: map[[32]byte]cached{}, refresh: map[string]string{}}
}

func check(x *exchanger, subject string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/xaa/ledgerline/mcp", nil)
	if subject != "" {
		r.Header.Set("x-subject-token", subject)
	}
	x.ServeHTTP(w, r)
	return w
}

func TestReturnsIDTokenAndCaches(t *testing.T) {
	var calls int32
	x := newExchanger(keycloakOK(t, &calls).URL)
	at := jwtWithExp(time.Now().Add(5 * time.Minute))
	for i := 0; i < 2; i++ {
		w := check(x, at)
		if w.Code != 200 || w.Header().Get("x-id-token") != keycloakIDToken {
			t.Fatalf("call %d: %d %q", i, w.Code, w.Header().Get("x-id-token"))
		}
	}
	if calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1 (cached)", calls)
	}
}

func TestRefusals(t *testing.T) {
	var calls int32
	for name, tc := range map[string]struct {
		status int
		body   string
		want   int
	}{
		"idp refuses":     {400, `{"error":"invalid_token"}`, 403},
		"idp unavailable": {503, ``, 503},
		"not an id token": {200, fmt.Sprintf(`{"access_token":"x","issued_token_type":%q}`, typeAccessToken), 503},
	} {
		t.Run(name, func(t *testing.T) {
			w := check(newExchanger(idp(t, &calls, tc.status, tc.body).URL), jwtWithExp(time.Now().Add(time.Minute)))
			if w.Code != tc.want || w.Header().Get("x-id-token") != "" {
				t.Fatalf("got %d %q, want %d and no token", w.Code, w.Header().Get("x-id-token"), tc.want)
			}
		})
	}
	if w := check(newExchanger("http://unused"), ""); w.Code != 403 {
		t.Fatalf("no subject token: %d, want 403", w.Code)
	}
}

func TestCacheExpiresWithTheEarlierToken(t *testing.T) {
	var calls int32
	x := newExchanger(idp(t, &calls, 200, fmt.Sprintf(`{"access_token":%q,"issued_token_type":%q}`,
		jwtWithExp(time.Now().Add(time.Hour)), typeIDToken)).URL)
	at := jwtWithExp(time.Now().Add(30 * time.Second)) // the access token expires first
	check(x, at)
	x.now = func() time.Time { return time.Now().Add(25 * time.Second) } // past exp - 10s
	check(x, at)
	if calls != 2 {
		t.Fatalf("token endpoint called %d times, want 2 (cache ends with the access token)", calls)
	}
}

// ---- upstreams ---------------------------------------------------------------

const gluuIss = "https://gluu.example"

// broker fakes Keycloak's Identity Brokering API v2 for upstream gluu: it
// checks the requesting app and the user's token, and answers with status
// and the stored tokens.
func broker(t *testing.T, calls *int32, status int, stored map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.Method != http.MethodPost || r.URL.Path != "/gluu/token" {
			t.Errorf("broker request %s %s", r.Method, r.URL.Path)
		}
		if id, secret, ok := r.BasicAuth(); !ok || id != "kagent" || secret != "s3cret" {
			t.Errorf("broker client auth = %q/%q/%v", id, secret, ok)
		}
		r.ParseForm()
		if r.PostForm.Get("token") == "" {
			t.Error("broker: no user token")
		}
		w.WriteHeader(status)
		if status == 200 {
			json.NewEncoder(w).Encode(stored)
		} else {
			fmt.Fprint(w, `{"error":"invalid_request","error_description":"User not associated to identity provider"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gluu fakes the upstream token endpoint: a refresh_token grant as S&V's
// client there, answered with status; on 200 a fresh ID token and, when
// rotate is set, a new refresh token.
func gluu(t *testing.T, calls *int32, status int, rotate string, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if id, secret, ok := r.BasicAuth(); !ok || id != "sv-at-gluu" || secret != "g-secret" {
			t.Errorf("upstream client auth = %q/%q/%v", id, secret, ok)
		}
		r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		if seen != nil {
			*seen = append(*seen, r.PostForm.Get("refresh_token"))
		}
		w.WriteHeader(status)
		switch status {
		case 200:
			json.NewEncoder(w).Encode(map[string]string{"id_token": jwtFor(gluuIss, "bob-gluu", time.Now().Add(5*time.Minute)), "refresh_token": rotate})
		case 400:
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withUpstream(keycloakURL, brokerURL, gluuURL string) *exchanger {
	x := newExchanger(keycloakURL, op{Name: "gluu", TokenURL: gluuURL, ClientID: "sv-at-gluu", secret: "g-secret"})
	x.brokerURL = brokerURL
	return x
}

var stored = map[string]string{"id_token": jwtFor(gluuIss, "bob-gluu", time.Now().Add(time.Hour)), "refresh_token": "rt-0"}

func TestUpstreamVouchesWhenTheUserHasAnAccountThere(t *testing.T) {
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 200, "", nil).URL)
	w := check(x, jwtWithExp(time.Now().Add(5*time.Minute)))
	if w.Code != 200 || payload(w.Header().Get("x-id-token")).Iss != gluuIss {
		t.Fatalf("got %d iss %q, want Gluu's ID token", w.Code, payload(w.Header().Get("x-id-token")).Iss)
	}
	if kc != 0 || b != 1 || g != 1 {
		t.Fatalf("calls keycloak=%d broker=%d gluu=%d, want 0 1 1 (renewed at the upstream even though the stored ID token is valid)", kc, b, g)
	}
}

func TestNoAccountAtTheUpstreamFallsToKeycloak(t *testing.T) {
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 400, nil).URL, gluu(t, &g, 200, "", nil).URL)
	w := check(x, jwtWithExp(time.Now().Add(5*time.Minute)))
	if w.Code != 200 || w.Header().Get("x-id-token") != keycloakIDToken || g != 0 {
		t.Fatalf("got %d, gluu calls %d; want Keycloak's ID token and no upstream call", w.Code, g)
	}
}

func TestUnavailableUpstreamFallsToKeycloak(t *testing.T) {
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 503, "", nil).URL)
	if w := check(x, jwtWithExp(time.Now().Add(5*time.Minute))); w.Code != 200 || w.Header().Get("x-id-token") != keycloakIDToken {
		t.Fatalf("503 upstream: got %d, want Keycloak's ID token", w.Code)
	}
	x = withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, "http://127.0.0.1:1")
	if w := check(x, jwtWithExp(time.Now().Add(5*time.Minute))); w.Code != 200 || w.Header().Get("x-id-token") != keycloakIDToken {
		t.Fatalf("unreachable upstream: got %d, want Keycloak's ID token", w.Code)
	}
}

func TestUpstreamRefusalIsFinal(t *testing.T) {
	// revoked or disabled at the upstream: no falling back to Keycloak
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 400, "", nil).URL)
	w := check(x, jwtWithExp(time.Now().Add(5*time.Minute)))
	if w.Code != 403 || w.Header().Get("x-id-token") != "" || kc != 0 {
		t.Fatalf("got %d, keycloak calls %d; want 403 and no fallback", w.Code, kc)
	}
	// no refresh token stored: the upstream can't be asked, so refused too
	x = withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, map[string]string{"id_token": stored["id_token"]}).URL, gluu(t, &g, 200, "", nil).URL)
	if w := check(x, jwtWithExp(time.Now().Add(5*time.Minute))); w.Code != 403 || kc != 0 {
		t.Fatalf("no refresh token: got %d, keycloak calls %d; want 403", w.Code, kc)
	}
}

func TestBrokerErrorFailsClosed(t *testing.T) {
	// kagent not allowed for the upstream (403) is a misconfiguration, not a reason to fall back
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 403, nil).URL, gluu(t, &g, 200, "", nil).URL)
	if w := check(x, jwtWithExp(time.Now().Add(5*time.Minute))); w.Code != 503 || kc != 0 {
		t.Fatalf("got %d, keycloak calls %d; want 503 and no fallback", w.Code, kc)
	}
}

func TestRotatedRefreshTokenIsKeptAndUpstreamAskedAgainAfterTTL(t *testing.T) {
	var kc, b, g int32
	var seen []string
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 200, "rt-1", &seen).URL)
	at := jwtWithExp(time.Now().Add(10 * time.Minute))
	check(x, at)
	check(x, at) // cached
	if g != 1 {
		t.Fatalf("upstream called %d times within the TTL, want 1", g)
	}
	x.now = func() time.Time { return time.Now().Add(upstreamTTL + time.Second) }
	if w := check(x, at); w.Code != 200 {
		t.Fatalf("after the TTL: %d", w.Code)
	}
	if len(seen) != 2 || seen[0] != "rt-0" || seen[1] != "rt-1" {
		t.Fatalf("refresh tokens used %v, want [rt-0 rt-1]", seen)
	}
}

func TestLoadOPs(t *testing.T) {
	dir := t.TempDir()
	for name, s := range map[string]string{"gluu": "g\n", "keycloak": "k"} {
		if err := writeFile(dir+"/"+name, s); err != nil {
			t.Fatal(err)
		}
	}
	kc, ups, err := loadOPs(`[{"name":"gluu","token_url":"https://g/token","client_id":"sv"},{"name":"keycloak","token_url":"http://kc/token","client_id":"kagent"}]`, dir)
	if err != nil || kc.Name != "keycloak" || kc.secret != "k" || len(ups) != 1 || ups[0].secret != "g" {
		t.Fatalf("got %+v %+v %v", kc, ups, err)
	}
	if _, _, err := loadOPs(`[]`, dir); err == nil {
		t.Fatal("empty OPS accepted")
	}
	if _, _, err := loadOPs(`[{"name":"okta","token_url":"x","client_id":"y"}]`, dir); err == nil {
		t.Fatal("missing secret accepted")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
