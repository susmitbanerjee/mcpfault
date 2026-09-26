package lint

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Target is a server to fetch tools from: either a stdio command or an HTTP URL.
type Target struct {
	Command []string
	Env     []string
	Dir     string
	URL     string
	SSE     bool // use the legacy HTTP+SSE transport for URL
}

// FetchTools connects with the official MCP client, lists every tool, and disconnects.
func FetchTools(ctx context.Context, t Target, version string) ([]Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var transport mcp.Transport
	switch {
	case len(t.Command) > 0:
		cmd := exec.Command(t.Command[0], t.Command[1:]...)
		cmd.Env, cmd.Dir = t.Env, t.Dir
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}
	case t.URL != "" && (t.SSE || strings.HasSuffix(strings.TrimRight(t.URL, "/"), "/sse")):
		transport = &mcp.SSEClientTransport{Endpoint: t.URL, HTTPClient: &http.Client{}}
	case t.URL != "":
		transport = &mcp.StreamableClientTransport{Endpoint: t.URL, DisableStandaloneSSE: true, MaxRetries: -1}
	default:
		return nil, fmt.Errorf("nothing to connect to")
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "mcpfault-lint", Version: version}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	defer session.Close()

	var tools []Tool
	params := &mcp.ListToolsParams{}
	for {
		res, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		raw, _ := json.Marshal(res.Tools)
		var page []Tool
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		tools = append(tools, page...)
		if res.NextCursor == "" {
			return tools, nil
		}
		params.Cursor = res.NextCursor
	}
}
