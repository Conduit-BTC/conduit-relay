package relay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/rs/zerolog"
)

type eventByIDStore struct {
	visibilityStoreStub
	ev *nostr.Event
}

func (s *eventByIDStore) QueryEvents(ctx context.Context, filters []nostr.Filter) ([]*nostr.Event, error) {
	_, _ = ctx, filters
	if s.ev == nil {
		return nil, nil
	}
	for _, f := range filters {
		for _, id := range f.IDs {
			if id == s.ev.ID {
				cp := *s.ev
				return []*nostr.Event{&cp}, nil
			}
		}
	}
	return nil, nil
}

type chanNotifier struct {
	ch chan string
}

func (n *chanNotifier) Notify(id string) {
	select {
	case n.ch <- id:
	default:
	}
}

func (n *chanNotifier) Listen() <-chan string { return n.ch }

func (n *chanNotifier) Close() error { return nil }

func TestRunImportedEventFanoutNotifiesPlugins(t *testing.T) {
	ev := &nostr.Event{ID: "imported-event-id", Kind: 30402, PubKey: "aa"}
	st := &eventByIDStore{ev: ev}
	rt := &recordingPluginRuntime{}
	srv, err := NewServer(config.DefaultConfig(), st, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetPluginRuntime(rt)
	delivered := make(chan []byte, 2)
	srv.subs.RegisterSender("local", func(b []byte) bool {
		delivered <- b
		return true
	})
	for _, sub := range []struct {
		id   string
		kind int
	}{{"matching", ev.Kind}, {"unmatched", 1}} {
		if err := srv.subs.Add("local", sub.id, []nostr.Filter{{Kinds: []int{sub.kind}}}); err != nil {
			t.Fatal(err)
		}
		srv.subs.FinishSnapshot("local", sub.id)
	}

	n := &chanNotifier{ch: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunImportedEventFanout(ctx, srv, st, n, zerolog.Nop())
	}()

	n.ch <- ev.ID

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rt.storedCount() == 1 && rt.observeCount() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rt.storedCount() != 1 {
		t.Fatalf("plugin stored notifies: %d", rt.storedCount())
	}
	got := rt.lastStored()
	if got.ev.ID != ev.ID || got.ev.Kind != 30402 || !got.stored {
		t.Fatalf("stored notify: %+v", got)
	}
	msg, ok := rt.lastObserve().(*nostr.EventMessage)
	if !ok || msg.Event.ID != ev.ID {
		t.Fatalf("observe: %#v", rt.observe)
	}
	select {
	case b := <-delivered:
		var raw []json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 3 || string(raw[0]) != `"EVENT"` || string(raw[1]) != `"matching"` {
			t.Fatalf("wrong live delivery: %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier event was not delivered locally")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fanout did not exit")
	}
	if len(delivered) != 0 {
		t.Fatal("notifier event was delivered to an unmatched subscription")
	}
}

func TestDeliverImportedEventNilSafe(t *testing.T) {
	var srv *Server
	srv.DeliverImportedEvent(&nostr.Event{})
	srv.DeliverImportedEvent(nil)
}
