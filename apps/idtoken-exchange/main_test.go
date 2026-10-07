package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

func jwtWithExp(exp time.Time) string {
	p, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(p) + ".s"
}

// session is Bob's S&V access token for a session from idp ("" for S&V's own login)
func session(idp string, exp time.Time) string {
	p, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "idp": idp})
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
		upstreams: upstreams, hc: http.DefaultClient, now: time.Now, cache: map[[32]byte]cached{},
		active: "keycloak"}
}

// result is what the gateway gets back for one request's headers.
type result struct {
	Code int // 200: continue with x-id-token set; else the immediate response
	id   string
}

func (r result) Header() http.Header {
	h := http.Header{}
	if r.id != "" {
		h.Set(idTokenHeader, r.id)
	}
	return h
}

// check runs one request's headers through the processor, with subject as
// the verified token in the metadata ("" for none).
func check(x *exchanger, subject string) result {
	md := &core.Metadata{FilterMetadata: map[string]*structpb.Struct{}}
	if subject != "" {
		md.FilterMetadata[metaNamespace], _ = structpb.NewStruct(map[string]any{metaToken: subject})
	}
	resp := (&processor{x: x}).requestHeaders(context.Background(), &extproc.ProcessingRequest{MetadataContext: md,
		Request: &extproc.ProcessingRequest_RequestHeaders{RequestHeaders: &extproc.HttpHeaders{}}})
	if ir := resp.GetImmediateResponse(); ir != nil {
		return result{Code: int(ir.GetStatus().GetCode())}
	}
	for _, h := range resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders() {
		if h.GetHeader().GetKey() == idTokenHeader && h.GetAppendAction() == core.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
			return result{Code: 200, id: string(h.GetHeader().GetRawValue())}
		}
	}
	return result{}
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
		if id, secret, ok := r.BasicAuth(); !ok || id != "xaa-egress" || secret != "e-secret" {
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
// client there (auth checks it), answered with status; on 200 a fresh ID
// token. seen collects the refresh tokens presented.
func gluu(t *testing.T, calls *int32, status int, auth func(*http.Request) error, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		r.ParseForm()
		if auth == nil {
			auth = basicAuth("sv-at-gluu", "g-secret")
		}
		if err := auth(r); err != nil {
			t.Errorf("upstream client auth: %v", err)
		}
		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		if seen != nil {
			*seen = append(*seen, r.PostForm.Get("refresh_token"))
		}
		w.WriteHeader(status)
		switch status {
		case 200:
			json.NewEncoder(w).Encode(map[string]string{"id_token": jwtFor(gluuIss, "bob-gluu", time.Now().Add(5*time.Minute))})
		case 400:
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func basicAuth(id, secret string) func(*http.Request) error {
	return func(r *http.Request) error {
		if u, p, ok := r.BasicAuth(); !ok || u != id || p != secret {
			return fmt.Errorf("basic = %q/%q/%v", u, p, ok)
		}
		return nil
	}
}

func withUpstream(keycloakURL, brokerURL, gluuURL string) *exchanger {
	x := newExchanger(keycloakURL, op{Name: "gluu", TokenURL: gluuURL, ClientID: "sv-at-gluu", secret: "g-secret"})
	x.brokerURL = brokerURL
	x.broker = op{Name: "broker", TokenURL: keycloakURL, ClientID: "xaa-egress", secret: "e-secret"}
	x.active = "gluu"
	return x
}

var stored = map[string]string{"id_token": jwtFor(gluuIss, "bob-gluu", time.Now().Add(time.Hour)), "refresh_token": "rt-0"}

func TestUpstreamVouchesForItsSession(t *testing.T) {
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 200, nil, nil).URL)
	w := check(x, session("gluu", time.Now().Add(5*time.Minute)))
	if w.Code != 200 || payload(w.Header().Get("x-id-token")).Iss != gluuIss {
		t.Fatalf("got %d iss %q, want Gluu's ID token", w.Code, payload(w.Header().Get("x-id-token")).Iss)
	}
	if kc != 0 || b != 1 || g != 1 {
		t.Fatalf("calls keycloak=%d broker=%d gluu=%d, want 0 1 1 (renewed at the upstream)", kc, b, g)
	}
}

func TestLocalSessionKeycloakVouches(t *testing.T) {
	// S&V's own login, even while Gluu is active: S&V's Keycloak, Gluu untouched
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 200, nil, nil).URL)
	w := check(x, session("", time.Now().Add(5*time.Minute)))
	if w.Code != 200 || w.Header().Get("x-id-token") != keycloakIDToken || b+g != 0 {
		t.Fatalf("got %d, broker %d, gluu %d; want Keycloak's ID token only", w.Code, b, g)
	}
}

