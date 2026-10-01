package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/nostr"
)

func TestSubscriptionManagerSubIDLength(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())
	long := make([]byte, cfg.MaxSubscriptionIDLength+1)
	for i := range long {
		long[i] = 'a'
	}
	err := m.Add("c1", string(long), nil)
	if err == nil {
		t.Fatal("expected error for long sub id")
	}
	if err := m.Add("c1", "ok", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	if m.SubCount("c1") != 1 {
		t.Fatalf("sub count %d", m.SubCount("c1"))
	}
}

func TestSubscriptionManagerMaxSubs(t *testing.T) {
	cfg := minimalRelayCfg()
	cfg.ConnectionLimits.MaxSubscriptionsPerConnection = 1
	m := NewSubscriptionManager(cfg, zerolog.Nop())
	m.RegisterSender("c1", func([]byte) bool { return true })
	if err := m.Add("c1", "a", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("c1", "b", nil); err != ErrTooManySubscriptions {
		t.Fatalf("got %v", err)
	}
}

func TestFiltersMatch_SearchNeverMatchesLive(t *testing.T) {
	ev := &nostr.Event{ID: strings.Repeat("1", 64), PubKey: strings.Repeat("2", 64), CreatedAt: 1, Kind: 1, Content: "hello"}
	q := "hello"
	f := nostr.Filter{Kinds: []int{1}, Search: &q}
	if filtersMatch([]nostr.Filter{f}, ev) {
		t.Fatal("subscription fan-out must not treat search as a live filter")
	}
}

func TestFiltersMatchOR(t *testing.T) {
	ev := &nostr.Event{Kind: 1, PubKey: "abc", Content: "x"}
	f1 := nostr.Filter{Kinds: []int{2}}
	f2 := nostr.Filter{Kinds: []int{1}}
	if filtersMatch([]nostr.Filter{f1, f2}, ev) != true {
		t.Fatal("expected OR match")
	}
}

func TestBroadcastRespectsClose(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())

	var sent atomic.Int64
	m.RegisterSender("c1", func(b []byte) bool {
		sent.Add(1)
		return true
	})
	if err := m.Add("c1", "sub1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	removeDone := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-removeDone
		m.Remove("c1", "sub1")
	}()

	for i := 0; i < 100; i++ {
		ev := &nostr.Event{ID: fmt.Sprintf("%064d", i), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "x"}
		m.Broadcast(ev, nil)
	}
	m.FinishSnapshot("c1", "sub1")
	close(removeDone)
	wg.Wait()

	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 100; i++ {
		ev := &nostr.Event{ID: fmt.Sprintf("%064d", i+200), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "x"}
		m.Broadcast(ev, nil)
	}

	if sent.Load() != 100 {
		t.Fatalf("expected exactly 100 events sent after close, got %d", sent.Load())
	}
}

func TestConcurrentBroadcastRemove(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())

	var sent atomic.Int64
	m.RegisterSender("c1", func(b []byte) bool {
		sent.Add(1)
		return true
	})
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	start := sync.WaitGroup{}
	start.Add(1)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			start.Wait()
			ev := &nostr.Event{ID: fmt.Sprintf("%064d", n), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "x"}
			m.Broadcast(ev, nil)
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		start.Wait()
		m.Remove("c1", "s1")
	}()

	start.Done()
	wg.Wait()

	if m.SubCount("c1") != 0 {
		t.Fatalf("sub count %d, expected 0", m.SubCount("c1"))
	}
}

func TestCloseThenBroadcastNeverSends(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())

	var sent atomic.Int64
	m.RegisterSender("c1", func(b []byte) bool {
		sent.Add(1)
		return true
	})
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}

	m.Remove("c1", "s1")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		for i := 0; i < 1000; i++ {
			select {
			case <-ctx.Done():
				done <- false
				return
			default:
				ev := &nostr.Event{ID: fmt.Sprintf("%064d", i), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "x"}
				m.Broadcast(ev, nil)
			}
		}
		done <- true
	}()

	<-done
	if sent.Load() != 0 {
		t.Fatalf("expected 0 events sent after close, got %d", sent.Load())
	}
}

