// embargo-guard: an example policy server for Ledgerline's gateway, an
// agentgateway MCP guardrail (ExtMCP, extmcp/ext_mcp.proto). With
// LEDGERLINE_TOOL_CHECK=example the gateway asks it before each tool call
// (demos/bob/ledgerline/embargo-guard.yaml):
//
//	tools/call request   research_note for a symbol under embargo (EMBARGO,
//	                     comma-separated): refused, with the reason the agent
//	                     is shown. Any other call: pass.
//	everything else      pass
//
// It decides from what the gateway sends: the tool's name and arguments (the
// call's params) and, as the processor's metadata, the claims of the caller's
// token as the gateway verified them. The gateway decides what a failure
// means (the processor's failureMode): params it can't read are an error to
// it, never a pass. Plain gRPC (h2c) on LISTEN: the mesh's mTLS covers the
// hop. One JSON log line per call: the tool, the symbol when there is one,
// the decision, and the caller's subject and client; never a token.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nickgamb/solo-lab/apps/embargo-guard/extmcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	toolsCall    = "tools/call"
	researchNote = "research_note"
)

type guard struct {
	extmcp.UnimplementedExtMcpServer
	embargo map[string]bool
}

var pass = &extmcp.Pass{}

// symbols: EMBARGO's list, as a set of upper-case symbols
func symbols(list string) map[string]bool {
	set := map[string]bool{}
	for _, s := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		set[strings.ToUpper(s)] = true
	}
	return set
}

// decide: the reason a tools/call is refused ("" when it passes), and the
// symbol it asks about, if any
func (g guard) decide(params []byte) (reason, tool, symbol string, err error) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return "", "", "", err
	}
	if s, ok := call.Arguments["symbol"].(string); ok {
		symbol = strings.ToUpper(strings.TrimSpace(s))
	}
	if call.Name == researchNote && g.embargo[symbol] {
		return "Ledgerline: research on " + symbol + " is under embargo", call.Name, symbol, nil
	}
	return "", call.Name, symbol, nil
}

// caller: who the verified token says is calling (metadata "claims")
func caller(md *structpb.Struct) (subject, client string) {
	claims := md.GetFields()["claims"].GetStructValue().GetFields()
	subject = claims["sub"].GetStringValue()
	for _, k := range []string{"azp", "client_id", "cid"} {
		if client = claims[k].GetStringValue(); client != "" {
			break
		}
	}
	return subject, client
}

func (g guard) CheckRequest(_ context.Context, r *extmcp.McpRequest) (*extmcp.McpRequestResult, error) {
	if r.GetMethod() != toolsCall {
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: pass}}, nil
	}
	reason, tool, symbol, err := g.decide(r.GetMcpRequest())
	subject, client := caller(r.GetMetadataContext())
	if err != nil {
		slog.Warn("unreadable call", "target", r.GetServiceNames(), "subject", subject, "client", client, "err", err)
		return nil, status.Error(codes.InvalidArgument, "unreadable tools/call params")
	}
	if reason != "" {
		slog.Info("refused", "tool", tool, "symbol", symbol, "subject", subject, "client", client, "reason", reason)
		return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: &extmcp.AuthorizationError{
			Code: extmcp.AuthorizationError_PERMISSION_DENIED, Reason: reason}}}, nil
	}
	slog.Info("passed", "tool", tool, "symbol", symbol, "subject", subject, "client", client)
	return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: pass}}, nil
}

func (guard) CheckResponse(context.Context, *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: pass}}, nil
}

func newServer(embargo string) *grpc.Server {
	srv := grpc.NewServer()
	extmcp.RegisterExtMcpServer(srv, guard{embargo: symbols(embargo)})
	hs := health.NewServer() // SERVING once registered: it holds no state
	healthpb.RegisterHealthServer(srv, hs)
	return srv
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":4445"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	srv := newServer(os.Getenv("EMBARGO"))
	go func() {
		slog.Info("listening (ExtMCP)", "addr", lis.Addr().String(), "embargo", os.Getenv("EMBARGO"))
		if err := srv.Serve(lis); err != nil {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	srv.GracefulStop()
}
