package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickgamb/solo-lab/apps/relationship-guard/extmcp"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeFGA answers Check and ListObjects from a small table: allowed checks
// as "user relation object" (contextual tuples count as held), objects per
// "user relation type". It records what it was asked.
type fakeFGA struct {
	allowed   map[string]bool
	objects   map[string][]string
	storeGone bool
	asked     []map[string]any
}

func (f *fakeFGA) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, `{"message":"unauthenticated"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/stores":
			id := "s1"
			if f.storeGone {
				id = "s2"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"stores": []map[string]string{{"id": id, "name": "sv-identity/sterling-vance"}}})
			return
		case strings.HasPrefix(r.URL.Path, "/stores/s1/") && f.storeGone:
			http.Error(w, `{"message":"store not found"}`, http.StatusNotFound)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.asked = append(f.asked, in)
		held := map[string]bool{}
		if c, ok := in["contextual_tuples"].(map[string]any); ok {
			for _, k := range c["tuple_keys"].([]any) {
				m := k.(map[string]any)
				held[m["user"].(string)+" "+m["relation"].(string)+" "+m["object"].(string)] = true
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/check"):
			k := in["tuple_key"].(map[string]any)
			key := k["user"].(string) + " " + k["relation"].(string) + " " + k["object"].(string)
			// acts_for for advisors needs the membership the token carries
			ok := f.allowed[key]
			if strings.Contains(key, " acts_for ") && f.allowed[key+" needs advisors"] {
				ok = held[k["user"].(string)+" member group:advisors"]
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": ok})
		case strings.HasSuffix(r.URL.Path, "/list-objects"):
			key := in["user"].(string) + " " + in["relation"].(string) + " " + in["type"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"objects": f.objects[key]})
		default:
			http.NotFound(w, r)
		}
	}))
}

func md(t *testing.T, kv map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(kv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newGuard(srv *httptest.Server) guard {
	return guard{fga: &fga{url: srv.URL, token: "k", store: "sv-identity/sterling-vance", http: &http.Client{Timeout: 2 * time.Second}}}
}

func call(t *testing.T, g guard, user string, groups []any, workload string) *extmcp.McpRequestResult {
	t.Helper()
	res, err := g.CheckRequest(context.Background(), &extmcp.McpRequest{
		Method: "tools/call", ServiceNames: []string{"bob-workspace-mcp-mcp"}, McpRequest: []byte(`{"name":"list_clients"}`),
		MetadataContext: md(t, map[string]any{"user": user, "groups": groups, "workload": workload, "client": "kagent", "server": "bob-workspace"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBobsAgentGetsHisClients(t *testing.T) {
	f := &fakeFGA{
		allowed: map[string]bool{
			"user:bob@sterling.lab acts_for agent:sv-agents.bob-assistant":                true,
			"user:bob@sterling.lab acts_for agent:sv-agents.bob-assistant needs advisors": true,
			"user:bob@sterling.lab can_call mcp_server:bob-workspace":                     true,
		},
		objects: map[string][]string{"user:bob@sterling.lab can_view client": {"client:c-1002", "client:c-1001"}},
	}
	srv := f.server(t)
	defer srv.Close()
	res := call(t, newGuard(srv), "Bob@Sterling.lab", []any{"advisors"}, "sv-agents/bob-assistant")
	if res.GetPass() == nil {
		t.Fatalf("want pass, got %v", res)
	}
	h := res.GetHeaderMutation().GetSet()
	if len(h) != 1 || h[0].GetKey() != headerClients || string(h[0].GetValue()) != "c-1001,c-1002" {
		t.Fatalf("header %v", h)
	}
	// group membership travelled as a contextual tuple, from the token
	if c, ok := f.asked[0]["contextual_tuples"].(map[string]any); !ok || len(c["tuple_keys"].([]any)) != 1 {
		t.Fatalf("contextual tuples: %v", f.asked[0])
	}
}

func TestRefusedWithTheReason(t *testing.T) {
	f := &fakeFGA{allowed: map[string]bool{
		"user:bob@sterling.lab acts_for agent:sv-agents.bob-assistant needs advisors": true,
		"user:bob@sterling.lab acts_for agent:sv-agents.bob-assistant":                true,
	}}
	srv := f.server(t)
	defer srv.Close()
	g := newGuard(srv)
	// not in advisors on this token: the agent doesn't act for him
	res := call(t, g, "bob@sterling.lab", nil, "sv-agents/bob-assistant")
	if e := res.GetError(); e == nil || e.GetCode() != extmcp.AuthorizationError_PERMISSION_DENIED ||
		!strings.Contains(e.GetReason(), "agent:sv-agents.bob-assistant does not act for user:bob@sterling.lab") {
		t.Fatalf("acts_for: %v", res)
	}
	// acts for him, but he may not call the server
	res = call(t, g, "bob@sterling.lab", []any{"advisors"}, "sv-agents/bob-assistant")
	if e := res.GetError(); e == nil || !strings.Contains(e.GetReason(), "may not call bob-workspace") {
		t.Fatalf("can_call: %v", res)
	}
	// no verified user at all
	if e := call(t, g, "", nil, "sv-agents/bob-assistant").GetError(); e == nil || !strings.Contains(e.GetReason(), "no verified user") {
		t.Fatal("no user")
	}
}

func TestAnAIClientIsTheAgent(t *testing.T) {
	if a := (caller{Client: "sv-mcp-client"}).agent(); a != "agent:client.sv-mcp-client" {
		t.Fatal(a)
	}
	if a := (caller{Workload: "sv-agents/bob-assistant", Client: "kagent"}).agent(); a != "agent:sv-agents.bob-assistant" {
		t.Fatal(a)
	}
}

func TestOnlyToolCalls(t *testing.T) {
	res, err := guard{}.CheckRequest(context.Background(), &extmcp.McpRequest{Method: "tools/list"})
	if err != nil || res.GetPass() == nil {
		t.Fatalf("%v %v", res, err)
	}
}

func TestAStoreRecreatedIsLookedUpAgain(t *testing.T) {
	f := &fakeFGA{allowed: map[string]bool{"user:bob@sterling.lab acts_for agent:sv-agents.bob-assistant": false}}
	srv := f.server(t)
	defer srv.Close()
	g := newGuard(srv)
	_ = call(t, g, "bob@sterling.lab", nil, "sv-agents/bob-assistant") // learns s1
	f.storeGone = true                                                 // s1 deleted, s2 created
	if e := call(t, g, "bob@sterling.lab", nil, "sv-agents/bob-assistant").GetError(); e == nil {
		t.Fatal("want a decision from the new store")
	}
}

func TestUnreachableIsAnError(t *testing.T) {
	g := guard{fga: &fga{url: "http://127.0.0.1:1", store: "x", http: &http.Client{Timeout: time.Second}}}
	_, err := g.CheckRequest(context.Background(), &extmcp.McpRequest{Method: "tools/call", ServiceNames: []string{"s"},
		MetadataContext: md(t, map[string]any{"user": "bob@sterling.lab", "workload": "a/b", "server": "s"})})
	if err == nil {
		t.Fatal("want an error: the gateway fails closed")
	}
}

// A read that fails in transit or gets a 5xx is tried once more; a 4xx isn't.
func TestOpenFGARetry(t *testing.T) {
	var calls atomic.Int32
	fail := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(calls.Add(1)) <= fail {
			http.Error(w, `{"message":"database failing over"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"allowed": true}`))
	}))
	defer srv.Close()
	f := &fga{url: srv.URL, http: &http.Client{Timeout: 2 * time.Second}}
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := f.do(context.Background(), http.MethodPost, "/check", map[string]any{}, &out); err != nil || !out.Allowed || calls.Load() != 2 {
		t.Fatalf("one 503, then an answer: %v %v after %d calls", err, out.Allowed, calls.Load())
	}
	calls.Store(0)
	fail = 2
	if err := f.do(context.Background(), http.MethodPost, "/check", map[string]any{}, &out); err == nil || calls.Load() != 2 {
		t.Fatalf("two 503s: refused after two tries, got %v after %d", err, calls.Load())
	}
}
