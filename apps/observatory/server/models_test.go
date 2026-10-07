package main

import (
	"encoding/json"
	"fmt"
	"io"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var gvrAGWPolicy = schema.GroupVersionResource{Group: "agentgateway.dev", Version: "v1alpha1", Resource: "agentgatewaypolicies"}

// fakeModels is the slice of the Kubernetes API the model routes use: the
// route, the backend (resourceVersion checked on update), the two policies
// (merge patches recorded), Secrets applied. OSS: the enterprise group isn't
// served.
type fakeModels struct {
	mu       sync.Mutex
	be       map[string]any
	rv       int
	patches  map[string][]string // policy -> merge patches
	secrets  []string
	secretAs []string
	labelled []bool
	updates  int
}

func (f *fakeModels) serve(t *testing.T) *Models {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		const (
			be   = "/apis/agentgateway.dev/v1alpha1/namespaces/agentgateway-system/agentgatewaybackends/llm"
			pols = "/apis/agentgateway.dev/v1alpha1/namespaces/agentgateway-system/agentgatewaypolicies/"
			secs = "/api/v1/namespaces/agentgateway-system/secrets/"
		)
		switch {
		case r.URL.Path == "/apis/gateway.networking.k8s.io/v1/namespaces/agentgateway-system/httproutes/llm":
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
				"metadata": map[string]any{"name": "llm", "namespace": "agentgateway-system"},
				"spec": map[string]any{"parentRefs": []any{map[string]any{"name": "ai-gateway"}},
					"rules": []any{map[string]any{"backendRefs": []any{map[string]any{"group": "agentgateway.dev", "kind": "AgentgatewayBackend", "name": "llm"}}}}}})
		case r.URL.Path == "/apis/gateway.networking.k8s.io/v1/namespaces/agentgateway-system/httproutes/llm-external":
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
				"metadata": map[string]any{"name": "llm-external", "namespace": "agentgateway-system"},
				"spec": map[string]any{"hostnames": []any{"llm.sv.lab"},
					"rules": []any{map[string]any{"backendRefs": []any{map[string]any{"group": "agentgateway.dev", "kind": "AgentgatewayBackend", "name": "llm"}}}}}})
		case r.URL.Path == "/api/v1/namespaces/agentgateway-system":
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "agentgateway-system",
				"annotations": map[string]any{"lab.solo.io/llm-provider": "ollama", "lab.solo.io/llm-fallback": ""}}})
		case r.URL.Path == be && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(f.be)
		case r.URL.Path == be && r.Method == http.MethodPut:
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if in["metadata"].(map[string]any)["resourceVersion"] != fmt.Sprint(f.rv) {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Conflict", "code": 409,
					"message": "the object has been modified"})
				return
			}
			f.rv++
			f.updates++
			in["metadata"].(map[string]any)["resourceVersion"] = fmt.Sprint(f.rv)
			f.be = in
			json.NewEncoder(w).Encode(in)
		case strings.HasPrefix(r.URL.Path, pols):
			name := strings.TrimPrefix(r.URL.Path, pols)
			pol := map[string]any{"apiVersion": "agentgateway.dev/v1alpha1", "kind": "AgentgatewayPolicy",
				"metadata": map[string]any{"name": name, "namespace": "agentgateway-system"}}
			switch name {
			case "llm-backend":
				pol["spec"] = map[string]any{"backend": map[string]any{"health": map[string]any{
					"eviction": map[string]any{"consecutiveFailures": 1, "duration": "30s"}}}}
			case "llm-callers":
				pol["spec"] = map[string]any{"traffic": map[string]any{
					"authorization": map[string]any{"action": "Allow", "policy": map[string]any{"matchExpressions": []any{
						`(source.identity.namespace == "sv-agents" && source.identity.serviceAccount in ["bob-assistant", "advisor-desk"]) || (source.identity.namespace == "kagent" && source.identity.serviceAccount == "kagent-ops")`}}},
					"retry": map[string]any{"attempts": 1, "codes": []any{429, 500, 502, 503, 504}}}}
			default:
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
				return
			}
			if r.Method == http.MethodPatch {
				b, _ := io.ReadAll(r.Body)
				if f.patches == nil {
					f.patches = map[string][]string{}
				}
				f.patches[name] = append(f.patches[name], string(b))
			}
			json.NewEncoder(w).Encode(pol)
		case strings.HasPrefix(r.URL.Path, secs) && r.Method == http.MethodPatch:
			f.secrets = append(f.secrets, strings.TrimPrefix(r.URL.Path, secs))
			f.secretAs = append(f.secretAs, r.Header.Get("Impersonate-User"))
			b, _ := io.ReadAll(r.Body)
			f.labelled = append(f.labelled, strings.Contains(string(b), `"`+modelCredsLabel+`":"true"`))
			w.Write(b)
		case strings.HasPrefix(r.URL.Path, "/apis/enterpriseagentgateway.solo.io/"), strings.HasPrefix(r.URL.Path, "/apis/ratelimit.solo.io/"):
			w.WriteHeader(http.StatusNotFound) // OSS: not served
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	k := &Kube{cfg: cfg, dyn: dyn, disco: dc, kinds: map[schema.GroupVersionResource]string{gvrAGWBackend: "AgentgatewayBackend", gvrAGWPolicy: "AgentgatewayPolicy"}}
	return &Models{k: k, res: &Resources{k: k, admin: "admins"}, traffic: NewTrafficStore(64, NewHub(), func() *Index { return &Index{} }),
		index: func() *Index { return &Index{} }}
}

