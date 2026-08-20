package conduitl2

import (
	"context"
	"iter"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	khatru "conduitl2/third_party/khatru"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"github.com/stretchr/testify/require"
)

func TestGiftWrapStoredResultsFilterMaliciousBackend(t *testing.T) {
	recipientA := nostr.Generate()
	recipientB := nostr.Generate()
	a := signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", recipientA.Public().Hex()}})
	b := signedGiftWrap(t, nostr.Generate(), recipientB.Public(), nostr.Tags{{"p", recipientB.Public().Hex()}})
	multi := signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", recipientA.Public().Hex()}, {"p", recipientB.Public().Hex()}})
	missing := signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nil)
	public := signedTextEvent(t, nostr.Generate(), "public")

	relay := khatru.NewRelay()
	var queryCalls atomic.Int32
	relay.QueryStored = func(context.Context, nostr.Filter) iter.Seq[nostr.Event] {
		queryCalls.Add(1)
		return func(yield func(nostr.Event) bool) {
			for _, event := range []nostr.Event{a, b, multi, missing, public} {
				if !yield(event) {
					return
				}
			}
		}
	}
	connectionContexts := make(chan context.Context, 1)
	authEvents := make(chan nostr.PubKey, 4)
	relay.OnConnect = func(ctx context.Context) { connectionContexts <- ctx }
	relay.OnAuth = func(_ context.Context, pubkey nostr.PubKey) { authEvents <- pubkey }
	ConfigureRelay(relay, Scope2Options{GiftWrapProtection: GiftWrapProtectionEnforce})
	queryCalls.Store(0)

	server := httptest.NewServer(relay)
	defer server.Close()
	client := connectTestRelay(t, server)
	defer client.Close()
	connectionContext := <-connectionContexts
	authenticateGiftWrapClient(t, client, recipientA, authEvents)
	assertSubscriptionRejected(t, client, nostr.Filter{})
	require.Zero(t, queryCalls.Load())

	filterA := giftWrapFilter(recipientA.Public())
	require.Equal(t, []nostr.ID{a.ID}, eventIDs(collectSeqEvents(relay.QueryStored(connectionContext, filterA))))
	require.Equal(t, []nostr.ID{a.ID}, eventIDs(collectSeqEvents(relay.QueryStored(khatru.SetNegentropy(connectionContext), filterA))))
	require.Equal(t, []nostr.ID{public.ID}, eventIDs(collectSeqEvents(relay.QueryStored(context.Background(), nostr.Filter{}))))
	reject, reason := relay.OnRequest(khatru.SetNegentropy(connectionContext), filterA)
	require.True(t, reject)
	require.Contains(t, reason, "negentropy")
	reject, reason = relay.OnRequest(khatru.SetNegentropy(connectionContext), nostr.Filter{})
	require.True(t, reject)
	require.Contains(t, reason, "wildcard")
}

func TestDefaultGiftWrapProtectionEnforcesStoredAndLiveRecipientIsolation(t *testing.T) {
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 500)
	authEvents := make(chan nostr.PubKey, 8)
	relay.OnAuth = func(_ context.Context, pubkey nostr.PubKey) { authEvents <- pubkey }
	ConfigureRelay(relay, Scope2Options{})

	server := httptest.NewServer(relay)
	defer server.Close()
	recipientA := nostr.Generate()
	recipientB := nostr.Generate()
	clientA := connectTestRelay(t, server)
	defer clientA.Close()
	clientB := connectTestRelay(t, server)
	defer clientB.Close()
	publicClient := connectTestRelay(t, server)
	defer publicClient.Close()
	publisher := connectTestRelay(t, server)
	defer publisher.Close()
	authenticateGiftWrapClient(t, clientA, recipientA, authEvents)
	authenticateGiftWrapClient(t, clientB, recipientB, authEvents)
	assertSubscriptionRejected(t, publicClient, nostr.Filter{})

	storedA := signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", recipientA.Public().Hex()}})
	storedB := signedGiftWrap(t, nostr.Generate(), recipientB.Public(), nostr.Tags{{"p", recipientB.Public().Hex()}})
	storedPublic := signedTextEvent(t, nostr.Generate(), "stored public")
	publishEvent(t, publisher, storedA)
	publishEvent(t, publisher, storedB)
	publishEvent(t, publisher, storedPublic)

	subA := subscribeTestRelay(t, clientA, giftWrapFilter(recipientA.Public()))
	defer subA.Unsub()
	subB := subscribeTestRelay(t, clientB, giftWrapFilter(recipientB.Public()))
	defer subB.Unsub()
	publicSub := subscribeTestRelay(t, publicClient, nostr.Filter{Kinds: []nostr.Kind{1}})
	defer publicSub.Unsub()

	require.Equal(t, []nostr.ID{storedA.ID}, eventIDs(collectUntilEOSE(t, subA, 3*time.Second)))
	require.Equal(t, []nostr.ID{storedB.ID}, eventIDs(collectUntilEOSE(t, subB, 3*time.Second)))
	require.Equal(t, []nostr.ID{storedPublic.ID}, eventIDs(collectUntilEOSE(t, publicSub, 3*time.Second)))

	liveA := signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", recipientA.Public().Hex()}})
	liveB := signedGiftWrap(t, nostr.Generate(), recipientB.Public(), nostr.Tags{{"p", recipientB.Public().Hex()}})
	livePublic := signedTextEvent(t, nostr.Generate(), "live public")
	publishEvent(t, publisher, liveA)
	publishEvent(t, publisher, liveB)
	publishEvent(t, publisher, livePublic)
	require.Equal(t, liveA.ID, receiveLiveEvent(t, subA).ID)
	require.Equal(t, liveB.ID, receiveLiveEvent(t, subB).ID)
	require.Equal(t, livePublic.ID, receiveLiveEvent(t, publicSub).ID)

	legacyMalformed := []nostr.Event{
		signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nil),
		signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p"}}),
		signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", "invalid"}}),
		signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", recipientA.Public().Hex()}, {"p", recipientB.Public().Hex()}}),
	}
	for _, event := range legacyMalformed {
		require.Zero(t, relay.BroadcastEvent(event))
		require.Zero(t, relay.ForceBroadcastEvent(event))
	}
	requireNoLiveEvent(t, subA, 100*time.Millisecond)
	requireNoLiveEvent(t, subB, 100*time.Millisecond)
	requireNoLiveEvent(t, publicSub, 100*time.Millisecond)
}

