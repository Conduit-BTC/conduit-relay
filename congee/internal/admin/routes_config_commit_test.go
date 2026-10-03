package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

type configAuditStore struct {
	storage.Store
	err    error
	cancel context.CancelFunc
	calls  int
}

func (s *configAuditStore) SaveConfigChange(ctx context.Context, _ storage.ConfigChange) error {
	s.calls++
	if s.cancel != nil {
		s.cancel()
		return ctx.Err()
	}
	return s.err
}

func TestCommittedConfigRestartHandlesAuditOutcome(t *testing.T) {
	for _, operation := range []string{"config", "nips"} {
		for _, failure := range []string{"unavailable", "canceled", "healthy"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				cfgPath := filepath.Join(dir, "config.json")
				cfg := config.DefaultConfig()
				cfg.Database.DSN = filepath.Join(dir, "source.db")
				cfg.NIPs.Enabled = append(cfg.NIPs.Enabled, 50)
				if err := config.WriteConfigAtomic(cfgPath, cfg); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				const privateError = "synthetic-private-audit-payload"
				meta := &configAuditStore{err: errors.New(privateError)}
				if failure == "healthy" {
					meta.err = nil
				}
				if failure == "canceled" {
					// Cancel at the audit boundary, after the file is committed.
					meta.cancel = cancel
				}
				var logs bytes.Buffer
				log := zerolog.New(&logs)
				restarted := make(chan struct{}, 1)
				restart := func() { restarted <- struct{}{} }
				var mu sync.Mutex
				var handler http.Handler
				var body []byte
				method := http.MethodPut
				switch operation {
				case "config":
					cfg.Relay.Port++
					body, _ = json.Marshal(cfg)
					handler = handlePutConfig(cfgPath, &mu, meta, log, restart)
				case "nips":
					method = http.MethodPatch
					body = []byte(`{"nip":50,"enabled":false}`)
					handler = handleNIPsPatch(cfgPath, &mu, meta, log, restart)
				}
				req := httptest.NewRequest(method, "/api/"+operation, bytes.NewReader(body)).WithContext(ctx)
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("committed write reported HTTP %d: %s", rr.Code, rr.Body.String())
				}
				var result map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				} else if result["ok"] != true {
					t.Fatalf("committed write did not report success: %v", result)
				}
				if result["restart_required"] != true || result["restarting"] != true {
					t.Fatalf("committed write did not schedule restart: %v", result)
				}
				warning, _ := result["audit_warning"].(string)
				if (warning != "") != (failure != "healthy") || meta.calls != 1 {
					t.Fatalf("incorrect audit warning or write attempt: result=%v calls=%d", result, meta.calls)
				}
				if strings.Contains(rr.Body.String()+logs.String(), privateError) {
					t.Fatal("audit error payload leaked into response or logs")
				}
				if strings.Contains(logs.String(), "changelog write failed") != (failure != "healthy") {
					t.Fatal("operational log does not match audit outcome")
				}
				loaded, err := config.LoadJSON(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "config":
					if loaded.Relay.Port != cfg.Relay.Port {
						t.Fatal("config update was not committed")
					}
				case "nips":
					if slices.Contains(loaded.NIPs.Enabled, 50) {
						t.Fatal("nip toggle was not committed")
					}
				}
				select {
				case <-restarted:
				case <-time.After(2 * time.Second):
					t.Fatal("committed config did not restart")
				}
			})
		}
	}
}

func TestPostMigrationConfigHandlesAuditOutcome(t *testing.T) {
	for _, outcome := range []string{"unavailable", "canceled", "healthy"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.json")
			cfg := config.DefaultConfig()
			cfg.Database.DSN = filepath.Join(dir, "source.db")
			if err := config.WriteConfigAtomic(cfgPath, cfg); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const privateError = "synthetic-private-audit-payload"
			meta := &configAuditStore{err: errors.New(privateError)}
			if outcome == "healthy" {
				meta.err = nil
			} else if outcome == "canceled" {
				meta.cancel = cancel
			}
			var logs bytes.Buffer
			var mu sync.Mutex
			target := migrationEndpoint{Type: "turso", DSN: filepath.Join(dir, "target.db")}
			committed := false
			restart, warning, err := applyPostMigrationDatabaseConfig(ctx, cfgPath, &mu, meta, target, zerolog.New(&logs), func(needRestart bool) {
				if meta.calls != 0 {
					t.Fatal("commit callback ran after the audit attempt")
				}
				loaded, err := config.LoadJSON(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Database.Type != target.Type || loaded.Database.DSN != target.DSN {
					t.Fatal("commit callback ran before database config was persisted")
				}
				if !needRestart {
					t.Fatal("database replacement did not require restart")
				}
				committed = true
			})
			if err != nil || !restart || !committed || meta.calls != 1 {
				t.Fatalf("committed replacement outcome incorrect: restart=%v committed=%v calls=%d err=%v", restart, committed, meta.calls, err)
			}
			if (warning != "") != (outcome != "healthy") {
				t.Fatalf("incorrect audit warning for %s", outcome)
			}
			if strings.Contains(warning+logs.String(), privateError) {
				t.Fatal("audit error payload leaked into warning or logs")
			}
			if strings.Contains(logs.String(), "changelog write failed") != (outcome != "healthy") {
				t.Fatal("operational log does not match audit outcome")
			}
		})
	}
}

func TestCommittedConfigAuditWarningWithoutRestartScheduler(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	if err := config.WriteConfigAtomic(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Relay.Port++
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	meta := &configAuditStore{err: context.Canceled}
	var mu sync.Mutex
	rr := httptest.NewRecorder()
	handlePutConfig(cfgPath, &mu, meta, zerolog.Nop(), nil).ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)))
	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || result["ok"] != true || result["restart_required"] != true || result["restarting"] != false || result["audit_warning"] == nil {
		t.Fatalf("manual restart outcome incorrect: %v", result)
	}
}

func TestPostMigrationConfigRejectsInvalidTargetBeforeAudit(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	if err := config.WriteConfigAtomic(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	meta := &configAuditStore{err: errors.New("audit unavailable")}
	var mu sync.Mutex
	restart, warning, err := applyPostMigrationDatabaseConfig(context.Background(), cfgPath, &mu, meta,
		migrationEndpoint{Type: "turso", DSN: ""}, zerolog.Nop())
	if err == nil || restart || warning != "" || meta.calls != 0 {
		t.Fatalf("invalid target treated as committed: restart=%v warning=%q err=%v calls=%d", restart, warning, err, meta.calls)
	}
	loaded, err := config.LoadJSON(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Equal(cfg, loaded) {
		t.Fatal("rejected target changed the config file")
	}
}