func ollamaBackend() map[string]any {
	return map[string]any{"apiVersion": "agentgateway.dev/v1alpha1", "kind": "AgentgatewayBackend",
		"metadata": map[string]any{"name": "llm", "namespace": "agentgateway-system", "resourceVersion": "3"},
		"spec": map[string]any{"ai": map[string]any{"provider": map[string]any{"openai": map[string]any{"model": "qwen3.8:27b"},
			"host": "host.docker.internal", "port": 11434}}}}
}

func groupsBackend() map[string]any {
	return map[string]any{"apiVersion": "agentgateway.dev/v1alpha1", "kind": "AgentgatewayBackend",
		"metadata": map[string]any{"name": "llm", "namespace": "agentgateway-system", "resourceVersion": "3"},
		"spec": map[string]any{"ai": map[string]any{"groups": []any{
			map[string]any{"providers": []any{map[string]any{"name": "primary", "custom": map[string]any{"model": "qwen3.8:27b", "providerOverride": "ollama",
				"formats": []any{map[string]any{"type": "Completions", "path": "/v1/chat/completions"}}}, "host": "host.docker.internal", "port": 11434}}},
			map[string]any{"providers": []any{map[string]any{"name": "fallback", "anthropic": map[string]any{"model": "claude-sonnet-5"},
				"policies": map[string]any{"auth": map[string]any{"secretRef": map[string]any{"name": "llm-anthropic"}}}}}},
		}}}}
}

const rules = `"rules":{"on5xx":true,"on429":false,"consecutiveFailures":2,"duration":"45s","retryAttempts":0}`

