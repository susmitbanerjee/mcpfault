package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/engine"
	"github.com/mcpfault/mcpfault/internal/proxy"
)

// The test binary doubles as a stdio MCP server when started with "__server__".
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__server__" {
		var n atomic.Int64
		if err := newServer(&n).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return
	}
	os.Exit(m.Run())
}

type payIn struct {
	Amount float64 `json:"amount"`
}

type payOut struct {
	PaymentID string  `json:"payment_id"`
	Amount    float64 `json:"amount"`
}

func newServer(n *atomic.Int64) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "billing", Version: "1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "create_payment", Description: "Charge the vendor"}, func(ctx context.Context, req *mcp.CallToolRequest, in payIn) (*mcp.CallToolResult, payOut, error) {
		return nil, payOut{PaymentID: fmt.Sprintf("p%d", n.Add(1)), Amount: in.Amount}, nil
	})
	return s
}

type harness struct {
	eng     *engine.Engine
	session *mcp.ClientSession
}

func parseFaults(t *testing.T, yaml string) []*config.Fault {
	t.Helper()
	sc, err := config.Parse([]byte("servers:\n  billing: {command: x}\n"+yaml), "t.fault.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return sc.Faults
}

type transportKind string

const (
	stdio      transportKind = "stdio"
	streamSSE  transportKind = "streamable-sse"
	streamJSON transportKind = "streamable-json"
	legacySSE  transportKind = "legacy-sse"
)

var allTransports = []transportKind{stdio, streamSSE, streamJSON, legacySSE}

func connect(t *testing.T, kind transportKind, faultsYAML string) *harness {
	t.Helper()
	eng := engine.New()
	eng.Configure(parseFaults(t, faultsYAML), true)
	// The SSE client ties its event stream to this context, so it must outlive the test body.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1.0.0"}, nil)

	var transport mcp.Transport
	if kind == stdio {
		c2sR, c2sW := io.Pipe()
		s2cR, s2cW := io.Pipe()
		shim := &proxy.Stdio{
			Server: "billing", Command: []string{os.Args[0], "__server__"}, Hooks: proxy.LocalHooks{Engine: eng},
			Stdin: c2sR, Stdout: s2cW, Stderr: os.Stderr,
			Exit: func(int) { s2cW.Close(); c2sR.Close() },
		}
		go func() { shim.Run(); s2cW.Close() }()
		transport = &mcp.IOTransport{Reader: s2cR, Writer: c2sW}
	} else {
		var n atomic.Int64
		srv := newServer(&n)
		var handler http.Handler
		path := "/mcp"
		if kind == legacySSE {
			handler, path = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil), "/sse"
		} else {
			handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: kind == streamJSON})
		}
		upstream := httptest.NewServer(handler)
		t.Cleanup(upstream.Close)
		u, _ := url.Parse(upstream.URL + path)
		p := &proxy.HTTP{Name: "billing", Upstream: u, Listen: "127.0.0.1:0", Hooks: proxy.LocalHooks{Engine: eng}, Released: eng.Released, OnFailure: eng.OnFailure}
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		endpoint := "http://" + p.Addr + path
		if kind == legacySSE {
			transport = &mcp.SSEClientTransport{Endpoint: endpoint}
		} else {
			transport = &mcp.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1}
		}
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect through proxy: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return &harness{eng: eng, session: session}
}

