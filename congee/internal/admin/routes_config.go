package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

func handleGetConfig(cfgPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			http.Error(w, `{"error":"read config failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

func handlePutConfig(cfgPath string, cfgMu *sync.Mutex, st storage.Store, log zerolog.Logger, scheduleRestart func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
			return
		}
		newCfg, err := config.ParseConfigJSON(body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		cfgMu.Lock()

		if err := config.RequireNIP11UploadFiles(cfgPath, newCfg); err != nil {
			cfgMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		prev, _ := os.ReadFile(cfgPath)
		needRestart := configRestartNeeded(prev, newCfg)

		if err := config.WriteConfigAtomic(cfgPath, newCfg); err != nil {
			cfgMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := config.PruneNIP11Assets(cfgPath, newCfg); err != nil {
			log.Warn().Err(err).Msg("nip11 asset cleanup failed")
		}

		diff := string(body)
		if len(prev) > 0 {
			diff = "previous_bytes=" + strconv.Itoa(len(prev)) + "\n" + string(config.RedactSecretsForLog(body))
		}
		auditWarning := recordCommittedConfigChange(r.Context(), st, log, "PUT /api/config", diff)
		cfgMu.Unlock()

		if needRestart && scheduleRestart != nil {
			go scheduleRestartSoon(scheduleRestart)
		}

		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{
			"ok":               true,
			"restart_required": needRestart,
			"restarting":       needRestart && scheduleRestart != nil,
		}
		if auditWarning != "" {
			result["audit_warning"] = auditWarning
		}
		_ = json.NewEncoder(w).Encode(result)
	}
}

// recordCommittedConfigChange treats changelog failure as a warning after the
// atomic config write. A failed audit must not prevent applying committed settings.
func recordCommittedConfigChange(ctx context.Context, meta storage.MetaStore, log zerolog.Logger, summary, diff string) string {
	if err := config.SaveConfigChange(ctx, meta, summary, diff); err != nil {
		// Storage errors can include config secrets; log only the fixed operation.
		log.Warn().Str("operation", summary).Msg("config saved but changelog write failed")
		return "Configuration was saved, but the audit changelog could not be recorded."
	}
	return ""
}

func configRestartNeeded(prevFile []byte, newCfg *config.Config) bool {
	if len(prevFile) == 0 {
		return true
	}
	prevCfg, err := config.ParseConfigJSON(prevFile)
	if err != nil {
		return true
	}
	return !config.Equal(prevCfg, newCfg)
}

func scheduleRestartSoon(scheduleRestart func()) {
	time.Sleep(150 * time.Millisecond)
	scheduleRestart()
}

func handleConfigChangelog(st storage.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		qctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		rows, err := st.QueryConfigChangelog(qctx, limit)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"changelog": rows})
	}
}
