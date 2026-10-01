package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// Run the production delivery boundary with the same channel ownership and
// shutdown lifecycle as listenLoop, without a live PostgreSQL connection.
func startNotifierBurst(ids []string) (*Notifier, <-chan struct{}, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	n := &Notifier{ctx: ctx, ch: make(chan string, 256), cancel: cancel}
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

// This driver runs through database/sql and Bun, including pooled connection
// acquisition. It models a backend that responds only to cancellation.
type blockedNotifyConnector struct {
	started chan struct{}
	stopped chan error
	release chan struct{}
}

func (c *blockedNotifyConnector) Connect(context.Context) (driver.Conn, error) {
	return &blockedNotifyConn{connector: c}, nil
}

func (c *blockedNotifyConnector) Driver() driver.Driver { return blockedNotifyDriver{} }

type blockedNotifyDriver struct{}

func (blockedNotifyDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type blockedNotifyConn struct{ connector *blockedNotifyConnector }

func (*blockedNotifyConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}
func (*blockedNotifyConn) Close() error { return nil }
func (*blockedNotifyConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected Begin")
}
func (c *blockedNotifyConn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	close(c.connector.started)
	select {
	case <-ctx.Done():
		c.connector.stopped <- ctx.Err()
		return nil, ctx.Err()
	case <-c.connector.release:
		return driver.RowsAffected(1), nil
	}
}

func newBlockedPublishNotifier(t *testing.T) (*Notifier, *sql.DB, *blockedNotifyConnector) {
	t.Helper()
	c := &blockedNotifyConnector{
		started: make(chan struct{}),
		stopped: make(chan error, 1),
		release: make(chan struct{}),
	}
	sqlDB := sql.OpenDB(c)
	sqlDB.SetMaxOpenConns(1)
	db := bun.NewDB(sqlDB, pgdialect.New())
	// An invalid listener DSN avoids an external connection. Publish still uses
	// the real Bun/database/sql execution path and constructor lifecycle.
	n, err := NewNotifier(db, "postgresql://%", "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(c.release)
		_ = n.Close()
		_ = db.Close()
	})
	return n, sqlDB, c
}

func startNotify(n *Notifier) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		n.Notify("test-event")
		close(done)
	}()
	return done
}

func TestNotifierPublishBoundsPoolAcquisition(t *testing.T) {
	n, db, c := newBlockedPublishNotifier(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := startNotify(n)
	select {
	case <-done:
	case <-time.After(3 * notifyTimeout):
		t.Fatal("notification blocked indefinitely behind exhausted connection pool")
	}
	if got := db.Stats().WaitCount; got != 1 {
		t.Fatalf("pool waits: got %d, want 1", got)
	}
	select {
	case <-c.started:
		t.Fatal("notification reached backend while the only connection was held")
	default:
	}
}

func TestNotifierPublishBoundsBackendExecution(t *testing.T) {
	n, _, c := newBlockedPublishNotifier(t)
	done := startNotify(n)
	select {
	case <-done:
	case <-time.After(3 * notifyTimeout):
		t.Fatal("notification blocked indefinitely behind an unresponsive backend")
	}
	if err := <-c.stopped; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backend execution returned %v, want deadline exceeded", err)
	}
}

func TestNotifierCloseCancelsPublish(t *testing.T) {
	n, _, c := newBlockedPublishNotifier(t)
	done := startNotify(n)
	select {
	case <-c.started:
	case <-time.After(3 * notifyTimeout):
		t.Fatal("notification did not reach backend")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * notifyTimeout):
		t.Fatal("shutdown did not cancel the in-flight notification")
	}
	if err := <-c.stopped; !errors.Is(err, context.Canceled) {
		t.Fatalf("backend execution returned %v, want cancellation", err)
	}
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
