package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mcpfault/mcpfault/internal/config"
)

func faults(t *testing.T, yaml string) []*config.Fault {
	t.Helper()
	sc, err := config.Parse([]byte("servers:\n  billing: {command: x}\n  crm: {command: y}\n"+yaml), "test.fault.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return sc.Faults
}

func okResponse(id, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
	return b
}

func TestFaultWindowAndRunGating(t *testing.T) {
	e := New()
	e.Configure(faults(t, "faults:\n  - {tool: pay, call: 2, repeat: 2, type: error}\n"), false)

	if d := e.OnCall("billing", json.RawMessage("1"), "pay", nil); d.Action != ActForward {
		t.Fatalf("outside a run faults must not fire, got %s", d.Action)
	}
	e.BeginRun(1)
	var got []string
	for i := 0; i < 5; i++ {
		got = append(got, e.OnCall("billing", json.RawMessage("1"), "pay", nil).Action)
	}
	want := []string{ActForward, ActRespond, ActRespond, ActForward, ActForward}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d: got %s want %s (all: %v)", i+1, got[i], want[i], got)
		}
	}
	e.EndRun()
	e.BeginRun(2)
	if d := e.OnCall("billing", json.RawMessage("1"), "pay", nil); d.Action != ActForward {
		t.Fatal("counters must reset per run")
	}
	if d := e.OnCall("billing", json.RawMessage("1"), "pay", nil); d.Action != ActRespond {
		t.Fatal("second call of run 2 should fault")
	}
	if n := len(e.Calls(2)); n != 2 {
		t.Fatalf("run 2 has %d calls, want 2", n)
	}
}

func TestServerQualifiedAndArgsFilters(t *testing.T) {
	e := New()
	e.Configure(faults(t, "faults:\n  - tool: crm/pay\n    type: hang\n    args: {region: eu}\n"), true)
	if d := e.OnCall("billing", nil, "pay", json.RawMessage(`{"region":"eu"}`)); d.Action != ActForward {
		t.Fatal("fault is scoped to crm")
	}
	if d := e.OnCall("crm", nil, "pay", json.RawMessage(`{"region":"us"}`)); d.Action != ActForward {
		t.Fatal("args filter should not match us")
	}
	if d := e.OnCall("crm", nil, "pay", json.RawMessage(`{"region":"eu"}`)); d.Action != ActHang {
		t.Fatalf("expected hang, got %s", d.Action)
	}
}

