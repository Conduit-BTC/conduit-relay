package main

import (
	"testing"

	khatru "conduitl2/third_party/khatru"
	"github.com/stretchr/testify/require"
)

func TestConfigureRelayInformationUsesProductionBranding(t *testing.T) {
	relay := khatru.NewRelay()

	configureRelayInformation(relay)

	require.Equal(t, "Conduit Relay", relay.Info.Name)
	require.Equal(
		t,
		"Conduit-operated relay for commerce discovery, merchant publishing, and protected private-message delivery.",
		relay.Info.Description,
	)
	require.Equal(
		t,
		"https://shop.conduit.market/pwa-192x192.png",
		relay.Info.Icon,
	)
}
