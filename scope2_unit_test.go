package conduitl2

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/stretchr/testify/require"
)

func TestParseSearchPlan(t *testing.T) {
	plan, ok := parseSearchPlan("conduit-l2:q=apple;sort=price_asc;cursor=abc;partial=1")
	require.True(t, ok)
	require.Equal(t, "apple", plan.Text)
	require.Equal(t, SortPriceAsc, plan.Sort)
	require.Equal(t, "abc", plan.Cursor)
	require.True(t, plan.Partial)
}

func TestRequestsOutOfScopeSearch(t *testing.T) {
	require.False(t, requestsOutOfScopeSearch(nostr.Filter{}))
	require.True(t, requestsOutOfScopeSearch(nostr.Filter{Search: "banana"}))
	require.False(t, requestsOutOfScopeSearch(nostr.Filter{Search: "conduit-l2:q=banana"}))
}

func TestProjectionLatestProductWinsByAddress(t *testing.T) {
	state := newScope2State(nil)

	oldest := productEventFixture(1, 9, 1000, "apple", `{"title":"Apple","summary":"old","price":10,"currency":"USD","updatedAt":1000}`)
	newest := productEventFixture(2, 9, 1005, "apple", `{"title":"Apple","summary":"new","price":11,"currency":"USD","updatedAt":1005}`)
	olderTieBreaker := productEventFixture(3, 9, 1005, "apple", `{"title":"Apple","summary":"tie","price":12,"currency":"USD","updatedAt":1005}`)

	state.applyEvent(oldest)
	state.applyEvent(olderTieBreaker)
	state.applyEvent(newest)

	got := state.products[productAddress(oldest)]
	require.Equal(t, newest.ID, got.Event.ID)
	require.False(t, got.Tombstoned)
}

func TestProjectionDeleteByEventAndAddress(t *testing.T) {
	state := newScope2State(nil)

	byEvent := productEventFixture(1, 9, 1000, "apple", `{"title":"Apple","summary":"fresh","price":10,"currency":"USD","updatedAt":1000}`)
	state.applyEvent(byEvent)
	state.applyEvent(deleteEventFixture(4, 1001, []string{byEvent.ID.Hex()}, nil))
	require.True(t, state.products[productAddress(byEvent)].Tombstoned)

	byAddress := productEventFixture(2, 9, 1002, "banana", `{"title":"Banana","summary":"yellow","price":7,"currency":"USD","updatedAt":1002}`)
	state.applyEvent(byAddress)

	address := fmt.Sprintf("%d:%s:%s", byAddress.Kind, byAddress.PubKey.Hex(), byAddress.Tags.GetD())
	state.applyEvent(deleteEventFixture(5, 1003, nil, []string{address}))
	require.True(t, state.products[productAddress(byAddress)].Tombstoned)

	revived := productEventFixture(3, 9, 1004, "banana", `{"title":"Banana","summary":"back","price":8,"currency":"USD","updatedAt":1004}`)
	state.applyEvent(revived)
	require.False(t, state.products[productAddress(revived)].Tombstoned)
	require.Equal(t, revived.ID, state.products[productAddress(revived)].Event.ID)
}

func TestEvaluatePriceSortUsesUpdatedAtTieBreakers(t *testing.T) {
	state := newScope2State(nil)

	e1 := productEventFixture(1, 1, 1000, "a", `{"title":"A","summary":"x","price":9,"currency":"USD","updatedAt":1002}`)
	e2 := productEventFixture(2, 2, 1001, "b", `{"title":"B","summary":"y","price":9,"currency":"USD","updatedAt":1005}`)
	e3 := productEventFixture(3, 3, 1001, "c", `{"title":"C","summary":"z","price":9,"currency":"USD","updatedAt":1005}`)

	state.applyEvent(e1)
	state.applyEvent(e2)
	state.applyEvent(e3)

	got, reason := state.evaluate(nostr.Filter{Kinds: []nostr.Kind{30402}, Limit: 10}, searchPlan{Sort: SortPriceAsc}, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 10})
	require.Empty(t, reason)
	require.Equal(t, []nostr.ID{e2.ID, e3.ID, e1.ID}, idsOf(got))
}

