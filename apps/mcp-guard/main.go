// mcp-guard: an agentgateway MCP guardrail (ExtMCP, extmcp/ext_mcp.proto) for
// the sv-mcp waypoint (demos/bob/manifests/20-waypoint.yaml). It masks what a
// tool returns before the agent, and so the model, sees it:
//
//	tools/call response   in the result's content[].text and structuredContent
//	                      (every string, at any depth): US SSNs (123-45-6789 ->
//	                      •••-••-6789) and account numbers, runs of 8 to 17
//	                      digits (4402918837 -> ••••8837). Anything masked: the
//	                      masked result (mutated); nothing: pass.
//	everything else       pass
//
// The gateway decides what a failure means (the processor's failureMode): an
// unreadable result is an error to it, never a pass. Plain gRPC (h2c) on
// LISTEN: the mesh's mTLS covers the hop. One JSON log line per masked call
// with the tool and how many values were masked, never the values.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/nickgamb/solo-lab/apps/mcp-guard/extmcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const toolsCall = "tools/call"

// the largest tool result it takes (the gRPC default is 4 MiB)
const maxMsg = 16 << 20

type guard struct {
	extmcp.UnimplementedExtMcpServer
}

var pass = &extmcp.Pass{}

func (guard) CheckRequest(context.Context, *extmcp.McpRequest) (*extmcp.McpRequestResult, error) {
	return &extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: pass}}, nil
}

func (guard) CheckResponse(_ context.Context, r *extmcp.McpResponse) (*extmcp.McpResponseResult, error) {
	if r.GetMethod() != toolsCall {
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: pass}}, nil
	}
	// the tool, as the policy's metadata names it (mcp.tool.name)
	tool := r.GetMetadataContext().GetFields()["tool"].GetStringValue()
	masked, n, err := maskToolResult(r.GetMcpResponse())
	if err != nil {
		slog.Warn("unreadable result", "tool", tool, "target", r.GetServiceNames(), "err", err)
		return nil, status.Error(codes.InvalidArgument, "unreadable tools/call result")
	}
	if n == 0 {
		return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: pass}}, nil
	}
	slog.Info("masked", "tool", tool, "target", r.GetServiceNames(), "masks", n)
	return &extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Mutated{Mutated: masked}}, nil
}

func newServer() *grpc.Server {
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(maxMsg))
	extmcp.RegisterExtMcpServer(srv, guard{})
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
	srv := newServer()
	go func() {
		slog.Info("listening (ExtMCP)", "addr", lis.Addr().String())
		if err := srv.Serve(lis); err != nil {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	srv.GracefulStop()
}