func TestGiftWrapCountsFailClosedAndPublicCountsWork(t *testing.T) {
	relay := khatru.NewRelay()
	var countCalls atomic.Int32
	relay.Count = func(context.Context, nostr.Filter) (uint32, error) {
		countCalls.Add(1)
		return 7, nil
	}
	authEvents := make(chan nostr.PubKey, 4)
	relay.OnAuth = func(_ context.Context, pubkey nostr.PubKey) { authEvents <- pubkey }
	ConfigureRelay(relay, Scope2Options{GiftWrapProtection: GiftWrapProtectionEnforce})

	server := httptest.NewServer(relay)
	defer server.Close()
	client := connectTestRelay(t, server)
	defer client.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	count, _, err := client.Count(ctx, nostr.Filter{Kinds: []nostr.Kind{1}}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	require.Equal(t, uint32(7), count)
	require.Equal(t, int32(1), countCalls.Load())

	count, _, err = client.Count(ctx, nostr.Filter{}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, int32(1), countCalls.Load())

	recipientA := nostr.Generate()
	recipientB := nostr.Generate()
	count, _, err = client.Count(ctx, giftWrapFilter(recipientA.Public()), nostr.SubscriptionOptions{})
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, int32(1), countCalls.Load())

	authenticateGiftWrapClient(t, client, recipientA, authEvents)
	for _, filter := range []nostr.Filter{
		giftWrapFilter(recipientA.Public()),
		giftWrapFilter(recipientB.Public()),
		{Kinds: []nostr.Kind{giftWrapKind, 1}, Tags: nostr.TagMap{"p": {recipientA.Public().Hex()}}},
	} {
		count, _, err = client.Count(ctx, filter, nostr.SubscriptionOptions{})
		require.NoError(t, err)
		require.Zero(t, count)
	}
	require.Equal(t, int32(1), countCalls.Load())
}

