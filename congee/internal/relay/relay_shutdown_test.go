package relay

import (
	"context"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/nip77"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

type shutdownNegStore struct {
	visibilityStoreStub
	blockCount bool
	started    chan struct{}
	canceled   chan struct{}
}

func (s *shutdownNegStore) block(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	return ctx.Err()
}

func (s *shutdownNegStore) CountEvents(ctx context.Context, _ []nostr.Filter) (int, error) {
	if s.blockCount {
		return 0, s.block(ctx)
	}
	return 0, nil
}

func (s *shutdownNegStore) QueryEventSyncItems(ctx context.Context, _ nostr.Filter) ([]storage.SyncItem, error) {
	return nil, s.block(ctx)
}

func TestShutdownCancelsActiveNEGStorageBeforeWaiting(t *testing.T) {
	for _, blockCount := range []bool{true, false} {
		name := "sync-query"
		if blockCount {
			name = "count-query"
		}
		t.Run(name, func(t *testing.T) {
			st := &shutdownNegStore{blockCount: blockCount, started: make(chan struct{}), canceled: make(chan struct{})}
			cfg := testRelayConfigNIP77()
			cfg.NIP77.MaxRecordsPerQuery = 100
			srv, err := NewServer(cfg, st, zerolog.Nop(), nil)
			if err != nil {
				t.Fatal(err)
			}
			RegisterNIP77(srv, st)
			c := newTestNegConn(t, srv, 100, 100)
			c.log = zerolog.Nop()
			srv.conns.Store(c.ID, c)
			t.Cleanup(func() {
				c.initiateShutdown()
				srv.negQueue.stop()
				srv.metricsCancel()
			})
			client := nip77.NewClientNegentropy(nip77.BuildVector(nil), 1<<20)
			if err := handleNEGOpen(c.ctx, srv, c, &nostr.NegOpenMessage{SubID: "blocked", Filter: nostr.Filter{Kinds: []int{1}}, InitialHex: client.Start()}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-st.started:
			case <-time.After(5 * time.Second):
				t.Fatal("queued NEG-OPEN did not reach storage")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- srv.Shutdown(ctx) }()
			select {
			case err := <-done:
				if err != nil || ctx.Err() != nil {
					t.Fatalf("shutdown missed its deadline: err=%v context=%v", err, ctx.Err())
				}
			case <-ctx.Done():
				// Release the old implementation's stuck worker before failing.
				c.initiateShutdown()
				<-done
				t.Fatal("shutdown waited for storage before canceling its connection context")
			}
			select {
			case <-st.canceled:
			default:
				t.Fatal("shutdown returned without canceling active storage")
			}
			if srv.negActiveSessions.Load() != 0 {
				t.Fatal("canceled worker leaked its session reservation")
			}
		})
	}
}
