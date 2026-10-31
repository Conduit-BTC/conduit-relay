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
	event := signedAuthEvent(t, secret, challenge, relayURL, "")

	pubkey, err := validateAuthEvent(event, challenge, relayURL)
	require.NoError(t, err)
	require.Equal(t, secret.Public(), pubkey)
}

func TestValidateAuthEventAcceptsNonEmptyContent(t *testing.T) {
	secret := nostr.Generate()
	challenge := "test-challenge"
	relayURL := "wss://relay.example/"
	event := signedAuthEvent(t, secret, challenge, relayURL, "client-auth-context")

	pubkey, err := validateAuthEvent(event, challenge, relayURL)
	require.NoError(t, err)
	require.Equal(t, secret.Public(), pubkey)
}

func TestValidateAuthEventRejectsMismatchedID(t *testing.T) {
	secret := nostr.Generate()
	challenge := "test-challenge"
	relayURL := "wss://relay.example/"
	event := signedAuthEvent(t, secret, challenge, relayURL, "client-auth-context")
	event.ID[0] ^= 0xff

	pubkey, err := validateAuthEvent(event, challenge, relayURL)
	require.EqualError(t, err, "event id is computed incorrectly")
	require.Equal(t, nostr.ZeroPK, pubkey)
}

func signedAuthEvent(
	t *testing.T,
	secret nostr.SecretKey,
	challenge string,
	relayURL string,
	content string,
) nostr.Event {
	t.Helper()
	event := nostr.Event{
		Kind:      nostr.KindClientAuthentication,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"relay", relayURL},
			{"challenge", challenge},
		},
		Content: content,
	}
	require.NoError(t, event.Sign(secret))
	return event
}
