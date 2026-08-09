package conduitl2

import (
	"strings"
	"testing"

	khatru "conduitl2/third_party/khatru"
	"fiatjaf.com/nostr"
	"github.com/stretchr/testify/require"
)

func TestAuthorizeGiftWrapFilterForIdentity(t *testing.T) {
	recipientA := nostr.Generate().Public()
	recipientB := nostr.Generate().Public()
	valid := giftWrapFilter(recipientA)

	tests := []struct {
		name     string
		filter   nostr.Filter
		identity nostr.PubKey
		authed   bool
		allowed  bool
	}{
		{name: "unauthenticated", filter: valid, authed: false},
		{name: "recipient matches identity", filter: valid, identity: recipientA, authed: true, allowed: true},
		{name: "recipient differs from identity", filter: valid, identity: recipientB, authed: true},
		{name: "missing p map", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}}, identity: recipientA, authed: true},
		{name: "nil p values", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": nil}}, identity: recipientA, authed: true},
		{name: "empty p values", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {}}}, identity: recipientA, authed: true},
		{name: "empty p value", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {""}}}, identity: recipientA, authed: true},
		{name: "short p value", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {"00"}}}, identity: recipientA, authed: true},
		{name: "long p value", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {strings.Repeat("0", 66)}}}, identity: recipientA, authed: true},
		{name: "nonhex p value", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {strings.Repeat("z", 64)}}}, identity: recipientA, authed: true},
		{name: "uppercase p value", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {strings.ToUpper(recipientA.Hex())}}}, identity: recipientA, authed: true},
		{name: "multiple recipients", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {recipientA.Hex(), recipientB.Hex()}}}, identity: recipientA, authed: true},
		{name: "duplicate recipient", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}, Tags: nostr.TagMap{"p": {recipientA.Hex(), recipientA.Hex()}}}, identity: recipientA, authed: true},
		{name: "mixed kinds", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind, 1}, Tags: nostr.TagMap{"p": {recipientA.Hex()}}}, identity: recipientA, authed: true},
		{name: "duplicate kind", filter: nostr.Filter{Kinds: []nostr.Kind{giftWrapKind, giftWrapKind}, Tags: nostr.TagMap{"p": {recipientA.Hex()}}}, identity: recipientA, authed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recipient, reason := authorizeGiftWrapFilterForIdentity(test.filter, test.identity, test.authed)
			if test.allowed {
				require.Empty(t, reason)
				require.Equal(t, recipientA, recipient)
				return
			}

			require.NotEmpty(t, reason)
			require.Equal(t, nostr.ZeroPK, recipient)
			require.NotContains(t, reason, recipientA.Hex())
			require.NotContains(t, reason, recipientB.Hex())
		})
	}
}

func TestSoleGiftWrapRecipient(t *testing.T) {
	recipientA := nostr.Generate().Public()
	recipientB := nostr.Generate().Public()

	tests := []struct {
		name    string
		tags    nostr.Tags
		allowed bool
	}{
		{name: "missing"},
		{name: "valueless", tags: nostr.Tags{{"p"}}},
		{name: "empty", tags: nostr.Tags{{"p", ""}}},
		{name: "invalid", tags: nostr.Tags{{"p", "invalid"}}},
		{name: "uppercase", tags: nostr.Tags{{"p", strings.ToUpper(recipientA.Hex())}}},
		{name: "one valid", tags: nostr.Tags{{"p", recipientA.Hex()}}, allowed: true},
		{name: "one valid with relay hint", tags: nostr.Tags{{"p", recipientA.Hex(), "wss://example.test"}}, allowed: true},
		{name: "duplicate", tags: nostr.Tags{{"p", recipientA.Hex()}, {"p", recipientA.Hex()}}},
		{name: "multiple", tags: nostr.Tags{{"p", recipientA.Hex()}, {"p", recipientB.Hex()}}},
		{name: "valid plus valueless", tags: nostr.Tags{{"p", recipientA.Hex()}, {"p"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recipient, ok := soleGiftWrapRecipient(nostr.Event{Kind: giftWrapKind, Tags: test.tags})
			require.Equal(t, test.allowed, ok)
			if test.allowed {
				require.Equal(t, recipientA, recipient)
			} else {
				require.Equal(t, nostr.ZeroPK, recipient)
			}
		})
	}
}

func TestMayDeliverGiftWrap(t *testing.T) {
	recipientA := nostr.Generate().Public()
	recipientB := nostr.Generate().Public()
	eventA := nostr.Event{Kind: giftWrapKind, Tags: nostr.Tags{{"p", recipientA.Hex()}}}
	eventB := nostr.Event{Kind: giftWrapKind, Tags: nostr.Tags{{"p", recipientB.Hex()}}}
	multi := nostr.Event{Kind: giftWrapKind, Tags: nostr.Tags{{"p", recipientA.Hex()}, {"p", recipientB.Hex()}}}

	require.True(t, mayDeliverGiftWrap(giftWrapFilter(recipientA), recipientA, true, eventA))
	require.False(t, mayDeliverGiftWrap(giftWrapFilter(recipientA), recipientA, false, eventA))
	require.False(t, mayDeliverGiftWrap(giftWrapFilter(recipientA), recipientB, true, eventA))
	require.False(t, mayDeliverGiftWrap(giftWrapFilter(recipientB), recipientA, true, eventA))
	require.False(t, mayDeliverGiftWrap(nostr.Filter{}, recipientA, true, eventA))
	require.False(t, mayDeliverGiftWrap(giftWrapFilter(recipientA), recipientA, true, eventB))
	require.False(t, mayDeliverGiftWrap(giftWrapFilter(recipientA), recipientA, true, multi))
	require.False(t, mayDeliverGiftWrap(giftWrapFilter(recipientA), recipientA, true, nostr.Event{Kind: giftWrapKind}))
}

func TestGiftWrapAuthRegistryTracksCurrentConnectedIdentity(t *testing.T) {
	registry := &giftWrapAuthRegistry{identities: make(map[*khatru.WebSocket]giftWrapConnectionIdentity)}
	ws := &khatru.WebSocket{}
	recipientA := nostr.Generate().Public()
	recipientB := nostr.Generate().Public()

	_, ok := registry.current(ws)
	require.False(t, ok)

	registry.connect(ws)
	_, ok = registry.current(ws)
	require.False(t, ok)

	registry.record(ws, recipientA)
	got, ok := registry.current(ws)
	require.True(t, ok)
	require.Equal(t, recipientA, got)

	registry.record(ws, recipientA)
	got, ok = registry.current(ws)
	require.True(t, ok)
	require.Equal(t, recipientA, got)

	registry.record(ws, recipientB)
	got, ok = registry.current(ws)
	require.True(t, ok)
	require.Equal(t, recipientB, got)

	registry.record(ws, recipientA)
	got, ok = registry.current(ws)
	require.True(t, ok)
	require.Equal(t, recipientA, got)

	registry.remove(ws)
	_, ok = registry.current(ws)
	require.False(t, ok)

	registry.record(ws, recipientB)
	_, ok = registry.current(ws)
	require.False(t, ok)
}

func giftWrapFilter(recipient nostr.PubKey) nostr.Filter {
	return nostr.Filter{
		Kinds: []nostr.Kind{giftWrapKind},
		Tags:  nostr.TagMap{"p": {recipient.Hex()}},
	}
}
