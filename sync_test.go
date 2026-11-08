package conduitl2

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	khatru "conduitl2/third_party/khatru"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"github.com/stretchr/testify/require"
)

func TestRelaySyncerHandleIncomingEventStoresAndBroadcastsLatest(t *testing.T) {
	relay, _ := newSyncTestRelay(t)
	broadcasts := make(chan nostr.Event, 8)
	relay.OnConnect = nil
	relay.OnDisconnect = nil

	client, sub := connectRelayForSyncTest(t, relay)
	defer client.Close()
	defer sub.Unsub()

	syncer := &RelaySyncer{relay: relay, cfg: SyncConfig{VerifySignatures: true}}

	sk := nostr.Generate()
	first := signedProductEvent(t, sk, 1000, "apple", `{"title":"Apple","summary":"one","price":10,"currency":"USD","updatedAt":1000}`)
	second := signedProductEvent(t, sk, 1001, "apple", `{"title":"Apple","summary":"two","price":11,"currency":"USD","updatedAt":1001}`)
	older := signedProductEvent(t, sk, 999, "apple", `{"title":"Apple","summary":"old","price":9,"currency":"USD","updatedAt":999}`)

	go func() {
		for evt := range sub.Events {
			broadcasts <- evt
		}
	}()

	syncer.handleIncomingEvent(t.Context(), first)
	syncer.handleIncomingEvent(t.Context(), second)
	syncer.handleIncomingEvent(t.Context(), older)

	require.Eventually(t, func() bool {
		stats := syncer.Stats()
		return stats.Imported == 2 && stats.Duplicates == 1
	}, 2*time.Second, 20*time.Millisecond)

	filter := nostr.Filter{Kinds: []nostr.Kind{30402}, Authors: []nostr.PubKey{second.PubKey}, Tags: nostr.TagMap{"d": []string{"apple"}}, Limit: 10}
	stored := collectSeqEvents(relay.QueryStored(t.Context(), filter))
	require.Len(t, stored, 1)
	require.Equal(t, second.ID, stored[0].ID)

	require.Eventually(t, func() bool { return len(broadcasts) == 2 }, 2*time.Second, 20*time.Millisecond)
	got1 := <-broadcasts
	got2 := <-broadcasts
	require.Equal(t, []nostr.ID{first.ID, second.ID}, []nostr.ID{got1.ID, got2.ID})
}

func TestRelaySyncerBackfillAdvancesState(t *testing.T) {
	relay, _ := newSyncTestRelay(t)
	sk := nostr.Generate()
	backfillStart := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	eventCreatedAt := backfillStart.Add(30 * time.Minute).Unix()
	evt := signedProductEvent(t, sk, eventCreatedAt, "banana", `{"title":"Banana","summary":"fresh","price":7,"currency":"USD"}`)

	pool := stubSyncPool{
		fetchMany: func(ctx context.Context, urls []string, filter nostr.Filter, _ nostr.SubscriptionOptions) chan nostr.RelayEvent {
			require.Equal(t, []nostr.Kind{30402, 5}, filter.Kinds)
			ch := make(chan nostr.RelayEvent, 1)
			ch <- nostr.RelayEvent{Event: evt}
			close(ch)
			return ch
		},
		subscribeMany: func(ctx context.Context, urls []string, filter nostr.Filter, _ nostr.SubscriptionOptions) chan nostr.RelayEvent {
			ch := make(chan nostr.RelayEvent)
			close(ch)
			return ch
		},
	}

	statePath := filepath.Join(t.TempDir(), "sync-state.json")
	syncer := &RelaySyncer{
		relay: relay,
		pool:  pool,
		cfg: SyncConfig{
			Enabled:          true,
			Relays:           []string{"wss://relay.example"},
			StatePath:        statePath,
			BackfillSince:    backfillStart,
			BackfillWindow:   time.Hour,
			FetchLimit:       10,
			VerifySignatures: true,
		},
	}

	require.NoError(t, syncer.backfill(t.Context()))

	stats := syncer.Stats()
	require.Equal(t, 1, stats.Imported)

	b, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var state syncState
	require.NoError(t, json.Unmarshal(b, &state))
	require.Greater(t, state.SyncedUntil, backfillStart.Unix())
}

