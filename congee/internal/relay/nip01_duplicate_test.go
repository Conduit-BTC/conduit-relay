package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

func assertPublishACK(t *testing.T, c *Conn, accepted bool, prefix string) {
	t.Helper()
	select {
	case raw := <-c.send:
		var frame []json.RawMessage
		if err := json.Unmarshal(raw, &frame); err != nil || len(frame) != 4 {
			t.Fatalf("invalid ACK: %s", raw)
		}
		var got bool
		var reason string
		if err := json.Unmarshal(frame[2], &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(frame[3], &reason); err != nil {
			t.Fatal(err)
		}
		if got != accepted || !strings.HasPrefix(reason, prefix) {
			t.Fatalf("ACK accepted=%v reason=%q", got, reason)
		}
	default:
		t.Fatal("missing ACK")
	}
}

func TestExactReplaysACKWithoutRepeatingHooks(t *testing.T) {
	for _, kind := range []int{1, 0, 3, 10050, 30078, 30402, 31989, 31990} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			ctx := context.Background()
			st, closeStore := openAuditTestStore(ctx, t, t.TempDir(), "events.db")
			defer closeStore()
			srv, err := NewServer(testRelayConfig(), st, zerolog.Nop(), nil)
			if err != nil {
				t.Fatal(err)
			}
			RegisterNIP01(srv, st)
			hooks := 0
			srv.AppendPostHook("duplicate_test", func(context.Context, HookEnv) error { hooks++; return nil })
			c := testConn(t, srv)
			priv, err := btcec.NewPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			ev := signedTestEvent(t, priv, kind)
			ev.Tags = [][]string{{"d", "synthetic-replay"}}
			if err := ev.Sign(priv); err != nil {
				t.Fatal(err)
			}
			publish := func(ev nostr.Event) {
				t.Helper()
				if err := handleEVENT(ctx, srv, st, c, &nostr.EventMessage{Event: ev}); err != nil {
					t.Fatal(err)
				}
			}
			publish(*ev)
			assertPublishACK(t, c, true, "")
			publish(*ev)
			assertPublishACK(t, c, true, "duplicate:")
			if hooks != 1 {
				t.Fatalf("duplicate ran hooks: %d", hooks)
			}
			invalid := *ev
			invalid.Content = "tampered"
			publish(invalid)
			assertPublishACK(t, c, false, "")
			if hooks != 1 {
				t.Fatal("invalid replay ran hooks")
			}
		})
	}
}

// Simulate another publisher winning between lookup and insert. The real store
// persists once, but this caller sees a conflict and must acknowledge the replay.
type concurrentDuplicateStore struct {
	storage.Store
	lookups int
	saves   int
}

func (s *concurrentDuplicateStore) HasEventID(ctx context.Context, id string) (bool, error) {
	s.lookups++
	if s.lookups == 1 {
		return false, nil
	}
	return s.Store.HasEventID(ctx, id)
}
func (s *concurrentDuplicateStore) SaveEvent(ctx context.Context, ev *nostr.Event) error {
	s.saves++
	if err := s.Store.SaveEvent(ctx, ev); err != nil {
		return err
	}
	return errors.New("synthetic concurrent insert conflict")
}
func TestConcurrentReplayACKDoesNotRunHooks(t *testing.T) {
	ctx := context.Background()
	st, closeStore := openAuditTestStore(ctx, t, t.TempDir(), "events.db")
	defer closeStore()
	wrapped := &concurrentDuplicateStore{Store: st}
	srv, err := NewServer(testRelayConfig(), wrapped, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	RegisterNIP01(srv, wrapped)
	hooks := 0
	srv.AppendPostHook("duplicate_test", func(context.Context, HookEnv) error { hooks++; return nil })
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ev := signedTestEvent(t, priv, 30402)
	c := testConn(t, srv)
	if err := handleEVENT(ctx, srv, wrapped, c, &nostr.EventMessage{Event: *ev}); err != nil {
		t.Fatal(err)
	}
	assertPublishACK(t, c, true, "duplicate:")
	if hooks != 0 || wrapped.saves != 1 || wrapped.lookups != 2 {
		t.Fatalf("hooks=%d saves=%d lookups=%d", hooks, wrapped.saves, wrapped.lookups)
	}
}

func TestAddressableNewerRevisionRejectsStaleReplay(t *testing.T) {
	ctx := context.Background()
	st, closeStore := openAuditTestStore(ctx, t, t.TempDir(), "events.db")
	defer closeStore()
	srv, err := NewServer(testRelayConfig(), st, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	RegisterNIP01(srv, st)
	hooks := 0
	srv.AppendPostHook("revision_test", func(context.Context, HookEnv) error { hooks++; return nil })
	c := testConn(t, srv)
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	old := signedTestEvent(t, priv, 30402)
	old.Tags = [][]string{{"d", "synthetic-revision"}}
	if err := old.Sign(priv); err != nil {
		t.Fatal(err)
	}
	newer := *old
	newer.CreatedAt++
	newer.Content = "newer synthetic revision"
	if err := newer.Sign(priv); err != nil {
		t.Fatal(err)
	}
	publish := func(ev nostr.Event) {
		t.Helper()
		if err := handleEVENT(ctx, srv, st, c, &nostr.EventMessage{Event: ev}); err != nil {
			t.Fatal(err)
		}
	}
	publish(*old)
	assertPublishACK(t, c, true, "")
	publish(newer)
	assertPublishACK(t, c, true, "")
	publish(*old)
	assertPublishACK(t, c, false, storage.ErrStaleReplaceable.Error())
	if hooks != 2 {
		t.Fatalf("stale revision ran post-hooks: %d", hooks)
	}
	events, err := st.QueryEvents(ctx, []nostr.Filter{{Kinds: []int{30402}, Authors: []string{old.PubKey}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != newer.ID {
		t.Fatal("stale replay displaced the newer revision")
	}
	if exists, err := st.HasEventID(ctx, old.ID); err != nil || exists {
		t.Fatalf("stale revision remains stored: exists=%v err=%v", exists, err)
	}
}
