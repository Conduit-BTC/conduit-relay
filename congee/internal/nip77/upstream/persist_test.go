package upstream

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/gorilla/websocket"
	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/db"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/plugin"
	"github.com/michmich112/congee/internal/relay"
	"github.com/michmich112/congee/internal/storage/turso"
	"github.com/rs/zerolog"
)

type recordingRuntime struct {
	mu       sync.Mutex
	stored   []*nostr.Event
	observed []string
}

func (r *recordingRuntime) Observe(msg any) {
	if ev, ok := msg.(*nostr.EventMessage); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.observed = append(r.observed, ev.Event.ID)
	}
}

func (r *recordingRuntime) InterceptREQ(context.Context, *nostr.ReqMessage) plugin.InterceptResult {
	return plugin.InterceptResult{Action: plugin.InterceptPassthrough}
}

func (r *recordingRuntime) EnqueueStoredEvent(ev *nostr.Event, stored bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stored {
		r.stored = append(r.stored, ev)
	}
}

func (r *recordingRuntime) Snapshot() []plugin.InstanceSnapshot { return nil }

func (r *recordingRuntime) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.stored))
	for _, ev := range r.stored {
		out = append(out, ev.ID)
	}
	return out
}

func TestPersistImportedEventNotifiesPlugins(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx := context.Background()
	st, closeFn, err := db.OpenTestStore(ctx, filepath.Join(t.TempDir(), "events.db"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	srv, err := relay.NewServer(config.DefaultConfig(), st, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := &recordingRuntime{}
	srv.SetPluginRuntime(rt)

	sch := NewScheduler(nil, st, srv, nil, zerolog.Nop())
	ev := &nostr.Event{
		ID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PubKey:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CreatedAt: 1,
		Kind:      30402,
		Content:   "imported",
		Sig:       "cc",
	}
	ok, err := sch.persistImportedEvent(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected import")
	}
	ids := rt.ids()
	if len(ids) != 1 || ids[0] != ev.ID {
		t.Fatalf("plugin stored: %v", ids)
	}

	ok, err = sch.persistImportedEvent(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("duplicate import should be skipped")
	}
	if got := rt.ids(); len(got) != 1 {
		t.Fatalf("duplicate should not re-notify plugins: %v", got)
	}

	// NIP-77 can fetch revisions out of order. Neither an older timestamp nor a
	// higher ID at the same timestamp may replace or notify for the winner.
	older := *ev
	older.ID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	older.CreatedAt = 0
	for _, stale := range []*nostr.Event{&older, {
		ID:     "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		PubKey: ev.PubKey, CreatedAt: ev.CreatedAt, Kind: ev.Kind,
		Content: "tied loser", Sig: ev.Sig,
	}} {
		ok, err := sch.persistImportedEvent(ctx, stale)
		if err != nil || ok {
			t.Fatalf("stale import: stored=%v err=%v", ok, err)
		}
	}
	if got := rt.ids(); len(got) != 1 {
		t.Fatalf("stale revisions should not notify plugins: %v", got)
	}
	stored, err := st.QueryEvents(ctx, []nostr.Filter{{Authors: []string{ev.PubKey}, Kinds: []int{ev.Kind}}})
	if err != nil || len(stored) != 1 || stored[0].ID != ev.ID {
		t.Fatalf("wrong retained revision: %+v err=%v", stored, err)
	}
}

func TestPersistImportedEventNilServer(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx := context.Background()
	st, closeFn, err := db.OpenTestStore(ctx, filepath.Join(t.TempDir(), "events.db"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()

	sch := NewScheduler(nil, st, nil, nil, zerolog.Nop())
	ev := &nostr.Event{
		ID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PubKey:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CreatedAt: 1,
		Kind:      1,
		Content:   "x",
		Sig:       "cc",
	}
	ok, err := sch.persistImportedEvent(ctx, ev)
	if err == nil || ok {
		t.Fatal("import without an admission policy must fail")
	}
	has, err := st.HasEventID(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("unvalidated event was stored")
	}
}

func TestPersistImportedEventAppliesRegisteredAdmissionPolicy(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	for _, policy := range []string{"signature", "nip17", "nip42", "nip29", "ephemeral"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			st, closeFn, err := db.OpenTestStore(ctx, filepath.Join(t.TempDir(), "events.db"), zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer closeFn()
			cfg := config.DefaultConfig()
			cfg.NIPs.Enabled = []int{1, 42, 29}
			cfg.NIP42.RequireAuthPublishKinds = []int{30402}
			srv, err := relay.NewServer(cfg, st, zerolog.Nop(), nil)
			if err != nil {
				t.Fatal(err)
			}
			relay.RegisterNIP01(srv, st)
			relay.RegisterNIP17(srv, st)
			relay.RegisterNIP42(srv, st)
			relay.RegisterNIP29(srv, st)
			rt := &recordingRuntime{}
			srv.SetPluginRuntime(rt)
			priv, err := btcec.NewPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			ev := &nostr.Event{PubKey: hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey())), CreatedAt: time.Now().Unix(), Kind: 1, Tags: [][]string{}, Content: "synthetic"}
			switch policy {
			case "nip17":
				ev.Kind = 1059
			case "nip42":
				ev.Kind = 30402
				ev.Tags = [][]string{{"d", "synthetic"}}
			case "nip29":
				ev.Kind = 9007
				ev.Tags = [][]string{{"h", "synthetic"}}
			case "ephemeral":
				ev.Kind = 20001
			}
			if err := ev.Sign(priv); err != nil {
				t.Fatal(err)
			}
			if policy == "signature" {
				ev.Content = "tampered"
			}
			sch := NewScheduler(cfg, st, srv, nil, zerolog.Nop())
			if ok, err := sch.persistImportedEvent(ctx, ev); ok || err == nil {
				t.Fatalf("policy %s admitted import", policy)
			}
			if has, err := st.HasEventID(ctx, ev.ID); err != nil || has {
				t.Fatalf("rejected event persisted: %t %v", has, err)
			}
			if len(rt.ids()) != 0 {
				t.Fatal("rejected event notified plugins")
			}
			ev.Kind, ev.Tags, ev.Content = 1, [][]string{}, "accepted synthetic"
			if err := ev.Sign(priv); err != nil {
				t.Fatal(err)
			}
			if ok, err := sch.persistImportedEvent(ctx, ev); !ok || err != nil {
				t.Fatalf("public write denied: %t %v", ok, err)
			}
		})
	}
}

func TestPersistImportedEventDeliversToLocalWebSocketSubscriptions(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx := context.Background()
	st, closeFn, err := db.OpenTestStore(ctx, filepath.Join(t.TempDir(), "events.db"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	cfg := config.DefaultConfig()
	srv, err := relay.NewServer(cfg, st, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	relay.RegisterNIP01(srv, st)
	rt := &recordingRuntime{}
	srv.SetPluginRuntime(rt)
	// Imports must retain their existing semantics, without re-running optional
	// WebSocket post-store mutation hooks or connection-scoped audit hooks.
	hookCalls := 0
	srv.AppendPostHook("unexpected_import_hook", func(context.Context, relay.HookEnv) error {
		hookCalls++
		return nil
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		select {
		case <-serveDone:
		case <-shutdownCtx.Done():
			t.Error("relay did not stop")
		}
	}()
	client, response, err := websocket.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/", nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	readMessage := func() (string, []json.RawMessage) {
		t.Helper()
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		var raw []json.RawMessage
		if err := client.ReadJSON(&raw); err != nil {
			t.Fatal(err)
		}
		var typ string
		if len(raw) < 2 || json.Unmarshal(raw[0], &typ) != nil {
			t.Fatalf("invalid relay reply: %s", raw)
		}
		return typ, raw
	}
	for _, sub := range []struct {
		id   string
		kind int
	}{{"listings", 30402}, {"unrelated", 1}, {"private", 1059}} {
		if err := client.WriteJSON([]any{"REQ", sub.id, nostr.Filter{Kinds: []int{sub.kind}}}); err != nil {
			t.Fatal(err)
		}
		typ, raw := readMessage()
		if typ != "EOSE" || string(raw[1]) != `"`+sub.id+`"` {
			t.Fatalf("subscription setup: %s %s", typ, raw)
		}
	}
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ev := &nostr.Event{PubKey: hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey())), CreatedAt: 2, Kind: 30402, Tags: [][]string{{"d", "listing"}}, Content: "imported"}
	if err := ev.Sign(priv); err != nil {
		t.Fatal(err)
	}
	sch := NewScheduler(cfg, st, srv, nil, zerolog.Nop())
	if ok, err := sch.persistImportedEvent(ctx, ev); err != nil || !ok {
		t.Fatalf("import: stored=%t err=%v", ok, err)
	}
	// A new snapshot provides a wire barrier after synchronous import delivery,
	// so missing/extra fanout is detected without waiting for a read timeout.
	barrier := func(id string, expected []*nostr.Event) {
		t.Helper()
		if err := client.WriteJSON([]any{"REQ", id, nostr.Filter{IDs: []string{strings.Repeat("f", 64)}}}); err != nil {
			t.Fatal(err)
		}
		got := 0
		for {
			typ, raw := readMessage()
			if typ == "EOSE" && string(raw[1]) == `"`+id+`"` {
				break
			}
			if typ != "EVENT" || len(raw) < 3 || got >= len(expected) {
				t.Fatalf("unexpected import reply: %s %s", typ, raw)
			}
			var delivered nostr.Event
			if err := json.Unmarshal(raw[2], &delivered); err != nil {
				t.Fatal(err)
			}
			if string(raw[1]) != `"listings"` || delivered.ID != expected[got].ID {
				t.Fatalf("wrong import delivery: sub=%s id=%s", raw[1], delivered.ID)
			}
			got++
		}
		if got != len(expected) {
			t.Fatalf("live imported events=%d want=%d", got, len(expected))
		}
	}
	barrier("first", []*nostr.Event{ev})
	older := *ev
	older.CreatedAt--
	if err := older.Sign(priv); err != nil {
		t.Fatal(err)
	}
	for _, skipped := range []*nostr.Event{ev, &older} {
		if ok, err := sch.persistImportedEvent(ctx, skipped); err != nil || ok {
			t.Fatalf("duplicate/stale import: stored=%t err=%v", ok, err)
		}
	}
	barrier("skipped", nil)
	// Even direct imported-event delivery must use the relay's privacy gate.
	// The upstream filter test independently prevents fetching this kind.
	private := &nostr.Event{PubKey: ev.PubKey, CreatedAt: 2, Kind: 1059, Tags: [][]string{{"p", ev.PubKey}}, Content: "wrapped"}
	if err := private.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if ok, err := sch.persistImportedEvent(ctx, private); err != nil || !ok {
		t.Fatalf("private import: stored=%t err=%v", ok, err)
	}
	barrier("privacy", nil)
	if hookCalls != 0 {
		t.Fatalf("imports ran WebSocket mutation hooks %d times", hookCalls)
	}
	if got := rt.ids(); len(got) != 2 || got[0] != ev.ID || got[1] != private.ID {
		t.Fatalf("plugin stored events: %v", got)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.observed) != 2 || rt.observed[0] != ev.ID || rt.observed[1] != private.ID {
		t.Fatalf("plugin observed events: %v", rt.observed)
	}
}
