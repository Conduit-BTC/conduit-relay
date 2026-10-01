import hashlib
from pathlib import Path
import runpy
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

audit = runpy.run_path(str(Path(__file__).with_name("congee-db-audit.py")))["audit"]


class DatabaseAuditTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "events.db"
        with sqlite3.connect(self.path) as db:
            db.executescript("""
                PRAGMA user_version=8;
                CREATE TABLE events(id TEXT PRIMARY KEY, content TEXT);
                CREATE VIRTUAL TABLE event_fts USING fts5(event_id UNINDEXED, content);
                CREATE TABLE event_fts_rowids(event_id TEXT PRIMARY KEY, fts_rowid INTEGER UNIQUE);
                INSERT INTO events VALUES('synthetic', 'synthetic search fixture');
                INSERT INTO event_fts VALUES('synthetic', 'synthetic search fixture');
                INSERT INTO event_fts_rowids VALUES('synthetic', 1);
            """)

    def test_valid_copy_is_unchanged(self):
        before = hashlib.sha256(self.path.read_bytes()).digest()
        self.assertEqual(audit(self.path)["status"], "passed")
        self.assertEqual(hashlib.sha256(self.path.read_bytes()).digest(), before)

    def test_equal_counts_do_not_hide_wrong_mapping(self):
        with sqlite3.connect(self.path) as db:
            db.execute("UPDATE event_fts SET event_id='wrong'")
        result = audit(self.path)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["checks"]["invalid_mappings"], 1)

    def test_missing_index_and_stale_content_are_detected(self):
        with sqlite3.connect(self.path) as db:
            db.execute("INSERT INTO events VALUES('missing', 'never print this content')")
            db.execute("UPDATE event_fts SET content='stale'")
        result = audit(self.path)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["checks"]["events_without_mapping"], 1)
        self.assertEqual(result["checks"]["content_mismatches"], 1)
        self.assertNotIn("never print", str(result))

    def test_orphan_search_row_is_detected(self):
        with sqlite3.connect(self.path) as db:
            db.execute("INSERT INTO event_fts VALUES('orphan', 'synthetic')")
        self.assertEqual(audit(self.path)["checks"]["fts_without_mapping"], 1)

    def test_empty_copy_has_zero_counts(self):
        with sqlite3.connect(self.path) as db:
            db.execute("DELETE FROM events")
            db.execute("DELETE FROM event_fts")
            db.execute("DELETE FROM event_fts_rowids")
        result = audit(self.path)
        self.assertEqual(result["status"], "passed")
        self.assertTrue(all(count == 0 for count in result["checks"].values()))

    def test_combined_passes_match_independent_coverage_queries(self):
        queries = {
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
                LEFT JOIN event_fts_rowids m ON m.fts_rowid=f.rowid WHERE m.event_id IS NULL""",
            "content_mismatches": """SELECT count(*) FROM event_fts_rowids m
                JOIN events e ON e.id=m.event_id JOIN event_fts f ON f.rowid=m.fts_rowid
                WHERE f.content IS NOT e.content""",
        }
        with sqlite3.connect(self.path) as db:
            db.executemany("INSERT INTO events VALUES(?, ?)",
                           [("missing", "synthetic"), ("missing-fts", None),
                            ("wrong-id", "synthetic"), ("null-content", None)])
            db.executemany("INSERT INTO event_fts(event_id, content) VALUES(?, ?)",
                           [("wrong", "synthetic"), ("orphan", "synthetic"),
                            ("null-content", "stale")])
            db.executemany("INSERT INTO event_fts_rowids VALUES(?, ?)",
                           [("missing-fts", 99), ("wrong-id", 2),
                            ("missing-event", 3), ("null-content", 4)])
            db.execute("UPDATE event_fts SET content='stale' WHERE rowid=1")
            expected = {name: db.execute(query).fetchone()[0] for name, query in queries.items()}
        result = audit(self.path)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["checks"], expected)

    def test_invalid_cache_budget_fails_closed(self):
        self.assertEqual(audit(self.path, cache_mib=0)["status"], "failed")

    def test_duplicate_mapping_cannot_inflate_totals_into_a_pass(self):
        with sqlite3.connect(self.path) as db:
            db.execute("DROP TABLE event_fts_rowids")
            db.execute("CREATE TABLE event_fts_rowids(event_id TEXT, fts_rowid INTEGER)")
            db.executemany("INSERT INTO event_fts_rowids VALUES('synthetic', 1)", [(), ()])
        result = audit(self.path)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["checks"]["events"], 1)
        self.assertEqual(result["checks"]["fts_rows"], 1)
        self.assertEqual(result["checks"]["mapped_rows"], 2)

    def test_missing_file_is_not_created(self):
        missing = self.path.with_name("missing.db")
        self.assertEqual(audit(missing)["status"], "failed")
        self.assertFalse(missing.exists())

    def test_wrong_version_stops_before_corpus_scan(self):
        with sqlite3.connect(self.path) as db:
            db.execute("PRAGMA user_version=7")
        result = audit(self.path)
        self.assertEqual(result["failed_check"], "schema_version")
        self.assertEqual(result["checks"], {})

    def test_deadline_interrupts_scan_and_fails_closed(self):
        with sqlite3.connect(self.path) as db:
            db.executemany("INSERT INTO events VALUES(?, 'synthetic')",
                           ((str(i),) for i in range(10000)))
        with patch("time.monotonic", side_effect=[0] + [2] * 100):
            result = audit(self.path, max_seconds=1)
        self.assertEqual(result["status"], "failed")
        self.assertTrue(result["timed_out"])

    def test_connection_closes_on_early_failure(self):
        connection = sqlite3.connect(self.path)
        connection.execute("PRAGMA user_version=7")
        connection.commit()
        with patch("sqlite3.connect", return_value=connection):
            self.assertEqual(audit(self.path)["failed_check"], "schema_version")
        with self.assertRaises(sqlite3.ProgrammingError):
            connection.execute("SELECT 1")


if __name__ == "__main__":
    unittest.main()
