package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Index maps what telemetry reports (pod IPs, SPIFFE ids, hostnames) back
// to graph nodes. Rebuilt with every graph.
type Index struct {
	byPod   map[string]string
	bySA    map[string][]string
	byOwner map[string]string
	hosts   map[string][]string
	svcWL   map[string][]string
	nodes   map[string]*Node
	edgeGW  string
	l4      map[string]float64
	sso     []SSO
	trusts  []Trust
	next    map[string][]string // request-path adjacency
}

func (ix *Index) gateway(ref string) string { // "ns/name" of a Gateway
	return ix.byOwner[strings.Replace(ref, "/", "/Gateway/", 1)]
}

func (ix *Index) identity(spiffe string) string {
	if ids := ix.bySA[spiffe]; len(ids) == 1 {
		return ids[0]
	}
	return ""
}

func (ix *Index) addr(a string) string {
	if h, _, ok := strings.Cut(a, ":"); ok {
		a = h
	}
	return ix.byPod[a]
}

func (ix *Index) host(h string) string {
	if h, _, ok := strings.Cut(h, ":"); ok {
		if ids := ix.hosts[h]; len(ids) == 1 {
			return ids[0]
		}
	}
	if ids := ix.hosts[h]; len(ids) == 1 {
		return ids[0]
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimSuffix(h, ".cluster.local"), ".svc"), ".")
	if len(parts) >= 2 {
		if ids := ix.svcWL[parts[1]+"/"+parts[0]]; len(ids) == 1 {
			return ids[0]
		}
	}
	return ""
}

// Traffic keeps a ring of recent events and per-edge counters for rates.
type TrafficStore struct {
	mu     sync.RWMutex
	ring   []Traffic
	at     []time.Time // when each ring entry arrived: the clock rates use
	next   int
	full   bool
	seq    atomic.Uint64
	counts map[string]*counter // edge id -> requests in the window
	hub    *Hub
	index  func() *Index
}

type counter struct{ ok, err []time.Time }

const window = 30 * time.Second

func NewTrafficStore(size int, hub *Hub, index func() *Index) *TrafficStore {
	return &TrafficStore{ring: make([]Traffic, size), at: make([]time.Time, size), counts: map[string]*counter{}, hub: hub, index: index}
}

func (s *TrafficStore) Add(t Traffic) {
	if t.ID == "" {
		t.ID = strconv.FormatUint(s.seq.Add(1), 36)
	}
	if t.Time == "" {
		t.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	s.mu.Lock()
	now := time.Now()
	s.ring[s.next], s.at[s.next] = t, now
	s.next = (s.next + 1) % len(s.ring)
	if s.next == 0 {
		s.full = true
	}
	for _, e := range t.edges() {
		c := s.counts[e]
		if c == nil {
			c = &counter{}
			s.counts[e] = c
		}
		if t.Outcome == "error" || t.Outcome == "denied" {
			c.err = append(c.err, now)
		} else {
			c.ok = append(c.ok, now)
		}
	}
	s.mu.Unlock()
	s.hub.Publish("traffic", t)
}

// edges a request crossed: caller -> gateway -> target.
func (t Traffic) edges() []string {
	var out []string
	switch {
	case t.Via != "" && t.Source != "" && t.Target != "":
		out = append(out, t.Source+">"+t.Via, t.Via+">"+t.Target)
	case t.Via != "" && t.Target != "": // the caller didn't resolve; the gateway's hop still happened
		out = append(out, t.Via+">"+t.Target)
	case t.Source != "" && t.Target != "":
		out = append(out, t.Source+">"+t.Target)
	}
	return out
}

func (s *TrafficStore) Recent(node string, limit int) []Traffic {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := s.next
	if s.full {
		n = len(s.ring)
	}
	var out []Traffic
	for i := 0; i < n && len(out) < limit; i++ {
		t := s.ring[(s.next-1-i+len(s.ring))%len(s.ring)]
		if node == "" || t.Source == node || t.Target == node || t.Via == node {
			out = append(out, t)
		}
	}
	return out
}

func (s *TrafficStore) Rates() (map[string]EdgeStats, float64, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-window)
	out := map[string]EdgeStats{}
	var total, errs float64
	for id, c := range s.counts {
		c.ok, c.err = trim(c.ok, cut), trim(c.err, cut)
		if len(c.ok)+len(c.err) == 0 {
			delete(s.counts, id)
			continue
		}
		out[id] = EdgeStats{RPS: float64(len(c.ok)+len(c.err)) / window.Seconds(), Errors: float64(len(c.err)) / window.Seconds()}
	}
	// totals count requests once, not once per hop, by arrival (the ring's
	// order, and the clock the edge rates use), not the record's own time
	n := s.next
	if s.full {
		n = len(s.ring)
	}
	for i := 0; i < n; i++ {
		k := (s.next - 1 - i + len(s.ring)) % len(s.ring)
		t := s.ring[k]
		if s.at[k].Before(cut) {
			break
		}
		if t.Kind == "lifecycle" || t.Kind == "substrate" || t.Kind == "continuity" || t.Kind == "model" {
			continue
		}
		total++
		if t.Outcome == "error" || t.Outcome == "denied" {
			errs++
		}
	}
	rate := 0.0
	if total > 0 {
		rate = errs / total
	}
	return out, total / window.Seconds(), rate
}

