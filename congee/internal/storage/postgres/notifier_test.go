package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Run the production delivery boundary with the same channel ownership and
// shutdown lifecycle as listenLoop, without a live PostgreSQL connection.
func startNotifierBurst(ids []string) (*Notifier, <-chan struct{}, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	n := &Notifier{ch: make(chan string, 256), cancel: cancel}
	atCapacity := make(chan struct{})
	done := make(chan error, 1)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer close(n.ch)
		for index, id := range ids {
			if index == cap(n.ch) {
				close(atCapacity)
			}
			if err := n.forwardEventID(ctx, id); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return n, atCapacity, done
}

func notifierBurstIDs(count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("event-%04d", index)
	}
	return ids
}

func TestNotifierBurstPreservesEveryEventID(t *testing.T) {
	ids := notifierBurstIDs(1025)
	n, atCapacity, done := startNotifierBurst(ids)
	t.Cleanup(func() { _ = n.Close() })
	select {
	case <-atCapacity:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not fill the notification buffer")
	}
	if len(n.ch) != cap(n.ch) {
		t.Fatalf("buffer contains %d IDs, want %d", len(n.ch), cap(n.ch))
	}

	// The consumer starts only after the buffer fills, as when prior database
	// event loads delay imported-event fanout during a burst.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for index, want := range ids {
		select {
		case got, ok := <-n.Listen():
			if !ok {
				t.Fatalf("delivery ended after %d IDs, want %d", index, len(ids))
			}
			if got != want {
				t.Fatalf("ID %d: got %q, want %q", index, got, want)
			}
		case <-ctx.Done():
			t.Fatalf("delivery stalled at ID %d: %v", index, ctx.Err())
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := <-n.Listen(); ok {
		t.Fatal("notification channel remained open after delivery ended")
	}
}

func TestNotifierCloseCancelsBlockedDelivery(t *testing.T) {
	n, atCapacity, done := startNotifierBurst(notifierBurstIDs(257))
	t.Cleanup(func() { _ = n.Close() })
	select {
	case <-atCapacity:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not fill the notification buffer")
	}
	var closers sync.WaitGroup
	for range 16 {
		closers.Add(1)
		go func() {
			defer closers.Done()
			if err := n.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	closed := make(chan struct{})
	go func() {
		closers.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind a full notification buffer")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("delivery returned %v, want context cancellation", err)
	}
	count := 0
	for range n.Listen() {
		count++
	}
	if count != cap(n.ch) {
		t.Fatalf("buffer contains %d IDs after Close, want %d", count, cap(n.ch))
	}
}
