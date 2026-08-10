package khatru

import (
	"testing"

	"fiatjaf.com/nostr"
	"github.com/stretchr/testify/require"
)

func TestValidateAuthEventAcceptsCanonicalEmptyContent(t *testing.T) {
	secret := nostr.Generate()
	challenge := "test-challenge"
	relayURL := "wss://relay.example/"
	event := nostr.Event{
		Kind:      nostr.KindClientAuthentication,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"relay", relayURL},
			{"challenge", challenge},
		},
	}
	require.NoError(t, event.Sign(secret))

	pubkey, err := validateAuthEvent(event, challenge, relayURL)
	require.NoError(t, err)
	require.Equal(t, secret.Public(), pubkey)
}
