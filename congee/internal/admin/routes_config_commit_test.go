package admin

import (
	"bufio"
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
	"github.com/michmich112/congee/internal/storage/turso"
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
	for _, operation := range []string{"config", "nips", "migration"} {
		for _, failure := range []string{"unavailable", "canceled", "healthy"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				if operation == "migration" && !turso.HasDriver() {
					t.Skip("libsql driver not available")
				}
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
					// Cancel at the audit boundary, after the file and migration are committed.
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
				case "migration":
					method = http.MethodPost
					body, _ = json.Marshal(migrationStartRequest{
						Source:            migrationEndpoint{Type: "turso", DSN: cfg.Database.DSN},
						Target:            migrationEndpoint{Type: "turso", DSN: filepath.Join(dir, "target.db")},
						MakeTargetPrimary: true,
					})
					handler = handleMigrationStart(log, cfgPath, &mu, meta, restart)
				}
				req := httptest.NewRequest(method, "/api/"+operation, bytes.NewReader(body)).WithContext(ctx)
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("committed write reported HTTP %d: %s", rr.Code, rr.Body.String())
				}
				var result map[string]any
				if operation == "migration" {
					result = migrationDoneResponse(t, rr.Body.Bytes())
					if result["config_updated"] != true || result["status"] != "ok" || result["config_error"] != nil {
						t.Fatalf("committed migration reported config failure: %v", result)
					}
				} else if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
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
				case "migration":
					if loaded.Database.DSN != filepath.Join(dir, "target.db") {
						t.Fatal("migration target was not committed")
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

func migrationDoneResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(body))
	var event string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		} else if event == "done" && strings.HasPrefix(line, "data: ") {
			var done map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &done); err != nil {
				t.Fatal(err)
			}
			return done
		}
	}
	t.Fatalf("missing migration done event: %s", body)
	return nil
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
