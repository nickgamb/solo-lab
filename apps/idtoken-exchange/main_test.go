package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func jwtWithExp(exp time.Time) string {
	p, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(p) + ".s"
}

// idp fakes the token endpoint: it checks the exchange request and answers
// with status and body.
func idp(t *testing.T, calls *int32, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
}

func newExchanger(url string) *exchanger {
	return &exchanger{tokenURL: url, clientID: "kagent", clientSecret: "s3cret", hc: http.DefaultClient,
		now: time.Now, cache: map[[32]byte]cached{}}
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
	id := jwtWithExp(time.Now().Add(5 * time.Minute))
	srv := idp(t, &calls, 200, fmt.Sprintf(`{"access_token":%q,"issued_token_type":%q,"token_type":"N_A"}`, id, typeIDToken))
	defer srv.Close()
	x := newExchanger(srv.URL)
	at := jwtWithExp(time.Now().Add(5 * time.Minute))
	for i := 0; i < 2; i++ {
		w := check(x, at)
		if w.Code != 200 || w.Header().Get("x-id-token") != id {
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
			srv := idp(t, &calls, tc.status, tc.body)
			defer srv.Close()
			w := check(newExchanger(srv.URL), jwtWithExp(time.Now().Add(time.Minute)))
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
	srv := idp(t, &calls, 200, fmt.Sprintf(`{"access_token":%q,"issued_token_type":%q}`,
		jwtWithExp(time.Now().Add(time.Hour)), typeIDToken))
	defer srv.Close()
	x := newExchanger(srv.URL)
	at := jwtWithExp(time.Now().Add(30 * time.Second)) // the access token expires first
	check(x, at)
	x.now = func() time.Time { return time.Now().Add(25 * time.Second) } // past exp - 10s
	check(x, at)
	if calls != 2 {
		t.Fatalf("token endpoint called %d times, want 2 (cache ends with the access token)", calls)
	}
}
