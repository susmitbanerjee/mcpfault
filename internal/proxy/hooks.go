// Package proxy contains the data planes: a stdio shim the agent launches in place of
// an MCP server, and an HTTP reverse proxy for remote MCP servers. Both ask the engine
// (directly or over the control API) what to do with each tools/call.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/mcpfault/mcpfault/internal/engine"
)

// Hooks is how a proxy consults the engine. ok=false means "no decision available,
// pass traffic through unchanged".
type Hooks interface {
	Hello(server string)
	Call(server string, id json.RawMessage, tool string, args json.RawMessage) (d engine.Decision, ok bool)
	Result(callID int64, response json.RawMessage) (d engine.Decision, ok bool)
	Tools(server string, result json.RawMessage)
}

// LocalHooks calls an in-process engine.
type LocalHooks struct{ Engine *engine.Engine }

func (h LocalHooks) Hello(server string) { h.Engine.Touch(server) }
func (h LocalHooks) Call(server string, id json.RawMessage, tool string, args json.RawMessage) (engine.Decision, bool) {
	h.Engine.Touch(server)
	return h.Engine.OnCall(server, id, tool, args), true
}
func (h LocalHooks) Result(callID int64, response json.RawMessage) (engine.Decision, bool) {
	return h.Engine.OnResult(callID, response), true
}
func (h LocalHooks) Tools(server string, result json.RawMessage) {
	h.Engine.OnToolsList(server, result)
}

// CallRequest is the body of POST /v1/hooks/call.
type CallRequest struct {
	Server string          `json:"server"`
	ID     json.RawMessage `json:"id"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
}

// ResultRequest is the body of POST /v1/hooks/result.
type ResultRequest struct {
	CallID   int64           `json:"call_id"`
	Response json.RawMessage `json:"response"`
}

// ToolsRequest is the body of POST /v1/hooks/tools.
type ToolsRequest struct {
	Server string          `json:"server"`
	Result json.RawMessage `json:"result"`
}

// RemoteHooks talks to a running mcpfault control server. If it can't be reached,
// traffic passes through unmodified and a single warning is printed.
type RemoteHooks struct {
	Addr   string
	client *http.Client
	warn   sync.Once
}

// NewRemoteHooks returns hooks for the control server at addr (host:port).
func NewRemoteHooks(addr string) *RemoteHooks {
	return &RemoteHooks{Addr: addr, client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *RemoteHooks) post(path string, body any, out any) bool {
	b, _ := json.Marshal(body)
	resp, err := h.client.Post("http://"+h.Addr+path, "application/json", bytes.NewReader(b))
	if err != nil {
		h.warn.Do(func() {
			fmt.Fprintf(os.Stderr, "mcpfault: control server at %s is not reachable; passing traffic through unmodified\n", h.Addr)
		})
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return false
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out) == nil
	}
	return true
}

func (h *RemoteHooks) Hello(server string) {
	h.post("/v1/hooks/hello", map[string]string{"server": server}, nil)
}

func (h *RemoteHooks) Call(server string, id json.RawMessage, tool string, args json.RawMessage) (engine.Decision, bool) {
	var d engine.Decision
	ok := h.post("/v1/hooks/call", CallRequest{Server: server, ID: id, Tool: tool, Args: args}, &d)
	return d, ok
}

func (h *RemoteHooks) Result(callID int64, response json.RawMessage) (engine.Decision, bool) {
	var d engine.Decision
	ok := h.post("/v1/hooks/result", ResultRequest{CallID: callID, Response: response}, &d)
	return d, ok
}

func (h *RemoteHooks) Tools(server string, result json.RawMessage) {
	h.post("/v1/hooks/tools", ToolsRequest{Server: server, Result: result}, nil)
}

// message is the subset of a JSON-RPC message the proxies inspect.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func parseMessage(b []byte) (*message, bool) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil, false // batches and non-JSON pass through untouched
	}
	var m message
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	return &m, true
}

func (m *message) isRequest() bool  { return len(m.ID) > 0 && m.Method != "" }
func (m *message) isResponse() bool { return len(m.ID) > 0 && m.Method == "" }
func (m *message) idKey() string    { return string(bytes.TrimSpace(m.ID)) }