func TestRelaySyncerStreamLiveUsesRecentOverlap(t *testing.T) {
	relay, _ := newSyncTestRelay(t)
	called := make(chan nostr.Filter, 1)
	pool := stubSyncPool{
		fetchMany: func(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent {
			ch := make(chan nostr.RelayEvent)
			close(ch)
			return ch
		},
		subscribeMany: func(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent {
			called <- filter
			ch := make(chan nostr.RelayEvent)
			close(ch)
			return ch
		},
	}

	now := time.Now().UTC()
	syncer := &RelaySyncer{
		relay: relay,
		pool:  pool,
		cfg:   SyncConfig{Relays: []string{"wss://relay.example"}, LiveLookback: 10 * time.Minute},
		state: syncState{SyncedUntil: now.Add(-2 * time.Minute).Unix()},
	}

	require.NoError(t, syncer.streamLive(t.Context()))
	filter := <-called
	require.GreaterOrEqual(t, int64(filter.Since), now.Add(-12*time.Minute).Unix())
	require.LessOrEqual(t, int64(filter.Since), now.Add(-1*time.Minute).Unix())
}

func TestTargetsProductDeletion(t *testing.T) {
	require.True(t, targetsProductDeletion(nostr.Event{Kind: 5, Tags: nostr.Tags{{"a", "30402:pubkey:dtag"}}}))
	require.True(t, targetsProductDeletion(nostr.Event{Kind: 5, Tags: nostr.Tags{{"k", "30402"}}}))
	require.False(t, targetsProductDeletion(nostr.Event{Kind: 5, Tags: nostr.Tags{{"k", "30023"}, {"a", "30023:pubkey:dtag"}}}))
}

func newSyncTestRelay(t *testing.T) (*khatru.Relay, *slicestore.SliceStore) {
	t.Helper()
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 500)
	ConfigureRelay(relay, Scope2Options{MaxQueryLimit: 50, DefaultQueryLimit: 25})
	base := relay.QueryStored
	relay.QueryStored = WrapProductQueries(relay, base, Scope2Options{MaxQueryLimit: 50, DefaultQueryLimit: 25})
	return relay, store
}

func connectRelayForSyncTest(t *testing.T, relay *khatru.Relay) (*nostr.Relay, *nostr.Subscription) {
	t.Helper()
	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	url := "ws" + server.URL[4:]
	client, err := nostr.RelayConnect(t.Context(), url, nostr.RelayOptions{})
	require.NoError(t, err)
	sub, err := client.Subscribe(t.Context(), nostr.Filter{Kinds: []nostr.Kind{30402}, Limit: 10}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	select {
	case <-sub.EndOfStoredEvents:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial subscription query")
	}
	return client, sub
}

func signedProductEvent(t *testing.T, sk nostr.SecretKey, createdAt int64, dTag, content string) nostr.Event {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Timestamp(createdAt), Kind: 30402, Tags: nostr.Tags{{"d", dTag}}, Content: content}
	require.NoError(t, evt.Sign(sk))
	return evt
}

func collectSeqEvents(seq iter.Seq[nostr.Event]) []nostr.Event {
	items := make([]nostr.Event, 0, 8)
	for evt := range seq {
		items = append(items, evt)
	}
	return items
}

type stubSyncPool struct {
	fetchMany     func(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent
	subscribeMany func(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent
}

func (s stubSyncPool) FetchMany(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent {
	return s.fetchMany(ctx, urls, filter, opts)
}

func (s stubSyncPool) SubscribeMany(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent {
	return s.subscribeMany(ctx, urls, filter, opts)
}

// A full response containing already-stored events must still split the window:
// the source may have more events than its response limit allows it to return.
func TestRelaySyncerBackfillSplitsDuplicateWindow(t *testing.T) {
	relay, _ := newSyncTestRelay(t)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	evt := signedProductEvent(t, nostr.Generate(), start.Add(10*time.Minute).Unix(), "apple", `{"title":"Apple"}`)
	syncer := &RelaySyncer{relay: relay, cfg: SyncConfig{
		BackfillSince: start, BackfillWindow: time.Hour, FetchLimit: 2,
		VerifySignatures: true,
	}}
	syncer.handleIncomingEvent(t.Context(), evt)
	var filters []nostr.Filter
	syncer.pool = stubSyncPool{fetchMany: func(_ context.Context, _ []string, filter nostr.Filter, _ nostr.SubscriptionOptions) chan nostr.RelayEvent {
		filters = append(filters, filter)
		ch := make(chan nostr.RelayEvent, 2)
		if len(filters) == 1 {
			ch <- nostr.RelayEvent{Event: evt}
			ch <- nostr.RelayEvent{Event: evt}
		}
		close(ch)
		return ch
	}}
	require.NoError(t, syncer.backfill(t.Context()))
	require.GreaterOrEqual(t, len(filters), 2)
	require.Equal(t, filters[0].Since, filters[1].Since, "retry the same start before advancing the watermark")
	require.Less(t, filters[1].Until, filters[0].Until, "split even when no new events were imported")
	require.Equal(t, 1, syncer.Stats().Imported)
	require.Equal(t, 2, syncer.Stats().Duplicates)
}

func TestRelaySyncerLoadStateRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          int64
	}{
		{"truncated", `{"synced_until":`, 0},
		{"valid", `{"synced_until":1234}`, 1234},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			var logs bytes.Buffer
			syncer := &RelaySyncer{cfg: SyncConfig{StatePath: path, Logger: log.New(&logs, "", 0)}}
			require.NoError(t, syncer.loadState())
			require.Equal(t, tc.want, syncer.state.SyncedUntil)
			if tc.name == "truncated" {
				require.Contains(t, logs.String(), "resetting watermark")
			} else {
				require.Empty(t, logs.String())
			}
		})
	}
	t.Run("read error remains fatal", func(t *testing.T) {
		syncer := &RelaySyncer{cfg: SyncConfig{StatePath: t.TempDir()}}
		require.ErrorContains(t, syncer.loadState(), "read sync state")
	})
}

func TestSyncStateReplacementPreservesPreviousOnWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	previous := syncState{SyncedUntil: 100}
	require.NoError(t, writeSyncState(path, previous))
	// A blocked temporary destination must not truncate the last good state.
	require.NoError(t, os.Mkdir(path+".tmp", 0o700))
	require.Error(t, writeSyncState(path, syncState{SyncedUntil: 200}))
	got, err := readSyncState(path)
	require.NoError(t, err)
	require.Equal(t, previous, got)
	require.NoError(t, os.Remove(path+".tmp"))
	require.NoError(t, writeSyncState(path, syncState{SyncedUntil: 200}))
	got, err = readSyncState(path)
	require.NoError(t, err)
	require.Equal(t, int64(200), got.SyncedUntil)
	_, err = os.Stat(path + ".tmp")
	require.True(t, os.IsNotExist(err))
}
