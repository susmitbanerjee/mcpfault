package invariants

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/engine"
)

func inv(t *testing.T, yaml string) *config.Invariant {
	t.Helper()
	sc, err := config.Parse([]byte("servers:\n  payments: {command: x}\n  crm: {command: y}\ninvariants:\n  - "+yaml+"\n"), "t.fault.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return sc.Invariants[0]
}

func call(seq int, server, tool, args string, committed *bool, result any) engine.Call {
	return engine.Call{ID: int64(seq), Seq: seq, Server: server, Tool: tool, Args: json.RawMessage(args), Committed: committed, Result: result, Forwarded: committed != nil}
}

var yes, no = true, false

var calls = []engine.Call{
	call(1, "payments", "pay", `{"amount":10}`, &yes, map[string]any{"id": "p1"}),
	call(2, "payments", "pay", `{"amount":10}`, &yes, map[string]any{"id": "p1"}),
	call(3, "payments", "pay", `{"amount":99}`, &yes, map[string]any{"id": "p2"}),
	call(4, "payments", "pay", `{"amount":10}`, &no, nil),
	call(5, "crm", "pay", `{"amount":10}`, &yes, map[string]any{"id": "x"}),
}

func TestDistinctCountsReplaysOnce(t *testing.T) {
	o, err := EvaluateLog(inv(t, "{tool: payments/pay, committed: true, distinct: result.id, max: 1}"), calls)
	if err != nil || o.Pass || !strings.HasPrefix(o.Detail, "2 distinct result.id") {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestFiltersAndBounds(t *testing.T) {
	for yaml, want := range map[string]bool{
		"{tool: payments/pay, committed: true, equals: 3}":                              true,
		"{tool: pay, equals: 5}":                                                        true,
		"{tool: payments/pay, committed: false, equals: 1}":                             true,
		"{tool: nothing, min: 1}":                                                       false,
		`{tool: payments/pay, committed: true, where: {args.amount: {ne: 10}}, max: 0}`: false,
		`{tool: pay, where: {args.amount: {gte: 50, lt: 100}}, equals: 1}`:              true,
		`{tool: pay, where: {args.amount: {in: [1, 99]}}, equals: 1}`:                   true,
		`{tool: pay, where: {result.id: {matches: "^p\\d$"}}, equals: 3}`:               true,
		`{tool: pay, where: {result: {exists: false}}, equals: 1}`:                      true,
		`{tool: pay, where: {server: crm}, equals: 1}`:                                  true,
	} {
		o, err := EvaluateLog(inv(t, yaml), calls)
		if err != nil || o.Pass != want {
			t.Errorf("%s: pass=%v want %v (%s) %v", yaml, o.Pass, want, o.Detail, err)
		}
	}
}

func TestDetailReferencesRunLocalCalls(t *testing.T) {
	o, _ := EvaluateLog(inv(t, `{tool: payments/pay, committed: true, where: {args.amount: {ne: 10}}, max: 0}`), calls)
	if o.Detail != "1 matching call (calls #3)" {
		t.Fatalf("detail %q", o.Detail)
	}
}

func TestOutput(t *testing.T) {
	if o := EvaluateOutput(inv(t, `{output: {matches: "^FINAL: PAID$"}}`), "done\nFINAL: PAID\n"); !o.Pass {
		t.Fatal(o.Detail)
	}
	o := EvaluateOutput(inv(t, `{output: {not_matches: "NOT_PAID"}}`), "FINAL: NOT_PAID")
	if o.Pass || !strings.Contains(o.Detail, "NOT_PAID") {
		t.Fatalf("%+v", o)
	}
}
