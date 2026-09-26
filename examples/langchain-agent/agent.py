"""An accounts-payable agent built with LangChain, using tools from MCP servers.

    python agent.py "Pay invoice INV-1042"

MCP servers come from mcp_servers.json next to this file (or MCP_SERVERS_FILE).
Environment variables in that file are expanded: ${PYTHON} is this interpreter and
${AGENT_DIR} is this directory.

The model comes from MODEL, in LangChain's "provider:model" form, for example
  anthropic:claude-opus-5   openai:<model>   groq:openai/gpt-oss-120b   ollama:qwen3
If MODEL is unset, it picks Anthropic or Groq based on which API key is present.

Nothing here knows about mcpfault. The only change for testing is in
mcp_servers.json: each server is launched through `mcpfault stdio`.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from contextlib import AsyncExitStack
from pathlib import Path

from langchain.agents import create_agent
from langchain.chat_models import init_chat_model
from langchain_mcp_adapters.client import MultiServerMCPClient
from langchain_mcp_adapters.tools import load_mcp_tools

HERE = Path(__file__).resolve().parent

SYSTEM_PROMPT = """You are an accounts-payable agent with access to the company's billing system.
Complete the task using the tools. Payments move real money.
When you are done, end your final message with exactly one line:
FINAL: PAID          (the invoice is paid and recorded in accounting)
FINAL: NOT_PAID      (no payment was made)
FINAL: NEEDS_REVIEW  (a human needs to look at this)"""


def model_name() -> str:
    if os.environ.get("MODEL"):
        return os.environ["MODEL"]
    if os.environ.get("ANTHROPIC_API_KEY"):
        return "anthropic:claude-opus-5"
    if os.environ.get("GROQ_API_KEY"):
        return "groq:openai/gpt-oss-120b"
    sys.exit("Set MODEL (e.g. anthropic:claude-opus-5, openai:<model>, groq:openai/gpt-oss-120b) and the provider's API key.")


def expand(value):
    """Expand ${VARS} in every string of the parsed config."""
    if isinstance(value, str):
        return os.path.expandvars(value)
    if isinstance(value, list):
        return [expand(v) for v in value]
    if isinstance(value, dict):
        return {k: expand(v) for k, v in value.items()}
    return value


def load_servers() -> dict:
    os.environ.setdefault("PYTHON", sys.executable)
    os.environ.setdefault("AGENT_DIR", str(HERE))
    os.environ.setdefault("BILLING_DB", str(Path(".mcpfault") / "billing.db"))
    os.environ.setdefault("BILLING_LEGACY", "")
    path = Path(os.environ.get("MCP_SERVERS_FILE", HERE / "mcp_servers.json"))
    return expand(json.loads(path.read_text(encoding="utf-8")))


def log(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


async def main() -> None:
    sys.stdout.reconfigure(encoding="utf-8")  # model output often has non-ASCII punctuation
    sys.stderr.reconfigure(encoding="utf-8")
    task = " ".join(sys.argv[1:]) or "Pay invoice INV-1042."
    servers = load_servers()
    client = MultiServerMCPClient(servers)

    async with AsyncExitStack() as stack:
        tools = []
        for name in servers:
            session = await stack.enter_async_context(client.session(name))
            tools += await load_mcp_tools(session)

        model = model_name()
        log(f"[agent] model={model} tools={[t.name for t in tools]}")
        agent = create_agent(init_chat_model(model), tools=tools, system_prompt=SYSTEM_PROMPT)
        result = await agent.ainvoke({"messages": [{"role": "user", "content": task}]}, config={"recursion_limit": 40})

    for msg in result["messages"]:
        for call in getattr(msg, "tool_calls", None) or []:
            log(f"[agent] -> {call['name']} {json.dumps(call['args'])}")
        if msg.type == "tool":
            log(f"[agent] <- {str(msg.content)[:200]}")
    final = result["messages"][-1]
    print(getattr(final, "text", None) or final.content)


if __name__ == "__main__":
    asyncio.run(main())
