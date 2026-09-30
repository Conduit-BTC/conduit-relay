package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/config"
	"github.com/rs/zerolog"
	"github.com/uptrace/bun/driver/pgdriver"
)

type legacyPostgresConnector struct{ conn *legacyPostgresConn }

func (c legacyPostgresConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c legacyPostgresConnector) Driver() driver.Driver                        { return legacyPostgresDriver{} }

type legacyPostgresDriver struct{}

func (legacyPostgresDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use legacyPostgresConnector")
}

// Exercise the real database/sql query path without requiring a PostgreSQL service.
type legacyPostgresConn struct {
	tableExists  bool
	version      *int64
	checkErr     error
	versionErr   error
	tables       []string
	versionReads int
}

func (*legacyPostgresConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported")
}
func (*legacyPostgresConn) Close() error { return nil }
func (*legacyPostgresConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}
func (c *legacyPostgresConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.Contains(query, "information_schema.tables") {
		if !strings.Contains(query, "CURRENT_SCHEMA()") || len(args) != 1 {
			return nil, errors.New("table check must use the current schema and a bound table name")
		}
		table, ok := args[0].Value.(string)
		if !ok {
			return nil, errors.New("table name must be a string")
		}
		c.tables = append(c.tables, table)
		if c.checkErr != nil {
			return nil, c.checkErr
		}
		return &legacyPostgresRows{values: []driver.Value{table == "congee_schema_version" && c.tableExists}}, nil
	}
	if query == "SELECT version FROM congee_schema_version WHERE id = 1" {
		c.versionReads++
		if !c.tableExists {
			return nil, errors.New("undefined schema version table")
		}
		if c.versionErr != nil {
			return nil, c.versionErr
		}
		rows := &legacyPostgresRows{}
		if c.version != nil {
			rows.values = []driver.Value{*c.version}
		}
		return rows, nil
	}
	return nil, errors.New("unexpected legacy metadata query")
}

type legacyPostgresRows struct{ values []driver.Value }

func (*legacyPostgresRows) Columns() []string { return []string{"value"} }
func (*legacyPostgresRows) Close() error      { return nil }
func (r *legacyPostgresRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	dest[0] = r.values[0]
	r.values = r.values[1:]
	return nil
}

func TestLegacyPostgresSchemaDiscovery(t *testing.T) {
	v6, v7 := int64(6), int64(7)
	permissionErr := errors.New("fixture permission denied")
	for _, tc := range []struct {
		name             string
		conn             legacyPostgresConn
		wantErr          error
		wantVersionReads int
		wantTableChecks  int
	}{
		{name: "fresh database", wantTableChecks: 1},
		{name: "empty version table", conn: legacyPostgresConn{tableExists: true}, wantVersionReads: 1, wantTableChecks: 1},
		{name: "current schema", conn: legacyPostgresConn{tableExists: true, version: &v7}, wantVersionReads: 1, wantTableChecks: 1},
		{name: "legacy schema still checks metadata", conn: legacyPostgresConn{tableExists: true, version: &v6}, wantVersionReads: 1, wantTableChecks: 5},
		{name: "table discovery error", conn: legacyPostgresConn{checkErr: permissionErr}, wantErr: permissionErr, wantTableChecks: 1},
		{name: "version read error", conn: legacyPostgresConn{tableExists: true, versionErr: permissionErr}, wantErr: permissionErr, wantVersionReads: 1, wantTableChecks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &tc.conn
			sqldb := sql.OpenDB(legacyPostgresConnector{conn})
			t.Cleanup(func() { _ = sqldb.Close() })
			err := copyLegacyMetaPostgres(context.Background(), sqldb, "", nil, zerolog.Nop())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("migration error: got %v want %v", err, tc.wantErr)
			}
			if conn.versionReads != tc.wantVersionReads || len(conn.tables) != tc.wantTableChecks {
				t.Fatalf("discovery queries: version=%d tables=%v", conn.versionReads, conn.tables)
			}
			if tc.wantTableChecks == 5 && strings.Join(conn.tables, ",") != "congee_schema_version,audit_log,config_changelog,relay_metric_buckets,ws_connection_sessions" {
				t.Fatalf("legacy metadata checks changed: %v", conn.tables)
			}
		})
	}
}

func TestLegacyPostgresSchemaDiscoveryPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sqldb := sql.OpenDB(legacyPostgresConnector{&legacyPostgresConn{}})
	defer sqldb.Close()
	if err := copyLegacyMetaPostgres(ctx, sqldb, "", nil, zerolog.Nop()); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestOpenFreshPostgresDatabase(t *testing.T) {
	dsn := testPostgresDSN(t)
	ctx := context.Background()
	pg := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer pg.Close()
	schema := fmt.Sprintf("congee_fresh_test_%d", time.Now().UnixNano())
	if _, err := pg.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pg.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid PostgreSQL test DSN")
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	handle, err := Open(ctx, config.DatabaseSection{
		Type:    "postgres",
		DSN:     u.String(),
		MetaDSN: filepath.Join(t.TempDir(), "meta.db"),
	}, "fresh-postgres-test", zerolog.Nop())
	if err != nil {
		t.Fatalf("open fresh database: %v", err)
	}
	defer handle.Close()
	var version int
	if err := pg.QueryRowContext(ctx, `SELECT version FROM "`+schema+`".congee_schema_version WHERE id = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 7 {
		t.Fatalf("fresh schema version: got %d want 7", version)
	}
}
