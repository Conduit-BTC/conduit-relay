package sqlevent

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"github.com/uptrace/bun"
)

const schemaVersion = 8

// CurrentSchemaVersion is the PRAGMA user_version / app-expected value for this binary.
func CurrentSchemaVersion() int { return schemaVersion }

// RunMigrations applies schema DDL until PRAGMA user_version reaches CurrentSchemaVersion.
func RunMigrations(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	return runMigrations(ctx, db, engine, log)
}

func runMigrations(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	for {
		var version int
		row := db.QueryRowContext(ctx, "PRAGMA user_version")
		if err := row.Scan(&version); err != nil {
			return fmt.Errorf("%s: read user_version: %w", engine, err)
		}
		log.Debug().Int("user_version", version).Msg("schema: read user_version")
		if version > schemaVersion {
			return fmt.Errorf("%s: unsupported schema version %d (need <= %d)", engine, version, schemaVersion)
		}
		if version == schemaVersion {
			log.Debug().Msg("schema: already at current version")
			return validateV8FTSLayout(ctx, db, engine)
		}
		if version == 0 {
			log.Debug().Msg("schema: user_version 0; applying fresh schema")
			if err := migrateFresh(ctx, db, engine, log); err != nil {
				return err
			}
			log.Debug().Msg("schema: fresh schema applied")
			return validateV8FTSLayout(ctx, db, engine)
		}
		switch version {
		case 1:
			log.Debug().Msg("schema: migrating v1 to v2")
			if err := migrateV1ToV2(ctx, db, engine, log); err != nil {
				return err
			}
		case 2:
			log.Debug().Msg("schema: migrating v2 to v3")
			if err := migrateV2ToV3(ctx, db, engine, log); err != nil {
				return err
			}
		case 3:
			log.Debug().Msg("schema: migrating v3 to v4")
			if err := migrateV3ToV4(ctx, db, engine, log); err != nil {
				return err
			}
		case 4:
			log.Debug().Msg("schema: migrating v4 to v5")
			if err := migrateV4ToV5(ctx, db, engine, log); err != nil {
				return err
			}
		case 5:
			log.Debug().Msg("schema: migrating v5 to v6")
			if err := migrateV5ToV6(ctx, db, engine, log); err != nil {
				return err
			}
		case 6:
			log.Debug().Msg("schema: migrating v6 to v7")
			if err := migrateV6ToV7(ctx, db, engine, log); err != nil {
				return err
			}
		case 7:
			log.Debug().Msg("schema: migrating v7 to v8")
			if err := migrateV7ToV8(ctx, db, engine, log); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported schema version %d", engine, version)
		}
	}
}