func trim(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	return ts[i:]
}

// ServeOTLP accepts OTLP/HTTP JSON logs from the collector: gateway and
// waypoint access logs, one record per request.
func (s *TrafficStore) ServeOTLP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" { // the collector's default
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer zr.Close()
		in = zr
	}
	body, err := io.ReadAll(io.LimitReader(in, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req otlpLogs
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "expected OTLP/JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	ix := s.index()
	for _, rl := range req.ResourceLogs {
		res := flatten(rl.Resource.Attributes)
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				a := flatten(lr.Attributes)
				for k, v := range res {
					if _, ok := a["resource."+k]; !ok {
						a["resource."+k] = v // always, even when the record has the same key
					}
				}
				if lr.Body.StringValue != "" {
					a["body"] = lr.Body.StringValue
				}
				if t, ok := normalize(a, lr.TimeUnixNano, ix); ok {
					s.Add(t)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

type otlpLogs struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes []kv `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				TimeUnixNano string   `json:"timeUnixNano"`
				Body         anyValue `json:"body"`
				Attributes   []kv     `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type kv struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue string          `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	BoolValue   *bool           `json:"boolValue"`
	ArrayValue  *struct {
		Values []anyValue `json:"values"`
	} `json:"arrayValue"`
	KvlistValue *struct {
		Values []kv `json:"values"`
	} `json:"kvlistValue"`
}

func (v anyValue) String() string {
	switch {
	case v.StringValue != "":
		return v.StringValue
	case len(v.IntValue) > 0:
		return strings.Trim(string(v.IntValue), `"`)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'f', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case v.ArrayValue != nil:
		var parts []string
		for _, x := range v.ArrayValue.Values {
			parts = append(parts, x.String())
		}
		return strings.Join(parts, ",")
	}
	return ""
}

func flatten(in []kv) map[string]string {
	out := map[string]string{}
	var rec func(prefix string, kvs []kv)
	rec = func(prefix string, kvs []kv) {
		for _, x := range kvs {
			if x.Value.KvlistValue != nil {
				rec(prefix+x.Key+".", x.Value.KvlistValue.Values)
				continue
			}
			out[prefix+x.Key] = x.Value.String()
		}
	}
	rec("", in)
	return out
}

// normalize turns one access-log record (agentgateway, kgateway/Envoy or an
// Istio waypoint) into a Traffic event, resolving endpoints to graph nodes.
func normalize(a map[string]string, ts string, ix *Index) (Traffic, bool) {
	t := Traffic{Kind: "http", Attrs: a, Outcome: "ok"}
	// first: decode and scrub credentials, so no field copied out below (the
	// path, the summary) can carry a raw token, e.g. ?id_token_hint=
	t.Tokens = tokens(a)
	if ns, err := strconv.ParseInt(ts, 10, 64); err == nil && ns > 0 {
		t.Time = time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := a[k]; v != "" {
				return v
			}
		}
		return ""
	}
	t.Method = get("http.method", "http.request.method", "method")
	t.Path = get("http.path", "url.path", "path")
	t.Status, _ = strconv.Atoi(get("http.status", "http.response.status_code", "response_code", "status"))
	t.Duration, _ = strconv.ParseFloat(strings.TrimSuffix(get("duration", "duration_ms"), "ms"), 64)
	t.Identity = get("src.identity", "source.principal", "downstream_peer_uri_san", "source.identity")
	t.User = get("jwt.preferred_username", "user", "jwt.sub")
	if t.User == "" {
		for _, tk := range t.Tokens {
			if u, _ := tk.Claims["preferred_username"].(string); u != "" {
				t.User = u
				break
			}
		}
	}
	host := get("http.host", "authority", "server.address")
	if t.Method == "" && t.Path == "" && t.Status == 0 {
		return t, false
	}

	gw := get("gateway")
	reporter := get("reporter", "resource.service.name", "resource.k8s.deployment.name")
	switch {
	case gw != "":
		t.Via = ix.gateway(gw)
		t.Reporter = gw
	default:
		ns, pod := get("resource.k8s.namespace.name"), get("resource.k8s.pod.name")
		t.Reporter = reporter
		if ns != "" && pod != "" {
			for id, n := range ix.nodes {
				for _, p := range n.Pods {
					if p.Name == pod && n.Namespace == ns {
						t.Via = id
					}
				}
			}
		}
	}
	if t.Identity != "" {
		t.Source = ix.identity(t.Identity)
	}
	if t.Source == "" {
		t.Source = ix.addr(get("src.addr", "downstream_remote_address", "source.address"))
	}
	t.Target = ix.addr(get("endpoint", "upstream_host", "upstream.address", "backend.endpoint"))
	if t.Target == "" {
		t.Target = ix.host(host)
	}
	if t.Target == t.Via {
		t.Target = ""
	}

	if p := get("protocol"); p == "mcp" || get("mcp.method.name") != "" {
		t.Kind = "mcp"
	} else if p == "llm" || get("gen_ai.request.model", "llm.request.model") != "" {
		t.Kind = "llm"
	} else if strings.Contains(t.Path, "/openid-connect/") || strings.Contains(t.Path, "/broker/") || strings.HasSuffix(t.Path, "/.well-known/openid-configuration") {
		t.Kind = "oidc"
	} else if strings.HasPrefix(t.Path, "/api/a2a") {
		t.Kind = "a2a"
	}

	switch {
	case t.Status == 401 || t.Status == 403:
		t.Outcome = "denied"
	case t.Status >= 500 || t.Status == 0 && get("error") != "":
		t.Outcome = "error"
	}
	t.Summary = summarize(t, a, host)
	return t, true
}

func summarize(t Traffic, a map[string]string, host string) string {
	// refused by the assurance gate: its decision names the resource's rules
	// and why
	if d := a["continuity.decision"]; d != "" && !strings.HasPrefix(d, "allow ") {
		return "Assurance rules: " + d
	}
	switch t.Kind {
	case "mcp":
		m := a["mcp.method.name"]
		if tool := a["mcp.tool.name"]; tool != "" {
			return fmt.Sprintf("MCP %s %s", m, tool)
		}
		if m != "" {
			return "MCP " + m
		}
	case "llm":
		// the model that answered, where the gateway logs it, and its provider
		model, provider := servedBy(t)
		if provider != "" {
			model = provider + "/" + model
		}
		cost := ""
		if c := usd(a["agw.ai.usage.cost.total"]); c != "" { // absent: unpriced
			cost = " · " + c
		}
		if in, out := a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"]; in != "" {
			return fmt.Sprintf("LLM %s · %s in / %s out%s", model, in, out, cost)
		}
		return "LLM " + model + cost
	case "oidc":
		p := t.Path
		if i := strings.LastIndex(p, "/"); i >= 0 {
			p = p[i+1:]
		}
		return "OIDC " + p + " · " + host
	}
	return fmt.Sprintf("%s %s%s", t.Method, host, t.Path)
}

// usd prints a call's cost in dollars to two significant digits (a call costs
// fractions of a cent); empty when it isn't a number.
func usd(v string) string {
	f, err := strconv.ParseFloat(v, 64)
	switch {
	case v == "" || err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f):
		return ""
	case f == 0:
		return "$0"
	case f >= 0.01:
		return fmt.Sprintf("$%.2f", f)
	}
	return fmt.Sprintf("$%.*f", int(-math.Floor(math.Log10(f)))+1, f)
}
