package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEdgesWithoutResolvedCaller(t *testing.T) {
	cases := []struct {
		t    Traffic
		want string
	}{
		{Traffic{Source: "a", Via: "gw", Target: "b"}, "a>gw gw>b"},
		{Traffic{Via: "gw", Target: "b"}, "gw>b"}, // the caller didn't resolve; the gateway's hop still counts
		{Traffic{Source: "a", Target: "b"}, "a>b"},
		{Traffic{Target: "b"}, ""},
	}
	for _, c := range cases {
		if got := strings.Join(c.t.edges(), " "); got != c.want {
			t.Errorf("%+v: edges %q, want %q", c.t, got, c.want)
		}
	}
}

// Totals follow arrival order: a record whose own timestamp is old (a
// delayed export) still counts once it arrives, and the scan doesn't stop
// at it.
func TestRatesCountByArrival(t *testing.T) {
	s := NewTrafficStore(16, NewHub(), func() *Index { return &Index{} })
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	s.Add(Traffic{Kind: "http", Outcome: "ok", Source: "a", Target: "b"})
	s.Add(Traffic{Kind: "http", Outcome: "denied", Source: "a", Target: "b", Time: old})
	s.Add(Traffic{Kind: "http", Outcome: "ok", Source: "a", Target: "b"})
	edges, rps, errRate := s.Rates()
	if got := rps * window.Seconds(); got != 3 {
		t.Errorf("requests in window: %v, want 3", got)
	}
	if errRate < 0.33 || errRate > 0.34 {
		t.Errorf("error rate %v, want 1/3", errRate)
	}
	if e := edges["a>b"]; e.RPS*window.Seconds() != 3 {
		t.Errorf("edge a>b: %+v", e)
	}
}

func TestResourceAttributesKept(t *testing.T) {
	a := map[string]string{"k8s.namespace.name": "from-record"}
	res := map[string]string{"k8s.namespace.name": "sv-mcp", "k8s.pod.name": "p"}
	for k, v := range res { // as ServeOTLP merges them
		if _, ok := a["resource."+k]; !ok {
			a["resource."+k] = v
		}
	}
	if a["resource.k8s.namespace.name"] != "sv-mcp" || a["k8s.namespace.name"] != "from-record" {
		t.Errorf("resource attribute lost: %v", a)
	}
}

func TestTokensRedactCredentialsInURLs(t *testing.T) {
	a := map[string]string{
		"http.path": "/realms/sterling-vance/broker/auth0/endpoint?state=abc&code=SplxlOBeZQQYbYS6WxSbIA&session_state=xyz",
		"redirect":  "https://app.example/cb#access_token=opaque123&token_type=Bearer",
		"login":     "/token?client_secret=hunter2&grant_type=client_credentials",
	}
	tokens(a)
	for k, v := range a {
		for _, secret := range []string{"SplxlOBeZQQYbYS6WxSbIA", "xyz", "opaque123", "hunter2"} {
			if strings.Contains(v, secret) {
				t.Errorf("%s still carries %q: %s", k, secret, v)
			}
		}
	}
	if !strings.Contains(a["http.path"], "state=abc") || !strings.Contains(a["login"], "grant_type=client_credentials") {
		t.Errorf("non-secret parameters must survive: %v", a)
	}
}

// A browser too slow to take every event still gets the latest snapshot of
// each topic once it catches up.
func TestHubResendsDroppedSnapshots(t *testing.T) {
	h := NewHub()
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	for len(h.subs) == 0 { // wait for the subscription
		time.Sleep(5 * time.Millisecond)
	}
	// overflow the browser's buffer with traffic, then publish a graph
	for i := 0; i < 600; i++ {
		h.Publish("traffic", i)
	}
	h.Publish("graph", map[string]string{"v": "latest"})
	found := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"v":"latest"`) {
				found <- true
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(3 * time.Second):
		t.Fatal("the latest graph never reached a slow browser")
	}
}
