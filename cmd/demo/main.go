package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"conduitl2"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
)

func main() {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		log.Fatalf("failed to load runtime config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	relay := khatru.NewRelay()

	store := openStore(cfg)
	defer store.Close()
	if err := store.Init(); err != nil {
		log.Fatalf("failed to init store: %v", err)
	}
	relay.UseEventstore(store, 500)

	opts := conduitl2.Scope2Options{
		MaxQueryLimit:     100,
		DefaultQueryLimit: 25,
		MaxProjectionScan: 2000,
		EnableNIP50:       true,
	}

	conduitl2.ConfigureRelay(relay, opts)
	baseQuery := relay.QueryStored
	relay.QueryStored = conduitl2.WrapProductQueries(baseQuery, opts)

	relay.Info.Name = "khatru conduit l2 scope2 demo"
	relay.Info.Description = "demo relay with conduit scope2 extensions enabled"

	if _, err := conduitl2.StartRelaySync(ctx, relay, cfg.Sync); err != nil {
		log.Fatalf("failed to start relay sync: %v", err)
	}

	log.Printf("listening on http://127.0.0.1:%s (ws://127.0.0.1:%s)", cfg.Port, cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, relay); err != nil {
		log.Fatalf("relay stopped: %v", err)
	}
}

func openStore(cfg runtimeConfig) eventstore.Store {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("failed to create data dir: %v", err)
	}
	return &boltdb.BoltBackend{Path: cfg.StorePath}
}
