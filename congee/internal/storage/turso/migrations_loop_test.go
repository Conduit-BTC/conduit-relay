package turso

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage/sqlitewriter"
	"github.com/rs/zerolog"
)

func execOnLibsqlFile(t *testing.T, ctx context.Context, path string, stmts []string) {
	t.Helper()
	sqldb, _, err := sqlitewriter.OpenLibsqlHandles(ctx, path, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqldb.Close() }()
	for _, q := range stmts {
		if err := sqlitewriter.ExecSQL(ctx, sqldb, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV7FTSRowidMapUpgradePreservesSearchAndFastDeletion(t *testing.T) {
	skipNoDriver(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v7.db")
	st, err := Open(ctx, path, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	pk := strings.Repeat("b", 64)
	old := &nostr.Event{ID: strings.Repeat("a", 64), PubKey: pk, CreatedAt: 1, Kind: 3, Content: "oldkeyword", Sig: strings.Repeat("c", 128)}
	if err := st.SaveEvent(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Model the on-disk v7 schema without changing or rebuilding its FTS rows.
	execOnLibsqlFile(t, ctx, path, []string{
		`DROP TRIGGER events_ai_fts`, `DROP TRIGGER events_au_fts`, `DROP TRIGGER events_ad_fts`,
		`DROP TABLE event_fts_rowids`,
		`CREATE TRIGGER events_ai_fts AFTER INSERT ON events BEGIN
			INSERT INTO event_fts(event_id, content) VALUES(new.id, new.content); END`,
		`CREATE TRIGGER events_ad_fts AFTER DELETE ON events BEGIN
			DELETE FROM event_fts WHERE event_id = old.id; END`,
		`PRAGMA user_version = 7`,
	})
	st, err = Open(ctx, path, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var mapped, actual int64
	if err := st.DB().QueryRowContext(ctx, `SELECT m.fts_rowid, f.rowid FROM event_fts_rowids m
		JOIN event_fts f ON f.rowid = m.fts_rowid WHERE m.event_id = ?`, old.ID).Scan(&mapped, &actual); err != nil || mapped != actual {
		t.Fatalf("v7 FTS row lost mapping: mapped=%d actual=%d err=%v", mapped, actual, err)
	}
	newer := &nostr.Event{ID: strings.Repeat("d", 64), PubKey: pk, CreatedAt: 2, Kind: 3, Content: "newkeyword", Sig: strings.Repeat("c", 128)}
	if err := st.SaveEvent(ctx, newer); err != nil {
		t.Fatal(err)
	}
	var oldRows, newRows int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_fts WHERE event_id = ?`, old.ID).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_fts WHERE event_id = ?`, newer.ID).Scan(&newRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 || newRows != 1 {
		t.Fatalf("FTS replacement rows old=%d new=%d", oldRows, newRows)
	}
	oldSearch, err := st.SearchEvents(ctx, "oldkeyword", nostr.Filter{Kinds: []int{3}})
	if err != nil || len(oldSearch) != 0 {
		t.Fatalf("old revision remains searchable: count=%d err=%v", len(oldSearch), err)
	}
	newSearch, err := st.SearchEvents(ctx, "newkeyword", nostr.Filter{Kinds: []int{3}})
	if err != nil || len(newSearch) != 1 || newSearch[0].ID != newer.ID {
		t.Fatalf("new revision missing from search: count=%d err=%v", len(newSearch), err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT m.fts_rowid, f.rowid FROM event_fts_rowids m
		JOIN event_fts f ON f.rowid = m.fts_rowid WHERE m.event_id = ?`, newer.ID).Scan(&mapped, &actual); err != nil || mapped != actual {
		t.Fatalf("new FTS row lacks mapping: mapped=%d actual=%d err=%v", mapped, actual, err)
	}
	if err := st.DeleteEvent(ctx, newer.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM event_fts`).Scan(&newRows); err != nil || newRows != 0 {
		t.Fatalf("FTS deletion failed: rows=%d err=%v", newRows, err)
	}
}

func TestCurrentV8FTSLayoutReopens(t *testing.T) {
	skipNoDriver(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "compatible.db")
	st, err := Open(ctx, path, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Extra valid indexes do not change the supported layout.
	execOnLibsqlFile(t, ctx, path, []string{`CREATE UNIQUE INDEX extra_fts_rowid_unique ON event_fts_rowids(fts_rowid)`})
	st, err = Open(ctx, path, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
}

func TestIncompatibleV8FTSLayoutFailsClosed(t *testing.T) {
	skipNoDriver(t)
	cases := []struct {
		name string
		ddl  []string
	}{
		{"missing map", []string{`DROP TABLE event_fts_rowids`}},
		{"wrong map columns", []string{`DROP TABLE event_fts_rowids`, `CREATE TABLE event_fts_rowids(event_id TEXT NOT NULL PRIMARY KEY, other INTEGER NOT NULL UNIQUE)`}},
		{"wrong map type", []string{`DROP TABLE event_fts_rowids`, `CREATE TABLE event_fts_rowids(event_id TEXT NOT NULL PRIMARY KEY, fts_rowid TEXT NOT NULL UNIQUE)`}},
		{"nullable map", []string{`DROP TABLE event_fts_rowids`, `CREATE TABLE event_fts_rowids(event_id TEXT PRIMARY KEY, fts_rowid INTEGER UNIQUE)`}},
		{"missing event uniqueness", []string{`DROP TABLE event_fts_rowids`, `CREATE TABLE event_fts_rowids(event_id TEXT NOT NULL, fts_rowid INTEGER NOT NULL UNIQUE)`}},
		{"missing rowid uniqueness", []string{`DROP TABLE event_fts_rowids`, `CREATE TABLE event_fts_rowids(event_id TEXT NOT NULL PRIMARY KEY, fts_rowid INTEGER NOT NULL)`}},
		{"composite rowid uniqueness", []string{`DROP TABLE event_fts_rowids`, `CREATE TABLE event_fts_rowids(event_id TEXT NOT NULL PRIMARY KEY, fts_rowid INTEGER NOT NULL, UNIQUE(event_id, fts_rowid))`}},
		{"partial rowid uniqueness", []string{`DROP TABLE event_fts_rowids`, `CREATE TABLE event_fts_rowids(event_id TEXT NOT NULL PRIMARY KEY, fts_rowid INTEGER NOT NULL)`, `CREATE UNIQUE INDEX partial_map ON event_fts_rowids(fts_rowid) WHERE fts_rowid > 0`}},
		{"missing insert trigger", []string{`DROP TRIGGER events_ai_fts`}},
		{"missing update trigger", []string{`DROP TRIGGER events_au_fts`}},
		{"missing delete trigger", []string{`DROP TRIGGER events_ad_fts`}},
		{"insert without rowid mapping", []string{`DROP TRIGGER events_ai_fts`, `CREATE TRIGGER events_ai_fts AFTER INSERT ON events BEGIN
			INSERT INTO event_fts(event_id, content) VALUES(new.id, new.content); END`}},
		{"slow update trigger", []string{`DROP TRIGGER events_au_fts`, `CREATE TRIGGER events_au_fts AFTER UPDATE ON events BEGIN
			DELETE FROM event_fts WHERE event_id = old.id;
			INSERT INTO event_fts(event_id, content) VALUES(new.id, new.content); END`}},
		{"slow delete trigger", []string{`DROP TRIGGER events_ad_fts`, `CREATE TRIGGER events_ad_fts AFTER DELETE ON events BEGIN
			DELETE FROM event_fts WHERE event_id = old.id; /* synthetic-private-schema */ END`}},
		{"ordinary FTS table", []string{`DROP TABLE event_fts`, `CREATE TABLE event_fts(event_id TEXT, content TEXT)`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "incompatible.db")
			st, err := Open(ctx, path, nil, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			ev := &nostr.Event{ID: strings.Repeat("a", 64), PubKey: strings.Repeat("b", 64), CreatedAt: 1, Kind: 1, Content: "synthetic", Sig: strings.Repeat("c", 128)}
			if err := st.SaveEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			execOnLibsqlFile(t, ctx, path, tc.ddl)
			st, err = Open(ctx, path, nil, zerolog.Nop())
			if err == nil {
				_ = st.Close()
				t.Fatal("incompatible schema 8 opened successfully")
			}
			if !strings.Contains(err.Error(), "schema 8") || strings.Contains(err.Error(), "synthetic-private-schema") {
				t.Fatalf("unexpected compatibility error: %v", err)
			}
			// A failed open must preserve the database for a separate rehearsal.
			sqldb, _, err := sqlitewriter.OpenLibsqlHandles(ctx, path, zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer sqldb.Close()
			var version, events int
			if err := sqldb.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if version != 8 || events != 1 {
				t.Fatalf("failed open modified data: version=%d events=%d", version, events)
			}
		})
	}
}

// TestRunMigrationsLoopsV6ToV7 builds a current file, re-adds ws_connection_sessions with user_version 6, and checks Open drops meta tables.
func TestRunMigrationsLoopsV6ToV7(t *testing.T) {
	skipNoDriver(t)
	ctx := context.Background()
	log := zerolog.Nop()
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.db")

	s, err := Open(ctx, path, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	execOnLibsqlFile(t, ctx, path, []string{
		`CREATE TABLE IF NOT EXISTS ws_connection_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conn_id TEXT NOT NULL,
			peer_ip TEXT NOT NULL,
			remote_addr TEXT NOT NULL,
			started_unix INTEGER NOT NULL,
			ended_unix INTEGER NOT NULL,
			total_req INTEGER NOT NULL DEFAULT 0,
			total_client_event INTEGER NOT NULL DEFAULT 0,
			series_json TEXT NOT NULL DEFAULT '[]',
			subs_json TEXT NOT NULL DEFAULT '[]'
		)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at INTEGER NOT NULL,
			action TEXT NOT NULL,
			detail TEXT,
			pubkey TEXT NOT NULL DEFAULT ''
		)`,
		`PRAGMA user_version = 6`,
	})

	s2, err := Open(ctx, path, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	db := s2.DB()
	var uv int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatal(err)
	}
	if uv != CurrentSchemaVersion() {
		t.Fatalf("user_version: got %d want %d", uv, CurrentSchemaVersion())
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ws_connection_sessions'`,
	).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ws_connection_sessions should be dropped: err=%v n=%d", err, n)
	}
}

// TestRunMigrationsLoopsFakeV5ToV7 keeps a current events schema but sets user_version to 5 with legacy meta tables present.
func TestRunMigrationsLoopsFakeV5ToV7(t *testing.T) {
	skipNoDriver(t)
	ctx := context.Background()
	log := zerolog.Nop()
	dir := t.TempDir()
	path := filepath.Join(dir, "multistep.db")

	s, err := Open(ctx, path, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	execOnLibsqlFile(t, ctx, path, []string{
		`CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at INTEGER NOT NULL,
			action TEXT NOT NULL,
			detail TEXT,
			pubkey TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS config_changelog (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at INTEGER NOT NULL,
			summary TEXT NOT NULL,
			json_diff TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS relay_metric_buckets (
			bucket_start_unix INTEGER NOT NULL PRIMARY KEY,
			events_stored INTEGER NOT NULL DEFAULT 0,
			events_rejected INTEGER NOT NULL DEFAULT 0,
			req_count INTEGER NOT NULL DEFAULT 0,
			close_count INTEGER NOT NULL DEFAULT 0,
			query_ms_sum INTEGER NOT NULL DEFAULT 0,
			query_ms_count INTEGER NOT NULL DEFAULT 0,
			subscriptions_open INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS ws_connection_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conn_id TEXT NOT NULL,
			peer_ip TEXT NOT NULL,
			remote_addr TEXT NOT NULL,
			started_unix INTEGER NOT NULL,
			ended_unix INTEGER NOT NULL,
			total_req INTEGER NOT NULL DEFAULT 0,
			total_client_event INTEGER NOT NULL DEFAULT 0,
			series_json TEXT NOT NULL DEFAULT '[]',
			subs_json TEXT NOT NULL DEFAULT '[]'
		)`,
		`PRAGMA user_version = 5`,
	})

	s2, err := Open(ctx, path, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	var uv int
	if err := s2.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatal(err)
	}
	if uv != CurrentSchemaVersion() {
		t.Fatalf("user_version after multi-step migrate: got %d want %d", uv, CurrentSchemaVersion())
	}
	var metaTables int
	if err := s2.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('audit_log','config_changelog','relay_metric_buckets','ws_connection_sessions')`,
	).Scan(&metaTables); err != nil || metaTables != 0 {
		t.Fatalf("meta tables should be dropped: err=%v n=%d", err, metaTables)
	}
}

// TestPreflightMigrationTargetCurrent checks preflight on a fresh Open database.
func TestPreflightMigrationTargetCurrent(t *testing.T) {
	skipNoDriver(t)
	ctx := context.Background()
	log := zerolog.Nop()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf.db")

	s, err := Open(ctx, path, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	out := PreflightMigrationTarget(ctx, path, log)
	if out.Status != "current" {
		t.Fatalf("status: got %q detail=%q", out.Status, out.Detail)
	}
	if out.ExpectedVersion != CurrentSchemaVersion() {
		t.Fatalf("expected_version: got %d", out.ExpectedVersion)
	}
	if out.ReportedVersion == nil || *out.ReportedVersion != CurrentSchemaVersion() {
		t.Fatalf("reported_version: %+v", out.ReportedVersion)
	}
}

func TestPreflightMigrationTargetEmptyPath(t *testing.T) {
	ctx := context.Background()
	out := PreflightMigrationTarget(ctx, "", zerolog.Nop())
	if out.Status != "unreadable" {
		t.Fatalf("got %q", out.Status)
	}
}
