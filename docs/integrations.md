# Connecting your agent

mcpfault doesn't import anything into your agent and doesn't care what language it's written in.
It only needs the agent's MCP traffic to pass through it, and there are two ways to do that:

- **stdio servers** (the agent starts the server as a process): put
  `mcpfault stdio --server NAME --` in front of the server command.
- **HTTP servers** (the agent connects by URL): point the agent at the proxy URL, e.g.
  `http://127.0.0.1:7362/mcp` instead of `http://localhost:8000/mcp`.

`NAME` is the server's key under `servers:` in your scenario. Keep your production config as it
is and make the change in a test profile or an environment-specific config file.

> Tested end to end in this repo: **LangChain** ([examples/langchain-agent](../examples/langchain-agent))
> and **Spring AI** ([examples/spring-ai-agent](../examples/spring-ai-agent)). The other snippets
> apply the same config change to each framework's documented MCP settings.

## LangChain / LangGraph (Python)

With [`langchain-mcp-adapters`](https://github.com/langchain-ai/langchain-mcp-adapters):

```python
from langchain_mcp_adapters.client import MultiServerMCPClient

client = MultiServerMCPClient({
    "billing": {                                   # stdio server
        "transport": "stdio",
        "command": "mcpfault",
        "args": ["stdio", "--server", "billing", "--", "python", "billing_server.py"],
    },
    "crm": {                                       # HTTP server, through the proxy
        "transport": "streamable_http",
        "url": "http://127.0.0.1:7362/mcp",
    },
})
```

`langchain-mcp-adapters` 0.3 requires the MCP Python SDK 1.x (`mcp<2`).

What the example found: a JSON-RPC error from a tool (`respond_with: rpc_error`) is raised as an
exception rather than shown to the model, so the agent run crashes. Tool errors
(`respond_with: tool_error`) are passed to the model.

## Spring AI (Java)

`application.yml`, with `spring-ai-starter-mcp-client`:

```yaml
spring:
  ai:
    mcp:
      client:
        # HTTP server, through the proxy
        streamable-http:
          connections:
            billing:
              url: http://127.0.0.1:7362
              endpoint: /mcp
        # stdio server, wrapped
        stdio:
          connections:
            ledger:
              command: mcpfault
              args: [stdio, --server, ledger, --, java, -jar, ledger-server.jar]
```

A Spring Boot agent is usually a long-running service, so start it as a scenario `service`
and trigger runs with `agent.http`. See
[examples/spring-ai-agent/lost-payment.fault.yaml](../examples/spring-ai-agent/lost-payment.fault.yaml).
The MCP client connects when the app starts, so the MCP server must be up before the Spring
service starts. List it first under `services`.

## OpenAI Agents SDK (Python)

```python
from agents.mcp import MCPServerStdio, MCPServerStreamableHttp

billing = MCPServerStdio(params={
    "command": "mcpfault",
    "args": ["stdio", "--server", "billing", "--", "python", "billing_server.py"],
})
crm = MCPServerStreamableHttp(params={"url": "http://127.0.0.1:7362/mcp"})
```

## Claude Agent SDK, Claude Code, Claude Desktop, Cursor, and other `mcpServers` JSON configs

```json
{
  "mcpServers": {
    "billing": {
      "command": "mcpfault",
      "args": ["stdio", "--server", "billing", "--", "python", "billing_server.py"]
    }
  }
}
```

## TypeScript MCP SDK / Vercel AI SDK

```ts
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const billing = new StdioClientTransport({
  command: "mcpfault",
  args: ["stdio", "--server", "billing", "--", "node", "billing-server.js"],
});
const crm = new StreamableHTTPClientTransport(new URL("http://127.0.0.1:7362/mcp"));
```

## Anything else

If your framework starts MCP servers from a command, wrap the command. If it connects by URL,
change the URL. mcpfault works at the protocol level (JSON-RPC over stdio, Streamable HTTP, or
HTTP+SSE), so the agent's language and framework don't matter.

## Checking the wiring

- The report warns when a declared server never connected through mcpfault:
  `✗ server "billing" never connected through mcpfault. Point your agent at: ...`
- `mcpfault proxy scenario.fault.yaml --open` runs the proxy with faults always on and shows
  every call live, so you can drive the agent by hand and watch.
- A shim that can't reach the control server prints
  `mcpfault: control server at 127.0.0.1:7361 is not reachable; passing traffic through unmodified`.

## Remote servers with real side effects

The HTTP proxy works with hosted MCP servers too: it forwards headers such as `Authorization`
unchanged. Remember that `lost_response`, `delay`, `corrupt` and `disconnect` let the call
**really execute**, so point the agent at test or sandbox accounts, never production.