func (h *harness) pay(t *testing.T, timeout time.Duration) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return h.session.CallTool(ctx, &mcp.CallToolParams{Name: "create_payment", Arguments: map[string]any{"amount": 1250}})
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// waitCalls waits until the engine has recorded n completed calls.
func (h *harness) waitCalls(t *testing.T, n int) []engine.Call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls := h.eng.Calls(-1)
		done := 0
		for _, c := range calls {
			if c.End != nil {
				done++
			}
		}
		if done >= n {
			return calls
		}
		if time.Now().After(deadline) {
			b, _ := json.MarshalIndent(calls, "", "  ")
			t.Fatalf("expected %d completed calls, have: %s", n, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func each(t *testing.T, fn func(t *testing.T, kind transportKind)) {
	for _, kind := range allTransports {
		t.Run(string(kind), func(t *testing.T) { fn(t, kind) })
	}
}

func TestPassthroughRecordsTruth(t *testing.T) {
	each(t, func(t *testing.T, kind transportKind) {
		h := connect(t, kind, "")
		if _, err := h.session.ListTools(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		res, err := h.pay(t, 10*time.Second)
		if err != nil || res.IsError {
			t.Fatalf("call failed: %v %v", err, res)
		}
		c := h.waitCalls(t, 1)[0]
		if !c.Forwarded || c.Committed == nil || !*c.Committed || c.Delivered.Kind != "result" {
			t.Fatalf("unexpected record: %+v", c)
		}
		if p, _ := c.Result.(map[string]any); p["payment_id"] != "p1" {
			t.Fatalf("payload: %v", c.Result)
		}
		deadline := time.Now().Add(2 * time.Second)
		for len(h.eng.Tools()["billing"]) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if len(h.eng.Tools()["billing"]) == 0 {
			t.Fatal("tools/list result was not recorded")
		}
	})
}

func TestLostResponse(t *testing.T) {
	each(t, func(t *testing.T, kind transportKind) {
		h := connect(t, kind, "faults:\n  - {tool: create_payment, type: lost_response, message: 504 Gateway Timeout}\n")
		res, err := h.pay(t, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || text(res) != "504 Gateway Timeout" {
			t.Fatalf("agent should see the injected error, got %+v", res)
		}
		c := h.waitCalls(t, 1)[0]
		if c.Committed == nil || !*c.Committed {
			t.Fatal("the server really executed the call")
		}
		if p, _ := c.Result.(map[string]any); p["payment_id"] != "p1" {
			t.Fatalf("payload: %v", c.Result)
		}
		res, err = h.pay(t, 10*time.Second)
		if err != nil || res.IsError {
			t.Fatalf("second call should pass through: %v %+v", err, res)
		}
	})
}

func TestErrorNeverReachesServer(t *testing.T) {
	each(t, func(t *testing.T, kind transportKind) {
		h := connect(t, kind, "faults:\n  - {tool: create_payment, type: error, respond_with: rpc_error, code: -32000, message: upstream unavailable}\n")
		_, err := h.pay(t, 10*time.Second)
		if err == nil || !strings.Contains(err.Error(), "upstream unavailable") {
			t.Fatalf("expected JSON-RPC error, got %v", err)
		}
		res, err := h.pay(t, 10*time.Second)
		if err != nil || res.IsError {
			t.Fatalf("second call: %v", err)
		}
		calls := h.waitCalls(t, 2)
		if calls[0].Forwarded || calls[0].Delivered.Kind != "rpc_error" {
			t.Fatalf("first call: %+v", calls[0])
		}
		if p, _ := calls[1].Result.(map[string]any); p["payment_id"] != "p1" {
			t.Fatalf("the server should have seen only the second call: %v", calls[1].Result)
		}
	})
}

func TestCorrupt(t *testing.T) {
	each(t, func(t *testing.T, kind transportKind) {
		h := connect(t, kind, "faults:\n  - {tool: create_payment, type: corrupt, corrupt: {set: {amount: 98000}}}\n")
		res, err := h.pay(t, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text(res), `"amount":98000`) {
			t.Fatalf("agent should see the edited payload, got %s", text(res))
		}
		if p := h.waitCalls(t, 1)[0].Result.(map[string]any); p["amount"] != float64(1250) {
			t.Fatalf("recorded truth should be unedited: %v", p)
		}
	})
}

func TestDelay(t *testing.T) {
	each(t, func(t *testing.T, kind transportKind) {
		h := connect(t, kind, "faults:\n  - {tool: create_payment, type: delay, delay: 400ms}\n")
		start := time.Now()
		if _, err := h.pay(t, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d < 400*time.Millisecond {
			t.Fatalf("response arrived after %v, expected at least 400ms", d)
		}
	})
}

func TestHang(t *testing.T) {
	each(t, func(t *testing.T, kind transportKind) {
		h := connect(t, kind, "faults:\n  - {tool: create_payment, type: hang}\n")
		if _, err := h.pay(t, 500*time.Millisecond); err == nil {
			t.Fatal("expected the call to time out")
		}
		if c := h.waitCalls(t, 1)[0]; c.Forwarded {
			t.Fatal("hung calls must not reach the server")
		}
	})
}

func TestDisconnectAfterCommit(t *testing.T) {
	each(t, func(t *testing.T, kind transportKind) {
		h := connect(t, kind, "faults:\n  - {tool: create_payment, type: disconnect}\n")
		if _, err := h.pay(t, 5*time.Second); err == nil {
			t.Fatal("expected the connection to drop")
		}
		c := h.waitCalls(t, 1)[0]
		if c.Committed == nil || !*c.Committed || c.Delivered.Kind != "disconnect" {
			t.Fatalf("record: %+v", c)
		}
	})
}
