"""A small billing system exposed over MCP, backed by SQLite.

This is the "system under test" for the examples: a real MCP server with real
persistence. Point it at a throwaway database and reset it with seed.py.

    python server.py --db billing.db                       # stdio
    python server.py --db billing.db --http --port 8765    # Streamable HTTP at /mcp
    python server.py --db billing.db --sse --port 8765     # legacy HTTP+SSE at /sse

    --legacy   no idempotency keys and no way to list payments, like many internal APIs
               (or set BILLING_LEGACY=1)
"""

from __future__ import annotations

import argparse
import os
import sqlite3
import uuid
from contextlib import closing
from datetime import datetime, timezone
from typing import Any

from mcp.types import ToolAnnotations

try:  # MCP Python SDK 2.x
    from mcp.server.mcpserver import MCPServer

    SDK_V2 = True
except ImportError:  # 1.x, which langchain-mcp-adapters still requires
    from mcp.server.fastmcp import FastMCP as MCPServer

    SDK_V2 = False

from schema import ensure_schema

parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
parser.add_argument("--db", default="billing.db", help="SQLite database file")
parser.add_argument("--http", action="store_true", help="serve Streamable HTTP instead of stdio")
parser.add_argument("--sse", action="store_true", help="serve the legacy HTTP+SSE transport")
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, default=8765)
parser.add_argument("--legacy", action="store_true", help="remove idempotency keys and payment lookups")
args = parser.parse_args()
args.legacy = args.legacy or os.environ.get("BILLING_LEGACY", "") not in ("", "0", "false")

INSTRUCTIONS = "Invoices, payments and the accounting ledger."
if SDK_V2:
    server = MCPServer("billing", instructions=INSTRUCTIONS, log_level="WARNING")
else:
    server = MCPServer("billing", instructions=INSTRUCTIONS, log_level="WARNING", host=args.host, port=args.port)
READ_ONLY = ToolAnnotations(readOnlyHint=True)


def db() -> sqlite3.Connection:
    # A fresh connection per call, so resets from seed.py are always visible.
    conn = sqlite3.connect(args.db, timeout=10, isolation_level=None)
    conn.row_factory = sqlite3.Row
    ensure_schema(conn)
    return conn


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def invoice_view(conn: sqlite3.Connection, invoice_id: str) -> dict[str, Any] | None:
    row = conn.execute("SELECT * FROM invoices WHERE invoice_id = ?", (invoice_id,)).fetchone()
    if row is None:
        return None
    recorded = [r["payment_id"] for r in conn.execute("SELECT payment_id FROM ledger WHERE invoice_id = ? ORDER BY rowid", (invoice_id,))]
    return {**dict(row), "recorded_payments": recorded}


@server.tool(annotations=READ_ONLY, structured_output=False)
def get_invoice(invoice_id: str) -> dict[str, Any]:
    """Fetch an invoice with its status and the payments recorded against it in the ledger."""
    with closing(db()) as conn:
        view = invoice_view(conn, invoice_id)
    if view is None:
        raise ValueError(f"Invoice {invoice_id} not found")
    return view


def _create_payment(reference: str, amount: float, currency: str, idempotency_key: str | None) -> dict[str, Any]:
    if amount <= 0:
        raise ValueError("amount must be positive")
    with closing(db()) as conn:
        conn.execute("BEGIN IMMEDIATE")
        if idempotency_key:
            existing = conn.execute("SELECT * FROM payments WHERE idempotency_key = ?", (idempotency_key,)).fetchone()
            if existing is not None:
                conn.execute("COMMIT")
                return {k: existing[k] for k in existing.keys() if k != "idempotency_key"}
        payment = {
            "payment_id": "pay_" + uuid.uuid4().hex[:10],
            "reference": reference,
            "amount": amount,
            "currency": currency.upper(),
            "status": "succeeded",
            "created_at": now(),
        }
        conn.execute(
            "INSERT INTO payments (payment_id, reference, amount, currency, status, idempotency_key, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
            (payment["payment_id"], reference, amount, payment["currency"], "succeeded", idempotency_key, payment["created_at"]),
        )
        conn.execute("COMMIT")
    return payment


if args.legacy:

    @server.tool(structured_output=False)
    def create_payment(reference: str, amount: float, currency: str) -> dict[str, Any]:
        """Send a payment to the vendor on file for the reference. Charges immediately."""
        return _create_payment(reference, amount, currency, None)

else:

    @server.tool(structured_output=False)
    def create_payment(reference: str, amount: float, currency: str, idempotency_key: str | None = None) -> dict[str, Any]:
        """Send a payment to the vendor on file for the reference. Charges immediately.

        idempotency_key is optional: requests with the same key return the original
        payment instead of charging again.
        """
        return _create_payment(reference, amount, currency, idempotency_key)

    @server.tool(annotations=READ_ONLY, structured_output=False)
    def list_payments(reference: str | None = None) -> dict[str, Any]:
        """List payments, optionally only those for one reference (e.g. an invoice id)."""
        with closing(db()) as conn:
            if reference:
                rows = conn.execute("SELECT * FROM payments WHERE reference = ? ORDER BY created_at", (reference,)).fetchall()
            else:
                rows = conn.execute("SELECT * FROM payments ORDER BY created_at").fetchall()
        return {"payments": [{k: r[k] for k in r.keys() if k != "idempotency_key"} for r in rows]}


@server.tool(structured_output=False)
def record_invoice_payment(invoice_id: str, payment_id: str, amount: float) -> dict[str, Any]:
    """Record a completed payment against an invoice in the accounting ledger and mark the invoice paid."""
    with closing(db()) as conn:
        conn.execute("BEGIN IMMEDIATE")
        if conn.execute("SELECT 1 FROM invoices WHERE invoice_id = ?", (invoice_id,)).fetchone() is None:
            conn.execute("ROLLBACK")
            raise ValueError(f"Invoice {invoice_id} not found")
        entry_id = "je_" + uuid.uuid4().hex[:10]
        conn.execute(
            "INSERT INTO ledger (entry_id, invoice_id, payment_id, amount, recorded_at) VALUES (?, ?, ?, ?, ?)",
            (entry_id, invoice_id, payment_id, amount, now()),
        )
        conn.execute("UPDATE invoices SET status = 'paid' WHERE invoice_id = ?", (invoice_id,))
        conn.execute("COMMIT")
    return {"entry_id": entry_id, "invoice_id": invoice_id, "payment_id": payment_id, "amount": amount, "invoice_status": "paid"}


if __name__ == "__main__":
    transport = "streamable-http" if args.http else "sse" if args.sse else "stdio"
    if SDK_V2 and transport != "stdio":
        server.run(transport, host=args.host, port=args.port)
    else:
        server.run(transport)
