package conduitl2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	khatru "conduitl2/third_party/khatru"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"github.com/stretchr/testify/require"
)

func TestScope2E2E_ProductBrowseSortAndCursor(t *testing.T) {
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 500)

	ConfigureRelay(relay, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 2, MaxProjectionScan: 200})
	base := relay.QueryStored
	relay.QueryStored = WrapProductQueries(base, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 2, MaxProjectionScan: 200})

	srv := httptest.NewServer(relay)
	defer srv.Close()

	url := "ws" + srv.URL[4:]
	client, err := nostr.RelayConnect(t.Context(), url, nostr.RelayOptions{})
	require.NoError(t, err)
	defer client.Close()

	sk := nostr.Generate()
	e1 := productEvent(t, sk, 1000, "p1", `{"title":"Apple","summary":"Red","price":19,"currency":"USD","updatedAt":1000}`)
	e2 := productEvent(t, sk, 1001, "p2", `{"title":"Banana","summary":"Yellow","price":7,"currency":"USD","updatedAt":1001}`)
	e3 := productEvent(t, sk, 1002, "p3", `{"title":"Carrot","summary":"Orange","price":12,"currency":"USD","updatedAt":1002}`)

	ctxPub, cancelPub := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelPub()
	require.NoError(t, client.Publish(ctxPub, e1))
	require.NoError(t, client.Publish(ctxPub, e2))
	require.NoError(t, client.Publish(ctxPub, e3))

	ctxSub, cancelSub := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelSub()

	first, err := client.Subscribe(ctxSub, nostr.Filter{
		Kinds:  []nostr.Kind{30402},
		Limit:  2,
		Search: "conduit-l2:q=;sort=price_asc",
	}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer first.Unsub()

	page1 := collectUntilEOSE(t, first, 5*time.Second)
	require.Len(t, page1, 2)
	require.Equal(t, e2.ID, page1[0].ID)
	require.Equal(t, e3.ID, page1[1].ID)

	cursor := BuildNextCursor(page1[1])

	second, err := client.Subscribe(ctxSub, nostr.Filter{
		Kinds:  []nostr.Kind{30402},
		Limit:  2,
		Search: "conduit-l2:q=;sort=price_asc;cursor=" + cursor,
	}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer second.Unsub()

	page2 := collectUntilEOSE(t, second, 5*time.Second)
	require.Len(t, page2, 1)
	require.Equal(t, e1.ID, page2[0].ID)
}

func TestScope2E2E_ProductBrowseOmitsDeletedLatestRevision(t *testing.T) {
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 500)

	ConfigureRelay(relay, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 10, MaxProjectionScan: 200})
	base := relay.QueryStored
	relay.QueryStored = WrapProductQueries(base, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 10, MaxProjectionScan: 200})

	srv := httptest.NewServer(relay)
	defer srv.Close()

	url := "ws" + srv.URL[4:]
	client, err := nostr.RelayConnect(t.Context(), url, nostr.RelayOptions{})
	require.NoError(t, err)
	defer client.Close()

	sk := nostr.Generate()
	product := productEvent(t, sk, 1000, "apple", `{"title":"Apple","summary":"Fresh","price":19,"currency":"USD","updatedAt":1000}`)
	deleteEvt := deleteEventForProduct(t, sk, 1001, product)

	ctxPub, cancelPub := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelPub()
	require.NoError(t, client.Publish(ctxPub, product))
	require.NoError(t, client.Publish(ctxPub, deleteEvt))

	ctxSub, cancelSub := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelSub()

	sub, err := client.Subscribe(ctxSub, nostr.Filter{
		Kinds:  []nostr.Kind{30402},
		Limit:  10,
		Search: "conduit-l2:q=apple;sort=newest",
	}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer sub.Unsub()

	page := collectUntilEOSE(t, sub, 5*time.Second)
	require.Empty(t, page)
}

