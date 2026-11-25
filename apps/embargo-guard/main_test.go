package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/nickgamb/solo-lab/apps/embargo-guard/extmcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestDecide(t *testing.T) {
	g := guard{embargo: symbols("NVDA, tsla")}
	for _, c := range []struct {
		params, reason, tool, symbol string
	}{
		{`{"name":"research_note","arguments":{"symbol":"NVDA"}}`, "Ledgerline: research on NVDA is under embargo", "research_note", "NVDA"},
		{`{"name":"research_note","arguments":{"symbol":" nvda "}}`, "Ledgerline: research on NVDA is under embargo", "research_note", "NVDA"},
		{`{"name":"research_note","arguments":{"symbol":"TSLA"}}`, "Ledgerline: research on TSLA is under embargo", "research_note", "TSLA"},
		{`{"name":"research_note","arguments":{"symbol":"MSFT"}}`, "", "research_note", "MSFT"},
		{`{"name":"research_note","arguments":{"symbol":42}}`, "", "research_note", ""},
		{`{"name":"research_note"}`, "", "research_note", ""},
		{`{"name":"sector_outlook","arguments":{"sector":"technology"}}`, "", "sector_outlook", ""},
		{`{"name":"account_info","arguments":{}}`, "", "account_info", ""},
		// only research notes are embargoed: another tool that names the symbol passes
		{`{"name":"sector_outlook","arguments":{"symbol":"NVDA"}}`, "", "sector_outlook", "NVDA"},
	} {
		reason, tool, symbol, err := g.decide([]byte(c.params))
		if err != nil || reason != c.reason || tool != c.tool || symbol != c.symbol {
			t.Errorf("decide(%s) = %q, %q, %q, %v; want %q, %q, %q", c.params, reason, tool, symbol, err, c.reason, c.tool, c.symbol)
		}
	}
	if _, _, _, err := g.decide([]byte(`not json`)); err == nil {
		t.Error("not JSON: no error")
	}
	if reason, _, _, _ := (guard{embargo: symbols("")}).decide([]byte(`{"name":"research_note","arguments":{"symbol":"NVDA"}}`)); reason != "" {
		t.Errorf("no embargo list: refused (%s)", reason)
	}
}

// the guard over gRPC, as the gateway calls it
func dial(t *testing.T) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := newServer("NVDA")
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///guard",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func claims(t *testing.T) *structpb.Struct {
	t.Helper()
	md, err := structpb.NewStruct(map[string]any{"claims": map[string]any{
		"sub": "11ed0000-0000-4000-8000-000000000b0b", "azp": "sterling-vance-kagent", "scope": "research:read"}})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func TestCheckRequest(t *testing.T) {
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	c := extmcp.NewExtMcpClient(dial(t))
	ctx := context.Background()

	refused, err := c.CheckRequest(ctx, &extmcp.McpRequest{Method: "tools/call", ServiceNames: []string{"research"},
		McpRequest: []byte(`{"name":"research_note","arguments":{"symbol":"NVDA"}}`), MetadataContext: claims(t)})
	if err != nil {
		t.Fatal(err)
	}
	if e := refused.GetError(); e.GetCode() != extmcp.AuthorizationError_PERMISSION_DENIED || e.GetReason() != "Ledgerline: research on NVDA is under embargo" {
		t.Errorf("embargoed symbol: %v", refused)
	}
	for _, want := range []string{`"msg":"refused"`, `"tool":"research_note"`, `"symbol":"NVDA"`,
		`"subject":"11ed0000-0000-4000-8000-000000000b0b"`, `"client":"sterling-vance-kagent"`, `"token":false`} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %s: %s", want, log.String())
		}
	}

	// handed the token too: said so in the log, the token itself never
	withToken := claims(t)
	withToken.Fields["token"] = structpb.NewStringValue("header.payload.signature")
	log.Reset()
	if got, err := c.CheckRequest(ctx, &extmcp.McpRequest{Method: "tools/call",
		McpRequest: []byte(`{"name":"account_info","arguments":{}}`), MetadataContext: withToken}); err != nil || got.GetPass() == nil {
		t.Errorf("with the token: %v, %v; want pass", got, err)
	}
	if !strings.Contains(log.String(), `"token":true`) || strings.Contains(log.String(), "header.payload.signature") {
		t.Errorf("log with the token: %s", log.String())
	}

	for _, r := range []*extmcp.McpRequest{
		{Method: "tools/call", McpRequest: []byte(`{"name":"research_note","arguments":{"symbol":"MSFT"}}`), MetadataContext: claims(t)},
		{Method: "tools/call", McpRequest: []byte(`{"name":"account_info","arguments":{}}`)}, // no claims: still decided
		{Method: "tools/list"},
		{Method: "initialize", McpRequest: []byte(`not json`)}, // only tools/call params are read
	} {
		got, err := c.CheckRequest(ctx, r)
		if err != nil || got.GetPass() == nil {
			t.Errorf("%s %s: %v, %v; want pass", r.GetMethod(), r.GetMcpRequest(), got, err)
		}
	}

	if _, err := c.CheckRequest(ctx, &extmcp.McpRequest{Method: "tools/call", McpRequest: []byte(`not json`)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unreadable params: %v; want InvalidArgument", err)
	}
}

func TestCheckResponseAndHealth(t *testing.T) {
	conn := dial(t)
	got, err := extmcp.NewExtMcpClient(conn).CheckResponse(context.Background(), &extmcp.McpResponse{Method: "tools/call", McpResponse: []byte(`{}`)})
	if err != nil || got.GetPass() == nil {
		t.Errorf("response: %v, %v; want pass", got, err)
	}
	h, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || h.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health: %v, %v", h, err)
	}
}
