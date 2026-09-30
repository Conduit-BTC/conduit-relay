package relay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/nip77"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

type negGroupPrivacyStore struct {
	visibilityStoreStub
	events        []*nostr.Event
	queryErr      error
	metadataCalls int
	maxBatch      int
	metadataFn    func(context.Context) (*nostr.Event, error)
}

func (st *negGroupPrivacyStore) QueryEventSyncItems(_ context.Context, f nostr.Filter) ([]storage.SyncItem, error) {
	var items []storage.SyncItem
	for _, ev := range st.events {
		if f.Matches(ev) {
			items = append(items, storage.SyncItem{ID: ev.ID, CreatedAt: ev.CreatedAt})
		}
	}
	return items, nil
}

func (st *negGroupPrivacyStore) CountEvents(ctx context.Context, filters []nostr.Filter) (int, error) {
	items, err := st.QueryEventSyncItems(ctx, filters[0])
	return len(items), err
}

func (st *negGroupPrivacyStore) QueryEvents(ctx context.Context, filters []nostr.Filter) ([]*nostr.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if st.queryErr != nil {
		return nil, st.queryErr
	}
	st.maxBatch = max(st.maxBatch, len(filters[0].IDs))
	var events []*nostr.Event
	for _, ev := range st.events {
		if filters[0].Matches(ev) {
			events = append(events, ev)
		}
	}
	return events, nil
}

func (st *negGroupPrivacyStore) GetLatestGroupMetadata39000(ctx context.Context, relayPubkey, groupID string) (*nostr.Event, error) {
	st.metadataCalls++
	if st.metadataFn != nil {
		return st.metadataFn(ctx)
	}
	if groupID != "private" {
		return nil, nil
	}
	return st.visibilityStoreStub.GetLatestGroupMetadata39000(ctx, relayPubkey, groupID)
}

func negGroupEvent(id string, createdAt int64, group string) *nostr.Event {
	ev := &nostr.Event{ID: id, Kind: 1, CreatedAt: createdAt}
	if group != "" {
		ev.Tags = [][]string{{"h", group}}
	}
	return ev
}

