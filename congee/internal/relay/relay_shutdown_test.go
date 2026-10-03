package relay

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"sync"
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

func TestConnectionRegistrationRetriesCollision(t *testing.T) {
	srv := &Server{}
	first := &Conn{ID: newConnID()}
	srv.registerConnection(first)
	second := &Conn{ID: first.ID}
	srv.registerConnection(second)
	if len(first.ID) != 32 || len(second.ID) != 32 || first.ID == second.ID {
		t.Fatal("connection IDs must use 128 bits and remain distinct after a collision")
	}
	for _, c := range []*Conn{first, second} {
		got, ok := srv.conns.Load(c.ID)
		if !ok || got != c {
			t.Fatal("registration replaced another connection")
		}
	}
}

type shutdownAuditStore struct {
	visibilityStoreStub
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *shutdownAuditStore) SaveWSConnectionSession(ctx context.Context, _ storage.WSConnectionSession) (int64, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return 1, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestShutdownWaitsForConnectionAuditAndHonorsDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "deadline"}[deadline], func(t *testing.T) {
			st := &shutdownAuditStore{started: make(chan struct{}), release: make(chan struct{})}
			srv, err := NewServer(testRelayConfig(), st, zerolog.Nop(), nil)
			if err != nil {
				t.Fatal(err)
			}
			peer, client := net.Pipe()
			defer client.Close()
			srv.connWG.Add(1)
			srv.open.Add(1)
			go srv.serveWS(peer, httptest.NewRequest("GET", "http://localhost/", nil), "127.0.0.1", false)
			client.Close()
			select {
			case <-st.started:
			case <-time.After(time.Second):
				t.Fatal("session audit did not start")
			}
			timeout := time.Second
			if deadline {
				timeout = 40 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- srv.Shutdown(ctx) }()
			if deadline {
				if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown error = %v", err)
				}
			} else {
				select {
				case err := <-done:
					t.Fatalf("returned before audit completed: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
			}
			close(st.release)
			if !deadline {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			srv.connWG.Wait()
		})
	}
}

func TestConnectionRegisteredDuringShutdownIsCanceled(t *testing.T) {
	srv, err := NewServer(testRelayConfig(), &visibilityStoreStub{}, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.connWG.Add(1)
	srv.open.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	for {
		srv.connMu.Lock()
		stopping := srv.shuttingDown
		srv.connMu.Unlock()
		if stopping {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("shutdown did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	peer, client := net.Pipe()
	defer client.Close()
	go srv.serveWS(peer, httptest.NewRequest("GET", "http://localhost/", nil), "127.0.0.1", false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	count := 0
	srv.conns.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatal("late connection survived shutdown")
	}
	resp := httptest.NewRecorder()
	srv.acceptWebSocket(resp, httptest.NewRequest("GET", "http://localhost/", nil))
	if resp.Code != 503 {
		t.Fatalf("admission after shutdown: %d", resp.Code)
	}
}
