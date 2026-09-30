package relay

import (
	"context"
	"time"

	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/rs/zerolog"
)

// RunImportedEventFanout loads events by id from NOTIFY/LISTEN, notifies plugins
// that subscribe to the event, and broadcasts to REQ subscriptions.
// It returns when ctx is done or the notifier channel is closed.
// Same-origin LISTEN is filtered by the notifier, so the importing instance
// calls DeliverImportedEvent directly instead of waiting for this loop.
func RunImportedEventFanout(ctx context.Context, s *Server, store storage.Store, n storage.EventNotifier, log zerolog.Logger) {
	if n == nil {
		n = storage.NoopNotifier{}
	}
	ch := n.Listen()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-ch:
			if !ok {
				return
			}
			if id == "" {
				continue
			}
			qctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			evs, err := store.QueryEvents(qctx, []nostr.Filter{{IDs: []string{id}}})
			cancel()
			if err != nil || len(evs) != 1 {
				log.Debug().Err(err).Str("event_id", id).Msg("imported event fetch skipped")
				continue
			}
			s.DeliverImportedEvent(evs[0])
		}
	}
}

// DeliverImportedEvent notifies plugins and matching local subscriptions after
// an imported event has been stored. It preserves imported-event semantics:
// WebSocket validation and post-store mutation hooks do not run again.
func (s *Server) DeliverImportedEvent(ev *nostr.Event) {
	if s == nil || ev == nil {
		return
	}
	s.NotifyPluginStoredEvent(ev, true)
	s.broadcastEvent(ev)
}
