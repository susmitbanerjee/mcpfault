// Package engine decides which faults apply to which tool calls and records,
// for every call, both what the MCP server did and what the agent was shown.
// All proxies (stdio shims and HTTP proxies) consult the same engine.
package engine

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/jsonx"
)

// Decision actions.
const (
	ActForward    = "forward"    // send the request to the server
	ActRespond    = "respond"    // don't send it; answer the agent with Message
	ActHang       = "hang"       // don't send it; never answer
	ActDisconnect = "disconnect" // drop the agent's connection
	ActDeliver    = "deliver"    // give the agent Message (possibly altered)
	ActWithhold   = "withhold"   // the server answered; the agent never hears about it
)

// Decision tells a proxy what to do next.
type Decision struct {
	CallID  int64           `json:"call_id"`
	Action  string          `json:"action"`
	Message json.RawMessage `json:"message,omitempty"`
	DelayMS int64           `json:"delay_ms,omitempty"`
}

// Delay returns the decision's delay as a duration.
func (d Decision) Delay() time.Duration { return time.Duration(d.DelayMS) * time.Millisecond }

// Outcome summarizes a JSON-RPC response as one side saw it.
type Outcome struct {
	Kind string `json:"kind"` // result | tool_error | rpc_error | withheld | disconnect | transport_error
	Text string `json:"text,omitempty"`
	Code int    `json:"code,omitempty"`
}

// FaultRef identifies the fault applied to a call.
type FaultRef struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

// Call is one tools/call as observed by a proxy.
type Call struct {
	ID            int64           `json:"id"`
	Seq           int             `json:"seq,omitempty"` // position within its run, set by the runner
	Run           int             `json:"run"`
	Server        string          `json:"server"`
	Tool          string          `json:"tool"`
	Args          json.RawMessage `json:"args"`
	Fault         *FaultRef       `json:"fault,omitempty"`
	Forwarded     bool            `json:"forwarded"`
	Committed     *bool           `json:"committed,omitempty"`
	Result        any             `json:"result,omitempty"`
	ServerOutcome *Outcome        `json:"server_outcome,omitempty"`
	Delivered     *Outcome        `json:"delivered,omitempty"`
	Start         time.Time       `json:"start"`
	End           *time.Time      `json:"end,omitempty"`

	rpcID json.RawMessage
	fault *config.Fault
}

// Event is published for live views.
type Event struct {
	Type string `json:"type"` // call | server | run_started | run_finished | session_started | session_finished | tools
	Call *Call  `json:"call,omitempty"`
	Data any    `json:"data,omitempty"`
}

// Engine is safe for concurrent use.
type Engine struct {
	mu        sync.Mutex
	faults    []*config.Fault
	always    bool
	run       int
	seen      map[int]int
	calls     []*Call
	byID      map[int64]*Call
	nextID    int64
	tools     map[string]json.RawMessage
	servers   map[string]time.Time
	hang      chan struct{}
	listeners map[int]func(Event)
	nextSub   int
}

// New returns an engine with no faults configured.
func New() *Engine {
	return &Engine{
		seen:      map[int]int{},
		byID:      map[int64]*Call{},
		tools:     map[string]json.RawMessage{},
		servers:   map[string]time.Time{},
		hang:      make(chan struct{}),
		listeners: map[int]func(Event){},
	}
}

// Configure replaces the fault plan. With always=true faults apply to every call
// (standalone proxy mode); otherwise only between BeginRun and EndRun.
func (e *Engine) Configure(faults []*config.Fault, always bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.faults, e.always = faults, always
	e.seen = map[int]int{}
	e.releaseHangsLocked()
}

// BeginRun starts attributing calls to run n and resets fault counters.
func (e *Engine) BeginRun(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.run = n
	e.seen = map[int]int{}
}

// EndRun stops attributing calls to the current run and releases hung requests.
func (e *Engine) EndRun() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.run = 0
	e.releaseHangsLocked()
}