func TestGiftWrapWritesRejectMalformedAndPreserveInternalDeletion(t *testing.T) {
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 500)
	ConfigureRelay(relay, Scope2Options{GiftWrapProtection: GiftWrapProtectionEnforce})

	server := httptest.NewServer(relay)
	defer server.Close()
	client := connectTestRelay(t, server)
	defer client.Close()
	author := nostr.Generate()
	recipientA := nostr.Generate().Public()
	recipientB := nostr.Generate().Public()

	invalid := []nostr.Event{
		signedGiftWrap(t, author, recipientA, nil),
		signedGiftWrap(t, author, recipientA, nostr.Tags{{"p"}}),
		signedGiftWrap(t, author, recipientA, nostr.Tags{{"p", "invalid"}}),
		signedGiftWrap(t, author, recipientA, nostr.Tags{{"p", recipientA.Hex()}, {"p", recipientA.Hex()}}),
		signedGiftWrap(t, author, recipientA, nostr.Tags{{"p", recipientA.Hex()}, {"p", recipientB.Hex()}}),
	}
	for _, event := range invalid {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		err := client.Publish(ctx, event)
		cancel()
		require.Error(t, err)
		require.Empty(t, collectSeqEvents(store.QueryEvents(nostr.Filter{IDs: []nostr.ID{event.ID}}, 1)))
	}

	valid := signedGiftWrap(t, author, recipientA, nostr.Tags{{"p", recipientA.Hex()}})
	publishEvent(t, client, valid)
	require.Equal(t, []nostr.ID{valid.ID}, eventIDs(collectSeqEvents(store.QueryEvents(nostr.Filter{IDs: []nostr.ID{valid.ID}}, 1))))

	outsider := nostr.Generate()
	protectedDeleteError := publishDeletionExpectingError(t, client, outsider, valid.ID)
	missingID := signedTextEvent(t, nostr.Generate(), "never stored").ID
	missingDeleteError := publishDeletionExpectingError(t, client, outsider, missingID)
	require.Equal(t, missingDeleteError, protectedDeleteError)
	require.Contains(t, protectedDeleteError, "nothing to delete")
	require.Equal(t, []nostr.ID{valid.ID}, eventIDs(collectSeqEvents(store.QueryEvents(nostr.Filter{IDs: []nostr.ID{valid.ID}}, 1))))

	deletion := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      nostr.KindDeletion,
		Tags:      nostr.Tags{{"e", valid.ID.Hex()}},
	}
	require.NoError(t, deletion.Sign(author))
	publishEvent(t, client, deletion)
	require.Empty(t, collectSeqEvents(store.QueryEvents(nostr.Filter{IDs: []nostr.ID{valid.ID}}, 1)))

	public := signedTextEvent(t, author, "public remains writable")
	publishEvent(t, client, public)
}

func TestGiftWrapAuthorizationFollowsCurrentIdentityAndReconnectLifecycle(t *testing.T) {
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 500)
	authEvents := make(chan nostr.PubKey, 8)
	connectionContexts := make(chan context.Context, 4)
	relay.OnAuth = func(_ context.Context, pubkey nostr.PubKey) { authEvents <- pubkey }
	relay.OnConnect = func(ctx context.Context) { connectionContexts <- ctx }
	ConfigureRelay(relay, Scope2Options{GiftWrapProtection: GiftWrapProtectionEnforce})

	server := httptest.NewServer(relay)
	defer server.Close()
	recipientA := nostr.Generate()
	recipientB := nostr.Generate()
	client := connectTestRelay(t, server)
	connectionContext := <-connectionContexts
	authenticateGiftWrapClient(t, client, recipientA, authEvents)
	subA := subscribeTestRelay(t, client, giftWrapFilter(recipientA.Public()))
	require.Empty(t, collectUntilEOSE(t, subA, 3*time.Second))

	sendReplacementAuth(t, client, connectionContext, recipientB, authEvents)
	relay.BroadcastEvent(signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", recipientA.Public().Hex()}}))
	requireNoLiveEvent(t, subA, 100*time.Millisecond)
	subA.Unsub()
	assertSubscriptionRejected(t, client, giftWrapFilter(recipientA.Public()))

	subB := subscribeTestRelay(t, client, giftWrapFilter(recipientB.Public()))
	require.Empty(t, collectUntilEOSE(t, subB, 3*time.Second))
	liveB := signedGiftWrap(t, nostr.Generate(), recipientB.Public(), nostr.Tags{{"p", recipientB.Public().Hex()}})
	relay.BroadcastEvent(liveB)
	require.Equal(t, liveB.ID, receiveLiveEvent(t, subB).ID)

	sendReplacementAuth(t, client, connectionContext, recipientA, authEvents)
	relay.BroadcastEvent(signedGiftWrap(t, nostr.Generate(), recipientB.Public(), nostr.Tags{{"p", recipientB.Public().Hex()}}))
	requireNoLiveEvent(t, subB, 100*time.Millisecond)
	subB.Unsub()
	assertSubscriptionRejected(t, client, giftWrapFilter(recipientB.Public()))

	currentASub := subscribeTestRelay(t, client, giftWrapFilter(recipientA.Public()))
	require.Empty(t, collectUntilEOSE(t, currentASub, 3*time.Second))
	liveA := signedGiftWrap(t, nostr.Generate(), recipientA.Public(), nostr.Tags{{"p", recipientA.Public().Hex()}})
	relay.BroadcastEvent(liveA)
	require.Equal(t, liveA.ID, receiveLiveEvent(t, currentASub).ID)
	currentASub.Unsub()
	require.NoError(t, client.Close())

	reopened := connectTestRelay(t, server)
	defer reopened.Close()
	<-connectionContexts
	authenticateGiftWrapClient(t, reopened, recipientB, authEvents)
	reopenedSubB := subscribeTestRelay(t, reopened, giftWrapFilter(recipientB.Public()))
	defer reopenedSubB.Unsub()
	require.Empty(t, collectUntilEOSE(t, reopenedSubB, 3*time.Second))
	reopenedLiveB := signedGiftWrap(t, nostr.Generate(), recipientB.Public(), nostr.Tags{{"p", recipientB.Public().Hex()}})
	relay.BroadcastEvent(reopenedLiveB)
	require.Equal(t, reopenedLiveB.ID, receiveLiveEvent(t, reopenedSubB).ID)
}

