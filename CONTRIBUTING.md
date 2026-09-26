# Contributing to mcpfault

Thanks for helping. Bug reports, new fault types, framework integration guides and example agents
are all welcome.

## Development setup

You need Go 1.25+.

```bash
make build        # builds ./bin/mcpfault
make test         # go test ./...
make check        # gofmt check + go vet + tests
```

The tests use the official Go MCP SDK as both client and server. They cover every fault type over
every transport (stdio, Streamable HTTP with JSON and SSE responses, legacy HTTP+SSE), plus a full
end-to-end run in which the test binary plays the agent, the stdio shim and the MCP server.
No Python, Java or LLM keys are needed.

The examples under `examples/` need Python 3.10+ and, for Spring AI, Java 17+ and Maven. See
[examples/README.md](examples/README.md).

## Repository layout

```
cmd/mcpfault/          main package
internal/cli/          cobra commands
internal/config/       scenario parsing and validation
internal/engine/       fault decisions and the call log (the core)
internal/proxy/        data planes: stdio shim, HTTP / SSE reverse proxy
internal/server/       control API for shims + web UI (embedded static files in ui/)
internal/runner/       runs a scenario: services, setup, agent, invariants
internal/invariants/   log / check / output invariants
internal/lint/         tool-contract heuristics and the MCP client used to list tools
internal/report/       report model, terminal output, JUnit
examples/              runnable system under test + LangChain and Spring AI agents
docs/                  scenario reference and integration guides
```

## Design rules

- **The proxy is transparent.** Anything mcpfault doesn't inject passes through byte-for-byte:
  unknown methods, notifications, server-to-client requests, headers. Proxies work at the JSON-RPC
  level on purpose and don't use an MCP SDK, so they don't depend on SDK versions.
- **Record the truth.** For every call, the engine keeps what the server did and what the agent saw
  as separate fields. Invariants depend on that distinction.
- **No agent code changes.** Integration is always a config change: wrap a command or change a URL.
- **Cross-platform.** Everything must work on Linux, macOS and Windows (process groups, shell
  quoting, path handling). CI runs all three.

## Adding a fault type

1. Add the type and its validation in `internal/config/config.go`.
2. Decide what it does in `internal/engine`: `OnCall` for faults that apply before the server,
   `OnResult` for faults that apply after it answers.
3. If it needs a new proxy action, handle it in `internal/proxy/stdio.go` and `internal/proxy/http.go`.
4. Add a test for every transport in `internal/proxy/proxy_test.go`.
5. Document it in `docs/scenarios.md` and the README table.

## Pull requests

- Keep changes focused, and include tests.
- Run `make check` before pushing.
- For user-facing changes, update the docs in the same PR.

## Windows note

Some Windows machines use Smart App Control, which can block freshly compiled test binaries in
`%TEMP%`. If `go test` fails with "An Application Control policy has blocked this file", point Go's
temporary directory into the repository and run again:

```bash
GOTMPDIR=$PWD/.gotmp go test ./...
```