func (e *Engine) releaseHangsLocked() {
	close(e.hang)
	e.hang = make(chan struct{})
}

// Released is closed when the current run ends; hung requests wait on it.
func (e *Engine) Released() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hang
}

// Subscribe registers a listener for events and returns a function that removes it.
func (e *Engine) Subscribe(fn func(Event)) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.nextSub
	e.nextSub++
	e.listeners[id] = fn
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.listeners, id)
	}
}

// Emit publishes an event to all listeners.
func (e *Engine) Emit(ev Event) {
	e.mu.Lock()
	fns := make([]func(Event), 0, len(e.listeners))
	for _, fn := range e.listeners {
		fns = append(fns, fn)
	}
	e.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

func (e *Engine) emitCall(snap Call) {
	e.Emit(Event{Type: "call", Call: &snap})
}

// Touch records that a server's proxy saw traffic.
func (e *Engine) Touch(server string) {
	e.mu.Lock()
	_, known := e.servers[server]
	e.servers[server] = time.Now()
	e.mu.Unlock()
	if !known {
		e.Emit(Event{Type: "server", Data: map[string]any{"name": server}})
	}
}

// Servers returns when each server was last seen.
func (e *Engine) Servers() map[string]time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]time.Time, len(e.servers))
	for k, v := range e.servers {
		out[k] = v
	}
	return out
}

// OnToolsList records a server's tools/list result.
func (e *Engine) OnToolsList(server string, result json.RawMessage) {
	var r struct {
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(result, &r) != nil || len(r.Tools) == 0 {
		return
	}
	e.mu.Lock()
	e.tools[server] = r.Tools
	e.mu.Unlock()
	e.Emit(Event{Type: "tools", Data: map[string]any{"server": server}})
}

// Tools returns the raw tools arrays seen per server.
func (e *Engine) Tools() map[string]json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]json.RawMessage, len(e.tools))
	for k, v := range e.tools {
		out[k] = v
	}
	return out
}

// Calls returns snapshots of the calls recorded for a run (run < 0 = all calls).
func (e *Engine) Calls(run int) []Call {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Call
	for _, c := range e.calls {
		if run < 0 || c.Run == run {
			out = append(out, *c)
		}
	}
	return out
}

// Reset forgets recorded calls (tools and servers are kept).
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls, e.byID = nil, map[int64]*Call{}
}

// OnCall is called when the agent sends tools/call. rpcID is the JSON-RPC id.
func (e *Engine) OnCall(server string, rpcID json.RawMessage, tool string, args json.RawMessage) Decision {
	e.mu.Lock()
	e.nextID++
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	c := &Call{ID: e.nextID, Run: e.run, Server: server, Tool: tool, Args: args, Start: time.Now(), rpcID: rpcID}
	var f *config.Fault
	if e.run > 0 || e.always {
		f = e.matchLocked(server, tool, args)
	}
	if f != nil {
		c.fault = f
		c.Fault = &FaultRef{Index: f.Index, Type: f.Type, Label: f.Label}
	}
	e.calls = append(e.calls, c)
	e.byID[c.ID] = c

	d := Decision{CallID: c.ID, Action: ActForward}
	switch {
	case f == nil:
		c.Forwarded = true
	case f.Type == config.FaultError:
		c.Committed = ptr(false)
		if f.RespondWith == "none" {
			d.Action = ActHang
			c.Delivered = &Outcome{Kind: "withheld"}
		} else {
			d.Action, d.Message, d.DelayMS = ActRespond, synthetic(rpcID, f), f.Delay.Milliseconds()
			c.Delivered = Describe(d.Message)
		}
		c.End = ptr(time.Now())
	case f.Type == config.FaultHang:
		c.Committed = ptr(false)
		d.Action = ActHang
		c.Delivered = &Outcome{Kind: "withheld"}
		c.End = ptr(time.Now())
	case f.Type == config.FaultDisconnect && f.When == "before":
		c.Committed = ptr(false)
		d.Action = ActDisconnect
		c.Delivered = &Outcome{Kind: "disconnect"}
		c.End = ptr(time.Now())
	default:
		c.Forwarded = true
	}
	snap := *c
	e.mu.Unlock()
	e.emitCall(snap)
	return d
}