func newNegGroupServer(t *testing.T, st *negGroupPrivacyStore) (*Server, *Conn) {
	t.Helper()
	cfg := testRelayConfigNIP77()
	cfg.NIPs.Enabled = append(cfg.NIPs.Enabled, 29, 42)
	srv, err := NewServer(cfg, st, zerolog.Nop(), testRelayIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	RegisterNIP77(srv, st)
	c := registerTestConn(t, srv, "neg-group")
	c.negSessions = newNegSessionMap()
	t.Cleanup(func() {
		c.negSessions.closeAll()
		srv.negQueue.stop()
		srv.metricsCancel()
	})
	return srv, c
}

func reconcileNegGroupIDs(t *testing.T, srv *Server, c *Conn, filter nostr.Filter) []string {
	t.Helper()
	client := nip77.NewClientNegentropy(nip77.BuildVector(nil), 1<<20)
	if err := handleNEGOpen(t.Context(), srv, c, &nostr.NegOpenMessage{SubID: "group", Filter: filter, InitialHex: client.Start()}); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		reply := negReservationReply(t, c)
		if reply[0] != "NEG-MSG" {
			t.Fatalf("expected reconciliation, got %v", reply)
		}
		out, err := client.Reconcile(reply[2].(string))
		if err != nil {
			t.Fatal(err)
		}
		if out == "" {
			var ids []string
			for id := range client.HaveNots {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			return ids
		}
		if err := handleNEGMsg(t.Context(), srv, c, &nostr.NegMsgMessage{SubID: "group", MessageHex: out}); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("reconciliation did not converge")
	return nil
}

func TestNEGOpenPreservesPrivateGroupReadPolicy(t *testing.T) {
	memberPK := strings.Repeat("d", 64)
	privateID := strings.Repeat("b", 64)
	publicIDs := []string{strings.Repeat("a", 64), strings.Repeat("c", 64)}
	for _, policy := range []struct {
		name      string
		auth      string
		mdErr     error
		memberErr error
		member    bool
	}{
		{name: "unauthenticated"},
		{name: "authenticated nonmember", auth: strings.Repeat("e", 64)},
		{name: "authenticated member", auth: memberPK, member: true},
		{name: "metadata failure", auth: memberPK, mdErr: errors.New("metadata unavailable"), member: true},
		{name: "membership failure", auth: memberPK, memberErr: errors.New("membership unavailable")},
	} {
		for _, scope := range []string{"kind", "group", "event ID"} {
			t.Run(policy.name+"/"+scope, func(t *testing.T) {
				st := &negGroupPrivacyStore{
					visibilityStoreStub: visibilityStoreStub{md: privateGroupMetadata(), mdErr: policy.mdErr,
						memberFn: func(_ context.Context, _, _, pk string) (bool, error) {
							return policy.member && pk == memberPK, policy.memberErr
						}},
					events: []*nostr.Event{negGroupEvent(publicIDs[0], 1, ""), negGroupEvent(privateID, 2, "private"), negGroupEvent(publicIDs[1], 3, "public")},
				}
				srv, c := newNegGroupServer(t, st)
				if policy.auth != "" {
					c.nip42AddPubkey(policy.auth)
				}
				filter := nostr.Filter{Kinds: []int{1}}
				var want []string
				switch scope {
				case "kind":
					want = slices.Clone(publicIDs)
				case "group":
					filter.Tag = map[string][]string{"#h": {"private"}}
				case "event ID":
					filter.IDs = []string{privateID}
				}
				if policy.member && policy.mdErr == nil && policy.memberErr == nil {
					want = append(want, privateID)
				}
				slices.Sort(want)
				if got := reconcileNegGroupIDs(t, srv, c, filter); !slices.Equal(got, want) {
					t.Fatalf("reconciled IDs %v, want %v", got, want)
				}
			})
		}
	}
}

func TestNEGOpenVisibilityQueryFailureReleasesSession(t *testing.T) {
	st := &negGroupPrivacyStore{events: []*nostr.Event{negGroupEvent(strings.Repeat("a", 64), 1, "")}, queryErr: errors.New("event lookup unavailable")}
	srv, c := newNegGroupServer(t, st)
	client := nip77.NewClientNegentropy(nip77.BuildVector(nil), 1<<20)
	if err := handleNEGOpen(t.Context(), srv, c, &nostr.NegOpenMessage{SubID: "group", Filter: nostr.Filter{Kinds: []int{1}}, InitialHex: client.Start()}); err != nil {
		t.Fatal(err)
	}
	reply := negReservationReply(t, c)
	if reply[0] != "NEG-ERR" || reply[2] != "error: query failed" || srv.negActiveSessions.Load() != 0 {
		t.Fatalf("visibility failure left a session or exposed data: %v", reply)
	}
}

func TestNEGOpenDoesNotDisclosePrivateRecordCount(t *testing.T) {
	st := &negGroupPrivacyStore{visibilityStoreStub: visibilityStoreStub{md: privateGroupMetadata()}}
	for i := range 3 {
		st.events = append(st.events, negGroupEvent(fmt.Sprintf("%064x", i+1), int64(i+1), "private"))
	}
	srv, c := newNegGroupServer(t, st)
	srv.cfg.NIP77.MaxRecordsPerQuery = 1
	client := nip77.NewClientNegentropy(nip77.BuildVector(nil), 1<<20)
	if err := handleNEGOpen(t.Context(), srv, c, &nostr.NegOpenMessage{SubID: "group", Filter: nostr.Filter{Kinds: []int{1}}, InitialHex: client.Start()}); err != nil {
		t.Fatal(err)
	}
	reply := negReservationReply(t, c)
	if reply[0] != "NEG-ERR" || reply[2] != "blocked: this query exceeds the maximum of 1 records" {
		t.Fatalf("unexpected count response: %v", reply)
	}
}

func TestNEGGroupVisibilityCancelsMetadataLookup(t *testing.T) {
	started := make(chan struct{})
	st := &negGroupPrivacyStore{
		events: []*nostr.Event{negGroupEvent(strings.Repeat("a", 64), 1, "private")},
		metadataFn: func(ctx context.Context) (*nostr.Event, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	srv, c := newNegGroupServer(t, st)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	filter := nostr.Filter{Kinds: []int{1}}
	items, _ := st.QueryEventSyncItems(ctx, filter)
	done := make(chan error, 1)
	go func() {
		_, err := srv.visibleNegSyncItems(ctx, c, filter, items)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata lookup did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lookup error=%v, want canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled reconciliation left its metadata lookup blocked")
	}
}

func TestNEGGroupVisibilityBoundsLoadsAndPreservesOrder(t *testing.T) {
	st := &negGroupPrivacyStore{visibilityStoreStub: visibilityStoreStub{md: privateGroupMetadata()}}
	for i := range 1025 {
		group := "private"
		if i%2 == 0 {
			group = "public"
		}
		st.events = append(st.events, negGroupEvent(fmt.Sprintf("%064x", i+1), int64(i+1), group))
	}
	srv, c := newNegGroupServer(t, st)
	filter := nostr.Filter{Kinds: []int{1}}
	items, _ := st.QueryEventSyncItems(t.Context(), filter)
	// A vanished event and stale timestamp must not survive the second lookup.
	items = append(items, storage.SyncItem{ID: strings.Repeat("f", 64), CreatedAt: 1})
	items[0].CreatedAt = 0
	got, err := srv.visibleNegSyncItems(t.Context(), c, filter, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 512 || st.maxBatch > 256 || st.metadataCalls != 2 {
		t.Fatalf("visible=%d, largest batch=%d, group lookups=%d", len(got), st.maxBatch, st.metadataCalls)
	}
	for i, item := range got {
		if item.CreatedAt != int64(2*i+3) {
			t.Fatalf("item %d is out of order: %v", i, item)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := srv.visibleNegSyncItems(ctx, c, filter, items); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled load returned IDs=%v, error=%v", got, err)
	}
	// Disabled NIP-29 keeps the original public reconciliation path.
	srv.cfg.NIPs.Enabled = []int{1, 11, 77}
	if got, err := srv.visibleNegSyncItems(t.Context(), c, filter, items); err != nil || !slices.Equal(got, items) {
		t.Fatalf("public reconciliation changed: %v", err)
	}
}