func migrateFresh(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS events (
			id TEXT NOT NULL PRIMARY KEY,
			pubkey TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			kind INTEGER NOT NULL,
			content TEXT NOT NULL,
			sig TEXT NOT NULL,
			d_tag TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_pubkey_kind ON events (pubkey, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_events_pubkey_kind_dtag ON events (pubkey, kind, d_tag)`,
		`CREATE INDEX IF NOT EXISTS idx_events_created_at ON events (created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS event_tags (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
			pos INTEGER NOT NULL,
			name TEXT NOT NULL,
			value TEXT NOT NULL DEFAULT '',
			full_json TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_event_tags_event_id ON event_tags (event_id)`,
		`CREATE INDEX IF NOT EXISTS idx_event_tags_name_value ON event_tags (name, value)`,
	}
	for i := range stmts {
		log.Debug().Int("ddl_step", i).Msg("schema: exec ddl statement")
		if _, err := db.ExecContext(ctx, stmts[i]); err != nil {
			return fmt.Errorf("%s: migrate: %w", engine, err)
		}
	}
	log.Debug().Msg("schema: creating fts5 and triggers")
	if err := createFTS5AndTriggers(ctx, db, engine, log); err != nil {
		return err
	}
	log.Debug().Int("schema_version", schemaVersion).Msg("schema: set user_version")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("%s: set user_version: %w", engine, err)
	}
	return nil
}

func migrateV1ToV2(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Own rollback synchronously. A canceled transaction context can otherwise
	// let database/sql return ErrTxDone while its background rollback still
	// holds libSQL's schema locks. Statements retain the caller's context.
	return conn.RunInTx(context.WithoutCancel(ctx), nil, func(_ context.Context, tx bun.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		log.Debug().Msg("schema v1->v2: fts5 and triggers")
		if err := createFTS5AndTriggers(ctx, tx, engine, log); err != nil {
			return err
		}
		// Replace a backfill left by an interrupted older binary. The rebuild
		// and version transition share a transaction, so retries cannot add
		// duplicate FTS rows or discard the previous index on failure.
		log.Debug().Msg("schema v1->v2: backfill event_fts")
		for _, statement := range []string{
			`DELETE FROM event_fts`,
			`DELETE FROM event_fts_rowids`,
			`INSERT INTO event_fts(event_id, content) SELECT id, content FROM events`,
			`PRAGMA user_version = 2`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("%s: migrate v1->v2: %w", engine, err)
			}
		}
		return ctx.Err()
	})
}

func migrateV2ToV3(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS relay_metric_buckets (
			bucket_start_unix INTEGER NOT NULL PRIMARY KEY,
			events_stored INTEGER NOT NULL DEFAULT 0,
			events_rejected INTEGER NOT NULL DEFAULT 0,
			req_count INTEGER NOT NULL DEFAULT 0,
			close_count INTEGER NOT NULL DEFAULT 0,
			query_ms_sum INTEGER NOT NULL DEFAULT 0,
			query_ms_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_relay_metric_buckets_start ON relay_metric_buckets (bucket_start_unix)`,
	}
	for i := range stmts {
		log.Debug().Int("ddl_step", i).Msg("schema v2->v3: relay_metric_buckets")
		if _, err := db.ExecContext(ctx, stmts[i]); err != nil {
			return fmt.Errorf("%s: migrate v2->v3: %w", engine, err)
		}
	}
	log.Debug().Int("schema_version", schemaVersion).Msg("schema v2->v3: set user_version 3 (chain v3->v4)")
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 3`); err != nil {
		return fmt.Errorf("%s: set user_version: %w", engine, err)
	}
	log.Debug().Msg("schema v2->v3: chain v3->v4")
	return migrateV3ToV4(ctx, db, engine, log)
}

func migrateV3ToV4(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	var colCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('relay_metric_buckets') WHERE name = 'subscriptions_open'`,
	).Scan(&colCount); err != nil {
		return fmt.Errorf("%s: migrate v3->v4: %w", engine, err)
	}
	if colCount == 0 {
		log.Debug().Msg("schema v3->v4: subscriptions_open on relay_metric_buckets")
		if _, err := db.ExecContext(ctx, `ALTER TABLE relay_metric_buckets ADD COLUMN subscriptions_open INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("%s: migrate v3->v4: %w", engine, err)
		}
	} else {
		log.Debug().Msg("schema v3->v4: subscriptions_open already present; skipping alter")
	}
	log.Debug().Msg("schema v3->v4: set user_version 4")
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 4`); err != nil {
		return fmt.Errorf("%s: set user_version: %w", engine, err)
	}
	return nil
}

func migrateV4ToV5(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_event_tags_name_value_event_id ON event_tags (name, value, event_id)`,
		`CREATE INDEX IF NOT EXISTS idx_event_tags_event_id_pos ON event_tags (event_id, pos)`,
		`DROP INDEX IF EXISTS idx_event_tags_event_id`,
		`CREATE INDEX IF NOT EXISTS idx_audit_pubkey_created_at ON audit_log (pubkey, created_at DESC)`,
	}
	for i := range stmts {
		log.Debug().Int("ddl_step", i).Msg("schema v4->v5: exec ddl statement")
		if _, err := db.ExecContext(ctx, stmts[i]); err != nil {
			return fmt.Errorf("%s: migrate v4->v5: %w", engine, err)
		}
	}
	log.Debug().Msg("schema v4->v5: set user_version 5")
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
		return fmt.Errorf("%s: set user_version: %w", engine, err)
	}
	return nil
}

func migrateV5ToV6(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	stmts := []string{
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
		`CREATE INDEX IF NOT EXISTS idx_ws_sessions_ended ON ws_connection_sessions (ended_unix DESC)`,
	}
	for i := range stmts {
		log.Debug().Int("ddl_step", i).Msg("schema v5->v6: ws_connection_sessions")
		if _, err := db.ExecContext(ctx, stmts[i]); err != nil {
			return fmt.Errorf("%s: migrate v5->v6: %w", engine, err)
		}
	}
	log.Debug().Msg("schema v5->v6: set user_version 6")
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 6`); err != nil {
		return fmt.Errorf("%s: set user_version: %w", engine, err)
	}
	return nil
}

func migrateV6ToV7(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	stmts := []string{
		`DROP INDEX IF EXISTS idx_ws_sessions_ended`,
		`DROP TABLE IF EXISTS ws_connection_sessions`,
		`DROP INDEX IF EXISTS idx_relay_metric_buckets_start`,
		`DROP TABLE IF EXISTS relay_metric_buckets`,
		`DROP INDEX IF EXISTS idx_config_changelog_created_at`,
		`DROP TABLE IF EXISTS config_changelog`,
		`DROP INDEX IF EXISTS idx_audit_pubkey_created_at`,
		`DROP INDEX IF EXISTS idx_audit_created_at`,
		`DROP TABLE IF EXISTS audit_log`,
	}
	for i := range stmts {
		log.Debug().Int("ddl_step", i).Msg("schema v6->v7: drop meta tables")
		if _, err := db.ExecContext(ctx, stmts[i]); err != nil {
			return fmt.Errorf("%s: migrate v6->v7: %w", engine, err)
		}
	}
	log.Debug().Msg("schema v6->v7: set user_version 7")
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 7`); err != nil {
		return fmt.Errorf("%s: set user_version: %w", engine, err)
	}
	return nil
}

// FTS5 cannot index its UNINDEXED event_id column. Keeping the existing FTS
// index and mapping each signed event ID to its FTS rowid makes replacement and
// deletion point lookups rather than scans of the full search corpus.
func migrateV7ToV8(ctx context.Context, db *bun.DB, engine string, log zerolog.Logger) error {
	var eventsExists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'events'`).Scan(&eventsExists); err != nil {
		return fmt.Errorf("%s: inspect events table: %w", engine, err)
	}
	if eventsExists == 0 {
		// Legacy metadata-only files can carry a v7 marker without an event
		// schema. Establish the fresh v8 event tables in that case.
		return migrateFresh(ctx, db, engine, log)
	}
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var ftsExists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'event_fts'`).Scan(&ftsExists); err != nil {
			return fmt.Errorf("%s: inspect FTS table: %w", engine, err)
		}
		if ftsExists == 0 {
			if _, err := tx.ExecContext(ctx, `CREATE VIRTUAL TABLE event_fts USING fts5(
				event_id UNINDEXED, content, tokenize = 'porter unicode61')`); err != nil {
				return fmt.Errorf("%s: create missing FTS table: %w", engine, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO event_fts(event_id, content) SELECT id, content FROM events`); err != nil {
				return fmt.Errorf("%s: backfill missing FTS table: %w", engine, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS event_fts_rowids (
			event_id TEXT NOT NULL PRIMARY KEY,
			fts_rowid INTEGER NOT NULL UNIQUE
		)`); err != nil {
			return fmt.Errorf("%s: create FTS rowid map: %w", engine, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM event_fts_rowids`); err != nil {
			return fmt.Errorf("%s: reset FTS rowid map: %w", engine, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_fts_rowids(event_id, fts_rowid)
			SELECT event_id, rowid FROM event_fts`); err != nil {
			return fmt.Errorf("%s: backfill FTS rowid map: %w", engine, err)
		}
		if err := createFTS5Triggers(ctx, tx, engine, log); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 8`); err != nil {
			return fmt.Errorf("%s: set user_version: %w", engine, err)
		}
		return nil
	})
}