// Two or more providers are written as one group each, in order, with their
// keys per provider; one is written as spec.ai.provider with the backend's
// auth. Only the failover fields of the policies are patched.
func TestPutModelsShapes(t *testing.T) {
	f := &fakeModels{rv: 3, be: ollamaBackend()}
	m := f.serve(t)
	put := func(rv, providers string) *httptest.ResponseRecorder {
		return call(m.Put, "/api/models/{ns}/{name}", http.MethodPut, "/api/models/agentgateway-system/llm",
			`{"resourceVersion":"`+rv+`","providers":`+providers+`,`+rules+`}`)
	}
	w := put("3", `[{"name":"ollama","kind":"ollama","model":"qwen3.8:27b","host":"host.docker.internal","port":11434},
		{"name":"anthropic","kind":"anthropic","model":"claude-sonnet-5","secret":"model-anthropic"}]`)
	if w.Code != http.StatusOK {
		t.Fatalf("groups: %d %s", w.Code, w.Body)
	}
	if obj(f.be, "spec", "ai", "provider") != nil {
		t.Fatal("spec.ai.provider kept next to the groups")
	}
	groups := slice(f.be, "spec", "ai", "groups")
	if len(groups) != 2 {
		t.Fatalf("groups %v", groups)
	}
	first := slice(groups[0].(map[string]any), "providers")[0].(map[string]any)
	if str(first, "custom", "providerOverride") != "ollama" || str(first, "custom", "model") != "qwen3.8:27b" || len(slice(first, "custom", "formats")) != 2 ||
		first["openai"] != nil || first["host"] != "host.docker.internal" {
		t.Fatalf("ollama provider %v: a custom provider speaking both APIs", first)
	}
	second := slice(groups[1].(map[string]any), "providers")[0].(map[string]any)
	if str(second, "name") != "anthropic" || str(second, "anthropic", "model") != "claude-sonnet-5" ||
		str(second, "policies", "auth", "secretRef", "name") != "model-anthropic" || second["host"] != nil {
		t.Fatalf("second provider %v", second)
	}
	var v ModelView
	json.Unmarshal(w.Body.Bytes(), &v)
	if v.Backend == nil || v.Backend.ResourceVersion != "4" || len(v.Providers) != 2 || v.Providers[0].Kind != "ollama" || v.Providers[1].Group != 1 || v.Providers[1].Secret != "model-anthropic" {
		t.Fatalf("view after save %+v", v)
	}
	if p := f.patches["llm-backend"]; len(p) != 1 || !strings.Contains(p[0], `"unhealthyCondition":null`) || strings.Contains(p[0], "promptGuard") ||
		!strings.Contains(p[0], `"consecutiveFailures":2`) || !strings.Contains(p[0], `"duration":"45s"`) {
		t.Fatalf("llm-backend patches %v", p)
	}
	if p := f.patches["llm-callers"]; len(p) != 1 || p[0] != `{"spec":{"traffic":{"retry":null}}}` {
		t.Fatalf("llm-callers patches %v: only the retry, removed", p)
	}

	w = put("4", `[{"name":"anthropic","kind":"anthropic","model":"claude-sonnet-5","secret":"model-anthropic"}]`)
	if w.Code != http.StatusOK {
		t.Fatalf("single: %d %s", w.Code, w.Body)
	}
	if slice(f.be, "spec", "ai", "groups") != nil || str(f.be, "spec", "ai", "provider", "anthropic", "model") != "claude-sonnet-5" ||
		str(f.be, "spec", "policies", "auth", "secretRef", "name") != "model-anthropic" {
		t.Fatalf("single provider spec %v", f.be["spec"])
	}
	if w := put("4", `[{"name":"ollama","kind":"openai","model":"m","host":"h","port":1}]`); w.Code != http.StatusConflict || w.Body.Len() > 100 {
		t.Fatalf("stale resourceVersion: %d %q, want 409 and a short text", w.Code, w.Body)
	}
	if w := call(m.Put, "/api/models/{ns}/{name}", http.MethodPut, "/api/models/agentgateway-system/other",
		`{"providers":[{"name":"a","kind":"openai","model":"m"}],`+rules+`}`); w.Code != http.StatusNotFound {
		t.Fatalf("a backend the route doesn't use: %d, want 404", w.Code)
	}
}

