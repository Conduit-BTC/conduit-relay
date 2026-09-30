package nostr

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFilterCursorIsInternal(t *testing.T) {
	f := Filter{Cursor: &QueryCursor{CreatedAt: 5, ID: "a"}, Kinds: []int{1}}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Cursor") || strings.Contains(string(b), "CreatedAt") {
		t.Fatalf("internal cursor emitted: %s", b)
	}
	if err := json.Unmarshal([]byte(`{"kinds":[1],"Cursor":{"CreatedAt":5,"ID":"a"},"cursor":{"created_at":5,"id":"a"}}`), &f); err != nil {
		t.Fatal(err)
	}
	if f.Cursor != nil {
		t.Fatal("client supplied an internal query cursor")
	}
}

func TestFilterCursorMatchesQueryOrder(t *testing.T) {
	f := Filter{Cursor: &QueryCursor{CreatedAt: 5, ID: "b"}}
	for _, tc := range []struct {
		at   int64
		id   string
		want bool
	}{
		{6, "c", false}, {5, "a", false}, {5, "b", false}, {5, "c", true}, {4, "a", true},
	} {
		if got := f.Matches(&Event{CreatedAt: tc.at, ID: tc.id}); got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
}
