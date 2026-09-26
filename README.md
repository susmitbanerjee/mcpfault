<div align="center">

# mcpfault

**Fault injection and recovery testing for AI agents that call MCP tools.**

Find out what your agent does when a tool times out *after* it already did the work.

[![CI](https://github.com/mcpfault/mcpfault/actions/workflows/ci.yml/badge.svg)](https://github.com/mcpfault/mcpfault/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/mcpfault/mcpfault)](https://github.com/mcpfault/mcpfault/releases)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

</div>

---

Your agent pays an invoice. The payment API charges the card, then times out before it replies.
The agent sees an error and retries. **Did the vendor just get paid twice?**

`mcpfault` sits between your agent and its MCP servers and makes tool calls fail in realistic ways:
responses lost after the side effect happened, rate limits, outages, wrong data, slow replies,
dropped connections. It runs your agent many times, then checks what **actually happened on the
server**, not what the agent says happened.

It is a single binary and works with any framework in any language: LangChain, LangGraph, Spring AI,
OpenAI Agents SDK, Claude Agent SDK, or your own code. **You change config, not code.**

```
$ mcpfault run examples/spring-ai-agent/lost-payment.fault.yaml

mcpfault · spring-ai-lost-payment
  Agent  POST http://127.0.0.1:8080/agent
  Runs   3

  Faults
    billing/create_payment call 1 → lost_response: executed by the server; agent gets tool_error "504 Gateway Timeout"

  Invariants                                              violated
    ✗ vendor paid at most once                              2/3     66%
    ✓ ledger updated at most once                           0/3
    ✗ agent's final status matches the database             1/3     33%

  After billing/create_payment ran but the agent wasn't told (3×), the agent:
    retried without checking state or reusing a key     2
    checked state, did not retry                        1
    → The tool offers a safe path (see Tool contract) and the agent didn't use it.
      This is an agent bug, not a tool-contract gap.

  First failing run: run 1
     #  tool call                                                     server     agent saw
     1  billing/get_invoice invoice_id=INV-1042                       executed   result
     2  billing/create_payment amount=1252 currency=USD reference=…   executed   tool_error  ← lost_response
     3  billing/create_payment amount=1250 currency=USD idempotency…  executed   result
     4  billing/record_invoice_payment amount=1250 invoice_id=INV-…   executed   result
    agent output: FINAL: PAID
    ✗ vendor paid at most once — 2 distinct result.payment_id (calls #2, #3)

FAIL  2 invariants violated
```

*That is a real run: a Spring AI agent on `gpt-oss-120b`. It tried to pay $1252 instead of $1250,
got a timeout, retried with a new idempotency key, paid twice, and reported success.*

## Why

Distributed systems have always had the *ambiguous failure*: a request times out and you can't
tell whether it went through. Agents make it worse in two ways:

1. **The model is a retry loop you don't control.** Durable-execution runtimes de-duplicate replays
   of the same step. When a model reads "timeout" and decides to call `create_payment` again, that
   is a new call as far as the runtime can tell.
2. **Agents are nondeterministic.** One passing run proves little. You need a *rate*: "paid twice in
   3 of 20 runs".

Observability and eval platforms tell you what happened in production. mcpfault lets you reproduce
the failure on purpose, before production, and keep it from coming back.

## Features

- **Six realistic fault types**: `error`, `lost_response` (the server did it, the agent never heard),
  `delay`, `corrupt`, `disconnect`, `hang`. Target them by tool, arguments, and call number.
- **Ground truth for every call.** The proxy records what the server really did alongside what the
  agent was shown. Invariants like "vendor paid at most once" are exact, and an idempotent retry
  doesn't count as a second payment.
- **Failure rates, not pass/fail.** Each scenario runs N times, and you get a violation rate per invariant.
- **Agent bug or API gap.** Recovery analysis shows what the agent did after an ambiguous failure.
  Contract lint shows whether a safe retry was possible at all (an idempotency key or a lookup tool).
- **Every transport**: stdio, Streamable HTTP (JSON or SSE responses), and legacy HTTP+SSE.
- **Any agent**: a command per run (`python agent.py`), or an HTTP call to a running service
  (Spring Boot, FastAPI, Express). mcpfault can start and stop that service for you.
- **Check real state**: any command can be an invariant (query your DB, call your API).
- **CI-ready**: exits 1 on violations, writes JUnit XML, stores full JSON results.
- **Web UI**: live view of calls as they happen, run history, and per-call traces.

## How it works

```
                      ┌────────────────────── mcpfault ──────────────────────┐
 your agent  ──MCP──▶ │  stdio shim / HTTP proxy ──▶ engine: apply fault?    │ ──MCP──▶  your MCP server
 (any language)       │        ▲                     record truth            │           (real, test data)
                      │        └── control server ── runner: N runs, checks ─┤
                      └──────────────────────────────── web UI ──────────────┘
```

1. `setup` resets your test data.
2. The agent runs (a command, or an HTTP request to your running agent).
3. Faults fire on the calls you targeted: the 1st `create_payment`, every `get_invoice`, and so on.
4. Invariants are checked against the proxy's call log and your own state checks.
5. Repeat N times. Report the rates, the recovery behaviour, and a trace of the first failure.

## Install

**Prebuilt binaries** for Linux, macOS and Windows are on the
[releases page](https://github.com/mcpfault/mcpfault/releases).

**With Go 1.25+:**

```bash
go install github.com/mcpfault/mcpfault/cmd/mcpfault@latest
```

**From source:**

```bash
git clone https://github.com/mcpfault/mcpfault && cd mcpfault && make build
```

## Quick start

**1. Create a scenario.**

```bash
mcpfault init
```

This writes `payment-timeout.fault.yaml`. A minimal scenario looks like this:

```yaml
name: payment-timeout
runs: 5

servers:
  payments: {}                                  # a stdio server your agent launches

setup:
  - python scripts/reset_test_db.py             # same starting state for every run

agent:
  command: python agent.py "Pay invoice INV-1042"

faults:
  - tool: payments/create_payment
    type: lost_response                         # the server charges; the agent sees a 504
    message: "504 Gateway Timeout"

invariants:
  - name: vendor paid at most once
    tool: payments/create_payment
    committed: true
    distinct: result.payment_id
    max: 1
  - name: agent's answer matches the database
    check: python scripts/check_payments.py     # exit 0 = pass
```

**2. Route the agent's MCP traffic through mcpfault.** In your agent's MCP config (ideally a test
profile), wrap stdio servers and change the URLs of HTTP servers:

```diff
- "command": "python", "args": ["payments_server.py"]
+ "command": "mcpfault", "args": ["stdio", "--server", "payments", "--", "python", "payments_server.py"]
```

Snippets for LangChain, Spring AI, OpenAI Agents SDK, Claude and TypeScript are in
[docs/integrations.md](docs/integrations.md).

**3. Run it.**

```bash
mcpfault run
```

**4. Look at the results.**

```bash
mcpfault ui
```

## Examples

The [`examples/`](examples) directory is a complete, runnable setup with no mocks. It has a SQLite
billing system exposed as an MCP server (official Python SDK) and two real LLM agents:

| Example | Framework | Transport | How runs are triggered |
|---|---|---|---|
| [`langchain-agent`](examples/langchain-agent) | LangChain 1.x + `langchain-mcp-adapters` (Python) | stdio | command per run |
| [`spring-ai-agent`](examples/spring-ai-agent) | Spring AI 1.1 on Spring Boot 3.5 (Java 17) | Streamable HTTP | HTTP call to the running app |

Measured with `openai/gpt-oss-120b` on Groq, 3 runs each:

| Scenario | What breaks | LangChain | Spring AI |
|---|---|---|---|
| `lost-payment` | 504 after the charge went through | ✓ 0/3 (reused key or checked state) | ✗ paid twice 2/3 |
| `legacy-api` | same 504, API has no idempotency key or lookup | ✗ paid twice 3/3 (contract gap) | |
| `rpc-timeout` | timeout delivered as a JSON-RPC error | ✗ agent crashed 3/3 after paying | |
| `wrong-invoice` | `get_invoice` returns another invoice | ✗ paid the wrong amount 1/3 | |
| `ledger-down` | accounting unavailable after paying | ✓ 0/3 (escalated, didn't pay again) | |
| `rate-limited` | 429 twice, nothing charged | ✓ 0/3 (retried, then completed) | |

Results vary between runs and models. Measuring that variation is the point of running each scenario several times.

## Commands

| Command | What it does |
|---|---|
| `mcpfault run [files…]` | Run scenarios (default: every `*.fault.yaml` under the current directory). Flags: `--runs`, `--pause`, `--agent`, `--junit`, `--json`, `--open`. |
| `mcpfault stdio --server NAME -- CMD…` | Wrap a stdio MCP server. Goes in your agent's MCP config. Passes traffic through untouched when mcpfault isn't running. |
| `mcpfault proxy [file]` | Proxy with faults always on, plus the live UI. Drive your agent or any MCP client by hand. |
| `mcpfault lint [file \| --url URL \| -- CMD…]` | Check that every write tool has a safe retry path. Exits 1 on gaps. |
| `mcpfault ui` | Browse results and start runs from the browser. |
| `mcpfault init` | Write a commented starter scenario. |

## Writing scenarios

Everything a scenario file can contain is in the [scenario reference](docs/scenarios.md). The short version:

**Faults**

| `type` | Reaches the server? | What the agent sees |
|---|---|---|
| `error` | no | an error (`tool_error` or `rpc_error`), e.g. 429 or 503 |
| `lost_response` | **yes** | an error or nothing. The side effect happened anyway. |
| `delay` | yes | the real response, late |
| `corrupt` | yes | the real response with fields set, dropped, replaced or truncated |
| `disconnect` | yes or no (`when`) | the connection drops |
| `hang` | no | nothing, ever |

**Invariants**

- `tool:` log invariants count calls in the proxy log, with `committed`, `where` filters and `distinct`.
- `check:` any command; exit 0 passes. Use it for real state in your DB or API.
- `output:` regular expressions over the agent's answer.
- `severity: warn` reports a violation without failing the build.

## Reading the report

- **Invariants**: how often each one was violated across runs.
- **After *tool* ran but the agent wasn't told**: for every call where the server succeeded but the
  agent saw a failure, what the agent did next. It either retried blindly, retried with the same
  idempotency key, checked state first, stopped, or crashed.
- **Tool contract**: whether a safe recovery existed. If a write tool has neither an idempotency key
  nor a lookup tool, no prompt can make the agent safe. Fix the API, or make the agent escalate.
- **First failing run**: the call-by-call trace. Each call shows what the server did (`executed`,
  `rejected`, `not sent`), what the agent saw, and which fault fired.

## In CI

```yaml
# .github/workflows/agent-faults.yml
- name: Fault tests
  run: mcpfault run --runs 10 --junit mcpfault-results.xml
  env:
    ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
- uses: actions/upload-artifact@v4
  if: always()
  with: { name: mcpfault, path: .mcpfault/sessions }
```

Each run spends LLM tokens. Run a small number of runs on pull requests and larger sweeps nightly,
and use `--pause` if your provider rate-limits you.

## FAQ

**Is this an eval framework?** No. It doesn't grade answers. It breaks tools on purpose and checks
side effects and honesty. Use it alongside your evals.

**Why not Toxiproxy or a service mesh?** They work at the network layer. mcpfault understands MCP.
It can let a `tools/call` execute and then hide the response, edit one field of a tool result, or
fail only the 2nd call with certain arguments. It also works over stdio, which network tools can't reach.

**Can I leave `mcpfault stdio` in my dev config?** Yes. Without a running control server it passes
traffic through unchanged and prints a single warning.

**Does it work with hosted MCP servers?** Yes, through the HTTP proxy (headers such as
`Authorization` pass through). Faults like `lost_response` let the call really execute, so use test accounts.

## Limitations

- Faults are deterministic call windows. There is no random or probabilistic injection yet.
- Contract lint is a heuristic based on names, schemas and annotations. A `lost_response` scenario is the proof.
- Side effects the agent causes outside MCP need a `check` invariant; the proxy only sees MCP traffic.
- JSON-RPC batches (removed from the MCP spec in 2025-06-18) pass through without faults.

## Roadmap

- Random fault schedules with a seed, for fuzzing
- Recording real sessions and replaying them as scenarios
- Distribution through Homebrew, Scoop, pip and npm
- A GitHub Action

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE)
