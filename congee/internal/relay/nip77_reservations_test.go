package relay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/michmich112/congee/internal/nip77"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

func TestNEGOpenProtectedKindChallengesThenAuthenticates(t *testing.T) {
	srv, st := newNegReservationServer(t, 32, nil)
	srv.cfg.NIPs.Enabled = append(srv.cfg.NIPs.Enabled, 42)
	srv.cfg.NIP42.RelayURL = "wss://relay.example/"
	srv.cfg.NIP42.RequireAuthSubscribeKinds = []int{1}
	c := newTestNegConn(t, srv, 100, 100)
	negReservationOpen(t, srv, c, "protected")
	challenge := negReservationReply(t, c)
	if len(challenge) != 2 || challenge[0] != "AUTH" || challenge[1] == "" {
		t.Fatalf("missing AUTH challenge: %v", challenge)
	}
	rejection := negReservationReply(t, c)
	if rejection[0] != "NEG-ERR" || !strings.HasPrefix(rejection[2].(string), "auth-required:") {
		t.Fatalf("unexpected unauthenticated response: %v", rejection)
	}
	if st.calls.Load() != 0 || srv.negActiveSessions.Load() != 0 {
		t.Fatal("unauthenticated NEG-OPEN reached storage or reserved capacity")
	}
	// Repeated rejection must retain the same challenge without sending it again.
	negReservationOpen(t, srv, c, "protected")
	if reply := negReservationReply(t, c); reply[0] != "NEG-ERR" {
		t.Fatalf("challenge was sent twice: %v", reply)
	}
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ev := nostr.Event{
		PubKey: hex.EncodeToString(priv.PubKey().SerializeCompressed()[1:]),
		Kind:   nip42AuthEventKind, CreatedAt: time.Now().Unix(),
		Tags: [][]string{{"relay", srv.cfg.NIP42.RelayURL}, {"challenge", challenge[1].(string)}},
	}
	if err := ev.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if err := handleNIP42AUTH(c.ctx, srv, c, &nostr.AuthMessage{Event: ev}); err != nil {
		t.Fatal(err)
	}
	if reply := negReservationReply(t, c); reply[0] != "OK" || reply[2] != true {
		t.Fatalf("challenge could not authenticate: %v", reply)
	}
	negReservationOpen(t, srv, c, "protected")
	negReservationStarted(t, st)
	st.release <- struct{}{}
	if reply := negReservationReply(t, c); reply[0] != "NEG-MSG" {
		t.Fatalf("authenticated retry failed: %v", reply)
	}
	c.negSessions.closeAll()
}

// The first real queue job blocks in storage, leaving later NEG-OPENs pending.
type negReservationStore struct {
	visibilityStoreStub
	first    sync.Once
	started  chan struct{}
	release  chan struct{}
	calls    atomic.Int32
	queryErr error
}

func (st *negReservationStore) QueryEventSyncItems(ctx context.Context, f nostr.Filter) ([]storage.SyncItem, error) {
	st.calls.Add(1)
	st.first.Do(func() {
		close(st.started)
		select {
		case <-st.release:
		case <-ctx.Done():
		}
	})
	return []storage.SyncItem{{ID: strings.Repeat("a", 64), CreatedAt: 1}}, st.queryErr
}

func newNegReservationServer(t *testing.T, max int, queryErr error) (*Server, *negReservationStore) {
	t.Helper()
	st := &negReservationStore{started: make(chan struct{}), release: make(chan struct{}), queryErr: queryErr}
	cfg := testRelayConfigNIP77()
	cfg.NIP77.MaxConcurrentSessions = max
	cfg.NIP77.MaxRecordsPerQuery = 0
	srv, err := NewServer(cfg, st, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	RegisterNIP77(srv, st)
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(st.release) })
		srv.negQueue.stop()
		srv.metricsCancel()
	})
	return srv, st
}

