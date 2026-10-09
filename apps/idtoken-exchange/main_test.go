package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

func jwtFor(iss, sub string, exp time.Time) string {
	p, _ := json.Marshal(map[string]any{"iss": iss, "sub": sub, "exp": exp.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(p) + ".s"
}

// idp fakes S&V's broker token endpoint. The exchange (counted in calls) is
// checked and answered with status and body; a refresh grant for the refresh
// token "rt-1" answers with idToken.
func idp(t *testing.T, calls *int32, status int, body, idToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); !ok || id != "kagent" || secret != "s3cret" {
			t.Errorf("client auth = %q/%q/%v", id, secret, ok)
		}
		r.ParseForm()
		if r.PostForm.Get("grant_type") == "refresh_token" {
			if r.PostForm.Get("refresh_token") != "rt-1" || r.PostForm.Get("scope") != "openid" {
				t.Errorf("refresh grant = %v", r.PostForm)
			}
			json.NewEncoder(w).Encode(map[string]string{"id_token": idToken, "access_token": "at-2", "refresh_token": "rt-2"})
			return
		}
		atomic.AddInt32(calls, 1)
		for k, want := range map[string]string{
			"grant_type": tokenExchange, "subject_token_type": typeAccessToken,
			"requested_token_type": typeRefreshToken, "scope": "openid",
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

// refreshIssued is the exchange's answer: a refresh token in the user's session
var refreshIssued = fmt.Sprintf(`{"refresh_token":"rt-1","issued_token_type":%q,"token_type":"Bearer"}`, typeRefreshToken)

// keycloakIDToken is what the fake Keycloak issues for the exchange
var keycloakIDToken = jwtFor("https://idp.sterling.lab/realms/sterling-vance", "bob-kc", time.Now().Add(5*time.Minute))

func keycloakOK(t *testing.T, calls *int32) *httptest.Server {
	return idp(t, calls, 200, refreshIssued, keycloakIDToken)
}

func newExchanger(brokerURL string) *exchanger {
	return &exchanger{broker: op{Name: "sterling-vance", TokenURL: brokerURL, ClientID: "kagent", secret: "s3cret"},
		hc: http.DefaultClient, now: time.Now, cache: map[[32]byte]cached{}}
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
		"idp refuses":         {400, `{"error":"invalid_token"}`, 403},
		"idp unavailable":     {503, ``, 503},
		"not a refresh token": {200, fmt.Sprintf(`{"access_token":"x","issued_token_type":%q}`, typeAccessToken), 503},
	} {
		t.Run(name, func(t *testing.T) {
			w := check(newExchanger(idp(t, &calls, tc.status, tc.body, keycloakIDToken).URL), jwtWithExp(time.Now().Add(time.Minute)))
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
	x := newExchanger(idp(t, &calls, 200, refreshIssued, jwtWithExp(time.Now().Add(time.Hour))).URL)
	at := jwtWithExp(time.Now().Add(30 * time.Second)) // the access token expires first
	check(x, at)
	x.now = func() time.Time { return time.Now().Add(25 * time.Second) } // past exp - 10s
	check(x, at)
	if calls != 2 {
		t.Fatalf("token endpoint called %d times, want 2 (cache ends with the access token)", calls)
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
	stream.Send(&extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_RequestTrailers{RequestTrailers: &extproc.HttpTrailers{}}})
	if resp, err := stream.Recv(); err != nil || resp.GetRequestTrailers() == nil {
		t.Fatalf("request trailers phase: %v %v", resp, err)
	}
	stream.Send(&extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_ResponseTrailers{ResponseTrailers: &extproc.HttpTrailers{}}})
	if resp, err := stream.Recv(); err != nil || resp.GetResponseTrailers() == nil {
		t.Fatalf("response trailers phase: %v %v", resp, err)
	}
	stream.CloseSend()
}

func TestNoMetadataTokenIsRefused(t *testing.T) {
	// a token in a header the caller controls is never read
	if w := check(newExchanger("http://unused"), ""); w.Code != 403 {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestCacheIsBounded(t *testing.T) {
	x := newExchanger("http://unused")
	now := time.Now()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.store([32]byte{1}, cached{exp: now.Add(-time.Second)}, now) // expired
	x.store([32]byte{2}, cached{exp: now.Add(time.Second)}, now)  // the oldest live one
	for i := 0; len(x.cache) < maxCached; i++ {
		x.store(sha256.Sum256([]byte(fmt.Sprint(i))), cached{exp: now.Add(time.Hour)}, now)
	}
	x.store([32]byte{3}, cached{exp: now.Add(time.Hour)}, now)
	if _, ok := x.cache[[32]byte{1}]; ok || len(x.cache) != maxCached {
		t.Fatalf("expired entry kept or cache at %d", len(x.cache))
	}
	x.store([32]byte{4}, cached{exp: now.Add(time.Hour)}, now)
	if _, ok := x.cache[[32]byte{2}]; ok || len(x.cache) != maxCached {
		t.Fatalf("oldest entry kept or cache at %d", len(x.cache))
	}
	if _, ok := x.cache[[32]byte{4}]; !ok {
		t.Fatal("new entry not stored")
	}
}
