package relay

import (
	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/relayidentity"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

// NewConduitServer keeps standard Nostr WebSockets and public NIP-11 discovery
// accessible to external clients, including clients without an Origin header.
// Event validation, authentication, and admission limits still apply.
func NewConduitServer(cfg *config.Config, store storage.Store, log zerolog.Logger, relayID *relayidentity.Identity) (*Server, error) {
	s, err := NewServer(cfg, store, log, relayID)
	if err != nil {
		return nil, err
	}
	s.publicDiscoveryCORS = true
	return s, nil
}
