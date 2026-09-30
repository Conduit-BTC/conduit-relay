package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/michmich112/congee/internal/config"
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
