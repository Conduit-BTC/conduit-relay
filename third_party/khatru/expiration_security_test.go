package khatru

import (
	"context"
	"iter"
	"strconv"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/stretchr/testify/require"
)

func TestExpirationManagerUsesNarrowInternalContext(t *testing.T) {
	secret := nostr.Generate()
	expired := nostr.Event{
		CreatedAt: nostr.Now() - 2,
		Kind:      1059,
		Tags: nostr.Tags{
			{"p", nostr.Generate().Public().Hex()},
			{"expiration", strconv.FormatInt(int64(nostr.Now()-1), 10)},
		},
	}
	require.NoError(t, expired.Sign(secret))

	queryCalls := 0
	deleted := false
	callbackCalls := 0
	manager := expirationManager{
		events: make(expiringEventHeap, 0),
		queryStored: func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
			require.True(t, IsInternalCall(ctx))
			queryCalls++
			return func(yield func(nostr.Event) bool) {
				yield(expired)
			}
		},
		deleteEvent: func(ctx context.Context, id nostr.ID) error {
			require.True(t, IsInternalCall(ctx))
			require.Equal(t, expired.ID, id)
			deleted = true
			return nil
		},
		deleteCallback: func(ctx context.Context, event nostr.Event) {
			require.True(t, IsInternalCall(ctx))
			require.Equal(t, expired.ID, event.ID)
			callbackCalls++
		},
	}

	manager.initialScan(context.Background())
	require.Len(t, manager.events, 1)
	manager.checkExpiredEvents(context.Background())

	require.True(t, deleted)
	require.Equal(t, 2, queryCalls)
	require.Equal(t, 1, callbackCalls)
	require.Empty(t, manager.events)
}
