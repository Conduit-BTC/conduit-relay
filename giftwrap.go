package conduitl2

import (
	"context"
	"iter"
	"sync"

	khatru "conduitl2/third_party/khatru"
	"fiatjaf.com/nostr"
)

type giftWrapConnectionIdentity struct {
	pubkey        nostr.PubKey
	authenticated bool
}

type giftWrapAuthRegistry struct {
	mu         sync.RWMutex
	identities map[*khatru.WebSocket]giftWrapConnectionIdentity
}

const (
	giftWrapKind = nostr.Kind(1059)

	giftWrapAuthRequiredReason  = "auth-required: kind 1059 requests require NIP-42 authentication"
	giftWrapMixedKindsReason    = "blocked: kind 1059 cannot be mixed with other kinds"
	giftWrapRecipientReason     = "blocked: kind 1059 requests require exactly one valid recipient"
	giftWrapIdentityReason      = "blocked: kind 1059 recipient does not match authenticated identity"
	giftWrapCountReason         = "blocked: protected event counts are not supported"
	giftWrapWildcardReadReason  = "blocked: wildcard subscriptions are not supported"
	giftWrapWildcardCountReason = "blocked: wildcard counts are not supported"
	giftWrapWriteReason         = "blocked: kind 1059 events require exactly one valid recipient"
)

func configureGiftWrapProtection(relay *khatru.Relay) {
	authRegistry := &giftWrapAuthRegistry{identities: make(map[*khatru.WebSocket]giftWrapConnectionIdentity)}

	prevConnect := relay.OnConnect
	relay.OnConnect = func(ctx context.Context) {
		if ws := khatru.GetConnection(ctx); ws != nil {
			authRegistry.connect(ws)
		}
		if prevConnect != nil {
			prevConnect(ctx)
		}
	}

	prevAuth := relay.OnAuth
	relay.OnAuth = func(ctx context.Context, pubkey nostr.PubKey) {
		if ws := khatru.GetConnection(ctx); ws != nil {
			authRegistry.record(ws, pubkey)
		}
		if prevAuth != nil {
			prevAuth(ctx, pubkey)
		}
	}

	prevDisconnect := relay.OnDisconnect
	relay.OnDisconnect = func(ctx context.Context) {
		if ws := khatru.GetConnection(ctx); ws != nil {
			authRegistry.remove(ws)
		}
		if prevDisconnect != nil {
			prevDisconnect(ctx)
		}
	}

	prevRequest := relay.OnRequest
	relay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if len(filter.Kinds) == 0 {
			return true, giftWrapWildcardReadReason
		}
		if requestsProtectedGiftWraps(filter) {
			if khatru.IsNegentropySession(ctx) {
				return true, "blocked: protected event negentropy is not supported"
			}
			if _, reason := authorizeGiftWrapFilter(ctx, filter, authRegistry); reason != "" {
				return true, reason
			}
		}

		if prevRequest != nil {
			return prevRequest(ctx, filter)
		}
		return false, ""
	}

	prevQuery := relay.QueryStored
	if prevQuery != nil {
		relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
			results := prevQuery(ctx, filter)
			if khatru.IsInternalCall(ctx) {
				return results
			}

			return func(yield func(nostr.Event) bool) {
				for event := range results {
					if !filter.Matches(event) {
						continue
					}
					if event.Kind == giftWrapKind && !mayDeliverStoredGiftWrap(ctx, filter, authRegistry, event) {
						continue
					}
					if !yield(event) {
						return
					}
				}
			}
		}
	}

	prevCount := relay.OnCount
	relay.OnCount = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if !khatru.IsInternalCall(ctx) {
			if len(filter.Kinds) == 0 {
				return true, giftWrapWildcardCountReason
			}
			if requestsProtectedGiftWraps(filter) {
				if _, reason := authorizeGiftWrapFilter(ctx, filter, authRegistry); reason != "" {
					return true, reason
				}
				return true, giftWrapCountReason
			}
		}

		if prevCount != nil {
			return prevCount(ctx, filter)
		}
		return false, ""
	}

	prevEvent := relay.OnEvent
	relay.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if event.Kind == giftWrapKind {
			if _, ok := soleGiftWrapRecipient(event); !ok {
				return true, giftWrapWriteReason
			}
		}

		if prevEvent != nil {
			return prevEvent(ctx, event)
		}
		return false, ""
	}

	prevPreventBroadcast := relay.PreventBroadcast
	relay.PreventBroadcast = func(ws *khatru.WebSocket, filter nostr.Filter, event nostr.Event) bool {
		if prevPreventBroadcast != nil && prevPreventBroadcast(ws, filter, event) {
			return true
		}
		if event.Kind != giftWrapKind {
			return false
		}

		identity, authed := authRegistry.current(ws)
		return !mayDeliverGiftWrap(filter, identity, authed, event)
	}
}

