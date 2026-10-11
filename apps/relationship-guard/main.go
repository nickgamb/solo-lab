// relationship-guard: an agentgateway MCP guardrail (ExtMCP,
// extmcp/ext_mcp.proto) that enforces S&V's entitlements on every tool call,
// as OpenFGA holds them (the RelationshipModel the continuity controller
// keeps in sync). Before a tools/call reaches an MCP server it asks OpenFGA:
//
//	acts_for   does this agent (the calling workload, or the AI client)
//	           act for this user?
//	can_call   may this user call this server?
//	can_view   which clients may this user see? Handed to the server in
//	           x-relationship-clients, set by the gateway (any copy the
//	           caller sent is replaced), so it answers for those alone.
//
// Who the user is, and their groups, come from the verified token the gateway
// passes as metadata: group membership is the IdP's, held for the call as
// contextual tuples, never copied into OpenFGA. A refusal names the reason;
// the gateway fails closed when this service or OpenFGA can't answer.
//
// EVALUATE (default :9002) answers the same question for the Observatory's
// what-if, read only: POST /v1/evaluate {"user","groups","agent","server"}.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nickgamb/solo-lab/apps/relationship-guard/extmcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const toolsCall = "tools/call"

type guard struct {
	extmcp.UnimplementedExtMcpServer
	fga *fga
}

var pass = &extmcp.Pass{}

// callerOf: who is on the call and which server it is for, from the
// metadata the gateway's policy passes (user, groups, workload, client, and
// server: the MCP server's name in the model).
func callerOf(md *structpb.Struct) (caller, string) {
	f := md.GetFields()
	c := caller{
		User:     f["user"].GetStringValue(),
		Workload: f["workload"].GetStringValue(),
		Client:   f["client"].GetStringValue(),
	}
	for _, g := range f["groups"].GetListValue().GetValues() {
		c.Groups = append(c.Groups, g.GetStringValue())
	}
	return c, f["server"].GetStringValue()
}

// toolOf: the tool a tools/call names, for the log.
func toolOf(params []byte) string {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params, &p)
	return p.Name
}

func (g guard) CheckRequest(ctx context.Context, r *extmcp.McpRequest) (*extmcp.McpRequestResult, error) {
	if r.GetMethod() != toolsCall {
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: pass}}, nil
	}
	c, server := callerOf(r.GetMetadataContext())
	if server == "" {
		return nil, status.Error(codes.InvalidArgument, "the gateway's policy names no server (metadata server)")
	}
	tool := toolOf(r.GetMcpRequest())
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second) // within the gateway's 5 s
	defer cancel()
	d, err := decide(ctx, g.fga, c, server)
	if err != nil {
		slog.Error("relationship", "user", c.User, "agent", c.agent(), "server", server, "tool", tool, "err", err)
		return nil, status.Error(codes.Unavailable, "relationships unavailable")
	}
	slog.Info("relationship", "user", c.User, "agent", c.agent(), "server", server, "tool", tool,
		"decision", map[bool]string{true: "allow", false: "deny"}[d.Allow], "reason", d.Reason, "clients", len(d.Clients))
	if !d.Allow {
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: &extmcp.AuthorizationError{
			Code: extmcp.AuthorizationError_PERMISSION_DENIED, Reason: "refused by S&V's relationships: " + d.Reason}}}, nil
	}
	return &extmcp.McpRequestResult{
		Result: &extmcp.McpRequestResult_Pass{Pass: pass},
		HeaderMutation: &extmcp.HeaderMutation{Set: []*extmcp.McpHeader{
			{Key: headerClients, Value: []byte(strings.Join(d.Clients, ","))},
		}},
	}, nil
}

func (guard) CheckResponse(context.Context, *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: pass}}, nil
}

// evaluate: the decision for a caller the Observatory describes, without a
// call. agent is an OpenFGA object (agent:...) or a workload (ns/sa).
func (g guard) evaluate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User   string   `json:"user"`
		Groups []string `json:"groups"`
		Agent  string   `json:"agent"`
		Server string   `json:"server"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	c := caller{User: in.User, Groups: in.Groups}
	switch {
	case strings.HasPrefix(in.Agent, "agent:client."):
		c.Client = strings.TrimPrefix(in.Agent, "agent:client.")
	case strings.HasPrefix(in.Agent, "agent:"):
		c.Workload = strings.Replace(strings.TrimPrefix(in.Agent, "agent:"), ".", "/", 1)
	default:
		c.Workload = in.Agent
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	d, err := decide(ctx, g.fga, c, in.Server)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if d.Clients == nil {
		d.Clients = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"agent": c.agent(), "decision": d})
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	g := guard{fga: &fga{
		url:   strings.TrimSuffix(os.Getenv("OPENFGA_URL"), "/"),
		token: os.Getenv("OPENFGA_TOKEN"),
		store: env("OPENFGA_STORE", "sv-identity/sterling-vance"),
		http:  &http.Client{Timeout: 3 * time.Second},
	}}
	if g.fga.url == "" {
		slog.Error("OPENFGA_URL is required")
		os.Exit(1)
	}

	lis, err := net.Listen("tcp", env("LISTEN", ":4445"))
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	srv := grpc.NewServer()
	extmcp.RegisterExtMcpServer(srv, g)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() {
		slog.Info("listening (ExtMCP)", "addr", lis.Addr().String())
		if err := srv.Serve(lis); err != nil {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/evaluate", g.evaluate)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	hs := &http.Server{Addr: env("EVALUATE", ":9002"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("evaluate", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	srv.GracefulStop()
	_ = hs.Shutdown(context.Background())
}