func TestPutModelsValidates(t *testing.T) {
	f := &fakeModels{rv: 3, be: ollamaBackend()}
	m := f.serve(t)
	ok := `{"name":"a","kind":"openai","model":"m"}`
	r := `"on5xx":true,"on429":true,"consecutiveFailures":1,"duration":"30s","retryAttempts":1`
	for name, body := range map[string]string{
		"no providers":      `{"providers":[],"rules":{` + r + `}}`,
		"five providers":    `{"providers":[` + strings.Repeat(ok+",", 4) + ok + `],"rules":{` + r + `}}`,
		"a name twice":      `{"providers":[` + ok + `,` + ok + `],"rules":{` + r + `}}`,
		"another kind":      `{"providers":[{"name":"a","kind":"gemini","model":"m"}],"rules":{` + r + `}}`,
		"no model":          `{"providers":[{"name":"a","kind":"openai","model":" "}],"rules":{` + r + `}}`,
		"ollama, no host":   `{"providers":[{"name":"a","kind":"ollama","model":"m"}],"rules":{` + r + `}}`,
		"a host, no port":   `{"providers":[{"name":"a","kind":"openai","model":"m","host":"ollama"}],"rules":{` + r + `}}`,
		"a bad host":        `{"providers":[{"name":"a","kind":"openai","model":"m","host":"http://x","port":80}],"rules":{` + r + `}}`,
		"a bad name":        `{"providers":[{"name":"A_b","kind":"openai","model":"m"}],"rules":{` + r + `}}`,
		"no condition":      `{"providers":[` + ok + `],"rules":{"consecutiveFailures":1,"duration":"30s"}}`,
		"too long out":      `{"providers":[` + ok + `],"rules":{"on5xx":true,"consecutiveFailures":1,"duration":"2h"}}`,
		"not a duration":    `{"providers":[` + ok + `],"rules":{"on5xx":true,"consecutiveFailures":1,"duration":"1.5s"}}`,
		"too many failures": `{"providers":[` + ok + `],"rules":{"on5xx":true,"consecutiveFailures":11,"duration":"30s"}}`,
		"too many retries":  `{"providers":[` + ok + `],"rules":{"on5xx":true,"consecutiveFailures":1,"duration":"30s","retryAttempts":4}}`,
		"no rules":          `{"providers":[` + ok + `]}`,
	} {
		if w := call(m.Put, "/api/models/{ns}/{name}", http.MethodPut, "/api/models/agentgateway-system/llm", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, w.Code)
		}
	}
	if f.updates != 0 {
		t.Fatalf("%d updates from invalid input", f.updates)
	}
}

// A provider's key is written only under a name a provider refers to, as the
// signed-in admin, labelled.
func TestPutModelSecretOnlyForReferencedNames(t *testing.T) {
	f := &fakeModels{rv: 3, be: groupsBackend()}
	m := f.serve(t)
	for _, c := range []struct {
		ns, name, body string
		want           int
	}{
		{"agentgateway-system", "llm-anthropic", `{"value":"sk-ant-x"}`, http.StatusOK},
		{"agentgateway-system", "enterprise-agentgateway-license", `{"value":"x"}`, http.StatusConflict},
		{"agentgateway-system", "llm-anthropic", `{"value":"  "}`, http.StatusBadRequest},
		{"agentgateway-system", "Bad_Name", `{"value":"x"}`, http.StatusBadRequest},
		{"sv-identity", "llm-anthropic", `{"value":"x"}`, http.StatusNotFound},
	} {
		w := call(m.PutSecret, "/api/models/{ns}/secrets/{name}", http.MethodPut, "/api/models/"+c.ns+"/secrets/"+c.name, c.body)
		if w.Code != c.want {
			t.Errorf("%s/%s %s: %d, want %d", c.ns, c.name, c.body, w.Code, c.want)
		}
		if w.Code == http.StatusOK && (strings.Contains(w.Body.String(), "sk-ant") || !strings.Contains(w.Body.String(), `"keys":["Authorization"]`)) {
			t.Errorf("response %s: keys only", w.Body)
		}
	}
	if !slices.Equal(f.secrets, []string{"llm-anthropic"}) || !slices.Equal(f.secretAs, []string{impersonatedUser}) || slices.Contains(f.labelled, false) {
		t.Fatalf("secrets %v written as %v, labelled %v", f.secrets, f.secretAs, f.labelled)
	}
}