func TestBroadcastBuffersUntilFinishSnapshot(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())

	var sent atomic.Int64
	m.RegisterSender("c1", func(b []byte) bool {
		sent.Add(1)
		return true
	})
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}

	ev1 := &nostr.Event{ID: strings.Repeat("1", 64), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "a"}
	ev2 := &nostr.Event{ID: strings.Repeat("3", 64), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "b"}
	m.Broadcast(ev1, nil)
	m.Broadcast(ev2, nil)
	if sent.Load() != 0 {
		t.Fatalf("expected 0 sends before FinishSnapshot, got %d", sent.Load())
	}

	m.FinishSnapshot("c1", "s1")
	if sent.Load() != 2 {
		t.Fatalf("expected 2 flushed events, got %d", sent.Load())
	}

	ev3 := &nostr.Event{ID: strings.Repeat("5", 64), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "c"}
	m.Broadcast(ev3, nil)
	if sent.Load() != 3 {
		t.Fatalf("expected immediate send after snapshot, got %d", sent.Load())
	}
}

func TestBroadcastSnapshotOverflowClosesGenerationAndAllowsRetry(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())

	var sent [][]byte
	m.RegisterSender("c1", func(b []byte) bool {
		sent = append(sent, b)
		return true
	})
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}

	generation, ok := m.SubGeneration("c1", "s1")
	if !ok {
		t.Fatal("missing snapshot generation")
	}
	entry := m.subs["c1"]["s1"]
	for i := 0; i < pendingLiveCap; i++ {
		ev := &nostr.Event{ID: fmt.Sprintf("%064d", i), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "x"}
		m.Broadcast(ev, nil)
	}
	if len(sent) != 0 || !m.IsSameSnapshot("c1", "s1", generation) {
		t.Fatal("buffer at capacity must remain open without sending")
	}
	ev := &nostr.Event{ID: strings.Repeat("a", 64), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "x"}
	m.Broadcast(ev, nil)
	if len(sent) != 1 {
		t.Fatalf("want exactly one overflow notice, got %d", len(sent))
	}
	var closed []string
	if err := json.Unmarshal(sent[0], &closed); err != nil {
		t.Fatal(err)
	}
	if len(closed) != 3 || closed[0] != "CLOSED" || closed[1] != "s1" ||
		!strings.HasPrefix(closed[2], "error:") || !strings.Contains(closed[2], "retry") {
		t.Fatalf("invalid overflow notice: %q", closed)
	}
	if m.SubCount("c1") != 0 || m.IsSameSnapshot("c1", "s1", generation) || len(entry.pendingLive) != 0 {
		t.Fatal("overflow did not invalidate and release the snapshot")
	}
	if m.withSnapshot("c1", "s1", generation, func(*subEntry) { t.Fatal("stale snapshot can still send") }) {
		t.Fatal("stale snapshot accepted")
	}
	m.FinishSnapshot("c1", "s1")
	m.Broadcast(ev, nil)
	if len(sent) != 1 {
		t.Fatal("closed snapshot continued sending events")
	}
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	if m.withSnapshot("c1", "s1", generation, func(*subEntry) { t.Fatal("stale snapshot affected replacement") }) {
		t.Fatal("old generation matched replacement")
	}
	m.FinishSnapshot("c1", "s1")
	m.Broadcast(ev, nil)
	if len(sent) != 2 || !strings.HasPrefix(string(sent[1]), `["EVENT","s1",`) {
		t.Fatal("replacement subscription did not resume live events")
	}
}

func TestBroadcastSnapshotOverflowFailedNoticeStillInvalidatesGeneration(t *testing.T) {
	m := NewSubscriptionManager(minimalRelayCfg(), zerolog.Nop())
	var attempted atomic.Int64
	m.RegisterSender("c1", func([]byte) bool {
		attempted.Add(1)
		return false
	})
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	generation, _ := m.SubGeneration("c1", "s1")
	entry := m.subs["c1"]["s1"]
	ev := &nostr.Event{ID: strings.Repeat("a", 64), PubKey: strings.Repeat("2", 64), Kind: 1}
	for i := 0; i < pendingLiveCap+2; i++ {
		m.Broadcast(ev, nil)
	}
	m.FinishSnapshot("c1", "s1")
	if attempted.Load() != 1 || m.IsSameSnapshot("c1", "s1", generation) || len(entry.pendingLive) != 0 {
		t.Fatal("failed CLOSED enqueue retained the overflowed snapshot")
	}
}