func TestNonIssuingUpstreamSessionKeycloakVouches(t *testing.T) {
	// Auth0 doesn't issue ID-JAGs: the broker vouches for its sessions while it is active
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 200, nil, nil).URL)
	x.setActive("auth0")
	if w := check(x, session("auth0", time.Now().Add(5*time.Minute))); w.Code != 200 || w.Header().Get("x-id-token") != keycloakIDToken || b+g != 0 {
		t.Fatalf("got %d, broker %d, gluu %d; want Keycloak's ID token only", w.Code, b, g)
	}
}

func TestNoStoredTokensFailsClosed(t *testing.T) {
	// a Gluu session with nothing at the broker is a fault, not a reason for Keycloak to vouch
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 400, nil).URL, gluu(t, &g, 200, nil, nil).URL)
	if w := check(x, session("gluu", time.Now().Add(5*time.Minute))); w.Code != 503 || kc+g != 0 {
		t.Fatalf("got %d, keycloak %d, gluu %d; want 503 and nothing else called", w.Code, kc, g)
	}
}

func TestActiveUpstreamUnavailableFailsUntilFailover(t *testing.T) {
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 503, nil, nil).URL)
	at := session("gluu", time.Now().Add(5*time.Minute))
	if w := check(x, at); w.Code != 503 || kc != 0 {
		t.Fatalf("503 upstream: got %d, keycloak calls %d; want 503 and no fallback", w.Code, kc)
	}
	x.setActive("keycloak")
	b, g = 0, 0
	// the Gluu session can't be vouched for any more; a new local sign-in can
	if w := check(x, at); w.Code != 403 || b+g+kc != 0 {
		t.Fatalf("Gluu session after failover: %d (broker %d gluu %d keycloak %d); want 403, nothing called", w.Code, b, g, kc)
	}
	if w := check(x, session("", time.Now().Add(5*time.Minute))); w.Code != 200 || w.Header().Get("x-id-token") != keycloakIDToken || b+g != 0 {
		t.Fatalf("local session after failover: %d; want Keycloak's ID token, Gluu untouched", w.Code)
	}
}

func TestFailoverDropsCachedUpstreamTokens(t *testing.T) {
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 200, nil, nil).URL)
	at := session("gluu", time.Now().Add(5*time.Minute))
	if w := check(x, at); payload(w.Header().Get("x-id-token")).Iss != gluuIss {
		t.Fatal("want Gluu's ID token while gluu is active")
	}
	x.setActive("keycloak")
	if w := check(x, at); w.Code != 403 || w.id != "" {
		t.Fatalf("after failover got %d with a token: the cached Gluu ID token must not be handed out", w.Code)
	}
}

func TestUnknownActiveTierFailsClosed(t *testing.T) {
	var kc int32
	x := newExchanger(keycloakOK(t, &kc).URL)
	x.active = ""
	if w := check(x, jwtWithExp(time.Now().Add(time.Minute))); w.Code != 503 || kc != 0 {
		t.Fatalf("got %d, keycloak calls %d; want 503", w.Code, kc)
	}
}