// A cut points the provider at a closed port and keeps its own host and port
// on the backend; a restore puts them back. Both are idempotent and show in
// the feed with who did it.
func TestModelOutageRoundTrip(t *testing.T) {
	f := &fakeModels{rv: 3, be: groupsBackend()}
	m := f.serve(t)
	outage := func(provider string, cut bool) int {
		return call(m.Outage, "/api/models/{ns}/{name}/outage", http.MethodPost, "/api/models/agentgateway-system/llm/outage",
			fmt.Sprintf(`{"provider":%q,"cut":%v}`, provider, cut)).Code
	}
	prov := func(i int) map[string]any {
		return slice(slice(f.be, "spec", "ai", "groups")[i].(map[string]any), "providers")[0].(map[string]any)
	}
	if c := outage("primary", true); c != http.StatusOK {
		t.Fatalf("cut: %d", c)
	}
	if p := prov(0); p["host"] != "host.docker.internal" || fmt.Sprint(p["port"]) != "1" {
		t.Fatalf("cut ollama-style provider: %v, want its host on port 1", p)
	}
	if c := outage("primary", true); c != http.StatusOK || f.updates != 1 {
		t.Fatalf("cut again: %d, %d updates, want no change", c, f.updates)
	}
	v, err := m.view(t.Context(), m.k.dyn)
	if err != nil || v.Providers[0].Outage == nil || v.Providers[0].Port != 11434 || v.Providers[0].Outage.By != "nick" {
		t.Fatalf("view while cut: %+v %v: the provider's own port, and who cut it", v.Providers, err)
	}
	if c := outage("fallback", true); c != http.StatusOK {
		t.Fatalf("cut cloud: %d", c)
	}
	if p := prov(1); p["host"] != unroutable || fmt.Sprint(p["port"]) != "1" {
		t.Fatalf("cut cloud provider: %v, want %s:1", p, unroutable)
	}
	if c := outage("primary", false); c != http.StatusOK {
		t.Fatalf("restore: %d", c)
	}
	if c := outage("fallback", false); c != http.StatusOK {
		t.Fatalf("restore cloud: %d", c)
	}
	if p := prov(0); p["host"] != "host.docker.internal" || fmt.Sprint(p["port"]) != "11434" {
		t.Fatalf("restored: %v", p)
	}
	if p := prov(1); p["host"] != nil || p["port"] != nil {
		t.Fatalf("restored cloud provider %v: no host or port, as before", p)
	}
	if md := f.be["metadata"].(map[string]any); md["annotations"] != nil {
		t.Fatalf("annotations left: %v", md["annotations"])
	}
	if c := outage("primary", false); c != http.StatusOK || f.updates != 4 {
		t.Fatalf("restore again: %d, %d updates, want no change", c, f.updates)
	}
	if c := outage("nobody", true); c != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", c)
	}
	feed := m.traffic.Recent("", 10)
	if len(feed) != 4 || feed[3].Outcome != "error" || feed[0].Outcome != "ok" || feed[3].User != "nick" || feed[3].Attrs["provider"] != "primary" {
		t.Fatalf("feed %+v: each cut and restore once, by the signed-in user", feed)
	}
}

