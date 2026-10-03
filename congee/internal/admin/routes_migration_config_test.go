package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michmich112/congee/internal/config"
	"github.com/rs/zerolog"
)

func TestMigrationRejectsAutomaticLiveCutover(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/migration/start", strings.NewReader(`{"make_target_primary":true}`))
	response := httptest.NewRecorder()
	restarted := false
	handleMigrationStart(zerolog.Nop(), "nonexistent-config", nil, nil, func() { restarted = true })(response, req)
	if response.Code != http.StatusConflict || restarted || !strings.Contains(response.Body.String(), "automatic database cutover is disabled") {
		t.Fatalf("unsafe cutover was not blocked: status=%d restarted=%t", response.Code, restarted)
	}
}

func TestMigrationCanonicalDBType(t *testing.T) {
	if g, w := migrationCanonicalDBType(""), "turso"; g != w {
		t.Fatalf("empty: got %q want %q", g, w)
	}
	if g, w := migrationCanonicalDBType("  SQLITE  "), "turso"; g != w {
		t.Fatalf("sqlite: got %q want %q", g, w)
	}
	if g, w := migrationCanonicalDBType("postgres"), "postgres"; g != w {
		t.Fatalf("postgres: got %q want %q", g, w)
	}
	if g, w := migrationCanonicalDBType("turso"), "turso"; g != w {
		t.Fatalf("turso: got %q want %q", g, w)
	}
}

func TestMigrationSourceMatchesConfig(t *testing.T) {
	cfg := &config.Config{
		Database: config.DatabaseSection{Type: "", DSN: "./congee.db"},
	}
	if !migrationSourceMatchesConfig(cfg, migrationEndpoint{Type: "turso", DSN: "./congee.db"}) {
		t.Fatal("expected match for empty type as turso")
	}
	if !migrationSourceMatchesConfig(cfg, migrationEndpoint{Type: "", DSN: "./congee.db"}) {
		t.Fatal("expected match for empty source type")
	}
	if !migrationSourceMatchesConfig(cfg, migrationEndpoint{Type: "sqlite", DSN: "./congee.db"}) {
		t.Fatal("expected match leftover sqlite as turso")
	}
	if migrationSourceMatchesConfig(cfg, migrationEndpoint{Type: "postgres", DSN: "./congee.db"}) {
		t.Fatal("expected mismatch for wrong type")
	}
	if migrationSourceMatchesConfig(cfg, migrationEndpoint{Type: "turso", DSN: "./other.db"}) {
		t.Fatal("expected mismatch for wrong dsn")
	}
}
