package conduitl2

import (
	"context"
	"slices"
	"strings"
	"testing"

	khatru "conduitl2/third_party/khatru"
	"fiatjaf.com/nostr"
	"github.com/stretchr/testify/require"
)

func TestParseGiftWrapProtectionMode(t *testing.T) {
	tests := []struct {
		raw  string
		want GiftWrapProtectionMode
	}{
		{raw: "", want: GiftWrapProtectionEnforce},
		{raw: "disabled", want: GiftWrapProtectionDisabled},
		{raw: "challenge-only", want: GiftWrapProtectionChallengeOnly},
		{raw: "enforce", want: GiftWrapProtectionEnforce},
		{raw: " ENFORCE ", want: GiftWrapProtectionEnforce},
	}

	for _, test := range tests {
		got, err := ParseGiftWrapProtectionMode(test.raw)
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}

	_, err := ParseGiftWrapProtectionMode("unknown")
	require.Error(t, err)
}

func TestGiftWrapProtectionReasonsMatchClientContract(t *testing.T) {
	require.True(t, strings.HasPrefix(giftWrapAuthRequiredReason, "auth-required:"))
	for _, reason := range []string{
		giftWrapMixedKindsReason,
		giftWrapRecipientReason,
		giftWrapIdentityReason,
		giftWrapCountReason,
		giftWrapWildcardReadReason,
		giftWrapWildcardCountReason,
		giftWrapNegentropyReason,
	} {
		require.True(t, strings.HasPrefix(reason, "restricted:"))
	}
}

func TestGiftWrapProtectionUnknownProgrammaticModeFailsClosed(t *testing.T) {
	opts := Scope2Options{GiftWrapProtection: GiftWrapProtectionMode(255)}
	withDefaults(&opts)

	require.Equal(t, GiftWrapProtectionEnforce, opts.GiftWrapProtection)
}

func TestGiftWrapProtectionModesPreserveValidWrites(t *testing.T) {
	recipient := nostr.Generate().Public()
	event := nostr.Event{Kind: giftWrapKind, Tags: nostr.Tags{{"p", recipient.Hex()}}}

	for _, mode := range []GiftWrapProtectionMode{
		GiftWrapProtectionDisabled,
		GiftWrapProtectionChallengeOnly,
		GiftWrapProtectionEnforce,
	} {
		relay := khatru.NewRelay()
		delegated := false
		relay.OnEvent = func(context.Context, nostr.Event) (bool, string) {
			delegated = true
			return false, ""
		}

		configureGiftWrapProtection(relay, mode)
		reject, reason := relay.OnEvent(context.Background(), event)

		require.False(t, reject)
		require.Empty(t, reason)
		require.True(t, delegated)
	}
}

func TestGiftWrapProtectionDisabledAndChallengeOnlyAdmitNormalReads(t *testing.T) {
	filter := nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}}

	for _, mode := range []GiftWrapProtectionMode{
		GiftWrapProtectionDisabled,
		GiftWrapProtectionChallengeOnly,
	} {
		relay := khatru.NewRelay()
		requestDelegated := false
		countDelegated := false
		relay.OnRequest = func(context.Context, nostr.Filter) (bool, string) {
			requestDelegated = true
			return false, ""
		}
		relay.OnCount = func(context.Context, nostr.Filter) (bool, string) {
			countDelegated = true
			return false, ""
		}

		configureGiftWrapProtection(relay, mode)
		reject, reason := relay.OnRequest(context.Background(), filter)
		require.False(t, reject)
		require.Empty(t, reason)
		require.True(t, requestDelegated)

		reject, reason = relay.OnCount(context.Background(), filter)
		require.False(t, reject)
		require.Empty(t, reason)
		require.True(t, countDelegated)
	}
}

func TestGiftWrapProtectionChallengeOnlyOffersAuthAfterPriorPolicy(t *testing.T) {
	relay := khatru.NewRelay()
	policyCalls := 0
	relay.OnRequest = func(context.Context, nostr.Filter) (bool, string) {
		policyCalls++
		return false, ""
	}

	offerCalls := 0
	configureGiftWrapChallengeHooks(relay, func(context.Context) {
		offerCalls++
		require.Equal(t, 1, policyCalls)
	})

	reject, reason := relay.OnRequest(context.Background(), nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}})
	require.False(t, reject)
	require.Empty(t, reason)
	require.Equal(t, 1, offerCalls)

	reject, reason = relay.OnRequest(context.Background(), nostr.Filter{Kinds: []nostr.Kind{1}})
	require.False(t, reject)
	require.Empty(t, reason)
	require.Equal(t, 1, offerCalls)

	relay = khatru.NewRelay()
	relay.OnRequest = func(context.Context, nostr.Filter) (bool, string) {
		return true, "blocked: prior policy"
	}
	configureGiftWrapChallengeHooks(relay, func(context.Context) {
		t.Fatal("AUTH must not be offered after prior policy rejects the request")
	})
	reject, reason = relay.OnRequest(context.Background(), nostr.Filter{Kinds: []nostr.Kind{giftWrapKind}})
	require.True(t, reject)
	require.Equal(t, "blocked: prior policy", reason)
}

func TestGiftWrapProtectionNIP11TagRequiresEnforcement(t *testing.T) {
	for _, test := range []struct {
		mode           GiftWrapProtectionMode
		wantProtection bool
	}{
		{mode: GiftWrapProtectionDisabled},
		{mode: GiftWrapProtectionChallengeOnly},
		{mode: GiftWrapProtectionEnforce, wantProtection: true},
	} {
		relay := khatru.NewRelay()
		ConfigureRelay(relay, Scope2Options{GiftWrapProtection: test.mode})
		info := relay.OverwriteRelayInformation(context.Background(), nil, *relay.Info)

		require.Contains(t, info.Tags, "giftwrap_read_policy:"+test.mode.String())
		require.Equal(t, test.wantProtection, slices.Contains(info.Tags, "protected_kind:1059"))
	}
}