func negReservationOpen(t *testing.T, srv *Server, c *Conn, id string) {
	t.Helper()
	client := nip77.NewClientNegentropy(nip77.BuildVector(nil), 1<<20)
	if err := handleNEGOpen(t.Context(), srv, c, &nostr.NegOpenMessage{SubID: id, Filter: nostr.Filter{Kinds: []int{1}}, InitialHex: client.Start()}); err != nil {
		t.Fatal(err)
	}
}

func negReservationStarted(t *testing.T, st *negReservationStore) {
	t.Helper()
	select {
	case <-st.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for queued storage load")
	}
}

func negReservationReply(t *testing.T, c *Conn) []any {
	t.Helper()
	select {
	case b := <-c.send:
		var reply []any
		if err := json.Unmarshal(b, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for NEG reply")
		return nil
	}
}

func TestNEGOpenRepeatedQueuedIDReleasesOneReservation(t *testing.T) {
	srv, st := newNegReservationServer(t, 32, nil)
	c := newTestNegConn(t, srv, 100, 100)
	negReservationOpen(t, srv, c, "same")
	negReservationStarted(t, st)
	for i := 0; i < 20; i++ {
		negReservationOpen(t, srv, c, "same")
		if n := srv.negActiveSessions.Load(); n != 1 {
			t.Fatalf("replacement reserved %d slots, want 1", n)
		}
	}
	if n := srv.negQueue.PendingDepth(); n != 20 {
		t.Fatalf("pending jobs = %d, want 20", n)
	}
	st.release <- struct{}{}
	if reply := negReservationReply(t, c); reply[0] != "NEG-MSG" {
		t.Fatalf("latest open failed: %v", reply)
	}
	if n := st.calls.Load(); n != 2 {
		t.Fatalf("stale queued jobs loaded storage: %d calls, want 2", n)
	}
	if err := handleNEGClose(t.Context(), srv, c, &nostr.NegCloseMessage{SubID: "same"}); err != nil {
		t.Fatal(err)
	}
	if n := srv.negActiveSessions.Load(); n != 0 {
		t.Fatalf("NEG-CLOSE leaked %d reservations", n)
	}
	negReservationOpen(t, srv, c, "again")
	if reply := negReservationReply(t, c); reply[0] != "NEG-MSG" {
		t.Fatalf("capacity not recovered: %v", reply)
	}
	c.negSessions.closeAll()
	if n := srv.negActiveSessions.Load(); n != 0 {
		t.Fatalf("disconnect leaked %d reservations", n)
	}
	select {
	case reply := <-c.send:
		t.Fatalf("stale job emitted response: %s", reply)
	default:
	}
}

func TestNEGOpenConcurrentConnectionsRespectGlobalCap(t *testing.T) {
	const max = 4
	srv, st := newNegReservationServer(t, max, nil)
	first := newTestNegConn(t, srv, 100, 100)
	negReservationOpen(t, srv, first, "first")
	negReservationStarted(t, st)
	const peers = 32
	conns := make([]*Conn, peers)
	for i := range conns {
		conns[i] = newTestNegConn(t, srv, 100, 100)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var exceeded atomic.Bool
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			negReservationOpen(t, srv, c, fmt.Sprintf("peer-%d", i))
			if srv.negActiveSessions.Load() > max {
				exceeded.Store(true)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := srv.negActiveSessions.Load(); exceeded.Load() || n != max {
		t.Fatalf("global reservations = %d, want %d; overshot=%v", n, max, exceeded.Load())
	}
	if n := srv.negQueue.PendingDepth(); n != max-1 {
		t.Fatalf("queued jobs = %d, want %d", n, max-1)
	}
	st.release <- struct{}{}
	if reply := negReservationReply(t, first); reply[0] != "NEG-MSG" {
		t.Fatalf("first open failed: %v", reply)
	}
	accepted := 1
	for _, c := range conns {
		reply := negReservationReply(t, c)
		if reply[0] == "NEG-MSG" {
			accepted++
		} else if reply[0] != "NEG-ERR" || !strings.Contains(reply[2].(string), "too many sync sessions") {
			t.Fatalf("unexpected reply: %v", reply)
		}
		c.negSessions.closeAll()
	}
	if accepted != max {
		t.Fatalf("accepted %d sessions, want %d", accepted, max)
	}
	first.negSessions.closeAll()
	if n := srv.negActiveSessions.Load(); n != 0 {
		t.Fatalf("close leaked %d reservations", n)
	}
}

func TestNEGOpenPendingCloseAndDisconnectCannotResurrectSession(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%v", disconnect), func(t *testing.T) {
			srv, st := newNegReservationServer(t, 1, nil)
			c := newTestNegConn(t, srv, 100, 100)
			negReservationOpen(t, srv, c, "pending")
			negReservationStarted(t, st)
			if disconnect {
				c.negSessions.closeAll()
			} else if err := handleNEGClose(t.Context(), srv, c, &nostr.NegCloseMessage{SubID: "pending"}); err != nil {
				t.Fatal(err)
			}
			if n := srv.negActiveSessions.Load(); n != 0 {
				t.Fatalf("pending close leaked %d reservations", n)
			}
			next := newTestNegConn(t, srv, 100, 100)
			negReservationOpen(t, srv, next, "next")
			st.release <- struct{}{}
			if reply := negReservationReply(t, next); reply[0] != "NEG-MSG" {
				t.Fatalf("capacity not recovered: %v", reply)
			}
			if n := srv.negActiveSessions.Load(); n != 1 {
				t.Fatalf("stale worker changed new reservation: %d", n)
			}
			if c.negSessions.count() != 0 {
				t.Fatal("closed pending session resurrected")
			}
			select {
			case reply := <-c.send:
				t.Fatalf("closed job replied: %s", reply)
			default:
			}
			next.negSessions.closeAll()
		})
	}
}

