package sqlitewriter

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/uptrace/bun"
)

func awaitWrite(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("write did not return")
		return nil
	}
}

func TestRunWriteCanceledQueuedTaskDoesNotRun(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t, t.TempDir()+"/queued.db", Options{QueueCapacity: 1})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = q.Close()
	})
	started := make(chan struct{})
	blocker := make(chan error, 1)
	go func() {
		blocker <- q.RunWrite(context.Background(), "blocker", func(context.Context, bun.IDB) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- q.RunWrite(ctx, "canceled-queued", func(context.Context, bun.IDB) error {
			ran.Store(true)
			return nil
		})
	}()
	deadline := time.After(3 * time.Second)
	for len(q.writes) != 1 {
		select {
		case <-deadline:
			t.Fatal("task was not queued")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := awaitWrite(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued caller: want Canceled, got %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := awaitWrite(t, blocker); err != nil {
		t.Fatal(err)
	}
	if err := q.RunWrite(context.Background(), "barrier", func(context.Context, bun.IDB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("canceled queued task executed")
	}
	if err := q.RunWrite(ctx, "already-canceled", func(context.Context, bun.IDB) error {
		ran.Store(true)
		return nil
	}); !errors.Is(err, context.Canceled) || ran.Load() {
		t.Fatalf("already canceled task executed or returned wrong error: %v", err)
	}
}

func TestRunWriteCallerCancellationWaitsForNativeCall(t *testing.T) {
	t.Parallel()
	var reconnects atomic.Int32
	q := newTestQueue(t, t.TempDir()+"/native.db", Options{
		TaskTimeout: time.Minute,
		OpenHandles: func(ctx context.Context, dsn string, log zerolog.Logger) (*sql.DB, *bun.DB, error) {
			reconnects.Add(1)
			return OpenLibsqlHandles(ctx, dsn, log)
		},
	})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = q.Close()
	})
	if err := q.RunWrite(context.Background(), "create-table", func(ctx context.Context, db bun.IDB) error {
		_, err := db.NewRaw("CREATE TABLE canceled_write (value INTEGER)").Exec(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, canceled := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- q.RunWrite(ctx, "native-call", func(ctx context.Context, db bun.IDB) error {
			// Keep rollback synchronous; the task context still controls SQL and
			// the simulated in-flight operation inside the transaction.
			return db.RunInTx(context.WithoutCancel(ctx), nil, func(_ context.Context, tx bun.Tx) error {
				if _, err := tx.NewRaw("INSERT INTO canceled_write VALUES (1)").Exec(ctx); err != nil {
					return err
				}
				close(started)
				select {
				case <-ctx.Done():
					close(canceled)
				case <-release:
					return nil
				}
				// Model a native call that cannot finish immediately on cancellation.
				<-release
				return ctx.Err()
			})
		})
	}()
	<-started
	cancel()
	if err := awaitWrite(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller: want Canceled, got %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("caller cancellation did not reach active task")
	}
	followup := make(chan error, 1)
	go func() {
		followup <- q.RunWrite(context.Background(), "followup", func(context.Context, bun.IDB) error { return nil })
	}()
	select {
	case err := <-followup:
		t.Fatalf("writer advanced before native call returned: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := awaitWrite(t, followup); err != nil {
		t.Fatal(err)
	}
	if reconnects.Load() != 0 {
		t.Fatal("caller cancellation reconnected a healthy handle")
	}
	var count int
	if err := q.DB().NewRaw("SELECT COUNT(*) FROM canceled_write").Scan(context.Background(), &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("canceled transaction committed")
	}
}

func TestCloseCancelsActiveWriteAndSkipsQueuedTask(t *testing.T) {
	t.Parallel()
	q := newTestQueue(t, t.TempDir()+"/shutdown.db", Options{TaskTimeout: time.Minute})
	// This fallback permits cleanup even when cancellation propagation regresses.
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = q.Close()
	})
	started, canceled := make(chan struct{}), make(chan struct{})
	active := make(chan error, 1)
	go func() {
		active <- q.RunWrite(context.Background(), "active", func(ctx context.Context, db bun.IDB) error {
			close(started)
			select {
			case <-ctx.Done():
				close(canceled)
				return ctx.Err()
			case <-release:
				return nil
			}
		})
	}()
	<-started
	var ran atomic.Bool
	queued := make(chan error, 1)
	go func() {
		queued <- q.RunWrite(context.Background(), "queued", func(context.Context, bun.IDB) error {
			ran.Store(true)
			return nil
		})
	}()
	deadline := time.After(3 * time.Second)
	for len(q.writes) == 0 {
		select {
		case <-deadline:
			t.Fatal("task was not queued")
		case <-time.After(time.Millisecond):
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- q.Close() }()
	if err := awaitWrite(t, closed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("shutdown did not cancel the active task")
	}
	if err := awaitWrite(t, active); !errors.Is(err, context.Canceled) && !errors.Is(err, q.closedErr) {
		t.Fatalf("unexpected active write error: %v", err)
	}
	if err := awaitWrite(t, queued); !errors.Is(err, q.closedErr) {
		t.Fatalf("unexpected queued write error: %v", err)
	}
	if ran.Load() {
		t.Fatal("queued task executed during shutdown")
	}
}
