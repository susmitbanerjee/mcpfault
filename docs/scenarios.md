# Scenario reference

A scenario is a YAML file named `*.fault.yaml`. It says which MCP servers the agent uses,
how to run the agent, which faults to inject, and what must stay true afterwards.
`mcpfault run` with no arguments runs every scenario under the current directory.

```yaml
name: payment-timeout            # default: file name
description: What this scenario checks.
runs: 5                          # default 5; override with --runs
timeout: 3m                      # per run; default 3m
pause: 0s                        # wait between runs (LLM rate limits); override with --pause

servers: { ... }                 # required
services: [ ... ]                # optional background processes
setup: [ ... ]                   # optional, before every run
teardown: [ ... ]                # optional, after every run
agent: { ... }                   # how to trigger one run
faults: [ ... ]
invariants: [ ... ]
```

## Commands

Any command field accepts either form:

```yaml
command: python agent.py "Pay invoice INV-1042"      # a string runs through the shell (sh -c / cmd /C)
command: [python, agent.py, "Pay invoice INV-1042"]  # a list runs directly, with no shell
```

Commands run from the directory you invoke `mcpfault` in. The directory containing the
`mcpfault` binary is put first on `PATH`, so agents can launch `mcpfault stdio` even if it
isn't installed globally.

### Template variables

Any command, URL, body or `env` value can use these:

| Variable | Value |
|---|---|
| `{{run}}` | run number, starting at 1 |
| `{{run_dir}}` | this run's result directory |
| `{{session_dir}}` | this session's result directory, good for a throwaway database |
| `{{scenario_dir}}` | directory of the scenario file |
| `{{control}}` | control server address, `127.0.0.1:7361` by default |
| `{{proxy.NAME}}` | proxy URL for http server `NAME` |
| `{{env.NAME}}` | environment variable `NAME` |

### Environment variables mcpfault sets

For the agent, services, setup, teardown and checks:

| Variable | Value |
|---|---|
| `MCPFAULT_CONTROL` | control server address |
| `MCPFAULT_RUN`, `MCPFAULT_RUN_DIR` | current run number and directory |
| `MCPFAULT_SESSION_DIR` | session directory |
| `MCPFAULT_PROXY_<NAME>` | proxy URL for each http server (name upper-cased, `-` becomes `_`) |

Checks also receive `MCPFAULT_AGENT_OUTPUT` (a file holding the agent's stdout, or its HTTP
response body) and `MCPFAULT_AGENT_EXIT_CODE`.

## servers

Each key is a server name. Faults and invariants refer to tools as `name/tool`, or just `tool`
to match that tool on any server.

### stdio servers

The agent starts the server itself. In the agent's MCP config, replace the server command
with `mcpfault stdio --server NAME -- <original command>`:

```yaml
servers:
  billing:
    command: [python, server.py, --db, "{{session_dir}}/billing.db"]   # optional: used by `mcpfault lint`
```

Shorthand: `billing: [python, server.py]`.

The shim connects to the control server on `127.0.0.1:7361` (use `--control` or
`MCPFAULT_CONTROL` to change it). When no control server is running it passes traffic
through unchanged and prints one warning.

### http servers

For servers reached by URL (Streamable HTTP, or the legacy HTTP+SSE transport). mcpfault
listens on `listen` and forwards to `url`. Point the agent at the proxy URL, which is the
same path on the listen address.

```yaml
servers:
  crm:
    url: http://localhost:8000/mcp       # the real server
    listen: 127.0.0.1:7362               # agent uses http://127.0.0.1:7362/mcp
```

`listen` defaults to `127.0.0.1:7362`, `7363`, … in alphabetical order of server names. Set it
explicitly if your agent's config hard-codes the URL.

Both Streamable HTTP responses (JSON or SSE) and legacy SSE sessions (`GET /sse` plus
`POST /messages?...`) are supported. Headers such as `Authorization` pass through.

## services

Background processes started once, before the first run, and stopped after the last. Use them
for an MCP server that runs over HTTP, or an agent that runs as a web service (Spring Boot,
FastAPI, Express).

```yaml
services:
  - name: billing-server
    command: [python, server.py, --http, --port, "8765"]
    ready: tcp://127.0.0.1:8765          # or http(s)://... (any HTTP response counts)
    ready_timeout: 2m                    # default 2m
    env: { LOG_LEVEL: warning }
    cwd: services/billing                # relative to where mcpfault runs
```

Services start in order; each must be ready before the next starts. Their output goes to
`<session_dir>/service-<name>.log`.

## setup / teardown

Commands run before and after every run. Use setup to reset test data so each run starts from
the same state. A setup command that exits non-zero turns the run into a run error.

## agent

Exactly one of `command` or `http`.

