package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/plugin"
	"github.com/rs/zerolog"
)

type reqAuthPlugin struct {
	recordingPluginRuntime
	result plugin.InterceptResult
}

func (p *reqAuthPlugin) InterceptREQ(context.Context, *nostr.ReqMessage) plugin.InterceptResult {
	return p.result
}

type reqAuthStore struct {
	visibilityStoreStub
	event *nostr.Event
	calls int
}

func (s *reqAuthStore) QueryEvents(_ context.Context, filters []nostr.Filter) ([]*nostr.Event, error) {
	s.calls++
	for _, f := range filters {
		if f.Matches(s.event) {
			return []*nostr.Event{s.event}, nil
		}
	}
	return nil, nil
}

func TestREQPluginEffectiveFiltersRequireAuthentication(t *testing.T) {
	for _, action := range []plugin.InterceptAction{plugin.InterceptReshapeREQ, plugin.InterceptRespond} {
		for _, tc := range []struct {
			name          string
			filters       []nostr.Filter
			authenticated bool
			nip17         bool
			closedPrefix  string
		}{
			{name: "protected", filters: []nostr.Filter{{Kinds: []int{4}}}, closedPrefix: "auth-required:"},
			{name: "mixed", filters: []nostr.Filter{{Kinds: []int{2, 4}}}, closedPrefix: "auth-required:"},
			{name: "wildcard", filters: []nostr.Filter{{}}, closedPrefix: "auth-required:"},
			{name: "authenticated", filters: []nostr.Filter{{Kinds: []int{4}}}, authenticated: true},
			{name: "public", filters: []nostr.Filter{{Kinds: []int{2}}}},
			{name: "gift-wrap-other-recipient", filters: []nostr.Filter{{Kinds: []int{1059}, Tag: map[string][]string{"#p": {strings.Repeat("b", 64)}}}}, authenticated: true, nip17: true, closedPrefix: "restricted:"},
		} {
			t.Run(fmt.Sprintf("action=%d/%s", action, tc.name), func(t *testing.T) {
				cfg := testRelayConfig()
				cfg.NIPs.Enabled = append(cfg.NIPs.Enabled, 42)
				if tc.nip17 {
					cfg.NIPs.Enabled = append(cfg.NIPs.Enabled, 17)
				}
				cfg.NIP42.RequireAuthSubscribeKinds = []int{4}
				cfg.NIP42.RelayURL = "wss://relay.example/"
				kind := 4
				if len(tc.filters[0].Kinds) > 0 {
					kind = tc.filters[0].Kinds[0]
				}
				st := &reqAuthStore{event: &nostr.Event{ID: strings.Repeat("c", 64), Kind: kind, CreatedAt: 1}}
				srv, err := NewServer(cfg, st, zerolog.Nop(), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(srv.metricsCancel)
				c := registerTestConn(t, srv, "plugin-auth")
				if tc.authenticated {
					c.nip42AddPubkey(strings.Repeat("a", 64))
				}
				srv.SetPluginRuntime(&reqAuthPlugin{result: plugin.InterceptResult{
					Action: action, Filters: tc.filters, SubscriptionFilters: tc.filters, EventIDs: []string{st.event.ID},
				}})
				if err := handleREQ(c.ctx, srv, c, &nostr.ReqMessage{SubID: "sub", Filters: []nostr.Filter{{Kinds: []int{1}}}}, false); err != nil {
					t.Fatal(err)
				}
				var replies [][]any
				for len(c.send) > 0 {
					var reply []any
					if err := json.Unmarshal(<-c.send, &reply); err != nil {
						t.Fatal(err)
					}
					replies = append(replies, reply)
				}
				if tc.closedPrefix != "" {
					if st.calls != 0 || srv.subs.SubCount(c.ID) != 0 {
						t.Fatalf("rejected plugin filters reached storage/subscriptions: calls=%d subs=%d", st.calls, srv.subs.SubCount(c.ID))
					}
					if !tc.authenticated {
						if len(replies) != 2 || replies[0][0] != "AUTH" || replies[0][1] == "" {
							t.Fatalf("missing challenge before rejection: %v", replies)
						}
						replies = replies[1:]
					}
					if len(replies) != 1 || replies[0][0] != "CLOSED" || replies[0][1] != "sub" || !strings.HasPrefix(replies[0][2].(string), tc.closedPrefix) {
						t.Fatalf("unexpected rejection: %v", replies)
					}
					return
				}
				if st.calls != 1 || srv.subs.SubCount(c.ID) != 1 {
					t.Fatalf("allowed filters failed: calls=%d subs=%d", st.calls, srv.subs.SubCount(c.ID))
				}
				if len(replies) != 2 || replies[0][0] != "EVENT" || replies[1][0] != "EOSE" {
					t.Fatalf("allowed query failed to deliver snapshot: %v", replies)
				}
			})
		}
	}
}
