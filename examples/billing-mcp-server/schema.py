"""Database schema shared by the server, the seed script and the checks."""

import sqlite3

SCHEMA = """
CREATE TABLE IF NOT EXISTS invoices (
    invoice_id TEXT PRIMARY KEY,
    vendor     TEXT NOT NULL,
    amount     REAL NOT NULL,
    currency   TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'unpaid'
);
CREATE TABLE IF NOT EXISTS payments (
    payment_id      TEXT PRIMARY KEY,
    reference       TEXT NOT NULL,
    amount          REAL NOT NULL,
    currency        TEXT NOT NULL,
    status          TEXT NOT NULL,
    idempotency_key TEXT UNIQUE,
    created_at      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ledger (
    entry_id    TEXT PRIMARY KEY,
    invoice_id  TEXT NOT NULL,
    payment_id  TEXT NOT NULL,
    amount      REAL NOT NULL,
    recorded_at TEXT NOT NULL
);
"""


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript(SCHEMA)