func TestAddResetsSnapshotBuffer(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())
	m.RegisterSender("c1", func([]byte) bool { return true })
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	ev := &nostr.Event{ID: strings.Repeat("1", 64), PubKey: strings.Repeat("2", 64), Kind: 1, Content: "x"}
	m.Broadcast(ev, nil)

	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	e := m.subs["c1"]["s1"]
	if e.snapshotDone.Load() {
		t.Fatal("replaced sub should reset snapshotDone")
	}
	if len(e.pendingLive) != 0 {
		t.Fatalf("replaced sub should clear pendingLive, got %d", len(e.pendingLive))
	}
	m.mu.Unlock()
}

func TestIsSameSnapshot(t *testing.T) {
	cfg := minimalRelayCfg()
	m := NewSubscriptionManager(cfg, zerolog.Nop())
	m.RegisterSender("c1", func([]byte) bool { return true })
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	opened, ok := m.SubGeneration("c1", "s1")
	if !ok {
		t.Fatal("SubGeneration")
	}
	if !m.IsSameSnapshot("c1", "s1", opened) {
		t.Fatal("expected same snapshot")
	}
	if m.IsSameSnapshot("c1", "s1", opened-1) {
		t.Fatal("expected stale generation to mismatch")
	}
	m.mu.Lock()
	m.subs["c1"]["s1"].openedUnix = 1
	m.mu.Unlock()
	if err := m.Add("c1", "s1", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.subs["c1"]["s1"].openedUnix = 1
	m.mu.Unlock()
	if m.IsSameSnapshot("c1", "s1", opened) {
		t.Fatal("expected replacement to invalidate stale generation")
	}
	reopened, ok := m.SubGeneration("c1", "s1")
	if !ok || !m.IsSameSnapshot("c1", "s1", reopened) {
		t.Fatal("expected new snapshot after replacement")
	}
	m.Remove("c1", "s1")
	if m.IsSameSnapshot("c1", "s1", reopened) {
		t.Fatal("expected closed sub to fail snapshot check")
	}
}

func minimalRelayCfg() *config.Config {
	return &config.Config{
		MaxSubscriptionIDLength: 64,
		ConnectionLimits: config.ConnectionLimitsSection{
			MaxSubscriptionsPerConnection: 10,
			MaxFiltersPerReq:              5,
			ConnectionsPerMinutePerIP:     100,
		},
	}
}

func TestBroadcastChecksVisibilityOnlyForMatchingConnections(t *testing.T) {
	m := NewSubscriptionManager(minimalRelayCfg(), zerolog.Nop())
	for i := 0; i < 100; i++ {
		id := fmt.Sprint(i)
		m.RegisterSender(id, func([]byte) bool { return true })
		if err := m.Add(id, "sub", []nostr.Filter{{Kinds: []int{7}}}); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	visible := func(string, *nostr.Event) bool { calls.Add(1); return true }
	m.Broadcast(&nostr.Event{ID: "event", Kind: 1}, visible)
	if calls.Load() != 0 {
		t.Fatal("visibility called for nonmatching subscriptions")
	}
	if err := m.Add("0", "one", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("0", "two", []nostr.Filter{{Kinds: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	m.Broadcast(&nostr.Event{ID: "event", Kind: 1}, visible)
	if calls.Load() != 1 {
		t.Fatalf("visibility calls %d, want one per matching connection", calls.Load())
	}
}

func TestBroadcastVisibilityDoesNotBlockReplacement(t *testing.T) {
	m := NewSubscriptionManager(minimalRelayCfg(), zerolog.Nop())
	var sent atomic.Int64
	m.RegisterSender("c", func([]byte) bool { sent.Add(1); return true })
	filters := []nostr.Filter{{Kinds: []int{1}}}
	if err := m.Add("c", "s", filters); err != nil {
		t.Fatal(err)
	}
	m.FinishSnapshot("c", "s")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		m.Broadcast(&nostr.Event{ID: "event", Kind: 1}, func(string, *nostr.Event) bool {
			close(entered)
			<-release
			return true
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("visibility did not start")
	}
	replaced := make(chan error, 1)
	go func() { replaced <- m.Add("c", "s", filters) }()
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("visibility held manager mutex")
	}
	close(release)
	<-done
	// Completion would flush any event wrongly buffered into the replacement.
	m.FinishSnapshot("c", "s")
	if sent.Load() != 0 {
		t.Fatal("old broadcast reached replacement subscription")
	}
}
