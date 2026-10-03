package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/sdk/plugin/pluginv1"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

type settingsCommitStub struct {
	interceptStub
	fail   bool
	before func()
}

func (s *settingsCommitStub) ApplySettings(context.Context, *pluginv1.ApplySettingsRequest, ...grpc.CallOption) (*pluginv1.ApplySettingsResponse, error) {
	if s.before != nil {
		s.before()
	}
	if s.fail {
		return nil, errors.New("settings rejected")
	}
	return &pluginv1.ApplySettingsResponse{}, nil
}
func TestApplySettingsCommitsOnlyAcceptedSettings(t *testing.T) {
	cfg := config.DefaultConfig()
	old := []byte(`{"mode":"old"}`)
	next := []byte(`{"mode":"new"}`)
	cfg.Plugins.Items = []config.PluginItem{{ID: "fixture", Settings: old}}
	path := filepath.Join(t.TempDir(), "config.json")
	m := NewManager(cfg, path, nil, zerolog.Nop())
	stub := &settingsCommitStub{fail: true}
	in := readyInterceptInstance(m, &interceptStub{})
	in.client = stub
	in.item = cfg.Plugins.Items[0]
	m.inst["fixture"] = in
	stub.before = func() {
		if string(m.hookSettings("fixture")) != string(old) {
			t.Fatal("unaccepted settings visible during RPC")
		}
	}
	if err := m.ApplySettings(context.Background(), "fixture", next); err == nil {
		t.Fatal("missing rejection")
	}
	if string(cfg.Plugins.Items[0].Settings) != string(old) || string(in.item.Settings) != string(old) {
		t.Fatal("rejected settings committed")
	}
	if err := m.PersistConfig(); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]string
	if err := json.Unmarshal(loaded.Plugins.Items[0].Settings, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["mode"] != "old" {
		t.Fatal("later write persisted rejected settings")
	}
	stub.fail = false
	if err := m.ApplySettings(context.Background(), "fixture", next); err != nil {
		t.Fatal(err)
	}
	if string(cfg.Plugins.Items[0].Settings) != string(next) || string(in.item.Settings) != string(next) {
		t.Fatal("accepted settings not committed")
	}
}

func TestApplySettingsDuringPluginStartup(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Plugins.Items = []config.PluginItem{{ID: "fixture", Settings: []byte(`{"mode":"old"}`)}}
	m := NewManager(cfg, "", nil, zerolog.Nop())
	in := &instance{item: cfg.Plugins.Items[0], state: stateStarting}
	m.inst["fixture"] = in
	snapshot := in.settingsJSON()
	snapshot[0] = '!'
	if in.settingsJSON()[0] != '{' {
		t.Fatal("settings snapshot aliases config")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = in.settingsJSON()
		}
	}()
	for range 1000 {
		if err := m.ApplySettings(context.Background(), "fixture", []byte(`{"mode":"new"}`)); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
