package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/plugin"
	"github.com/rs/zerolog"
)

func TestPluginMutationsReportPersistenceFailure(t *testing.T) {
	for _, operation := range []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"install", http.MethodPost, "/api/plugins/install", nil},
		{"disable", http.MethodPost, "/api/plugins/fixture/disable", nil},
		{"uninstall", http.MethodPost, "/api/plugins/fixture/uninstall", []byte(`{"wipe_data":true}`)},
		{"settings", http.MethodPut, "/api/plugins/fixture/settings", []byte(`{"mode":"fixture"}`)},
		{"intercept-log", http.MethodPut, "/api/plugins/fixture/intercept-log", []byte(`{"limit":2}`)},
	} {
		t.Run(operation.name, func(t *testing.T) {
			_, handler, cfgPath, stop := interceptLogHTTP(t)
			defer stop()
			if operation.name == "install" {
				src := t.TempDir()
				if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(`{"id":"installed-fixture","api_version":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				operation.body, err = json.Marshal(map[string]any{"path": src, "enable": false})
				if err != nil {
					t.Fatal(err)
				}
			}
			// A directory at the target makes atomic rename fail on any user account.
			if err := os.Rename(cfgPath, cfgPath+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cfgPath, 0o700); err != nil {
				t.Fatal(err)
			}
			response := interceptLogReq(t, handler, operation.method, operation.path, interceptLogHTTPPassword, operation.body)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("mutation hid persistence failure: status=%d body=%s", response.Code, response.Body.String())
			}
			var result struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.OK || result.Error != "plugin change could not be persisted; runtime may have changed" {
				t.Fatalf("unexpected persistence response: %+v", result)
			}
			if err := os.Remove(cfgPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(cfgPath+".saved", cfgPath); err != nil {
				t.Fatal(err)
			}
			response = interceptLogReq(t, handler, operation.method, operation.path, interceptLogHTTPPassword, operation.body)
			if response.Code != http.StatusOK {
				t.Fatalf("mutation did not recover after persistence restored: status=%d body=%s", response.Code, response.Body.String())
			}
			persisted, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			switch operation.name {
			case "install":
				if _, idx := config.PluginItemByID(persisted, "installed-fixture"); idx < 0 {
					t.Fatal("installed plugin not saved")
				}
			case "disable":
				if item, _ := config.PluginItemByID(persisted, "fixture"); item.Enabled {
					t.Fatal("disabled plugin not saved")
				}
			case "uninstall":
				if _, idx := config.PluginItemByID(persisted, "fixture"); idx >= 0 {
					t.Fatal("uninstalled plugin remained in config")
				}
			case "settings":
				item, _ := config.PluginItemByID(persisted, "fixture")
				var settings map[string]string
				if err := json.Unmarshal(item.Settings, &settings); err != nil {
					t.Fatal(err)
				}
				if settings["mode"] != "fixture" {
					t.Fatal("settings not saved")
				}
			case "intercept-log":
				if persisted.Plugins.InterceptLogSize == nil || *persisted.Plugins.InterceptLogSize != 2 {
					t.Fatal("intercept log limit not saved")
				}
			}
		})
	}
}

func TestConfigReplacementBlocksPluginMutations(t *testing.T) {
	for _, replacement := range []string{"config", "nips", "icon", "banner", "migration"} {
		for _, op := range []struct{ method, path, body string }{
			{"POST", "/api/plugins/install", `{"path":"unused"}`},
			{"POST", "/api/plugins/fixture/enable", ""},
			{"POST", "/api/plugins/fixture/disable", ""},
			{"POST", "/api/plugins/fixture/uninstall", `{}`},
			{"PUT", "/api/plugins/fixture/settings", `{}`},
			{"PUT", "/api/plugins/fixture/intercept-log", `{"limit":2}`},
		} {
			t.Run(replacement+op.path, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.json")
				cfg := config.DefaultConfig()
				cfg.Database.DSN = filepath.Join(dir, "source.db")
				cfg.Plugins.Directory = filepath.Join(dir, "plugins")
				cfg.Plugins.Items = []config.PluginItem{{ID: "fixture"}}
				if err := config.WriteConfigAtomic(path, cfg); err != nil {
					t.Fatal(err)
				}
				m := plugin.NewManager(cfg, path, nil, zerolog.Nop())
				defer m.Stop()
				s := NewServer(cfg, path, &configAuditStore{}, nil, zerolog.Nop(), interceptLogHTTPPassword, dir, nil, nil, config.RelayInstanceResolution{}, m)
				handler := s.http.Handler
				var committedCode int
				switch replacement {
				case "config":
					next, err := config.Load(path)
					if err != nil {
						t.Fatal(err)
					}
					next.Relay.Port++
					body, err := json.Marshal(next)
					if err != nil {
						t.Fatal(err)
					}
					committedCode = interceptLogReq(t, handler, "PUT", "/api/config", interceptLogHTTPPassword, body).Code
				case "nips":
					committedCode = interceptLogReq(t, handler, "PATCH", "/api/nips", interceptLogHTTPPassword, []byte(`{"nip":50,"enabled":true}`)).Code
				case "icon", "banner":
					upload := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						r.URL.Path = "/api/relay-assets/" + replacement
						handler.ServeHTTP(w, r)
					})
					committedCode = postRelayAsset(t, upload, onePixelPNG(), "fixture.png", interceptLogHTTPPassword).Code
				case "migration":
					body, err := json.Marshal(migrationStartRequest{
						Source:            migrationEndpoint{Type: "turso", DSN: cfg.Database.DSN},
						Target:            migrationEndpoint{Type: "turso", DSN: filepath.Join(dir, "target.db")},
						MakeTargetPrimary: true,
					})
					if err != nil {
						t.Fatal(err)
					}
					committedCode = interceptLogReq(t, handler, "POST", "/api/migration/start", interceptLogHTTPPassword, body).Code
				}
				if committedCode != http.StatusOK {
					t.Fatalf("replacement status %d", committedCode)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				response := interceptLogReq(t, handler, op.method, op.path, interceptLogHTTPPassword, []byte(op.body))
				if response.Code != http.StatusConflict {
					t.Fatalf("stale mutation status %d", response.Code)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Fatal("plugin mutation overwrote replacement")
				}
			})
		}
	}

}
