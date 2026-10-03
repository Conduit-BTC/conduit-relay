package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/michmich112/congee/internal/nostr"
)

func TestReqEventByIDIsolatesConsecutiveSubscriptions(t *testing.T) {
	// The first and second IDs share the old subscription ID prefix. The
	// third request repeats the first event to cover subscription ID reuse.
	firstID := strings.Repeat("a", 64)
	secondID := strings.Repeat("a", 8) + strings.Repeat("b", 56)
	ids := []string{firstID, secondID, firstID, strings.Repeat("c", 64)}
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		done <- func() error {
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			var previousSubID, firstSubID string
			for i, id := range ids {
				var request []json.RawMessage
				if err := conn.ReadJSON(&request); err != nil {
					return err
				}
				if len(request) != 3 || string(request[0]) != `"REQ"` {
					return fmt.Errorf("fetch %d: expected REQ", i)
				}
				var subID string
				if err := json.Unmarshal(request[1], &subID); err != nil {
					return err
				}
				var filter struct {
					IDs []string `json:"ids"`
				}
				if err := json.Unmarshal(request[2], &filter); err != nil || len(filter.IDs) != 1 || filter.IDs[0] != id {
					return fmt.Errorf("fetch %d: unexpected filter", i)
				}
				if i == 0 {
					firstSubID = subID
				}
				var frames [][]any
				switch i {
				case 1:
					// A matching event from a prior subscription must be ignored.
					frames = append(frames, []any{"EVENT", previousSubID, nostr.Event{ID: id, Content: "stale"}})
				case 2:
					frames = append(frames,
						[]any{"EOSE", firstSubID},
						[]any{"EVENT", firstSubID, nostr.Event{ID: id, Content: "stale"}},
					)
				case 3:
					frames = append(frames,
						[]any{"EOSE"},
						[]any{"EOSE", 123},
						[]any{"EVENT", 123, nostr.Event{ID: id, Content: "stale"}},
					)
				}
				if i < 3 {
					frames = append(frames, []any{"EVENT", subID, nostr.Event{ID: id, Content: fmt.Sprintf("fetch %d", i)}})
				}
				// EOSE follows EVENT and remains queued when the next fetch starts.
				frames = append(frames, []any{"EOSE", subID})
				for _, frame := range frames {
					if err := conn.WriteJSON(frame); err != nil {
						return err
					}
				}
				var closeFrame []string
				if err := conn.ReadJSON(&closeFrame); err != nil {
					return err
				}
				if len(closeFrame) != 2 || closeFrame[0] != "CLOSE" || closeFrame[1] != subID {
					return fmt.Errorf("fetch %d: expected CLOSE for current subscription", i)
				}
				previousSubID = subID
			}
			return nil
		}()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialUpstream(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i, id := range ids {
		event, err := client.reqEventByID(ctx, id)
		if i == 3 {
			if event != nil || err == nil || !strings.Contains(err.Error(), "event not found") {
				t.Fatalf("current EOSE: event=%v error=%v", event, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		if event.ID != id || event.Content != fmt.Sprintf("fetch %d", i) {
			t.Fatalf("fetch %d accepted an event from another subscription: %+v", i, event)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
