package main

// The wire model the UI renders. The server owns the graph; the browser only
// lays it out, so every client sees the same derivation of the cluster.

type Ref struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

type Pod struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Node     string `json:"node,omitempty"`
	Zone     string `json:"zone,omitempty"`
	IP       string `json:"ip,omitempty"`
	Restarts int32  `json:"restarts"`
	Started  string `json:"started,omitempty"`
}

type Group struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Domain string `json:"domain,omitempty"`
	Order  int    `json:"order"`
}

type Node struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"` // gateway waypoint agent mcp idp db llm ui controller substrate workload external cluster
	Label     string         `json:"label"`
	Sub       string         `json:"sub,omitempty"`
	Group     string         `json:"group"`
	Namespace string         `json:"namespace,omitempty"`
	Ref       *Ref           `json:"ref,omitempty"`
	Related   []Ref          `json:"related,omitempty"`
	Pods      []Pod          `json:"pods,omitempty"`
	Badges    []string       `json:"badges,omitempty"`
	Products  []string       `json:"products,omitempty"` // Solo products this node is, or runs
	Status    string         `json:"status"`             // ok warn down idle
	Identity  []string       `json:"identity,omitempty"`
	Summary   map[string]any `json:"summary,omitempty"`

	kindLabel string // observatory.solo.io/kind on the workload, if any
}

type Edge struct {
	ID       string   `json:"id"`
	Source   string   `json:"source"`
	Target   string   `json:"target"`
	Kind     string   `json:"kind"` // http mcp a2a llm oidc mesh db
	Label    string   `json:"label,omitempty"`
	Hosts    []string `json:"hosts,omitempty"` // for calls into the edge gateway: the hostnames asked for
	Declared bool     `json:"declared"`
	Observed bool     `json:"observed"`
}

type Graph struct {
	Version int64   `json:"version"`
	Groups  []Group `json:"groups"`
	Nodes   []Node  `json:"nodes"`
	Edges   []Edge  `json:"edges"`
}

// Traffic is one request (or lifecycle event) seen anywhere in the lab.
type Traffic struct {
	ID       string            `json:"id"`
	Time     string            `json:"time"`
	Kind     string            `json:"kind"` // http mcp a2a llm oidc lifecycle substrate continuity model
	Reporter string            `json:"reporter"`
	Source   string            `json:"source,omitempty"`   // node id
	Target   string            `json:"target,omitempty"`   // node id
	Via      string            `json:"via,omitempty"`      // node id of the gateway/waypoint that saw it
	Identity string            `json:"identity,omitempty"` // caller SPIFFE id
	User     string            `json:"user,omitempty"`
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path,omitempty"`
	Status   int               `json:"status,omitempty"`
	Duration float64           `json:"durationMs,omitempty"`
	Summary  string            `json:"summary"`
	Outcome  string            `json:"outcome"` // ok denied error info
	Attrs    map[string]string `json:"attrs,omitempty"`
	Tokens   []Token           `json:"tokens,omitempty"` // credentials the request carried, decoded
}

// Token is a credential seen on a request, as its claims. The token itself
// never leaves the server: only a fingerprint identifies it.
type Token struct {
	Source      string         `json:"source"`   // where it was seen: the gateway's JWT policy, a header
	Verified    bool           `json:"verified"` // checked against the issuer's keys by the gateway
	Fingerprint string         `json:"fingerprint,omitempty"`
	Header      map[string]any `json:"header,omitempty"`
	Claims      map[string]any `json:"claims"`
}

// Stats are rates over the last window, keyed by edge id.
type Stats struct {
	Window  int                  `json:"windowSeconds"`
	Edges   map[string]EdgeStats `json:"edges"`
	RPS     float64              `json:"rps"`
	ErrRate float64              `json:"errRate"`
}

type EdgeStats struct {
	RPS    float64 `json:"rps"`
	Errors float64 `json:"errors"`
	L4     float64 `json:"l4"` // mesh connections/s from ztunnel
}