func TestEvaluateRejectsMixedCurrencyWithoutPartial(t *testing.T) {
	state := newScope2State(nil)

	state.applyEvent(productEventFixture(1, 1, 1000, "usd", `{"title":"USD","summary":"x","price":9,"currency":"USD","updatedAt":1000}`))
	state.applyEvent(productEventFixture(2, 2, 1001, "eur", `{"title":"EUR","summary":"y","price":8,"currency":"EUR","updatedAt":1001}`))

	got, reason := state.evaluate(nostr.Filter{Kinds: []nostr.Kind{30402}, Limit: 10}, searchPlan{Sort: SortPriceAsc}, Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 10})
	require.Nil(t, got)
	require.Equal(t, "blocked: mixed-currency price sort requires partial=1 or trusted normalization", reason)
}

func TestEvaluateMixedCurrencyPartialKeepsDeterministicLargestCohort(t *testing.T) {
	state := newScope2State(nil)

	eur1 := productEventFixture(1, 1, 1000, "eur-1", `{"title":"EUR1","summary":"x","price":11,"currency":"EUR","updatedAt":1000}`)
	eur2 := productEventFixture(2, 2, 1002, "eur-2", `{"title":"EUR2","summary":"x","price":7,"currency":"EUR","updatedAt":1002}`)
	usd1 := productEventFixture(3, 3, 1001, "usd-1", `{"title":"USD1","summary":"x","price":6,"currency":"USD","updatedAt":1001}`)
	usd2 := productEventFixture(4, 4, 1003, "usd-2", `{"title":"USD2","summary":"x","price":8,"currency":"USD","updatedAt":1003}`)

	state.applyEvent(eur1)
	state.applyEvent(eur2)
	state.applyEvent(usd1)
	state.applyEvent(usd2)

	filter := nostr.Filter{Kinds: []nostr.Kind{30402}, Limit: 10}
	plan := searchPlan{Sort: SortPriceAsc, Partial: true}
	opts := Scope2Options{MaxQueryLimit: 10, DefaultQueryLimit: 10}

	first, reason := state.evaluate(filter, plan, opts)
	require.Empty(t, reason)
	second, reason := state.evaluate(filter, plan, opts)
	require.Empty(t, reason)

	require.Equal(t, []nostr.ID{eur2.ID, eur1.ID}, idsOf(first))
	require.Equal(t, idsOf(first), idsOf(second))
}

func TestCursorRoundTrip(t *testing.T) {
	products := []productEnvelope{
		{Event: nostr.Event{ID: mustID(1), CreatedAt: 10}},
		{Event: nostr.Event{ID: mustID(2), CreatedAt: 9}},
		{Event: nostr.Event{ID: mustID(3), CreatedAt: 8}},
	}

	b, err := json.Marshal(cursorState{ID: products[1].Event.ID.Hex()})
	require.NoError(t, err)
	cursor := base64.RawURLEncoding.EncodeToString(b)

	idx := findCursorStart(products, cursor)
	require.Equal(t, 2, idx)
}

func idsOf(products []productEnvelope) []nostr.ID {
	ids := make([]nostr.ID, 0, len(products))
	for _, product := range products {
		ids = append(ids, product.Event.ID)
	}
	return ids
}

func productEventFixture(idSeed byte, authorSeed byte, createdAt int64, dTag, content string) nostr.Event {
	return nostr.Event{
		ID:        mustID(idSeed),
		PubKey:    mustPubKey(authorSeed),
		CreatedAt: nostr.Timestamp(createdAt),
		Kind:      30402,
		Tags:      nostr.Tags{{"d", dTag}},
		Content:   content,
	}
}

func deleteEventFixture(seed byte, createdAt int64, eventIDs []string, addresses []string) nostr.Event {
	tags := make(nostr.Tags, 0, len(eventIDs)+len(addresses))
	for _, id := range eventIDs {
		tags = append(tags, nostr.Tag{"e", id})
	}
	for _, address := range addresses {
		tags = append(tags, nostr.Tag{"a", address})
	}

	return nostr.Event{
		ID:        mustID(seed),
		PubKey:    mustPubKey(seed),
		CreatedAt: nostr.Timestamp(createdAt),
		Kind:      5,
		Tags:      tags,
	}
}

func mustID(seed byte) nostr.ID {
	var id nostr.ID
	for i := 0; i < 32; i++ {
		id[i] = seed
	}
	return id
}

func mustPubKey(seed byte) nostr.PubKey {
	var pubkey nostr.PubKey
	for i := 0; i < 32; i++ {
		pubkey[i] = seed
	}
	return pubkey
}
