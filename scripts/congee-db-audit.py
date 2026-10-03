#!/usr/bin/env python3
"""Audit an isolated SQLite event database copy; emit counts, never event data."""

import argparse
from contextlib import closing
import json
from pathlib import Path
import sqlite3
import time


CHECKS = ("events", "fts_rows", "mapped_rows", "events_without_mapping",
          "mappings_without_event", "invalid_mappings", "fts_without_mapping",
          "content_mismatches")

# Check each direction once. Count base rows independently so missing schema
# constraints cannot inflate all three totals into a false pass.
PASSES = (
    (("events", "events_without_mapping"), """SELECT (SELECT count(*) FROM events),
        coalesce(sum(m.event_id IS NULL), 0) FROM events e
        LEFT JOIN event_fts_rowids m ON m.event_id=e.id"""),
    (("mapped_rows", "mappings_without_event", "invalid_mappings", "content_mismatches"),
     """SELECT (SELECT count(*) FROM event_fts_rowids), coalesce(sum(e.id IS NULL), 0),
        coalesce(sum(f.rowid IS NULL OR f.event_id IS NOT m.event_id), 0),
        coalesce(sum(e.id IS NOT NULL AND f.rowid IS NOT NULL AND f.content IS NOT e.content), 0)
        FROM event_fts_rowids m LEFT JOIN events e ON e.id=m.event_id
        LEFT JOIN event_fts f ON f.rowid=m.fts_rowid"""),
    (("fts_rows", "fts_without_mapping"), """SELECT (SELECT count(*) FROM event_fts),
        coalesce(sum(m.event_id IS NULL), 0) FROM event_fts f
        LEFT JOIN event_fts_rowids m ON m.fts_rowid=f.rowid"""),
)


def audit(path, max_seconds=600, emit=lambda value: None, cache_mib=64):
    deadline = time.monotonic() + max_seconds
    result = {"status": "failed", "checks": {}}
    check = "open"
    try:
        # mode=ro forbids creating a missing database or changing stored rows.
        # Do not use immutable=1: a restored snapshot can contain a WAL.
        with closing(sqlite3.connect(Path(path).resolve().as_uri() + "?mode=ro", uri=True,
                                    timeout=min(max_seconds, 5))) as db:
            db.execute("PRAGMA query_only=ON")
            if not 1 <= cache_mib <= 1024:
                raise ValueError()
            db.execute(f"PRAGMA cache_size=-{cache_mib * 1024}")
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
            for names, query in PASSES:
                check = names[0]
                values = db.execute(query).fetchone()
                for name, count in zip(names, values):
                    result["checks"][name] = count
                    emit({name: count})
            db.rollback()
        counts = result["checks"]
        if (counts["events"] == counts["fts_rows"] == counts["mapped_rows"] and
                all(counts[name] == 0 for name in CHECKS[3:])):
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
    parser.add_argument("--cache-mib", type=int, default=64)
    args = parser.parse_args()
    if not 1 <= args.max_seconds <= 14400:
        parser.error("--max-seconds must be between 1 and 14400")
    if not 1 <= args.cache_mib <= 1024:
        parser.error("--cache-mib must be between 1 and 1024")
    result = audit(args.database, args.max_seconds,
                   emit=lambda value: print(json.dumps(value), flush=True), cache_mib=args.cache_mib)
    print(json.dumps(result), flush=True)
    return 0 if result["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
