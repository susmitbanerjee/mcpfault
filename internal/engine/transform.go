package engine

import (
	"encoding/json"
	"strings"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/jsonx"
)

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

// synthetic builds the response the agent sees when a fault replaces the real one.
func synthetic(id json.RawMessage, f *config.Fault) json.RawMessage {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id}
	if f.RespondWith == "rpc_error" {
		resp.Error = map[string]any{"code": f.Code, "message": f.Message}
	} else {
		resp.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": f.Message}},
			"isError": true,
		}
	}
	b, _ := json.Marshal(resp)
	return b
}

// Describe summarizes a JSON-RPC response message.
func Describe(msg json.RawMessage) *Outcome {
	var m struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return &Outcome{Kind: "transport_error", Text: truncate(string(msg), 300)}
	}
	if m.Error != nil {
		return &Outcome{Kind: "rpc_error", Code: m.Error.Code, Text: truncate(m.Error.Message, 500)}
	}
	kind := "result"
	if isErrorResult(m.Result) {
		kind = "tool_error"
	}
	return &Outcome{Kind: kind, Text: truncate(resultText(m.Result), 500)}
}

func resultText(result json.RawMessage) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(result, &r)
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Payload is the structured data of a tool result: structuredContent if present,
// otherwise the first text block parsed as JSON. Returns nil if neither applies.
func Payload(result json.RawMessage) any {
	var r struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(result, &r) != nil {
		return nil
	}
	if len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		return jsonx.Decode(r.StructuredContent)
	}
	for _, c := range r.Content {
		if c.Type == "text" {
			return jsonx.Decode([]byte(c.Text))
		}
	}
	return nil
}

// Corrupt applies a corrupt spec to a JSON-RPC response carrying a tool result.
// Order: empty, replace, set, drop, truncate.
func Corrupt(response json.RawMessage, spec *config.Corrupt) json.RawMessage {
	var msg map[string]any
	if json.Unmarshal(response, &msg) != nil || spec == nil {
		return response
	}
	result, _ := msg["result"].(map[string]any)
	if result == nil {
		return response
	}
	if spec.Empty {
		result["content"] = []any{}
		delete(result, "structuredContent")
		return marshal(msg, response)
	}

	resultRaw, _ := json.Marshal(result)
	payload := Payload(resultRaw)
	hasPayload := payload != nil
	if spec.Replace != nil {
		payload, hasPayload = jsonx.Normalize(spec.Replace), true
	}
	if obj, ok := payload.(map[string]any); ok {
		for path, v := range spec.Set {
			jsonx.Set(obj, path, jsonx.Normalize(v))
		}
		for _, path := range spec.Drop {
			jsonx.Delete(obj, path)
		}
	}
	if hasPayload {
		text, _ := json.Marshal(payload)
		if _, ok := result["structuredContent"]; ok {
			result["structuredContent"] = payload
		}
		content, _ := result["content"].([]any)
		replaced := false
		for _, item := range content {
			if block, ok := item.(map[string]any); ok && block["type"] == "text" {
				block["text"] = string(text)
				replaced = true
				break
			}
		}
		if !replaced {
			result["content"] = append([]any{map[string]any{"type": "text", "text": string(text)}}, content...)
		}
	}
	if spec.Truncate {
		content, _ := result["content"].([]any)
		for _, item := range content {
			if block, ok := item.(map[string]any); ok && block["type"] == "text" {
				if s, ok := block["text"].(string); ok && len(s) > 1 {
					block["text"] = s[:len(s)/2]
				}
				break
			}
		}
		delete(result, "structuredContent")
	}
	return marshal(msg, response)
}

func marshal(v any, fallback json.RawMessage) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return fallback
	}
	return b
}