func TestWatchActiveReadsTheControllersDecision(t *testing.T) {
	var active atomic.Value
	active.Store("gluu")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/continuity.lab.solo.io/v1alpha1/namespaces/sv-identity/identitycontinuities/sterling-vance" ||
			r.Header.Get("Authorization") != "Bearer sa-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"status":{"active":%q}}`, active.Load())
	}))
	defer api.Close()
	tok := t.TempDir() + "/token"
	if err := writeFile(tok, "sa-token\n"); err != nil {
		t.Fatal(err)
	}
	x := newExchanger("http://unused")
	x.active = ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go x.watchActive(ctx, api.Client(), api.URL, tok, "sv-identity/sterling-vance", 10*time.Millisecond)
	waitFor := func(want string) {
		for i := 0; i < 200; i++ {
			x.mu.Lock()
			got := x.active
			x.mu.Unlock()
			if got == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("active never became %q", want)
	}
	waitFor("gluu")
	active.Store("keycloak")
	waitFor("keycloak")
}

func TestUpstreamRefusalIsFinal(t *testing.T) {
	// revoked or disabled at the upstream: no falling back to Keycloak
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 400, nil, nil).URL)
	w := check(x, session("gluu", time.Now().Add(5*time.Minute)))
	if w.Code != 403 || w.Header().Get("x-id-token") != "" || kc != 0 {
		t.Fatalf("got %d, keycloak calls %d; want 403 and no fallback", w.Code, kc)
	}
	// no refresh token stored: the upstream can't be asked, so refused too
	x = withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, map[string]string{"id_token": stored["id_token"]}).URL, gluu(t, &g, 200, nil, nil).URL)
	if w := check(x, session("gluu", time.Now().Add(5*time.Minute))); w.Code != 403 || kc != 0 {
		t.Fatalf("no refresh token: got %d, keycloak calls %d; want 403", w.Code, kc)
	}
}

func TestBrokerErrorFailsClosed(t *testing.T) {
	// kagent not allowed for the upstream (403) is a misconfiguration, not a reason to fall back
	var kc, b, g int32
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 403, nil).URL, gluu(t, &g, 200, nil, nil).URL)
	if w := check(x, session("gluu", time.Now().Add(5*time.Minute))); w.Code != 503 || kc != 0 {
		t.Fatalf("got %d, keycloak calls %d; want 503 and no fallback", w.Code, kc)
	}
}

func TestStoredRefreshTokenAndUpstreamAskedAgainAfterTTL(t *testing.T) {
	var kc, b, g int32
	var seen []string
	x := withUpstream(keycloakOK(t, &kc).URL, broker(t, &b, 200, stored).URL, gluu(t, &g, 200, nil, &seen).URL)
	at := session("gluu", time.Now().Add(10*time.Minute))
	check(x, at)
	check(x, at) // cached
	if g != 1 {
		t.Fatalf("upstream called %d times within the TTL, want 1", g)
	}
	x.now = func() time.Time { return time.Now().Add(upstreamTTL + time.Second) }
	if w := check(x, at); w.Code != 200 {
		t.Fatalf("after the TTL: %d", w.Code)
	}
	if len(seen) != 2 || seen[0] != "rt-0" || seen[1] != "rt-0" {
		t.Fatalf("refresh tokens used %v, want the stored one each time", seen)
	}
}