// OnResult is called with the server's JSON-RPC response to a forwarded call.
func (e *Engine) OnResult(callID int64, response json.RawMessage) Decision {
	e.mu.Lock()
	c := e.byID[callID]
	if c == nil {
		e.mu.Unlock()
		return Decision{CallID: callID, Action: ActDeliver, Message: response}
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(response, &msg)
	committed := len(msg.Error) == 0 && !isErrorResult(msg.Result)
	c.Committed = &committed
	c.Result = Payload(msg.Result)
	c.ServerOutcome = Describe(response)
	c.End = ptr(time.Now())

	d := Decision{CallID: callID, Action: ActDeliver, Message: response}
	f := c.fault
	switch {
	case f == nil:
	case f.Type == config.FaultDelay:
		d.DelayMS = f.Delay.Milliseconds()
	case f.Type == config.FaultLostResponse:
		if f.RespondWith == "none" {
			d.Action, d.Message = ActWithhold, nil
		} else {
			d.Message, d.DelayMS = synthetic(c.rpcID, f), f.Delay.Milliseconds()
		}
	case f.Type == config.FaultCorrupt:
		if len(msg.Error) == 0 {
			d.Message = Corrupt(response, f.Corrupt)
		}
		d.DelayMS = f.Delay.Milliseconds()
	case f.Type == config.FaultDisconnect:
		d.Action, d.Message = ActDisconnect, nil
	}
	switch d.Action {
	case ActWithhold:
		c.Delivered = &Outcome{Kind: "withheld"}
	case ActDisconnect:
		c.Delivered = &Outcome{Kind: "disconnect"}
	default:
		c.Delivered = Describe(d.Message)
	}
	snap := *c
	e.mu.Unlock()
	e.emitCall(snap)
	return d
}

// OnFailure records a transport-level failure talking to the server (e.g. HTTP 500, process exit).
func (e *Engine) OnFailure(callID int64, text string) {
	e.mu.Lock()
	c := e.byID[callID]
	if c == nil {
		e.mu.Unlock()
		return
	}
	c.ServerOutcome = &Outcome{Kind: "transport_error", Text: truncate(text, 500)}
	if c.Delivered == nil {
		c.Delivered = &Outcome{Kind: "transport_error", Text: truncate(text, 500)}
	}
	c.End = ptr(time.Now())
	snap := *c
	e.mu.Unlock()
	e.emitCall(snap)
}

func (e *Engine) matchLocked(server, tool string, args json.RawMessage) *config.Fault {
	var chosen *config.Fault
	var decoded any
	for _, f := range e.faults {
		if f.Tool != tool || (f.Server != "" && f.Server != server) {
			continue
		}
		if len(f.Args) > 0 {
			if decoded == nil {
				decoded = jsonx.Decode(args)
			}
			if !argsMatch(f.Args, decoded) {
				continue
			}
		}
		e.seen[f.Index]++
		n := e.seen[f.Index]
		inWindow := n >= f.Call && (f.Repeat == 0 || n < f.Call+f.Repeat)
		if chosen == nil && inWindow {
			chosen = f
		}
	}
	return chosen
}

func argsMatch(want map[string]any, args any) bool {
	for path, v := range want {
		got, ok := jsonx.Get(args, path)
		if !ok || !jsonx.Equal(got, v) {
			return false
		}
	}
	return true
}

func isErrorResult(result json.RawMessage) bool {
	var r struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(result, &r)
	return r.IsError
}

func ptr[T any](v T) *T { return &v }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