```yaml
agent:
  command: [python, agent.py, "Pay invoice INV-1042"]   # started once per run; stdout is the output
  env: { MODEL: "anthropic:claude-opus-5" }
  cwd: agents/payments
```

```yaml
agent:
  http:                                   # call an agent that is already running
    url: http://127.0.0.1:8080/agent
    method: POST                          # default POST
    headers: { Authorization: "Bearer {{env.AGENT_TOKEN}}" }
    body: '{"message": "Pay invoice INV-1042"}'
```

A run is an error if the command exits non-zero or times out, or if the HTTP call fails or
returns a status of 400 or above. The response body is the agent's output.

## faults

```yaml
faults:
  - tool: billing/create_payment     # required: tool or server/tool
    args: { currency: USD }          # only calls whose arguments match (dot paths allowed)
    call: 1                          # first matching call to affect (1-based, per run); default 1
    repeat: 1                        # how many matching calls to affect, or "all"; default 1
    type: lost_response              # see below
    respond_with: tool_error         # tool_error | rpc_error | none
    message: "504 Gateway Timeout"
    code: -32001                     # for rpc_error
    delay: 2s                        # extra delay before the injected or real response
```

| `type` | Reaches the server? | What the agent sees |
|---|---|---|
| `error` | no | `message` as a tool error (`isError: true`) or a JSON-RPC error. Good for 429, 503, auth failures. |
| `lost_response` | **yes**, the side effect happens | an error (default "Request timed out"), or nothing with `respond_with: none` |
| `delay` | yes | the real response, `delay` later. Pair it with the agent's own timeout. |
| `corrupt` | yes | the real response, edited (see below) |
| `disconnect` | yes (`when: after`, default) or no (`when: before`) | the connection drops |
| `hang` | no | nothing, ever. The request is released when the run ends. |

Counters reset at the start of every run, so `call: 1` means "the first call in each run".
When several faults match the same call, the first one listed applies.

**`respond_with`**: a tool error (`isError: true` result) and a JSON-RPC error are different
failures to most frameworks. One is shown to the model; the other often surfaces as an
exception. Test both.

### corrupt

Edits the tool result's JSON: `structuredContent` if the server sent it, otherwise the first
text block parsed as JSON. Applied in this order:

```yaml
corrupt:
  empty: true                         # remove all content
  replace: { payments: [] }           # replace the whole payload
  set: { amount: 98000, meta.region: eu }   # set fields (dot paths)
  drop: [currency]                    # remove fields
  truncate: true                      # cut the text in half (invalid JSON)
```

The call record keeps the server's original payload, so invariants still see the truth.

## invariants

Evaluated after every run. `severity: warn` violations are reported but don't fail the session.

### Log invariants

Count the tool calls that went through mcpfault.

```yaml
- name: vendor paid at most once
  tool: billing/create_payment
  committed: true                 # only calls the server executed successfully
  where:                          # optional filters on the call record
    args.currency: USD
    args.amount: { gte: 1000 }
  distinct: result.payment_id     # count distinct values instead of calls
  max: 1                          # and/or min, equals
```

Each call record has these fields, and `where` and `distinct` use dot paths into them:

| Field | Meaning |
|---|---|
| `server`, `tool` | where the call went |
| `args.*` | the arguments the agent sent |
| `forwarded` | whether the request reached the server |
| `committed` | the server returned a successful result (even if the agent never saw it) |
| `result.*` | the server's real result payload |
| `fault.type` | the fault applied to this call, if any |
| `delivered.kind` | what the agent saw: `result`, `tool_error`, `rpc_error`, `withheld`, `disconnect`, `transport_error` |

`where` operators: literal equality, or `eq`, `ne`, `in`, `gt`, `gte`, `lt`, `lte`,
`exists`, `matches` (regex).

Count `distinct` result ids rather than calls when retries are allowed: a correct agent may
retry with the same idempotency key, and the server then returns the original payment.

### Check invariants

Run any command. Exit code 0 passes, and the last line of output is shown as the detail.
This is how you check real state: query a database, call an API, read a file.

```yaml
- name: agent's answer matches the ledger
  check: [python, checks/ledger.py, "--db", "{{session_dir}}/billing.db"]
```

### Output invariants

Regular expressions over the agent's output (multiline mode):

```yaml
- output: { matches: "FINAL: (PAID|NEEDS_REVIEW)" }
- output: { not_matches: "(?i)payment failed" }
```

## Result files

Every session writes to `.mcpfault/sessions/<timestamp>-<name>/`:

```
report.json                 full results: every run, every call, every invariant
service-<name>.log          output of each service
run-001/agent.out.txt       agent stdout (or HTTP response body)
run-001/agent.err.txt       agent stderr
run-001/setup.log           setup command output
run-001/checks.log          check command output
```

`mcpfault ui` browses these sessions.