func TestNEGOpenLoadErrorReleasesReservation(t *testing.T) {
	srv, st := newNegReservationServer(t, 1, errors.New("query failed"))
	c := newTestNegConn(t, srv, 100, 100)
	negReservationOpen(t, srv, c, "fail")
	negReservationStarted(t, st)
	st.release <- struct{}{}
	if reply := negReservationReply(t, c); reply[0] != "NEG-ERR" {
		t.Fatalf("want load failure: %v", reply)
	}
	if n := srv.negActiveSessions.Load(); n != 0 {
		t.Fatalf("failed load leaked %d reservations", n)
	}
	if c.negSessions.count() != 0 {
		t.Fatal("failed load remains in session map")
	}
}

func TestNEGOpenFullQueueReleasesLatestReservation(t *testing.T) {
	srv, st := newNegReservationServer(t, 1, nil)
	c := newTestNegConn(t, srv, 100, 100)
	negReservationOpen(t, srv, c, "same")
	negReservationStarted(t, st)
	for i := 0; i < cap(srv.negQueue.jobs); i++ {
		negReservationOpen(t, srv, c, "same")
	}
	negReservationOpen(t, srv, c, "same")
	if reply := negReservationReply(t, c); reply[0] != "NEG-ERR" || !strings.Contains(reply[2].(string), "queue full") {
		t.Fatalf("want queue full rejection: %v", reply)
	}
	if n := srv.negActiveSessions.Load(); n != 0 {
		t.Fatalf("full queue leaked %d reservations", n)
	}
	if c.negSessions.count() != 0 {
		t.Fatal("rejected open remains in session map")
	}
	st.release <- struct{}{}
	// stop waits until the in-flight load has finished; pending jobs are all stale.
	srv.negQueue.stop()
	if n := srv.negActiveSessions.Load(); n != 0 {
		t.Fatalf("stale worker changed count: %d", n)
	}
	select {
	case reply := <-c.send:
		t.Fatalf("rejected session resurrected: %s", reply)
	default:
	}
}
