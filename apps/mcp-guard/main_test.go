package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/nickgamb/solo-lab/apps/mcp-guard/extmcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMaskString(t *testing.T) {
	for _, c := range []struct {
		in, want string
		n        int
	}{
		{"account 4402918837", "account ••••8837", 1},
		{"SSN 123-45-6789, card 4111111111111111", "SSN •••-••-6789, card ••••1111", 2},
		{"12345678 and 12345678901234567", "••••5678 and ••••4567", 2},
		{"1234567 is too short", "1234567 is too short", 0},
		{"123456789012345678 is too long", "123456789012345678 is too long", 0},
		{"since 2026-09-15, id c-1001, ssn_last4 4417", "since 2026-09-15, id c-1001, ssn_last4 4417", 0},
		{"inside a word: ab44029188", "inside a word: ab44029188", 0},
		{"11ed0000-0000-4000-8000-000000000b0b", "11ed0000-0000-4000-8000-000000000b0b", 0},
		{"••••8837", "••••8837", 0}, // masking twice changes nothing
	} {
		got, n := maskString(c.in)
		if got != c.want || n != c.n {
			t.Errorf("maskString(%q) = %q, %d; want %q, %d", c.in, got, n, c.want, c.n)
		}
	}
}

// what FastMCP returns for get_client: the record as JSON text and as
// structuredContent
const getClient = `{"content":[{"type":"text","text":"{\n  \"id\": \"c-1002\",\n  \"name\": \"Marcus Webb\",\n  \"account_number\": \"7730418265\"\n}"}],` +
	`"structuredContent":{"id":"c-1002","name":"Marcus Webb","account_number":"7730418265","aum":12500000,` +
	`"accounts":[{"kind":"ira","number":"55102938471","notes":["joint with 98765432109"]}]},"isError":false}`

func TestMaskToolResult(t *testing.T) {
	out, n, err := maskToolResult([]byte(getClient))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("masks = %d, want 4", n)
	}
	for _, leaked := range []string{"7730418265", "55102938471", "98765432109"} {
		if bytes.Contains(out, []byte(leaked)) {
			t.Errorf("%s still in %s", leaked, out)
		}
	}
	var r struct {
		Content []struct {
			Type, Text string
		}
		StructuredContent struct {
			ID            string          `json:"id"`
			AccountNumber string          `json:"account_number"`
			AUM           json.RawMessage `json:"aum"`
			Accounts      []struct {
				Kind, Number string
				Notes        []string
			}
		}
		IsError *bool `json:"isError"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("masked result is not JSON: %v: %s", err, out)
	}
	sc := r.StructuredContent
	if len(r.Content) != 1 || r.Content[0].Type != "text" || !strings.Contains(r.Content[0].Text, `"account_number": "••••8265"`) {
		t.Errorf("content = %+v", r.Content)
	}
	if sc.ID != "c-1002" || sc.AccountNumber != "••••8265" || string(sc.AUM) != "12500000" {
		t.Errorf("structuredContent = %+v (aum %s)", sc, sc.AUM)
	}
	if len(sc.Accounts) != 1 || sc.Accounts[0].Number != "••••8471" || sc.Accounts[0].Notes[0] != "joint with ••••2109" {
		t.Errorf("nested = %+v", sc.Accounts)
	}
	if r.IsError == nil || *r.IsError {
		t.Errorf("isError = %v, want false kept", r.IsError)
	}
}

func TestMaskToolResultNothingToMask(t *testing.T) {
	out, n, err := maskToolResult([]byte(`{"content":[{"type":"text","text":"Alice Chen, since 2026-09-15"}],"structuredContent":{"n":12345678901}}`))
	if err != nil || n != 0 || out != nil {
		t.Errorf("= %s, %d, %v; want nil, 0, nil (numbers are not masked)", out, n, err)
	}
	if _, _, err := maskToolResult([]byte(`not json`)); err == nil {
		t.Error("not JSON: no error")
	}
}

// the guard over gRPC, as the gateway calls it
func dial(t *testing.T) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := newServer()
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///guard",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

func TestGuard(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cc := dial(t)
	c := extmcp.NewExtMcpClient(cc)
	ctx := context.Background()
	md, _ := structpb.NewStruct(map[string]any{"tool": "get_client"})

	// tools/call with an account number: mutated, and logged without it
	r, err := c.CheckResponse(ctx, &extmcp.McpResponse{ServiceNames: []string{"bob-workspace"}, Method: "tools/call",
		MetadataContext: md, McpResponse: []byte(getClient)})
	if err != nil {
		t.Fatal(err)
	}
	if m := r.GetMutated(); m == nil || bytes.Contains(m, []byte("7730418265")) || !bytes.Contains(m, []byte("••••8265")) {
		t.Errorf("CheckResponse = %v, want the masked result", r)
	}
	var line struct {
		Msg, Tool string
		Target    []string
		Masks     int
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.Msg != "masked" || line.Tool != "get_client" ||
		line.Masks != 4 || len(line.Target) != 1 || line.Target[0] != "bob-workspace" {
		t.Errorf("log = %s (%v)", logs.Bytes(), err)
	}
	for _, leaked := range []string{"7730418265", "8265", "55102938471", "98765432109"} {
		if strings.Contains(logs.String(), leaked) {
			t.Errorf("log carries %s: %s", leaked, logs.String())
		}
	}

	// nothing to mask, another method, the request phase: pass
	for _, req := range []*extmcp.McpResponse{
		{Method: "tools/call", McpResponse: []byte(`{"content":[{"type":"text","text":"Alice Chen"}]}`)},
		{Method: "tools/list", McpResponse: []byte(`{"tools":[{"name":"get_client","description":"account 4402918837"}]}`)},
	} {
		if r, err := c.CheckResponse(ctx, req); err != nil || r.GetPass() == nil {
			t.Errorf("CheckResponse(%s) = %v, %v; want pass", req.Method, r, err)
		}
	}
	params := []byte(`{"name":"get_client","arguments":{"name":"4402918837"}}`)
	if r, err := c.CheckRequest(ctx, &extmcp.McpRequest{Method: "tools/call", McpRequest: params}); err != nil || r.GetPass() == nil {
		t.Errorf("CheckRequest = %v, %v; want pass", r, err)
	}

	// an unreadable result is an error: the gateway's failureMode decides
	if _, err := c.CheckResponse(ctx, &extmcp.McpResponse{Method: "tools/call", McpResponse: []byte("{")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unreadable result: err = %v, want InvalidArgument", err)
	}

	// health
	h, err := healthpb.NewHealthClient(cc).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || h.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health = %v, %v", h, err)
	}
}
