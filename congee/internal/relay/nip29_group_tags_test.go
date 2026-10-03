package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/michmich112/congee/internal/nostr"
)

type groupTagAdmissionStore struct {
	negGroupPrivacyStore
	saved int
}

func (st *groupTagAdmissionStore) SaveEvent(context.Context, *nostr.Event) error {
	st.saved++
	return nil
}

func TestNIP29AdmissionRejectsMultipleGroupTags(t *testing.T) {
	for _, kind := range []int{1, 20001, nostr.NIP29KindPutUser, nostr.NIP29KindJoinRequest, nostr.NIP29KindLeaveReq} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			st := &groupTagAdmissionStore{}
			srv := testVisibilityServer(t, st, testRelayIdentity(t))
			t.Cleanup(srv.metricsCancel)
			RegisterNIP01(srv, st)
			RegisterNIP29(srv, st)
			c := registerTestConn(t, srv, "multiple-group-tags")
			priv, err := btcec.NewPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			ev := signedTestEvent(t, priv, kind)
			ev.Tags = [][]string{{"h", "public"}, {"h", "private"}}
			if err := ev.Sign(priv); err != nil {
				t.Fatal(err)
			}
			if err := handleEVENT(t.Context(), srv, st, c, &nostr.EventMessage{Event: *ev}); err != nil {
				t.Fatal(err)
			}
			var reply []any
			if err := json.Unmarshal(<-c.send, &reply); err != nil {
				t.Fatal(err)
			}
			if len(reply) != 4 || reply[0] != "OK" || reply[1] != ev.ID || reply[2] != false || reply[3] != "invalid: nip-29 events may contain only one nonempty h tag" {
				t.Fatalf("expected group-tag rejection, got %v", reply)
			}
			if st.saved != 0 || st.metadataCalls != 0 {
				t.Fatalf("invalid event reached storage or group policy: saved=%d metadata=%d", st.saved, st.metadataCalls)
			}
		})
	}
}

func TestNIP29GroupTagPolicyPreservesCompatibleEvents(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		tags     [][]string
	}{
		{name: "no group"},
		{name: "single group", tags: [][]string{{"h", "public"}}},
		{name: "empty tags ignored", tags: [][]string{{"h"}, {"h", ""}, {"h", "public"}}},
		{name: "nip29 disabled", disabled: true, tags: [][]string{{"h", "public"}, {"h", "private"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &negGroupPrivacyStore{}
			srv := testVisibilityServer(t, st, testRelayIdentity(t))
			t.Cleanup(srv.metricsCancel)
			if tc.disabled {
				srv.cfg.NIPs.Enabled = []int{1, 11}
			}
			RegisterNIP01(srv, st)
			RegisterNIP29(srv, st)
			priv, err := btcec.NewPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			ev := signedTestEvent(t, priv, 1)
			ev.Tags = tc.tags
			if err := ev.Sign(priv); err != nil {
				t.Fatal(err)
			}
			if err := srv.validators.Validate(t.Context(), nil, ev); err != nil {
				t.Fatalf("compatible event rejected: %v", err)
			}
			if !srv.EventVisibleToSubscription("reader", ev) {
				t.Fatal("compatible public event was withheld")
			}
		})
	}
}

func TestEventVisibilityRejectsMultipleGroupTags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		groups     []string
		giftWrap   bool
		missingKey bool
	}{
		{name: "public first", groups: []string{"public", "private"}},
		{name: "private first", groups: []string{"private", "public"}},
		{name: "repeated group", groups: []string{"public", "public"}},
		{name: "gift wrap", groups: []string{"public", "private"}, giftWrap: true},
		{name: "missing relay identity", groups: []string{"public", "private"}, missingKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &negGroupPrivacyStore{visibilityStoreStub: visibilityStoreStub{md: privateGroupMetadata(), memberFn: func(context.Context, string, string, string) (bool, error) { return true, nil }}}
			srv := testVisibilityServer(t, st, testRelayIdentity(t))
			t.Cleanup(srv.metricsCancel)
			c := registerTestConn(t, srv, "multiple-group-tags")
			pk := strings.Repeat("d", 64)
			c.nip42AddPubkey(pk)
			ev := groupTaggedEvent()
			ev.Tags = nil
			for _, group := range tc.groups {
				ev.Tags = append(ev.Tags, []string{"h", group})
			}
			if tc.giftWrap {
				srv.cfg.NIPs.Enabled = append(srv.cfg.NIPs.Enabled, 17)
				ev.Kind = 1059
				ev.Tags = append(ev.Tags, []string{"p", pk})
			}
			if tc.missingKey {
				srv.relayID = nil
			}
			if srv.EventVisibleToSubscription(c.ID, ev) {
				t.Fatal("ambiguous group event must be withheld even from an authenticated member or gift-wrap recipient")
			}
			if st.metadataCalls != 0 {
				t.Fatal("group policy must not select one of multiple group tags")
			}
		})
	}
}

func TestNEGOpenRejectsMultipleGroupTagsAfterPublicVisibilityCacheHit(t *testing.T) {
	publicID := strings.Repeat("a", 64)
	ambiguousID := strings.Repeat("b", 64)
	for _, scope := range []string{"kind", "protected group", "event ID", "missing relay identity"} {
		t.Run(scope, func(t *testing.T) {
			ambiguous := negGroupEvent(ambiguousID, 2, "public")
			ambiguous.Tags = append(ambiguous.Tags, []string{"h", "private"})
			st := &negGroupPrivacyStore{
				visibilityStoreStub: visibilityStoreStub{md: privateGroupMetadata()},
				events:              []*nostr.Event{negGroupEvent(publicID, 1, "public"), ambiguous},
			}
			srv, c := newNegGroupServer(t, st)
			filter := nostr.Filter{Kinds: []int{1}}
			var want []string
			switch scope {
			case "kind", "missing relay identity":
				want = []string{publicID}
			case "protected group":
				filter.Tag = map[string][]string{"#h": {"private"}}
			case "event ID":
				filter.IDs = []string{ambiguousID}
			}
			if scope == "missing relay identity" {
				srv.relayID = nil
			}
			if got := reconcileNegGroupIDs(t, srv, c, filter); !slices.Equal(got, want) {
				t.Fatalf("reconciled IDs %v, want %v", got, want)
			}
		})
	}
}