func authorizeGiftWrapFilter(ctx context.Context, filter nostr.Filter, authRegistry *giftWrapAuthRegistry) (nostr.PubKey, string) {
	identity, authed := authRegistry.current(khatru.GetConnection(ctx))
	return authorizeGiftWrapFilterForIdentity(filter, identity, authed)
}

func authorizeGiftWrapFilterForIdentity(filter nostr.Filter, identity nostr.PubKey, authed bool) (nostr.PubKey, string) {
	if !authed {
		return nostr.ZeroPK, giftWrapAuthRequiredReason
	}
	if len(filter.Kinds) != 1 || filter.Kinds[0] != giftWrapKind {
		return nostr.ZeroPK, giftWrapMixedKindsReason
	}

	recipients := filter.Tags["p"]
	if len(recipients) != 1 {
		return nostr.ZeroPK, giftWrapRecipientReason
	}
	recipient, ok := parseCanonicalPubKey(recipients[0])
	if !ok {
		return nostr.ZeroPK, giftWrapRecipientReason
	}
	if recipient != identity {
		return nostr.ZeroPK, giftWrapIdentityReason
	}
	return recipient, ""
}

func mayDeliverStoredGiftWrap(ctx context.Context, filter nostr.Filter, authRegistry *giftWrapAuthRegistry, event nostr.Event) bool {
	identity, authed := authRegistry.current(khatru.GetConnection(ctx))
	return mayDeliverGiftWrap(filter, identity, authed, event)
}

func mayDeliverGiftWrap(filter nostr.Filter, identity nostr.PubKey, authed bool, event nostr.Event) bool {
	recipient, reason := authorizeGiftWrapFilterForIdentity(filter, identity, authed)
	if reason != "" || !filter.Matches(event) {
		return false
	}
	eventRecipient, ok := soleGiftWrapRecipient(event)
	return ok && eventRecipient == recipient
}

func soleGiftWrapRecipient(event nostr.Event) (nostr.PubKey, bool) {
	var raw string
	count := 0
	for _, tag := range event.Tags {
		if len(tag) == 0 || tag[0] != "p" {
			continue
		}
		count++
		if len(tag) < 2 {
			return nostr.ZeroPK, false
		}
		raw = tag[1]
	}
	if count != 1 {
		return nostr.ZeroPK, false
	}

	return parseCanonicalPubKey(raw)
}

func parseCanonicalPubKey(raw string) (nostr.PubKey, bool) {
	pubkey, err := nostr.PubKeyFromHex(raw)
	if err != nil || raw != pubkey.Hex() {
		return nostr.ZeroPK, false
	}
	return pubkey, true
}

func (registry *giftWrapAuthRegistry) record(ws *khatru.WebSocket, pubkey nostr.PubKey) {
	registry.mu.Lock()
	defer registry.mu.Unlock()

	_, ok := registry.identities[ws]
	if !ok {
		return
	}
	registry.identities[ws] = giftWrapConnectionIdentity{pubkey: pubkey, authenticated: true}
}

func (registry *giftWrapAuthRegistry) current(ws *khatru.WebSocket) (nostr.PubKey, bool) {
	if ws == nil {
		return nostr.ZeroPK, false
	}

	registry.mu.RLock()
	current, ok := registry.identities[ws]
	registry.mu.RUnlock()
	if !ok || !current.authenticated {
		return nostr.ZeroPK, false
	}
	return current.pubkey, true
}

func (registry *giftWrapAuthRegistry) connect(ws *khatru.WebSocket) {
	registry.mu.Lock()
	registry.identities[ws] = giftWrapConnectionIdentity{}
	registry.mu.Unlock()
}

func (registry *giftWrapAuthRegistry) remove(ws *khatru.WebSocket) {
	registry.mu.Lock()
	delete(registry.identities, ws)
	registry.mu.Unlock()
}
