package relay

import (
	"context"
	"errors"
	"testing"

	"github.com/michmich112/congee/internal/nostr"
)

func TestNIP29RestrictedWriteMetadataFailureRejectsEvent(t *testing.T) {
	for _, lookupErr := range []error{errors.New("database unavailable"), errors.New("invalid metadata encoding"), context.DeadlineExceeded} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			st := &visibilityStoreStub{
				mdErr: lookupErr,
				memberFn: func(context.Context, string, string, string) (bool, error) {
					t.Fatal("membership must not bypass a failed metadata lookup")
					return true, nil
				},
			}
			srv := testVisibilityServer(t, st, testRelayIdentity(t))
			if err := nip29ValidateRestrictedWrite(context.Background(), st, srv, groupTaggedEvent()); !errors.Is(err, lookupErr) {
				t.Fatalf("expected metadata lookup failure, got %v", err)
			}
		})
	}
}

func TestNIP29RestrictedWritePreservesGroupPolicy(t *testing.T) {
	restricted := &nostr.Event{Kind: nostr.NIP29KindGroupMetadata, Tags: [][]string{{"restricted"}}}
	memberErr := errors.New("membership unavailable")
	for _, tc := range []struct {
		name        string
		metadata    *nostr.Event
		member      bool
		memberErr   error
		checkMember bool
		wantErr     bool
	}{
		{name: "missing metadata"},
		{name: "unrestricted group", metadata: &nostr.Event{Kind: nostr.NIP29KindGroupMetadata}},
		{name: "restricted member", metadata: restricted, member: true, checkMember: true},
		{name: "restricted nonmember", metadata: restricted, checkMember: true, wantErr: true},
		{name: "membership failure", metadata: restricted, memberErr: memberErr, checkMember: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memberChecked := false
			st := &visibilityStoreStub{
				md: tc.metadata,
				memberFn: func(context.Context, string, string, string) (bool, error) {
					memberChecked = true
					return tc.member, tc.memberErr
				},
			}
			srv := testVisibilityServer(t, st, testRelayIdentity(t))
			err := nip29ValidateRestrictedWrite(context.Background(), st, srv, groupTaggedEvent())
			if (err != nil) != tc.wantErr || memberChecked != tc.checkMember {
				t.Fatalf("error=%v membership checked=%v; expected error=%v membership checked=%v", err, memberChecked, tc.wantErr, tc.checkMember)
			}
			if tc.memberErr != nil && !errors.Is(err, tc.memberErr) {
				t.Fatalf("expected membership lookup failure, got %v", err)
			}
		})
	}
}
