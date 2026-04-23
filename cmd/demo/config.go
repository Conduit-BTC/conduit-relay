package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"conduitl2"
)

type runtimeConfig struct {
	Port      string
	DataDir   string
	StorePath string
	Sync      conduitl2.SyncConfig
}

func loadRuntimeConfig() (runtimeConfig, error) {
	port := getenv("PORT", "3334")
	dataDir := getenv("DATA_DIR", filepath.Join("tmp", "demo-data"))
	storePath := filepath.Join(dataDir, "events.db")
	statePath := filepath.Join(dataDir, "sync-state.json")

	syncCfg := conduitl2.DefaultSyncConfig(statePath)
	syncCfg.Enabled = getenvBool("SYNC_ENABLED", false)
	syncCfg.Relays = splitList(getenv("SYNC_RELAYS", strings.Join(defaultRelayList(), ",")))
	syncCfg.Logger = log.New(os.Stderr, "[conduitl2-sync] ", log.LstdFlags)

	if raw := strings.TrimSpace(os.Getenv("SYNC_FETCH_LIMIT")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("parse SYNC_FETCH_LIMIT: %w", err)
		}
		syncCfg.FetchLimit = v
	}
	if raw := strings.TrimSpace(os.Getenv("SYNC_BACKFILL_WINDOW")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("parse SYNC_BACKFILL_WINDOW: %w", err)
		}
		syncCfg.BackfillWindow = d
	}
	if raw := strings.TrimSpace(os.Getenv("SYNC_LIVE_LOOKBACK")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("parse SYNC_LIVE_LOOKBACK: %w", err)
		}
		syncCfg.LiveLookback = d
	}
	if raw := strings.TrimSpace(os.Getenv("SYNC_BACKFILL_SINCE")); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("parse SYNC_BACKFILL_SINCE: %w", err)
		}
		syncCfg.BackfillSince = ts.UTC()
	}

	return runtimeConfig{
		Port:      port,
		DataDir:   dataDir,
		StorePath: storePath,
		Sync:      syncCfg,
	}, nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			items = append(items, part)
		}
	}
	return items
}

func defaultRelayList() []string {
	return []string{"wss://relay.damus.io", "wss://nos.lol", "wss://relay.primal.net", "wss://relay.plebeian.market"}
}
