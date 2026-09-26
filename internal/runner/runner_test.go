package runner_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpfault/mcpfault/internal/cli"
	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/engine"
	"github.com/mcpfault/mcpfault/internal/report"
	"github.com/mcpfault/mcpfault/internal/runner"
	"github.com/mcpfault/mcpfault/internal/server"
)

// The test binary plays three parts, so the whole pipeline runs for real:
//
//	agent (__agent__) → mcpfault stdio shim (stdio ...) → MCP server (__server__)
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__server__":
			runServer()
			return
		case "__agent__":
			os.Exit(runAgent(os.Args[2]))
		case "stdio":
			os.Exit(cli.Execute("test"))
		}
	}
	os.Exit(m.Run())
}

type payIn struct {
	Amount         float64 `json:"amount"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

type payOut struct {
	PaymentID string `json:"payment_id"`
}

func runServer() {
	var n atomic.Int64
	var mu sync.Mutex
	keys := map[string]string{}
	s := mcp.NewServer(&mcp.Implementation{Name: "billing", Version: "1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "create_payment", Description: "Charge the vendor"}, func(ctx context.Context, req *mcp.CallToolRequest, in payIn) (*mcp.CallToolResult, payOut, error) {
		mu.Lock()
		defer mu.Unlock()
		if id, ok := keys[in.IdempotencyKey]; ok && in.IdempotencyKey != "" {
			return nil, payOut{PaymentID: id}, nil
		}
		id := fmt.Sprintf("p%d", n.Add(1))
		if in.IdempotencyKey != "" {
			keys[in.IdempotencyKey] = id
		}
		return nil, payOut{PaymentID: id}, nil
	})
	s.Run(context.Background(), &mcp.StdioTransport{})
}

// runAgent is a scripted agent: "naive" retries blindly, "careful" reuses an idempotency key.
func runAgent(style string) int {
	ctx := context.Background()
	cmd := exec.Command(os.Args[0], "stdio", "--server", "billing", "--control", os.Getenv("MCPFAULT_CONTROL"), "--", os.Args[0], "__server__")
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 3
	}
	defer session.Close()
	if style != "unlisted" {
		if _, err := session.ListTools(ctx, nil); err != nil { // as real frameworks do
			return 3
		}
	}
	args := map[string]any{"amount": 1250}
	if style == "careful" || style == "unlisted" {
		args["idempotency_key"] = "pay-INV-1042"
	}
	for attempt := 0; attempt < 2; attempt++ {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_payment", Arguments: args})
		if err == nil && !res.IsError {
			fmt.Println("FINAL: PAID")
			return 0
		}
	}
	fmt.Println("FINAL: NOT_PAID")
	return 0
}

func scenario(t *testing.T, style string) *config.Scenario {
	t.Helper()
	exe, _ := filepath.Abs(os.Args[0])
	yaml := fmt.Sprintf(`
name: e2e-%s
runs: 2
servers:
  billing: {command: stub}
agent:
  command: [%q, __agent__, %s]
faults:
  - tool: billing/create_payment
    type: lost_response
    message: 504 Gateway Timeout
invariants:
  - name: paid at most once
    tool: billing/create_payment
    committed: true
    distinct: result.payment_id
    max: 1
  - output: {matches: "FINAL: PAID"}
`, style, exe, style)
	file := filepath.Join(t.TempDir(), "e2e.fault.yaml")
	os.WriteFile(file, []byte(yaml), 0o644)
	sc, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func run(t *testing.T, style string) *report.Report {
	t.Helper()
	eng := engine.New()
	srv := &server.Server{Engine: eng, Addr: "127.0.0.1:0", Version: "test", Mode: "run"}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rep, err := runner.Run(context.Background(), eng, scenario(t, style), runner.Options{OutDir: t.TempDir(), Control: srv.Addr, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rep.Dir, "report.json")); err != nil {
		t.Fatalf("report.json not written: %v", err)
	}
	return rep
}

func TestNaiveAgentDoublePays(t *testing.T) {
	rep := run(t, "naive")
	if rep.Passed || rep.Status != "failed" {
		t.Fatalf("expected failure, got status %s", rep.Status)
	}
	if st := rep.Invariants[0]; st.Failed != 2 || st.Evaluated != 2 {
		t.Fatalf("paid-at-most-once: %+v", st)
	}
	if len(rep.Recovery) != 1 || rep.Recovery[0].Outcomes[report.RetriedBlind] != 2 {
		t.Fatalf("recovery: %+v", rep.Recovery)
	}
	if !rep.Servers[0].Connected || rep.Servers[0].Calls != 4 {
		t.Fatalf("server info: %+v", rep.Servers[0])
	}
	if !strings.Contains(rep.Runs[0].Invariants[0].Detail, "2 distinct result.payment_id") {
		t.Fatalf("detail: %s", rep.Runs[0].Invariants[0].Detail)
	}
}

func TestCarefulAgentPasses(t *testing.T) {
	rep := run(t, "careful")
	if !rep.Passed {
		for _, r := range rep.Runs {
			t.Logf("run %d: %s %s %+v", r.Index, r.Status, r.Error, r.Invariants)
		}
		t.Fatal("expected pass")
	}
	if rep.Recovery[0].Outcomes[report.RetriedSameKey] != 2 {
		t.Fatalf("recovery: %+v", rep.Recovery)
	}
	var found bool
	for _, f := range rep.Contract {
		if f.Tool == "create_payment" && f.Level == "info" {
			found = true
		}
	}
	if !found {
		t.Fatalf("contract should note the optional key: %+v", rep.Contract)
	}
}

func TestRecoveryWithoutToolsList(t *testing.T) {
	rep := run(t, "unlisted")
	if rep.Recovery[0].Outcomes[report.RetriedSameKey] != 2 {
		t.Fatalf("same-key retries should be recognized from the arguments alone: %+v", rep.Recovery)
	}
}

func TestJUnit(t *testing.T) {
	rep := run(t, "naive")
	var b strings.Builder
	if err := report.WriteJUnit(&b, []*report.Report{rep}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`<testsuite name="e2e-naive"`, `failures="1"`, `violated in 2/2 runs`} {
		if !strings.Contains(out, want) {
			t.Fatalf("junit missing %q:\n%s", want, out)
		}
	}
}