func TestLostResponseRecordsTruthAndLies(t *testing.T) {
	e := New()
	e.Configure(faults(t, "faults:\n  - {tool: pay, type: lost_response, message: 504 Gateway Timeout}\n"), true)
	d := e.OnCall("billing", json.RawMessage(`"abc"`), "pay", json.RawMessage(`{"amount":10}`))
	if d.Action != ActForward {
		t.Fatalf("lost_response must reach the server, got %s", d.Action)
	}
	res := e.OnResult(d.CallID, okResponse(`"abc"`, `{"payment_id":"p1"}`))
	var msg struct {
		ID     string `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(res.Message, &msg); err != nil {
		t.Fatal(err)
	}
	if !msg.Result.IsError || msg.Result.Content[0].Text != "504 Gateway Timeout" || msg.ID != "abc" {
		t.Fatalf("agent should see a tool error with the original id, got %s", res.Message)
	}
	c := e.Calls(-1)[0]
	if c.Committed == nil || !*c.Committed {
		t.Fatal("call should be recorded as committed")
	}
	if p, _ := c.Result.(map[string]any); p["payment_id"] != "p1" {
		t.Fatalf("result payload not recorded: %v", c.Result)
	}
	if c.Delivered.Kind != "tool_error" || c.ServerOutcome.Kind != "result" {
		t.Fatalf("outcomes: server=%v delivered=%v", c.ServerOutcome, c.Delivered)
	}
}

func TestErrorVariantsAndDisconnect(t *testing.T) {
	e := New()
	e.Configure(faults(t, `faults:
  - {tool: a, type: error, respond_with: rpc_error, code: -32000, message: nope}
  - {tool: b, type: error, respond_with: none}
  - {tool: c, type: disconnect, when: before}
  - {tool: d, type: disconnect}
  - {tool: e, type: lost_response, respond_with: none}
`), true)
	d := e.OnCall("s", json.RawMessage("7"), "a", nil)
	if d.Action != ActRespond || !json.Valid(d.Message) {
		t.Fatalf("a: %+v", d)
	}
	var m struct{ Error struct{ Code int } }
	json.Unmarshal(d.Message, &m)
	if m.Error.Code != -32000 {
		t.Fatalf("rpc error code %d", m.Error.Code)
	}
	if d := e.OnCall("s", nil, "b", nil); d.Action != ActHang {
		t.Fatalf("b: %s", d.Action)
	}
	if d := e.OnCall("s", nil, "c", nil); d.Action != ActDisconnect {
		t.Fatalf("c: %s", d.Action)
	}
	d = e.OnCall("s", json.RawMessage("1"), "d", nil)
	if d.Action != ActForward {
		t.Fatalf("d before: %s", d.Action)
	}
	if r := e.OnResult(d.CallID, okResponse("1", "{}")); r.Action != ActDisconnect {
		t.Fatalf("d after: %s", r.Action)
	}
	d = e.OnCall("s", json.RawMessage("2"), "e", nil)
	if r := e.OnResult(d.CallID, okResponse("2", "{}")); r.Action != ActWithhold {
		t.Fatalf("e: %s", r.Action)
	}
}

func TestDelayAndCorrupt(t *testing.T) {
	e := New()
	e.Configure(faults(t, `faults:
  - {tool: slow, type: delay, delay: 1500ms}
  - tool: inv
    type: corrupt
    corrupt: {set: {amount: 98000, meta.flag: true}, drop: [currency]}
`), true)
	d := e.OnCall("s", json.RawMessage("1"), "slow", nil)
	if r := e.OnResult(d.CallID, okResponse("1", "{}")); r.Delay() != 1500*time.Millisecond {
		t.Fatalf("delay %v", r.Delay())
	}
	d = e.OnCall("s", json.RawMessage("2"), "inv", nil)
	r := e.OnResult(d.CallID, okResponse("2", `{"id":"INV-1","amount":1250,"currency":"USD"}`))
	var msg struct {
		Result json.RawMessage `json:"result"`
	}
	json.Unmarshal(r.Message, &msg)
	got, _ := json.Marshal(Payload(msg.Result))
	if string(got) != `{"amount":98000,"id":"INV-1","meta":{"flag":true}}` {
		t.Fatalf("corrupted payload: %s", got)
	}
	if p := e.Calls(-1)[1].Result.(map[string]any); p["amount"] != float64(1250) {
		t.Fatal("the recorded truth must be the server's original payload")
	}
}

func TestCorruptEmptyTruncateAndStructured(t *testing.T) {
	structured := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"a\":1}"}],"structuredContent":{"a":1}}}`)
	out := Corrupt(structured, &config.Corrupt{Replace: map[string]any{"b": 2}})
	var m struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	json.Unmarshal(out, &m)
	if m.Result.StructuredContent["b"] != float64(2) {
		t.Fatalf("replace should update structuredContent: %s", out)
	}
	if Payload(extractResult(Corrupt(structured, &config.Corrupt{Truncate: true}))) != nil {
		t.Fatal("truncated JSON should no longer parse")
	}
	if Describe(Corrupt(structured, &config.Corrupt{Empty: true})).Text != "" {
		t.Fatal("empty should remove content")
	}
}

func extractResult(msg json.RawMessage) json.RawMessage {
	var m struct {
		Result json.RawMessage `json:"result"`
	}
	json.Unmarshal(msg, &m)
	return m.Result
}

func TestEventsAndReleased(t *testing.T) {
	e := New()
	var events []string
	unsub := e.Subscribe(func(ev Event) { events = append(events, ev.Type) })
	e.Touch("s")
	e.OnCall("s", nil, "x", nil)
	unsub()
	e.OnCall("s", nil, "x", nil)
	if len(events) != 2 || events[0] != "server" || events[1] != "call" {
		t.Fatalf("events: %v", events)
	}
	ch := e.Released()
	e.BeginRun(1)
	e.EndRun()
	select {
	case <-ch:
	default:
		t.Fatal("EndRun should release hung requests")
	}
}
