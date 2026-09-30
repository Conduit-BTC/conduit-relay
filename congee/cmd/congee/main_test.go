package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/config"
)

func TestListenerFailureHelper(t *testing.T) {
	if os.Getenv("CONGEE_TEST_LISTENER_FAILURE") == "1" {
		main()
	}
}

func TestListenerFailureShutsDownProcess(t *testing.T) {
	for _, listener := range []string{"relay", "admin"} {
		t.Run(listener, func(t *testing.T) {
			blocked, err := net.Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			defer blocked.Close()
			blockedPort := blocked.Addr().(*net.TCPAddr).Port
			cfg := config.DefaultConfig()
			cfg.Relay.Port = blockedPort
			if listener == "admin" {
				available, err := net.Listen("tcp", ":0")
				if err != nil {
					t.Fatal(err)
				}
				cfg.Relay.Port = available.Addr().(*net.TCPAddr).Port
				if err := available.Close(); err != nil {
					t.Fatal(err)
				}
				cfg.Admin.Port = blockedPort
			}
			dir := t.TempDir()
			cfg.Database.DSN = filepath.Join(dir, "events.db")
			cfg.Database.MetaDSN = filepath.Join(dir, "meta.db")
			configPath := filepath.Join(dir, "config.json")
			if err := config.WriteConfigAtomic(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestListenerFailureHelper$")
			cmd.Dir = dir
			cmd.Env = []string{
				"CONGEE_TEST_LISTENER_FAILURE=1",
				"CONFIG_PATH=" + configPath,
				"ENABLE_ADMIN_UI=" + strconv.FormatBool(listener == "admin"),
				"ADMIN_PASSWORD=synthetic-test-password",
			}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("process did not exit after %s listener failure: %v", listener, ctx.Err())
			}
			if err != nil {
				t.Fatalf("process failed: %v\n%s", err, output)
			}
			for _, message := range []string{listener + " server stopped", "listener failure shutdown", "bye"} {
				if !strings.Contains(string(output), `"message":"`+message+`"`) {
					t.Fatalf("missing %q log:\n%s", message, output)
				}
			}
		})
	}
}
