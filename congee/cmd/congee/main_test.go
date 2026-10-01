package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/audit"
	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/db"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

func TestListenerFailureHelper(t *testing.T) {
	if os.Getenv("CONGEE_TEST_LISTENER_FAILURE") == "1" {
		main()
	}
}

func TestShutdownFailureHelper(t *testing.T) {
	server := os.Getenv("CONGEE_TEST_SHUTDOWN_FAILURE")
	if server == "" {
		return
	}
	marker := os.Getenv("CONGEE_TEST_SHUTDOWN_MARKER")
	defer func() { _ = os.WriteFile(marker+"-storage-closed", nil, 0600) }()
	log := zerolog.New(os.Stderr)
	if os.Getenv("CONGEE_TEST_SHUTDOWN_LOG_DISABLED") == "1" {
		log = zerolog.Nop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdownOrExit(ctx, server, func(ctx context.Context) error { return ctx.Err() }, log)
	_ = os.WriteFile(marker+"-replacement-started", nil, 0600)
}

func TestShutdownFailureExitsWithoutClosingStorageOrRestarting(t *testing.T) {
	for _, server := range []string{"relay", "admin"} {
		for _, disabled := range []bool{false, true} {
			t.Run(server+"/logging-disabled="+strconv.FormatBool(disabled), func(t *testing.T) {
				dir := t.TempDir()
				marker := filepath.Join(dir, "shutdown")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownFailureHelper$")
				cmd.Dir = dir
				cmd.Env = []string{
					"CONGEE_TEST_SHUTDOWN_FAILURE=" + server,
					"CONGEE_TEST_SHUTDOWN_MARKER=" + marker,
				}
				if disabled {
					cmd.Env = append(cmd.Env, "CONGEE_TEST_SHUTDOWN_LOG_DISABLED=1")
				}
				output, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("failed shutdown did not terminate: %v", ctx.Err())
				}
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("failed shutdown must exit 1: err=%v output=%s", err, output)
				}
				for _, suffix := range []string{"-storage-closed", "-replacement-started"} {
					if _, err := os.Stat(marker + suffix); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("failed shutdown reached unsafe path %s: %v", suffix, err)
					}
				}
				if !disabled && (!strings.Contains(string(output), `"server":"`+server+`"`) || !strings.Contains(string(output), "shutdown failed; exiting without closing storage or restarting")) {
					t.Fatalf("missing shutdown failure diagnostic: %s", output)
				}
			})
		}
	}
}

func TestSuccessfulShutdownReturnsAfterTeardown(t *testing.T) {
	ctx := context.Background()
	finished := false
	shutdownOrExit(ctx, "relay", func(got context.Context) error {
		if got != ctx {
			t.Fatal("shutdown context was not forwarded")
		}
		finished = true
		return nil
	}, zerolog.Nop())
	if !finished {
		t.Fatal("shutdown returned before teardown")
	}
}

type blockedAuditStore struct {
	storage.Store
	ready   chan struct{}
	release chan struct{}
	closed  atomic.Bool
	close   func() error
}

func (s *blockedAuditStore) SaveAuditEntry(ctx context.Context, entry storage.AuditEntry) error {
	close(s.ready)
	<-s.release
	return s.Store.SaveAuditEntry(ctx, entry)
}

func (s *blockedAuditStore) Close() error {
	s.closed.Store(true)
	return s.close()
}

func TestDrainAuditBeforeClosingStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	store, closeStore, err := db.OpenTestStore(ctx, path, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	blocked := &blockedAuditStore{Store: store, ready: make(chan struct{}), release: make(chan struct{}), close: closeStore}
	audit.StartAsyncWriter(ctx, blocked, zerolog.Nop())
	defer audit.StopAsyncWriter()
	audit.Enqueue(storage.AuditEntry{CreatedAt: 1, Action: "shutdown_fixture"})
	select {
	case <-blocked.ready:
	case <-time.After(2 * time.Second):
		close(blocked.release)
		t.Fatal("audit writer did not start")
	}
	done := make(chan error, 1)
	go func() { done <- drainAuditAndCloseStore(blocked) }()
	select {
	case err := <-done:
		close(blocked.release)
		t.Fatalf("store closed before pending audit was drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if blocked.closed.Load() {
		close(blocked.release)
		t.Fatal("store closed during audit save")
	}
	close(blocked.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("audit drain did not finish")
	}
	if !blocked.closed.Load() {
		t.Fatal("store remained open after drain")
	}
	reopened, closeReopened, err := db.OpenTestStore(ctx, path, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer closeReopened()
	rows, err := reopened.QueryAuditLog(ctx, storage.AuditQuery{Action: "shutdown_fixture", Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending audit was not persisted: count=%d err=%v", len(rows), err)
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
