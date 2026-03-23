package conduitl2

import (
	"encoding/base64"
	"encoding/json"
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

func TestSortProductsByPriceAndUpdatedAt(t *testing.T) {
	e1 := productEnvelope{Event: nostr.Event{ID: mustID(1), CreatedAt: 10}, UpdatedAt: 11, Price: 12, Currency: "USD", HasPrice: true}
	e2 := productEnvelope{Event: nostr.Event{ID: mustID(2), CreatedAt: 9}, UpdatedAt: 20, Price: 8, Currency: "USD", HasPrice: true}
	e3 := productEnvelope{Event: nostr.Event{ID: mustID(3), CreatedAt: 8}, UpdatedAt: 19, Price: 20, Currency: "USD", HasPrice: true}

	byPrice := sortProducts([]productEnvelope{e1, e2, e3}, SortPriceAsc, false, false)
	require.Equal(t, mustID(2), byPrice[0].Event.ID)
	require.Equal(t, mustID(1), byPrice[1].Event.ID)
	require.Equal(t, mustID(3), byPrice[2].Event.ID)

	byUpdated := sortProducts([]productEnvelope{e1, e2, e3}, SortUpdatedAtDesc, false, false)
	require.Equal(t, mustID(2), byUpdated[0].Event.ID)
	require.Equal(t, mustID(3), byUpdated[1].Event.ID)
	require.Equal(t, mustID(1), byUpdated[2].Event.ID)
}

func TestSortProductsMixedCurrencyPartial(t *testing.T) {
	e1 := productEnvelope{Event: nostr.Event{ID: mustID(1), CreatedAt: 10}, Price: 12, Currency: "USD", HasPrice: true}
	e2 := productEnvelope{Event: nostr.Event{ID: mustID(2), CreatedAt: 9}, Price: 8, Currency: "EUR", HasPrice: true}

	all := sortProducts([]productEnvelope{e1, e2}, SortPriceAsc, false, false)
	require.Len(t, all, 2)

	partial := sortProducts([]productEnvelope{e1, e2}, SortPriceAsc, false, true)
	require.Len(t, partial, 1)
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

func mustID(seed byte) nostr.ID {
	var id nostr.ID
	for i := 0; i < 32; i++ {
		id[i] = seed
	}
	return id
}
