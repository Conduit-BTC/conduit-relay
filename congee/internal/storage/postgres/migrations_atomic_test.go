package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// This driver exercises Bun's actual transaction path without a PostgreSQL
// service. It rejects writes outside a transaction and can fail specific SQL.
type migrationConnector struct{ conn *migrationConn }

func (c migrationConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c migrationConnector) Driver() driver.Driver                        { return migrationDriver{} }

type migrationDriver struct{}

func (migrationDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use migrationConnector")
}

type migrationConn struct {
	failPrefix string
	failErr    error
	pending    []string
	committed  []string
	history    []string
	inTx       bool
	commits    int
	rollbacks  int
}

func (*migrationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported")
}
func (*migrationConn) Close() error { return nil }
func (c *migrationConn) Begin() (driver.Tx, error) {
	if c.inTx {
		return nil, errors.New("transaction already open")
	}
	c.inTx = true
	c.pending = nil
	return c, nil
}
func (c *migrationConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.inTx {
		return nil, errors.New("migration write outside transaction")
	}
	c.history = append(c.history, query)
	if c.failPrefix != "" && strings.HasPrefix(query, c.failPrefix) {
		return nil, c.failErr
	}
	c.pending = append(c.pending, query)
	return driver.RowsAffected(1), nil
}
func (c *migrationConn) Commit() error {
	c.commits++
	c.committed = append(c.committed, c.pending...)
	c.pending = nil
	c.inTx = false
	return nil
}
func (c *migrationConn) Rollback() error {
	c.rollbacks++
	c.pending = nil
	c.inTx = false
	return nil
}

func migrationTestDB(t *testing.T, conn *migrationConn) *bun.DB {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(migrationConnector{conn}), pgdialect.New())
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertFreshMigrationCommitted(t *testing.T, conn *migrationConn) {
	t.Helper()
	if conn.commits != 1 || conn.inTx || len(conn.pending) != 0 {
		t.Fatalf("migration did not commit once: commits=%d inTx=%v pending=%d", conn.commits, conn.inTx, len(conn.pending))
	}
	if len(conn.committed) < 2 {
		t.Fatal("missing committed schema statements")
	}
	last := conn.committed[len(conn.committed)-1]
	if !strings.HasPrefix(last, "INSERT INTO congee_schema_version") || !strings.Contains(last, fmt.Sprintf("VALUES (1, %d)", schemaVersion)) {
		t.Fatalf("schema version must be the final statement, got %q", last)
	}
	for _, query := range conn.committed[:len(conn.committed)-1] {
		if !strings.HasPrefix(query, "CREATE ") {
			t.Fatalf("version recorded before DDL completed: %q", query)
		}
	}
}

// Run against the existing opt-in test database to verify PostgreSQL DDL
// rollback, including a target left partly initialized by an older binary.
func TestPostgresMigrateFreshFailurePreservesSchemaAndVersion(t *testing.T) {
	dsn := testPostgresDSN(t)
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	schema := bun.Ident(fmt.Sprintf("congee_migration_test_%d", time.Now().UnixNano()))
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA ?", schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, "DROP SCHEMA ? CASCADE", schema); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	})
	if _, err := db.ExecContext(ctx, "SET search_path TO ?", schema); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE congee_schema_version (id SMALLINT PRIMARY KEY, version INT NOT NULL)",
		"INSERT INTO congee_schema_version (id, version) VALUES (1, 6)",
		// The last tag index cannot be built without name/value. Earlier DDL
		// succeeds, so this catches a version bump or table leaked on failure.
		"CREATE TABLE event_tags (event_id VARCHAR(128))",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateFresh(ctx, db, zerolog.Nop()); err == nil {
		t.Fatal("expected malformed legacy tag table to fail migration")
	}
	var version int
	if err := db.QueryRowContext(ctx, "SELECT version FROM congee_schema_version WHERE id = 1").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 6 {
		t.Fatalf("failed migration changed version to %d", version)
	}
	var eventsExist bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('events') IS NOT NULL").Scan(&eventsExist); err != nil {
		t.Fatal(err)
	}
	if eventsExist {
		t.Fatal("failed migration left the events table behind")
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE event_tags"); err != nil {
		t.Fatal(err)
	}
	if err := migrateFresh(ctx, db, zerolog.Nop()); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT version FROM congee_schema_version WHERE id = 1").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("retry version: got %d want %d", version, schemaVersion)
	}
}

func TestMigrateFreshRecordsVersionAfterDDLInTransaction(t *testing.T) {
	conn := &migrationConn{}
	if err := migrateFresh(context.Background(), migrationTestDB(t, conn), zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	assertFreshMigrationCommitted(t, conn)
	if conn.rollbacks != 0 {
		t.Fatal("successful migration rolled back")
	}
}

func TestMigrateFreshFailureRollsBackAndCanRetry(t *testing.T) {
	for _, failure := range []string{
		"CREATE TABLE IF NOT EXISTS event_tags",
		"INSERT INTO congee_schema_version",
	} {
		t.Run(failure, func(t *testing.T) {
			injected := errors.New("injected migration failure")
			conn := &migrationConn{failPrefix: failure, failErr: injected}
			db := migrationTestDB(t, conn)
			if err := migrateFresh(context.Background(), db, zerolog.Nop()); !errors.Is(err, injected) {
				t.Fatalf("expected injected failure, got %v", err)
			}
			if conn.rollbacks != 1 || conn.commits != 0 || conn.inTx || len(conn.pending) != 0 || len(conn.committed) != 0 {
				t.Fatalf("failed migration left writes: rollbacks=%d commits=%d inTx=%v pending=%d committed=%d", conn.rollbacks, conn.commits, conn.inTx, len(conn.pending), len(conn.committed))
			}
			if failure != "INSERT INTO congee_schema_version" {
				for _, query := range conn.history {
					if strings.HasPrefix(query, "INSERT INTO congee_schema_version") {
						t.Fatal("failed DDL attempted to record the schema version")
					}
				}
			}
			conn.failPrefix = ""
			if err := migrateFresh(context.Background(), db, zerolog.Nop()); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			assertFreshMigrationCommitted(t, conn)
		})
	}
}
