#!/usr/bin/env python3
"""Audit an isolated SQLite event database copy; emit counts, never event data."""

import argparse
from contextlib import closing
import json
from pathlib import Path
import sqlite3
import time


CHECKS = {
    "events": "SELECT count(*) FROM events",
    "fts_rows": "SELECT count(*) FROM event_fts",
    "mapped_rows": "SELECT count(*) FROM event_fts_rowids",
    "events_without_mapping": """SELECT count(*) FROM events e
        LEFT JOIN event_fts_rowids m ON m.event_id=e.id WHERE m.event_id IS NULL""",
    "mappings_without_event": """SELECT count(*) FROM event_fts_rowids m
        LEFT JOIN events e ON e.id=m.event_id WHERE e.id IS NULL""",
    "invalid_mappings": """SELECT count(*) FROM event_fts_rowids m
        LEFT JOIN event_fts f ON f.rowid=m.fts_rowid
        WHERE f.rowid IS NULL OR f.event_id IS NOT m.event_id""",
    "fts_without_mapping": """SELECT count(*) FROM event_fts f
        LEFT JOIN event_fts_rowids m ON m.fts_rowid=f.rowid
        WHERE m.event_id IS NULL""",
    "content_mismatches": """SELECT count(*) FROM event_fts_rowids m
        JOIN events e ON e.id=m.event_id JOIN event_fts f ON f.rowid=m.fts_rowid
        WHERE f.content IS NOT e.content""",
}


def audit(path, max_seconds=600, emit=lambda value: None):
    deadline = time.monotonic() + max_seconds
    result = {"status": "failed", "checks": {}}
    check = "open"
    try:
        # mode=ro forbids creating a missing database or changing stored rows.
        # Do not use immutable=1: a restored snapshot can contain a WAL.
        with closing(sqlite3.connect(Path(path).resolve().as_uri() + "?mode=ro", uri=True,
                                    timeout=min(max_seconds, 5))) as db:
            db.execute("PRAGMA query_only=ON")
            db.set_progress_handler(lambda: int(time.monotonic() >= deadline), 10000)
            check = "schema_version"
            result["schema_version"] = db.execute("PRAGMA user_version").fetchone()[0]
            emit({check: result[check]})
            if result[check] != 8:
                result["failed_check"] = check
                return result
            check = "quick_check"
            result["quick_check_ok"] = db.execute("PRAGMA quick_check").fetchall() == [("ok",)]
            emit({"quick_check_ok": result["quick_check_ok"]})
            if not result["quick_check_ok"]:
                result["failed_check"] = check
                return result
            # One read transaction gives all counts the same snapshot.
            db.execute("BEGIN")
            for check, query in CHECKS.items():
                count = db.execute(query).fetchone()[0]
                result["checks"][check] = count
                emit({check: count})
            db.rollback()
        counts = result["checks"]
        if (counts["events"] == counts["fts_rows"] == counts["mapped_rows"] and
                all(counts[name] == 0 for name in list(CHECKS)[3:])):
            result["status"] = "passed"
        else:
            result["failed_check"] = "search_coverage"
    except (OSError, ValueError, sqlite3.Error):
        # Database errors can contain data. Never include exception text.
        result["failed_check"] = check
        result["timed_out"] = time.monotonic() >= deadline
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("database", type=Path, help="Isolated snapshot copy; never the live database")
    parser.add_argument("--max-seconds", type=int, default=600)
    args = parser.parse_args()
    if not 1 <= args.max_seconds <= 3600:
        parser.error("--max-seconds must be between 1 and 3600")
    result = audit(args.database, args.max_seconds,
                   emit=lambda value: print(json.dumps(value), flush=True))
    print(json.dumps(result), flush=True)
    return 0 if result["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