// A provider cut while the chain is saved stays cut, at its edited target;
// one removed takes its note with it.
func TestPutModelsKeepsOutage(t *testing.T) {
	f := &fakeModels{rv: 3, be: groupsBackend()}
	m := f.serve(t)
	call(m.Outage, "/api/models/{ns}/{name}/outage", http.MethodPost, "/api/models/agentgateway-system/llm/outage", `{"provider":"primary","cut":true}`)
	w := call(m.Put, "/api/models/{ns}/{name}", http.MethodPut, "/api/models/agentgateway-system/llm",
		`{"resourceVersion":"4","providers":[{"name":"primary","kind":"openai","model":"qwen3.8:8b","host":"10.0.0.9","port":11435}],`+rules+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if p := obj(f.be, "spec", "ai", "provider"); p["host"] != "10.0.0.9" || fmt.Sprint(p["port"]) != "1" {
		t.Fatalf("provider %v: still cut, on its new host", p)
	}
	var o ModelOutage
	json.Unmarshal([]byte(strmap(f.be, "metadata", "annotations")[outageAnno+"primary"]), &o)
	if o.Host != "10.0.0.9" || o.Port != 11435 || o.By != "nick" {
		t.Fatalf("outage note %+v: the edited target, restored later", o)
	}
}

func TestModelStats(t *testing.T) {
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	llm := func(ago time.Duration, status int, a map[string]string) Traffic {
		out := "ok"
		if status >= 500 {
			out = "error"
		}
		return Traffic{Kind: "llm", Time: at(ago), Status: status, Outcome: out, Attrs: a}
	}
	ps := []ModelProvider{{Name: "primary", Kind: "openai", Model: "gpt-5-mini"}, {Name: "fallback", Kind: "anthropic", Model: "claude-sonnet-5"}}
	ts := []Traffic{ // newest first
		llm(time.Second, 200, map[string]string{"gen_ai.request.model": "gpt-5-mini", "gen_ai.response.model": "claude-sonnet-5", "gen_ai.provider.name": "anthropic"}),
		llm(2*time.Second, 503, map[string]string{"gen_ai.request.model": "gpt-5-mini"}),
		llm(3*time.Second, 200, map[string]string{"gen_ai.request.model": "gpt-5-mini", "gen_ai.response.model": "gpt-5-mini-2025-08-07"}),
		llm(4*time.Second, 429, map[string]string{"gen_ai.request.model": "gpt-5-mini", "gen_ai.provider.name": "openai"}),
		llm(5*time.Second, 200, map[string]string{"gen_ai.request.model": "llama3"}),
		{Kind: "http", Time: at(time.Second), Status: 200},
		llm(time.Hour, 200, map[string]string{"gen_ai.request.model": "gpt-5-mini"}),
	}
	other := modelStats(ts, ps, now.Add(-modelWindow))
	if s := ps[0].Stats; s.Calls != 3 || s.Errors != 2 || s.LastStatus != 503 || s.LastServed != at(2*time.Second) {
		t.Errorf("primary %+v", s)
	}
	if s := ps[1].Stats; s.Calls != 1 || s.Errors != 0 || s.LastStatus != 200 {
		t.Errorf("fallback %+v: served by its model, whatever was asked", s)
	}
	if other.Calls != 1 || other.LastStatus != 200 {
		t.Errorf("unattributed %+v", other)
	}
}

func TestParseModelRules(t *testing.T) {
	r := parseRules(map[string]any{"spec": map[string]any{"backend": map[string]any{"health": map[string]any{
		"unhealthyCondition": "(response.code >= 500)  ||  response.code == 429"}}}}, nil)
	if !r.On5xx || !r.On429 || r.Custom {
		t.Errorf("%+v", r)
	}
	r = parseRules(map[string]any{"spec": map[string]any{"backend": map[string]any{"health": map[string]any{
		"unhealthyCondition": "response.code >= 500 || response.duration > 5s"}}}}, nil)
	if !r.On5xx || r.On429 || !r.Custom {
		t.Errorf("custom condition %+v", r)
	}
	if r = parseRules(map[string]any{"spec": map[string]any{"backend": map[string]any{"health": map[string]any{}}}}, nil); !r.On5xx || r.Custom {
		t.Errorf("no condition is the default (a 5xx or no answer): %+v", r)
	}
	if got := unhealthyWhen(true, true); got != nil {
		t.Errorf("a condition replaces the default that catches no answer: %v", got)
	}
	callers := func(expr string) []ModelCaller {
		return parseCallers(map[string]any{"spec": map[string]any{"traffic": map[string]any{"authorization": map[string]any{
			"action": "Allow", "policy": map[string]any{"matchExpressions": []any{expr}}}}}})
	}
	got := callers(`(source.identity.namespace == "sv-agents" && source.identity.serviceAccount in ["bob-assistant", "advisor-desk"])
		|| (source.identity.namespace == "kagent" && source.identity.serviceAccount == "kagent-ops")`)
	if len(got) != 3 || got[1].ServiceAccount != "advisor-desk" || got[2].Namespace != "kagent" {
		t.Errorf("callers %+v", got)
	}
	if got := callers(`source.identity.namespace == "sv-agents" && request.headers["x"] == "y"`); got != nil {
		t.Errorf("not a plain list: %+v, want nil", got)
	}
}

func TestLLMSummaryNamesTheServedModel(t *testing.T) {
	tr := Traffic{Kind: "llm", Attrs: map[string]string{"gen_ai.request.model": "gpt-5-mini", "gen_ai.response.model": "claude-sonnet-5",
		"gen_ai.provider.name": "anthropic", "gen_ai.usage.input_tokens": "12", "gen_ai.usage.output_tokens": "30"}}
	if got := summarize(tr, tr.Attrs, ""); got != "LLM anthropic/claude-sonnet-5 · 12 in / 30 out" {
		t.Errorf("summary %q", got)
	}
	tr.Attrs = map[string]string{"gen_ai.request.model": "qwen3.8:27b"}
	if got := summarize(tr, tr.Attrs, ""); got != "LLM qwen3.8:27b" {
		t.Errorf("summary %q", got)
	}
}

// The view: providers as they stand (an older OpenAI-compatible Ollama too),
// who may call, and the route for callers outside the mesh.
func TestModelsView(t *testing.T) {
	f := &fakeModels{rv: 3, be: ollamaBackend()}
	m := f.serve(t)
	v, err := m.view(t.Context(), m.k.dyn)
	if err != nil {
		t.Fatal(err)
	}
	if v.Edition != "oss" || len(v.Providers) != 1 || v.Providers[0].Kind != "openai" || v.Providers[0].Name != "primary" || v.Providers[0].Port != 11434 {
		t.Fatalf("view %+v", v)
	}
	if v.External == nil || !slices.Equal(v.External.Hosts, []string{"llm.sv.lab"}) || len(v.Callers) != 3 || v.Declared["provider"] != "ollama" {
		t.Fatalf("external %+v callers %+v declared %v", v.External, v.Callers, v.Declared)
	}
	if !v.Rules.On5xx || v.Rules.On429 || v.Rules.Duration != "30s" || v.Rules.RetryAttempts != 1 || len(v.Rules.RetryCodes) != 5 {
		t.Fatalf("rules %+v", v.Rules)
	}
	f.be = groupsBackend()
	if v, _ = m.view(t.Context(), m.k.dyn); v.Providers[0].Kind != "ollama" || v.Providers[0].Model != "qwen3.8:27b" || v.Providers[1].Secret != "llm-anthropic" {
		t.Fatalf("groups %+v", v.Providers)
	}
}

func TestUSD(t *testing.T) {
	for in, want := range map[string]string{"0.0012345": "$0.0012", "0.000031": "$0.000031", "1.234": "$1.23", "0": "$0", "": "", "x": "", "-1": ""} {
		if got := usd(in); got != want {
			t.Errorf("usd(%q) = %q, want %q", in, got, want)
		}
	}
	a := map[string]string{"gen_ai.request.model": "m", "gen_ai.usage.input_tokens": "1", "gen_ai.usage.output_tokens": "2", "agw.ai.usage.cost.total": "0.0012"}
	if got := summarize(Traffic{Kind: "llm", Attrs: a}, a, ""); got != "LLM m · 1 in / 2 out · $0.0012" {
		t.Errorf("summary %q", got)
	}
}

// Budgets and token limits read as rules; the configs the budget controller
// generates are not limits of their own.
func TestEnterpriseRules(t *testing.T) {
	obj := func(name string, spec map[string]any) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name, "namespace": "agentgateway-system"}, "spec": spec}}
	}
	bs := budgetRules([]unstructured.Unstructured{obj("llm-budgets", map[string]any{"budgets": []any{
		map[string]any{"name": "dev", "subject": map[string]any{"virtualKey": "developer"}, "limit": map[string]any{"unit": "USD", "amount": int64(5)},
			"window": map[string]any{"unit": "Day"}, "onBudgetExceeded": "Block"}}})})
	if len(bs) != 1 || bs[0].Subject["virtualKey"] != "developer" || bs[0].Amount != 5 || bs[0].Unit != "USD" || bs[0].Window != "Day" || bs[0].Action != "Block" {
		t.Fatalf("budgets %+v", bs)
	}
	ls := tokenLimits([]unstructured.Unstructured{
		obj("agw-budget-llm-budgets-1", map[string]any{"raw": map[string]any{"descriptors": []any{map[string]any{"key": "x"}}}}),
		obj("llm-tokens-per-agent", map[string]any{"raw": map[string]any{
			"descriptors": []any{
				map[string]any{"key": "caller", "rateLimit": map[string]any{"unit": "MINUTE", "requestsPerUnit": int64(100000)}},
				map[string]any{"key": "caller", "value": "sv-agents/advisor-desk", "rateLimit": map[string]any{"unit": "MINUTE", "requestsPerUnit": int64(2000)}}},
			"rateLimits": []any{map[string]any{"type": "TOKEN"}}}}),
	})
	if len(ls) != 2 || !ls[0].Tokens || ls[1].Value != "sv-agents/advisor-desk" || ls[1].PerUnit != 2000 || ls[0].Unit != "MINUTE" {
		t.Fatalf("limits %+v", ls)
	}
}
