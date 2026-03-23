package main

import (
	"log"
	"net/http"
	"os"

	"conduitl2"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
)

func main() {
	relay := khatru.NewRelay()

	store := &slicestore.SliceStore{}
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

	port := os.Getenv("PORT")
	if port == "" {
		port = "3334"
	}

	log.Printf("listening on http://127.0.0.1:%s (ws://127.0.0.1:%s)", port, port)
	if err := http.ListenAndServe(":"+port, relay); err != nil {
		log.Fatalf("relay stopped: %v", err)
	}
}