func TestScope2E2E_ProductBrowseSearchesAndSortsTagOnlyProducts(t *testing.T) {
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 500)

	ConfigureRelay(relay, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 10, MaxProjectionScan: 200})
	base := relay.QueryStored
	relay.QueryStored = WrapProductQueries(base, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 10, MaxProjectionScan: 200})

	srv := httptest.NewServer(relay)
	defer srv.Close()

	url := "ws" + srv.URL[4:]
	client, err := nostr.RelayConnect(t.Context(), url, nostr.RelayOptions{})
	require.NoError(t, err)
	defer client.Close()

	sk := nostr.Generate()
	apple := productEventWithTags(t, sk, 1000, "apple", "", nostr.Tags{{"d", "apple"}, {"title", "Apple Jam"}, {"summary", "Spiced pantry staple"}, {"price", "19", "USD"}, {"published_at", "1000"}})
	banana := productEventWithTags(t, sk, 1001, "banana", "", nostr.Tags{{"d", "banana"}, {"title", "Banana Bread"}, {"summary", "Bakery loaf"}, {"price", "7", "USD"}, {"published_at", "1001"}})
	carrot := productEventWithTags(t, sk, 1002, "carrot", "", nostr.Tags{{"d", "carrot"}, {"title", "Carrot Cake"}, {"summary", "Dessert slice"}, {"price", "12", "USD"}, {"published_at", "1002"}})

	ctxPub, cancelPub := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelPub()
	require.NoError(t, client.Publish(ctxPub, apple))
	require.NoError(t, client.Publish(ctxPub, banana))
	require.NoError(t, client.Publish(ctxPub, carrot))

	ctxSub, cancelSub := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelSub()

	sub, err := client.Subscribe(ctxSub, nostr.Filter{
		Kinds:  []nostr.Kind{30402},
		Limit:  10,
		Search: "conduit-l2:q=cake;sort=price_asc",
	}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer sub.Unsub()

	page := collectUntilEOSE(t, sub, 5*time.Second)
	require.Len(t, page, 1)
	require.Equal(t, carrot.ID, page[0].ID)

	subAll, err := client.Subscribe(ctxSub, nostr.Filter{
		Kinds:  []nostr.Kind{30402},
		Limit:  10,
		Search: "conduit-l2:q=;sort=price_asc",
	}, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer subAll.Unsub()

	pageAll := collectUntilEOSE(t, subAll, 5*time.Second)
	require.Equal(t, []nostr.ID{banana.ID, carrot.ID, apple.ID}, eventIDs(pageAll))
}

func TestScope2E2E_ProtectedKindRequiresRecipientScopedNIP42AndNIP11Advertises(t *testing.T) {
	relay := khatru.NewRelay()
	store := &slicestore.SliceStore{}
	require.NoError(t, store.Init())
	relay.UseEventstore(store, 200)
	ConfigureRelay(relay, Scope2Options{})

	srv := httptest.NewServer(relay)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/nostr+json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var info map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	tagsAny, has := info["tags"]
	require.True(t, has)
	tagsRaw, ok := tagsAny.([]any)
	require.True(t, ok)

	containsConduitL2 := false
	containsProductDetail := false
	containsProfileLookup := false
	for _, tag := range tagsRaw {
		tagStr, ok := tag.(string)
		if !ok {
			continue
		}
		switch tagStr {
		case "conduit_l2":
			containsConduitL2 = true
		case "product_detail_resolution":
			containsProductDetail = true
		case "profile_decoration_lookup":
			containsProfileLookup = true
		}
	}
	require.True(t, containsConduitL2)
	require.False(t, containsProductDetail)
	require.False(t, containsProfileLookup)

	url := "ws" + srv.URL[4:]
	unauth, err := nostr.RelayConnect(t.Context(), url, nostr.RelayOptions{})
	require.NoError(t, err)
	defer unauth.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	recipientA := nostr.Generate()
	recipientB := nostr.Generate()
	filterA := nostr.Filter{Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": []string{recipientA.Public().Hex()}}}

	blockedSub, err := unauth.Subscribe(ctx, filterA, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer blockedSub.Unsub()

	select {
	case reason := <-blockedSub.ClosedReason:
		require.Contains(t, reason, "auth-required")
	case <-ctx.Done():
		t.Fatal("expected CLOSED auth-required")
	}

	authd, err := nostr.RelayConnect(t.Context(), url, nostr.RelayOptions{})
	require.NoError(t, err)
	defer authd.Close()

	firstTry, err := authd.Subscribe(ctx, filterA, nostr.SubscriptionOptions{})
	require.NoError(t, err)
	defer firstTry.Unsub()

	select {
	case reason := <-firstTry.ClosedReason:
		require.Contains(t, reason, "auth-required")
	case <-ctx.Done():
		t.Fatal("expected initial auth-required CLOSED before auth retry")
	}
	require.NoError(t, authd.Auth(ctx, func(_ context.Context, evt *nostr.Event) error {
		return evt.Sign(recipientA)
	}))

	var authedOK bool
	for range 8 {
		okSub, err := authd.Subscribe(ctx, filterA, nostr.SubscriptionOptions{})
		require.NoError(t, err)

		select {
		case <-okSub.EndOfStoredEvents:
			authedOK = true
			okSub.Unsub()
			goto doneAuth
		case reason := <-okSub.ClosedReason:
			okSub.Unsub()
			if reason == "auth-required: kind 1059 requests require NIP-42 authentication" {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			t.Fatalf("unexpected CLOSED after auth: %s", reason)
		case <-ctx.Done():
			okSub.Unsub()
			t.Fatal("expected authenticated EOSE after retry")
		}
	}

doneAuth:
	require.True(t, authedOK)

	for name, filter := range map[string]nostr.Filter{
		"unfiltered":          {Kinds: []nostr.Kind{1059}},
		"mismatched":          {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": []string{recipientB.Public().Hex()}}},
		"missing recipient":   {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{}},
		"empty recipient":     {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": []string{}}},
		"invalid recipient":   {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": []string{"not-a-pubkey"}}},
		"multiple recipients": {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": []string{recipientA.Public().Hex(), recipientB.Public().Hex()}}},
		"duplicate recipient": {Kinds: []nostr.Kind{1059}, Tags: nostr.TagMap{"p": []string{recipientA.Public().Hex(), recipientA.Public().Hex()}}},
		"mixed kinds":         {Kinds: []nostr.Kind{1059, 1}, Tags: nostr.TagMap{"p": []string{recipientA.Public().Hex()}}},
	} {
		t.Run(name, func(t *testing.T) {
			sub, err := authd.Subscribe(ctx, filter, nostr.SubscriptionOptions{})
			require.NoError(t, err)
			defer sub.Unsub()

			select {
			case reason := <-sub.ClosedReason:
				require.NotContains(t, reason, recipientA.Public().Hex())
				require.NotContains(t, reason, recipientB.Public().Hex())
			case <-sub.EndOfStoredEvents:
				t.Fatal("expected recipient-invalid subscription to be rejected")
			case <-ctx.Done():
				t.Fatal("expected recipient-invalid subscription to be rejected")
			}
		})
	}
}

func productEvent(t *testing.T, sk nostr.SecretKey, createdAt int64, dTag, content string) nostr.Event {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Timestamp(createdAt), Kind: 30402, Tags: nostr.Tags{{"d", dTag}}, Content: content}
	require.NoError(t, evt.Sign(sk))
	return evt
}

func productEventWithTags(t *testing.T, sk nostr.SecretKey, createdAt int64, dTag, content string, tags nostr.Tags) nostr.Event {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Timestamp(createdAt), Kind: 30402, Tags: tags, Content: content}
	require.NoError(t, evt.Sign(sk))
	return evt
}

func deleteEventForProduct(t *testing.T, sk nostr.SecretKey, createdAt int64, product nostr.Event) nostr.Event {
	t.Helper()
	address := fmt.Sprintf("%d:%s:%s", product.Kind, product.PubKey.Hex(), product.Tags.GetD())
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(createdAt),
		Kind:      5,
		Tags:      nostr.Tags{{"e", product.ID.Hex()}, {"a", address}},
	}
	require.NoError(t, evt.Sign(sk))
	return evt
}

func collectUntilEOSE(t *testing.T, sub *nostr.Subscription, timeout time.Duration) []nostr.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	items := make([]nostr.Event, 0, 8)
	for {
		select {
		case evt := <-sub.Events:
			items = append(items, evt)
		case <-sub.EndOfStoredEvents:
			return items
		case reason := <-sub.ClosedReason:
			t.Fatalf("subscription closed unexpectedly: %s", reason)
		case <-ctx.Done():
			t.Fatal("timed out waiting for EOSE")
		}
	}
}

func eventIDs(events []nostr.Event) []nostr.ID {
	ids := make([]nostr.ID, 0, len(events))
	for _, evt := range events {
		ids = append(ids, evt.ID)
	}
	return ids
}
