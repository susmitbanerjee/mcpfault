"""Reset the billing database to a known state: two unpaid invoices, no payments.

    python seed.py --db billing.db
"""

import argparse
import sqlite3
from contextlib import closing

from schema import ensure_schema

INVOICES = [
    ("INV-1042", "Acme Supplies", 1250.00, "USD"),
    ("INV-1043", "Globex Corp", 380.00, "EUR"),
]

parser = argparse.ArgumentParser()
parser.add_argument("--db", default="billing.db")
args = parser.parse_args()

with closing(sqlite3.connect(args.db, timeout=10, isolation_level=None)) as conn:
    ensure_schema(conn)
    conn.execute("BEGIN IMMEDIATE")
    conn.execute("DELETE FROM ledger")
    conn.execute("DELETE FROM payments")
    conn.execute("DELETE FROM invoices")
    conn.executemany("INSERT INTO invoices (invoice_id, vendor, amount, currency) VALUES (?, ?, ?, ?)", INVOICES)
    conn.execute("COMMIT")
print(f"seeded {args.db}: {len(INVOICES)} unpaid invoices")