func createFTS5AndTriggers(ctx context.Context, db bun.IDB, engine string, log zerolog.Logger) error {
	fts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS event_fts USING fts5(
			event_id UNINDEXED,
			content,
			tokenize = 'porter unicode61'
		)`,
		`CREATE TABLE IF NOT EXISTS event_fts_rowids (
			event_id TEXT NOT NULL PRIMARY KEY,
			fts_rowid INTEGER NOT NULL UNIQUE
		)`,
	}
	for i := range fts {
		log.Debug().Int("fts_step", i).Msg("schema: fts5 ddl")
		if _, err := db.ExecContext(ctx, fts[i]); err != nil {
			return fmt.Errorf("%s: fts5: %w", engine, err)
		}
	}
	return createFTS5Triggers(ctx, db, engine, log)
}

// Keep validation and creation on the same definitions. Schema version 8 has
// existed with different FTS layouts, so its version marker alone is insufficient.
var fts5Triggers = []struct{ name, ddl string }{
	{"events_ai_fts", `CREATE TRIGGER events_ai_fts AFTER INSERT ON events BEGIN
			INSERT INTO event_fts(event_id, content) VALUES (new.id, new.content);
			INSERT INTO event_fts_rowids(event_id, fts_rowid) VALUES (new.id, last_insert_rowid());
		END`},
	{"events_au_fts", `CREATE TRIGGER events_au_fts AFTER UPDATE ON events BEGIN
			DELETE FROM event_fts WHERE rowid = (SELECT fts_rowid FROM event_fts_rowids WHERE event_id = old.id);
			DELETE FROM event_fts_rowids WHERE event_id = old.id;
			INSERT INTO event_fts(event_id, content) VALUES (new.id, new.content);
			INSERT INTO event_fts_rowids(event_id, fts_rowid) VALUES (new.id, last_insert_rowid());
		END`},
	{"events_ad_fts", `CREATE TRIGGER events_ad_fts AFTER DELETE ON events BEGIN
			DELETE FROM event_fts WHERE rowid = (SELECT fts_rowid FROM event_fts_rowids WHERE event_id = old.id);
			DELETE FROM event_fts_rowids WHERE event_id = old.id;
		END`},
}

func createFTS5Triggers(ctx context.Context, db bun.IDB, engine string, log zerolog.Logger) error {
	for i, trigger := range fts5Triggers {
		log.Debug().Int("fts_step", i).Msg("schema: fts5 trigger ddl")
		if _, err := db.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger.name); err != nil {
			return fmt.Errorf("%s: fts5 trigger: %w", engine, err)
		}
		if _, err := db.ExecContext(ctx, trigger.ddl); err != nil {
			return fmt.Errorf("%s: fts5 trigger: %w", engine, err)
		}
	}
	return nil
}

func validateV8FTSLayout(ctx context.Context, db *bun.DB, engine string) error {
	// Inspect structure only. Do not scan the search corpus or expose schema SQL
	// in errors; operators must rehearse an incompatible database separately.
	checks := []struct {
		name, query string
		want        int
	}{
		{"FTS columns", `SELECT COUNT(*) FROM pragma_table_info('event_fts')`, 2},
		{"FTS column names", `SELECT COUNT(*) FROM pragma_table_info('event_fts') WHERE name IN ('event_id', 'content')`, 2},
		{"rowid map columns", `SELECT COUNT(*) FROM pragma_table_info('event_fts_rowids')`, 2},
		{"rowid map column types", `SELECT COUNT(*) FROM pragma_table_info('event_fts_rowids')
			WHERE (name = 'event_id' AND upper(type) = 'TEXT' AND "notnull" = 1 AND pk = 1)
				OR (name = 'fts_rowid' AND upper(type) = 'INTEGER' AND "notnull" = 1 AND pk = 0)`, 2},
		{"rowid map uniqueness", `SELECT EXISTS(SELECT 1 FROM pragma_index_list('event_fts_rowids') AS i
			WHERE i."unique" = 1 AND i.partial = 0
				AND (SELECT COUNT(*) FROM pragma_index_info(i.name)) = 1
				AND EXISTS (SELECT 1 FROM pragma_index_info(i.name) WHERE name = 'fts_rowid'))`, 1},
	}
	for _, check := range checks {
		var got int
		if err := db.QueryRowContext(ctx, check.query).Scan(&got); err != nil {
			return fmt.Errorf("%s: cannot inspect schema 8 %s", engine, check.name)
		}
		if got != check.want {
			return fmt.Errorf("%s: incompatible schema 8 FTS layout (%s); stop and rehearse database compatibility", engine, check.name)
		}
	}
	var ftsDDL string
	if err := db.QueryRowContext(ctx, `SELECT coalesce(sql, '') FROM sqlite_master WHERE type = 'table' AND name = 'event_fts'`).Scan(&ftsDDL); err != nil {
		return fmt.Errorf("%s: cannot inspect schema 8 FTS table", engine)
	}
	normalizeDDL := func(ddl string) string { return strings.ToLower(strings.Join(strings.Fields(ddl), "")) }
	if !strings.Contains(normalizeDDL(ftsDDL), "usingfts5(event_idunindexed,content,") {
		return fmt.Errorf("%s: incompatible schema 8 FTS table; stop and rehearse database compatibility", engine)
	}
	for _, trigger := range fts5Triggers {
		var ddl string
		if err := db.QueryRowContext(ctx, `SELECT coalesce(sql, '') FROM sqlite_master
			WHERE type = 'trigger' AND name = ? AND tbl_name = 'events'`, trigger.name).Scan(&ddl); err != nil {
			return fmt.Errorf("%s: missing schema 8 FTS trigger %s", engine, trigger.name)
		}
		if normalizeDDL(ddl) != normalizeDDL(trigger.ddl) {
			return fmt.Errorf("%s: incompatible schema 8 FTS trigger %s; stop and rehearse database compatibility", engine, trigger.name)
		}
	}
	return nil
}