func TestPrivateKeyJWTAtTheUpstream(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	ck := &clientKey{key: k, kid: thumbprint(&k.PublicKey)}
	var kc, b, g int32
	verify := func(r *http.Request) error {
		if _, _, ok := r.BasicAuth(); ok {
			return errors.New("basic auth sent with private_key_jwt")
		}
		if r.PostForm.Get("client_assertion_type") != assertionType || r.PostForm.Get("client_id") != "sv-at-gluu" {
			return fmt.Errorf("form %v", r.PostForm)
		}
		parts := strings.Split(r.PostForm.Get("client_assertion"), ".")
		if len(parts) != 3 {
			return errors.New("not a JWT")
		}
		d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, d[:], sig); err != nil {
			return err
		}
		var h map[string]string
		hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
		json.Unmarshal(hb, &h)
		var c map[string]any
		cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
		json.Unmarshal(cb, &c)
		if h["kid"] != ck.kid || c["iss"] != "sv-at-gluu" || c["sub"] != "sv-at-gluu" || c["aud"] != gluuIss || c["jti"] == "" {
			return fmt.Errorf("header %v claims %v", h, c)
		}
		if exp := int64(c["exp"].(float64)); exp-time.Now().Unix() > 61 {
			return fmt.Errorf("assertion lives %ds", exp-time.Now().Unix())
		}
		return nil
	}
	up := gluu(t, &g, 200, verify, nil)
	x := newExchanger(keycloakOK(t, &kc).URL, op{Name: "gluu", Issuer: gluuIss, TokenURL: up.URL, ClientID: "sv-at-gluu", Auth: "private_key_jwt", key: ck})
	x.brokerURL = broker(t, &b, 200, stored).URL
	x.broker = op{Name: "broker", ClientID: "xaa-egress", secret: "e-secret"}
	x.active = "gluu"
	if w := check(x, session("gluu", time.Now().Add(5*time.Minute))); w.Code != 200 || payload(w.id).Iss != gluuIss {
		t.Fatalf("got %d", w.Code)
	}
}

func TestProcessStream(t *testing.T) {
	// over gRPC: request headers get x-id-token; other phases pass through
	var kc int32
	x := newExchanger(keycloakOK(t, &kc).URL)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	extproc.RegisterExternalProcessorServer(srv, &processor{x: x})
	go srv.Serve(lis)
	defer srv.Stop()
	conn, err := grpc.NewClient("passthrough:///buf", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := extproc.NewExternalProcessorClient(conn).Process(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	md, _ := structpb.NewStruct(map[string]any{metaToken: jwtWithExp(time.Now().Add(time.Minute))})
	stream.Send(&extproc.ProcessingRequest{MetadataContext: &core.Metadata{FilterMetadata: map[string]*structpb.Struct{metaNamespace: md}},
		Request: &extproc.ProcessingRequest_RequestHeaders{RequestHeaders: &extproc.HttpHeaders{}}})
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	set := resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	if len(set) != 1 || set[0].GetHeader().GetKey() != idTokenHeader || string(set[0].GetHeader().GetRawValue()) != keycloakIDToken {
		t.Fatalf("response %v", resp)
	}
	stream.Send(&extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extproc.HttpHeaders{}}})
	if resp, err := stream.Recv(); err != nil || resp.GetResponseHeaders() == nil {
		t.Fatalf("response headers phase: %v %v", resp, err)
	}
	stream.CloseSend()
}

func TestNoMetadataTokenIsRefused(t *testing.T) {
	// a token in a header the caller controls is never read
	if w := check(newExchanger("http://unused"), ""); w.Code != 403 {
		t.Fatalf("got %d, want 403", w.Code)
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
	if _, _, err := loadOPs(`[{"name":"gluu","token_url":"x","client_id":"y","auth":"private_key_jwt"},{"name":"keycloak","token_url":"x","client_id":"k"}]`, dir); err == nil {
		t.Fatal("private_key_jwt without a key accepted")
	}
	if _, _, err := loadOPs(`[{"name":"keycloak","token_url":"x","client_id":"k","auth":"tls_client_auth"}]`, dir); err == nil {
		t.Fatal("unknown auth method accepted")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }

func TestStaleContinuityStateStopsAnswers(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer api.Close()
	tok := t.TempDir() + "/token"
	if err := writeFile(tok, "sa-token"); err != nil {
		t.Fatal(err)
	}
	old := staleAfter
	staleAfter = 20 * time.Millisecond
	defer func() { staleAfter = old }()
	x := newExchanger("http://unused") // active "keycloak", from an earlier read
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go x.watchActive(ctx, api.Client(), api.URL, tok, "sv-identity/sterling-vance", 5*time.Millisecond)
	for i := 0; i < 200; i++ {
		x.mu.Lock()
		a := x.active
		x.mu.Unlock()
		if a == "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("active tier kept after the continuity state went stale")
}
