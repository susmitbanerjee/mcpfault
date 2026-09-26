package config

import (
	"strings"
	"testing"
	"time"
)

const base = "servers:\n  billing: {command: python server.py}\n"

func TestDefaultsAndServers(t *testing.T) {
	sc, err := Parse([]byte(`
servers:
  zeta: {url: "http://localhost:9000/mcp"}
  alpha: {url: "https://example.com/mcp", listen: "127.0.0.1:9999"}
  local: [python, server.py]
agent: {command: python agent.py}
`), "/tmp/checkout.fault.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != "checkout" || sc.Runs != 5 || sc.Timeout != 3*time.Minute {
		t.Fatalf("defaults: %+v", sc)
	}
	got := map[string]*Server{}
	for _, s := range sc.Servers {
		got[s.Name] = s
	}
	if got["alpha"].ProxyURL() != "http://127.0.0.1:9999/mcp" {
		t.Fatalf("alpha proxy url %s", got["alpha"].ProxyURL())
	}
	if got["zeta"].Transport != "http" || got["zeta"].Listen != "127.0.0.1:7364" {
		t.Fatalf("zeta: %+v", got["zeta"])
	}
	if got["local"].Transport != "stdio" || len(got["local"].Command.Argv) != 2 {
		t.Fatalf("local: %+v", got["local"])
	}
}

func TestFaultNormalization(t *testing.T) {
	sc, err := Parse([]byte(base+`faults:
  - {tool: billing/pay, type: lost_response}
  - {tool: pay, type: error, repeat: all, call: 3}
  - {tool: pay, type: delay, delay: 250ms}
`), "x.fault.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := sc.Faults
	if f[0].Server != "billing" || f[0].Message != "Request timed out" || f[0].Code != -32001 || f[0].RespondWith != "tool_error" {
		t.Fatalf("lost_response defaults: %+v", f[0])
	}
	if f[1].Repeat != 0 || f[1].Call != 3 || !strings.Contains(f[1].Label, "calls 3+") {
		t.Fatalf("repeat all: %+v", f[1])
	}
	if f[2].Delay != 250*time.Millisecond {
		t.Fatalf("delay: %v", f[2].Delay)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"fualts: []\n": `field fualts not found`,
		"faults:\n  - {tool: pay, type: explode}\n":            "`type` must be one of",
		"faults:\n  - {tool: nope/pay, type: hang}\n":          `unknown server "nope"`,
		"faults:\n  - {tool: pay, type: delay}\n":              "need `delay`",
		"faults:\n  - {tool: pay, type: error, repeat: 0}\n":   "`repeat` must be",
		"faults:\n  - {tool: pay, type: delay, delay: soon}\n": "not a duration",
		"invariants:\n  - {tool: pay}\n":                       "need `max`, `min` or `equals`",
		"invariants:\n  - {tool: pay, max: 1, check: x}\n":     "exactly one of",
		"agent: {}\n": "exactly one of `command`",
	}
	for yaml, want := range cases {
		_, err := Parse([]byte(base+yaml), "x.fault.yaml")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want error containing %q", yaml, err, want)
		}
	}
	if _, err := Parse([]byte("runs: 3\n"), "x.fault.yaml"); err == nil {
		t.Error("servers should be required")
	}
}

func TestInvariantNamesAndKinds(t *testing.T) {
	sc, err := Parse([]byte(base+`invariants:
  - {tool: billing/pay, committed: true, distinct: result.id, max: 1}
  - {tool: pay, equals: 2}
  - {check: [python, check.py], severity: warn}
  - {output: {not_matches: "FINAL: FAILED"}}
`), "x.fault.yaml")
	if err != nil {
		t.Fatal(err)
	}
	inv := sc.Invariants
	if inv[0].Kind != InvLog || inv[0].Name != "distinct result.id of billing/pay ≤ 1" {
		t.Fatalf("0: %q", inv[0].Name)
	}
	if inv[1].Name != "pay calls = 2" {
		t.Fatalf("1: %q", inv[1].Name)
	}
	if inv[2].Kind != InvCheck || inv[2].Severity != "warn" {
		t.Fatalf("2: %+v", inv[2])
	}
	if inv[3].Kind != InvOutput || inv[3].NotMatch == nil {
		t.Fatalf("3: %+v", inv[3])
	}
}

func TestExpand(t *testing.T) {
	t.Setenv("MCPFAULT_TEST_VAR", "hello")
	vars := map[string]string{"run_dir": "/tmp/r1", "proxy.billing": "http://127.0.0.1:7362/mcp"}
	got := Expand("{{run_dir}}/db {{ proxy.billing }} {{env.MCPFAULT_TEST_VAR}} {{unknown}}", vars)
	if got != "/tmp/r1/db http://127.0.0.1:7362/mcp hello {{unknown}}" {
		t.Fatalf("got %q", got)
	}
	c := Command{Argv: []string{"python", "{{run_dir}}/x.py"}}.Expand(vars)
	if c.Argv[1] != "/tmp/r1/x.py" {
		t.Fatalf("argv %v", c.Argv)
	}
}
