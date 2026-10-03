package sqlevent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michmich112/congee/internal/storage/sqlitewriter"
	"github.com/rs/zerolog"
	"github.com/uptrace/bun"
)

type cancelBackfillHook struct {
	cancel      context.CancelFunc
	queryPrefix string
}

func (*cancelBackfillHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *cancelBackfillHook) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if h.cancel != nil && event.Err == nil && strings.HasPrefix(event.Query, h.queryPrefix) {
		h.cancel()
		h.cancel = nil
	}
}

func TestV1BackfillCancellationAndPartialUpgradeCanRetry(t *testing.T) {
	if !sqlitewriter.HasLibsqlDriver() {
		t.Skip("libsql driver requires CGO")
	}
	for _, testCase := range []struct {
		partial     bool
		queryPrefix string
	}{
		{false, "INSERT INTO event_fts(event_id, content) SELECT"},
		{true, "INSERT INTO event_fts(event_id, content) SELECT"},
		{false, "PRAGMA user_version = 2"},
		{true, "PRAGMA user_version = 2"},
	} {
		t.Run(fmt.Sprintf("partial=%v/cancel_after=%s", testCase.partial, testCase.queryPrefix), func(t *testing.T) {
			partial := testCase.partial
			ctx := context.Background()
			_, db, err := sqlitewriter.OpenLibsqlHandles(ctx, filepath.Join(t.TempDir(), "v1.db"), zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			for _, statement := range []string{
				`CREATE TABLE events (id TEXT PRIMARY KEY, pubkey TEXT NOT NULL, created_at INTEGER NOT NULL, kind INTEGER NOT NULL, content TEXT NOT NULL, sig TEXT NOT NULL, d_tag TEXT NOT NULL DEFAULT '')`,
				`CREATE TABLE event_tags (id INTEGER PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE, pos INTEGER NOT NULL, name TEXT NOT NULL, value TEXT NOT NULL, full_json TEXT NOT NULL)`,
				`CREATE TABLE audit_log (pubkey TEXT NOT NULL, created_at INTEGER NOT NULL)`,
				`INSERT INTO events VALUES ('one', 'publisher', 1, 1, 'firstkeyword', 'signature', ''), ('two', 'publisher', 2, 1, 'secondkeyword', 'signature', '')`,
				`PRAGMA user_version = 1`,
			} {
				if _, err := db.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if partial {
				for _, statement := range []string{
					`CREATE VIRTUAL TABLE event_fts USING fts5(event_id UNINDEXED, content, tokenize = 'porter unicode61')`,
					// Model an older binary interrupted after a partial or repeated backfill.
					`INSERT INTO event_fts(event_id, content) VALUES ('one', 'firstkeyword'), ('one', 'firstkeyword')`,
				} {
					if _, err := db.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			interrupted, cancel := context.WithCancel(ctx)
			defer cancel()
			db.AddQueryHook(&cancelBackfillHook{cancel: cancel, queryPrefix: testCase.queryPrefix})
			if err := migrateV1ToV2(interrupted, db, "turso", zerolog.Nop()); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected migration cancellation, got %v", err)
			}
			var version, ftsExists, mapExists int
			if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
				t.Fatalf("failed upgrade changed version: version=%d err=%v", version, err)
			}
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'event_fts'), EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'event_fts_rowids')`).Scan(&ftsExists, &mapExists); err != nil {
				t.Fatal(err)
			}
			if (ftsExists == 1) != partial || mapExists != 0 {
				t.Fatalf("failed upgrade leaked schema: fts=%d map=%d", ftsExists, mapExists)
			}
			if partial {
				var rows int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_fts`).Scan(&rows); err != nil || rows != 2 {
					t.Fatalf("rollback did not preserve previous index: rows=%d err=%v", rows, err)
				}
			}
			if err := RunMigrations(ctx, db, "turso", zerolog.Nop()); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
				t.Fatalf("retry version=%d err=%v", version, err)
			}
			var rows, mapped int
			if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM event_fts), (SELECT COUNT(*) FROM event_fts_rowids)`).Scan(&rows, &mapped); err != nil || rows != 2 || mapped != 2 {
				t.Fatalf("retry produced duplicate or unmapped FTS rows: rows=%d mapped=%d err=%v", rows, mapped, err)
			}
			for _, token := range []string{"firstkeyword", "secondkeyword"} {
				var found int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_fts WHERE event_fts MATCH ?`, token).Scan(&found); err != nil || found != 1 {
					t.Fatalf("retry search %s: count=%d err=%v", token, found, err)
				}
			}
			if err := RunMigrations(ctx, db, "turso", zerolog.Nop()); err != nil {
				t.Fatalf("reopen failed: %v", err)
			}
		})
	}
}
