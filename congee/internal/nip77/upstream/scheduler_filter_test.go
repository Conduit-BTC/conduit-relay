package upstream

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/gorilla/websocket"
	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/db"
	"github.com/michmich112/congee/internal/nip77"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/michmich112/congee/internal/storage/turso"
	"github.com/rs/zerolog"
)

// advertiseUpstreamEvents deliberately advertises every event, even when it does
// not match NEG-OPEN's filter. This models an untrusted upstream, not a relay
// that already enforces the same filter on our behalf.
func advertiseUpstreamEvents(t *testing.T, events []*nostr.Event) (string, <-chan error) {
	t.Helper()
	items := make([]storage.SyncItem, 0, len(events))
	byID := make(map[string]*nostr.Event, len(events))
	for _, ev := range events {
		items = append(items, storage.SyncItem{ID: ev.ID, CreatedAt: ev.CreatedAt})
		byID[ev.ID] = ev
	}
	result := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		defer func() { result <- err }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		neg := nip77.NewServerNegentropy(nip77.BuildVector(items), 1<<20)
		for {
			var raw []json.RawMessage
			if readErr := conn.ReadJSON(&raw); readErr != nil {
				return // The sync client closes the socket after all fetches.
			}
			if len(raw) < 2 {
				err = fmt.Errorf("short upstream request")
				return
			}
			var typ, subID string
			_ = json.Unmarshal(raw[0], &typ)
			_ = json.Unmarshal(raw[1], &subID)
			switch typ {
			case "NEG-OPEN", "NEG-MSG":
				index := 2
				if typ == "NEG-OPEN" {
					index = 3
				}
				if len(raw) <= index {
					err = fmt.Errorf("short negentropy request")
					return
				}
				var message string
				_ = json.Unmarshal(raw[index], &message)
				var reply string
				reply, err = neg.Reconcile(message)
				if err == nil {
					err = conn.WriteJSON([]any{"NEG-MSG", subID, reply})
				}
			case "REQ":
				var filter nostr.Filter
				if len(raw) < 3 {
					err = fmt.Errorf("short fetch request")
					return
				}
				if err = json.Unmarshal(raw[2], &filter); err == nil && len(filter.IDs) == 1 {
					err = conn.WriteJSON([]any{"EVENT", subID, byID[filter.IDs[0]]})
				}
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http"), result
}

func TestSyncFilterRejectsSignedEventsOutsideConfiguredFilter(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	otherPriv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pubkey := hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey()))
	since, until := int64(100), int64(200)
	filter := nostr.Filter{Authors: []string{pubkey}, Kinds: []int{1}, Since: &since, Until: &until, Tag: map[string][]string{"#t": {"allowed"}}}
	events := make([]*nostr.Event, 0, 12)
	appendSigned := func(name string, kind int, createdAt int64, tags [][]string, key *btcec.PrivateKey) {
		t.Helper()
		ev := &nostr.Event{PubKey: hex.EncodeToString(schnorr.SerializePubKey(key.PubKey())), CreatedAt: createdAt, Kind: kind, Tags: tags, Content: name}
		if err := ev.Sign(key); err != nil {
			t.Fatal(err)
		}
		if err := ev.VerifySig(); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	allowedTags := [][]string{{"t", "allowed"}}
	appendSigned("lower boundary", 1, since, allowedTags, priv)
	appendSigned("upper boundary", 1, until, allowedTags, priv)
	appendSigned("matching fields but outside ID filter", 1, 150, allowedTags, priv)
	appendSigned("excluded kind", 2, 150, allowedTags, priv)
	appendSigned("gift wrap", 1059, 150, allowedTags, priv)
	appendSigned("ephemeral gift wrap", 21059, 150, allowedTags, priv)
	appendSigned("excluded author", 1, 150, allowedTags, otherPriv)
	appendSigned("excluded tag", 1, 150, [][]string{{"t", "other"}}, priv)
	appendSigned("missing tag", 1, 150, nil, priv)
	appendSigned("before since", 1, since-1, allowedTags, priv)
	appendSigned("after until", 1, until+1, allowedTags, priv)
	for _, restrictIDs := range []bool{false, true} {
		t.Run(fmt.Sprintf("restrictIDs=%t", restrictIDs), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st, closeFn, err := db.OpenTestStore(ctx, filepath.Join(t.TempDir(), "events.db"), zerolog.Nop())
			if err != nil {
				t.Fatal(err)
			}
			defer closeFn()
			f := filter
			wantImported := 3
			if restrictIDs {
				f.IDs = []string{events[0].ID, events[1].ID}
				wantImported = 2
			}
			url, upstreamResult := advertiseUpstreamEvents(t, events)
			client, err := dialUpstream(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			sch := NewScheduler(config.DefaultConfig(), st, nil, nil, zerolog.Nop())
			need, imported, err := syncFilter(ctx, sch, zerolog.Nop(), client, url, f, 1<<20)
			_ = client.Close()
			if err != nil || need != len(events) || imported != wantImported {
				t.Fatalf("sync: need=%d imported=%d err=%v", need, imported, err)
			}
			for i, ev := range events {
				has, err := st.HasEventID(ctx, ev.ID)
				if err != nil || has != (i < wantImported) {
					t.Fatalf("event %q stored=%t err=%v", ev.Content, has, err)
				}
			}
			select {
			case err := <-upstreamResult:
				if err != nil {
					t.Fatalf("upstream: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("upstream did not finish")
			}
		})
	}
}