func connectTestRelay(t *testing.T, server *httptest.Server) *nostr.Relay {
	t.Helper()
	client, err := nostr.RelayConnect(t.Context(), "ws"+server.URL[4:], nostr.RelayOptions{})
	require.NoError(t, err)
	return client
}

func authenticateGiftWrapClient(t *testing.T, client *nostr.Relay, secret nostr.SecretKey, authEvents <-chan nostr.PubKey) {
	t.Helper()
	assertSubscriptionRejected(t, client, giftWrapFilter(secret.Public()))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, client.Auth(ctx, func(_ context.Context, event *nostr.Event) error {
		return event.Sign(secret)
	}))
	waitForAuthenticatedPubkey(t, authEvents, secret.Public())
}

func waitForAuthenticatedPubkey(t *testing.T, authEvents <-chan nostr.PubKey, want nostr.PubKey) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-authEvents:
			if got == want {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for NIP-42 authentication")
		}
	}
}

func assertSubscriptionRejected(t *testing.T, client *nostr.Relay, filter nostr.Filter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	sub, err := client.Subscribe(ctx, filter, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer sub.Unsub()
	select {
	case <-sub.ClosedReason:
	case <-sub.EndOfStoredEvents:
		t.Fatal("expected subscription rejection")
	case <-ctx.Done():
		t.Fatal("timed out waiting for subscription rejection")
	}
}

func subscribeTestRelay(t *testing.T, client *nostr.Relay, filter nostr.Filter) *nostr.Subscription {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	sub, err := client.Subscribe(ctx, filter, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	return sub
}

func publishEvent(t *testing.T, client *nostr.Relay, event nostr.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, client.Publish(ctx, event))
}

func publishDeletionExpectingError(t *testing.T, client *nostr.Relay, author nostr.SecretKey, target nostr.ID) string {
	t.Helper()
	event := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      nostr.KindDeletion,
		Tags:      nostr.Tags{{"e", target.Hex()}},
	}
	require.NoError(t, event.Sign(author))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := client.Publish(ctx, event)
	require.Error(t, err)
	return err.Error()
}

func receiveLiveEvent(t *testing.T, sub *nostr.Subscription) nostr.Event {
	t.Helper()
	select {
	case event := <-sub.Events:
		return event
	case reason := <-sub.ClosedReason:
		t.Fatalf("subscription closed unexpectedly: %s", reason)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for live event")
	}
	return nostr.Event{}
}

func requireNoLiveEvent(t *testing.T, sub *nostr.Subscription, duration time.Duration) {
	t.Helper()
	select {
	case event := <-sub.Events:
		t.Fatalf("unexpected live event %s", event.ID.Hex())
	case reason := <-sub.ClosedReason:
		t.Fatalf("subscription closed unexpectedly: %s", reason)
	case <-time.After(duration):
	}
}

func signedGiftWrap(t *testing.T, author nostr.SecretKey, recipient nostr.PubKey, tags nostr.Tags) nostr.Event {
	t.Helper()
	event := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      giftWrapKind,
		Tags:      tags,
		Content:   "encrypted for " + recipient.Hex()[:8],
	}
	require.NoError(t, event.Sign(author))
	return event
}

func signedTextEvent(t *testing.T, author nostr.SecretKey, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Content: content}
	require.NoError(t, event.Sign(author))
	return event
}

func sendReplacementAuth(
	t *testing.T,
	client *nostr.Relay,
	connectionContext context.Context,
	secret nostr.SecretKey,
	authEvents <-chan nostr.PubKey,
) {
	t.Helper()
	ws := khatru.GetConnection(connectionContext)
	require.NotNil(t, ws)
	authEvent := nostr.Event{
		CreatedAt: nostr.Now(),
		Kind:      nostr.KindClientAuthentication,
		Tags: nostr.Tags{
			{"relay", client.URL},
			{"challenge", ws.Challenge},
		},
	}
	require.NoError(t, authEvent.Sign(secret))
	payload, err := (nostr.AuthEnvelope{Event: authEvent}).MarshalJSON()
	require.NoError(t, err)
	require.NoError(t, client.WriteWithError(payload))
	waitForAuthenticatedPubkey(t, authEvents, secret.Public())
}
