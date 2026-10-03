// Package sqlitewriter provides a resilient single-writer queue for SQLite stores.
package sqlitewriter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/uptrace/bun"
)

const (
	DefaultQueueCapacity = 1024
	// DefaultTaskTimeout bounds one writer task; callers blocked on RunWrite are released on timeout.
	DefaultTaskTimeout = 2 * time.Minute
	writerRestartDelay = 250 * time.Millisecond
	// reconnectTimeout caps waiting for an in-flight sqlite/libsql call after a hard
	// task timeout, and the subsequent reconnect once that call has returned.
	reconnectTimeout = 30 * time.Second
)

// HandlesOpener opens a database and returns sql.DB + bun.DB handles.
type HandlesOpener func(ctx context.Context, dsn string, log zerolog.Logger) (*sql.DB, *bun.DB, error)

// Options configures a writer queue.
type Options struct {
	Engine        string // e.g. "turso", "sqlitemeta"
	Log           zerolog.Logger
	DSN           string
	OpenHandles   HandlesOpener
	QueueCapacity int
	TaskTimeout   time.Duration
}

// Queue serializes mutating SQLite/libSQL work on one goroutine with panic recovery,
// hard timeouts (callers unblock; reconnect waits until the native call returns),
// optional reconnect on I/O errors, and debug tracing.
type Queue struct {
	engine      string
	log         zerolog.Logger
	dsn         string
	openHandles HandlesOpener
	sqldb       *sql.DB
	writes      chan *task
	wg          sync.WaitGroup
	cancel      context.CancelFunc
	baseCtx     context.Context
	shutdown    atomic.Bool
	closedErr   error

	dbMu sync.RWMutex
	db   *bun.DB

	taskTimeout   time.Duration
	queueCapacity int
	taskSeq       atomic.Uint64
}

type task struct {
	id    uint64
	label string
	ctx   context.Context
	run   func(ctx context.Context, db bun.IDB) error
	done  chan error
}

// New attaches a writer queue to an already-opened database. Call Close when shutting down.
func New(sqldb *sql.DB, db *bun.DB, opts Options) *Queue {
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = DefaultQueueCapacity
	}
	if opts.TaskTimeout <= 0 {
		opts.TaskTimeout = DefaultTaskTimeout
	}
	log := opts.Log
	if log.GetLevel() == zerolog.NoLevel {
		log = zerolog.Nop()
	}
	log = log.With().Str("component", "sqlitewriter").Str("engine", opts.Engine).Logger()

	baseCtx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		engine:        opts.Engine,
		log:           log,
		dsn:           strings.TrimSpace(opts.DSN),
		openHandles:   opts.OpenHandles,
		sqldb:         sqldb,
		db:            db,
		writes:        make(chan *task, opts.QueueCapacity),
		cancel:        cancel,
		baseCtx:       baseCtx,
		closedErr:     fmt.Errorf("%s: store closed", opts.Engine),
		taskTimeout:   opts.TaskTimeout,
		queueCapacity: opts.QueueCapacity,
	}
	q.wg.Add(1)
	go q.supervisor(baseCtx)
	log.Debug().
		Int("queue_cap", opts.QueueCapacity).
		Dur("task_timeout", opts.TaskTimeout).
		Msg("writer supervisor started")
	return q
}

// DB returns the Bun handle used for concurrent reads. Safe under reconnect.
func (q *Queue) DB() *bun.DB {
	q.dbMu.RLock()
	defer q.dbMu.RUnlock()
	return q.db
}

// RunWrite enqueues one mutating task and blocks until it completes, times out, or the store closes.
func (q *Queue) RunWrite(ctx context.Context, label string, run func(ctx context.Context, db bun.IDB) error) error {
	if q.shutdown.Load() {
		return q.closedErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if label == "" {
		label = "write"
	}
	id := q.taskSeq.Add(1)
	t := &task{
		id:    id,
		label: label,
		ctx:   ctx,
		run:   run,
		done:  make(chan error, 1),
	}
	queueLen := len(q.writes)
	evt := q.log.Debug().
		Str("writer_label", label).
		Uint64("task_id", id).
		Int("queue_len", queueLen).
		Int("queue_cap", q.queueCapacity)
	if queueLen >= q.queueCapacity {
		evt.Msg("writer task enqueue blocked: queue full")
	} else if queueLen >= q.queueCapacity-4 {
		evt.Msg("writer task enqueue: queue nearly full")
	} else {
		evt.Msg("writer task enqueue")
	}
	select {
	case q.writes <- t:
	case <-ctx.Done():
		q.log.Debug().
			Str("writer_label", label).
			Uint64("task_id", id).
			Err(ctx.Err()).
			Int("queue_len", len(q.writes)).
			Msg("writer task enqueue cancelled")
		return ctx.Err()
	case <-q.baseCtx.Done():
		return q.closedErr
	}
	q.log.Debug().
		Str("writer_label", label).
		Uint64("task_id", id).
		Int("queue_len", len(q.writes)).
		Msg("writer task queued")
	select {
	case err := <-t.done:
		if err != nil {
			q.log.Debug().
				Str("writer_label", label).
				Uint64("task_id", id).
				Err(err).
				Msg("writer task finished with error")
		}
		return err
	case <-q.baseCtx.Done():
		return q.closedErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the supervisor and closes the underlying sql.DB.
func (q *Queue) Close() error {
	q.shutdown.Store(true)
	q.cancel()
	q.wg.Wait()
	q.dbMu.Lock()
	defer q.dbMu.Unlock()
	if q.sqldb != nil {
		_ = ExecSQL(context.Background(), q.sqldb, `PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	if q.db != nil {
		_ = q.db.Close()
		q.db = nil
	}
	if q.sqldb != nil {
		err := q.sqldb.Close()
		q.sqldb = nil
		return err
	}
	return nil
}

func (q *Queue) supervisor(ctx context.Context) {
	defer q.wg.Done()
	for {
		q.writerLoop(ctx)
		if q.shutdown.Load() {
			return
		}
		q.log.Error().Msg("writer loop exited unexpectedly; restarting")
		select {
		case <-time.After(writerRestartDelay):
		case <-ctx.Done():
			return
		}
	}
}

func (q *Queue) writerLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			q.log.Error().
				Interface("panic", r).
				Bytes("stack", debug.Stack()).
				Msg("writer loop panic; supervisor will restart")
		}
	}()
	for {
		select {
		case <-ctx.Done():
			q.drainOnShutdown()
			return
		case t := <-q.writes:
			if t == nil {
				continue
			}
			q.executeTask(t)
		}
	}
}

func (q *Queue) drainOnShutdown() {
	for {
		select {
		case t := <-q.writes:
			t.done <- q.closedErr
		default:
			return
		}
	}
}

func (q *Queue) executeTask(t *task) {
	ctx, cancel := context.WithCancel(t.ctx)
	stop := context.AfterFunc(q.baseCtx, cancel)
	defer stop()
	defer cancel()
	queueLen := len(q.writes)
	q.log.Debug().
		Str("writer_label", t.label).
		Uint64("task_id", t.id).
		Int("queue_len", queueLen).
		Int("queue_cap", q.queueCapacity).
		Msg("writer task dequeued")

	var (
		err      error
		notified bool
	)
	notifyCaller := func(e error) {
		if notified {
			return
		}
		notified = true
		t.done <- e
	}
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: writer task panic: %v", q.engine, r)
			q.log.Error().
				Str("writer_label", t.label).
				Uint64("task_id", t.id).
				Interface("panic", r).
				Bytes("stack", debug.Stack()).
				Int64("duration_ms", time.Since(start).Milliseconds()).
				Msg("writer task panic recovered")
		}
		notifyCaller(err)
	}()

	if q.baseCtx.Err() != nil {
		err = q.closedErr
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if pingErr := q.ping(ctx); pingErr != nil {
		if err = ctx.Err(); err != nil {
			return
		}
		q.log.Warn().
			Err(pingErr).
			Str("writer_label", t.label).
			Uint64("task_id", t.id).
			Msg("writer pre-task ping failed; attempting reconnect")
		if reconnErr := q.reconnect(ctx); reconnErr != nil {
			err = fmt.Errorf("%s: ping failed and reconnect failed: ping=%w reconnect=%v", q.engine, pingErr, reconnErr)
			q.log.Error().Err(err).Str("writer_label", t.label).Uint64("task_id", t.id).Msg("writer reconnect failed")
			return
		}
	}

	err = q.runTaskOnce(ctx, t, notifyCaller)
	if err != nil && ctx.Err() == nil && isReconnectable(err) {
		q.log.Warn().
			Err(err).
			Str("writer_label", t.label).
			Uint64("task_id", t.id).
			Msg("writer task failed with reconnectable error; retrying after reconnect")
		if reconnErr := q.reconnect(ctx); reconnErr != nil {
			err = fmt.Errorf("%s: %w (reconnect: %v)", q.engine, err, reconnErr)
			return
		}
		err = q.runTaskOnce(ctx, t, notifyCaller)
	}

	dur := time.Since(start)
	if err != nil {
		evt := q.log.Warn().
			Str("writer_label", t.label).
			Uint64("task_id", t.id).
			Int64("duration_ms", dur.Milliseconds()).
			Err(err)
		if errors.Is(err, context.DeadlineExceeded) {
			evt.Msg("writer task timed out")
		} else {
			evt.Msg("writer task failed")
		}
		return
	}
	q.log.Debug().
		Str("writer_label", t.label).
		Uint64("task_id", t.id).
		Int64("duration_ms", dur.Milliseconds()).
		Msg("writer task completed")
}

func (q *Queue) runTaskOnce(ctx context.Context, t *task, notifyCaller func(error)) error {
	runCtx, cancel := context.WithTimeout(ctx, q.taskTimeout)
	defer cancel()
	if err := runCtx.Err(); err != nil {
		return err
	}
	q.dbMu.RLock()
	db := q.db
	q.dbMu.RUnlock()
	if db == nil {
		return fmt.Errorf("%s: database not open", q.engine)
	}
	q.log.Debug().
		Str("writer_label", t.label).
		Uint64("task_id", t.id).
		Msg("writer task executing sqlite operation")

	type taskResult struct {
		err error
	}
	done := make(chan taskResult, 1)
	go func() {
		var res taskResult
		defer func() {
			if r := recover(); r != nil {
				res.err = fmt.Errorf("%s: writer task panic: %v", q.engine, r)
				q.log.Error().
					Str("writer_label", t.label).
					Uint64("task_id", t.id).
					Interface("panic", r).
					Bytes("stack", debug.Stack()).
					Msg("writer task panic recovered")
			}
			done <- res
		}()
		res.err = t.run(runCtx, db)
	}()

	select {
	case res := <-done:
		return res.err
	case <-runCtx.Done():
		err := runCtx.Err()
		notifyCaller(err)
		q.log.Warn().
			Str("writer_label", t.label).
			Uint64("task_id", t.id).
			Dur("task_timeout", q.taskTimeout).
			Msg("writer task canceled; waiting for in-flight sqlite call")

		waitCtx, waitCancel := context.WithTimeout(context.Background(), reconnectTimeout)
		defer waitCancel()
		select {
		case <-done:
		case <-waitCtx.Done():
			q.log.Error().
				Str("writer_label", t.label).
				Uint64("task_id", t.id).
				Msg("in-flight sqlite call still running after reconnect wait; skip close until it returns")
			<-done
		}

		// Caller cancellation and shutdown do not require replacing a healthy
		// handle. Keep hard-timeout recovery only while the caller and queue live.
		if ctx.Err() != nil {
			return err
		}
		reconnCtx, reconnCancel := context.WithTimeout(ctx, reconnectTimeout)
		defer reconnCancel()
		if reconnErr := q.reconnect(reconnCtx); reconnErr != nil {
			q.log.Error().
				Err(reconnErr).
				Str("writer_label", t.label).
				Uint64("task_id", t.id).
				Msg("writer reconnect after hard timeout failed")
		}
		return err
	}
}

func (q *Queue) ping(parent context.Context) error {
	q.dbMu.RLock()
	sqldb := q.sqldb
	q.dbMu.RUnlock()
	if sqldb == nil {
		return errors.New("sql.DB is nil")
	}
	// Reads and writes share a single native SQLite connection. A busy pool is
	// healthy: probing it must not close the handle underneath the active reader.
	stats := sqldb.Stats()
	if stats.MaxOpenConnections > 0 && stats.InUse >= stats.MaxOpenConnections {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	err := sqldb.PingContext(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		// A reader may acquire the connection after the stats check. Let the
		// write use its existing task deadline instead of reconnecting on contention.
		q.log.Debug().Msg("writer ping timed out waiting for shared connection")
		return nil
	}
	return err
}

// reconnect closes and reopens the database handle. Writer tasks block during reconnect.
func (q *Queue) reconnect(ctx context.Context) error {
	q.dbMu.Lock()
	defer q.dbMu.Unlock()

	q.log.Warn().Str("dsn_len", fmt.Sprint(len(q.dsn))).Msg("writer reconnect begin")

	if q.db != nil {
		_ = q.db.Close()
		q.db = nil
	}
	if q.sqldb != nil {
		_ = q.sqldb.Close()
		q.sqldb = nil
	}

	if q.dsn == "" {
		return errors.New("reconnect: empty dsn")
	}
	if q.openHandles == nil {
		return errors.New("reconnect: no opener configured")
	}

	sqldb, bunDB, err := q.openHandles(ctx, q.dsn, q.log)
	if err != nil {
		return err
	}
	q.sqldb = sqldb
	q.db = bunDB
	q.log.Info().Msg("writer reconnect completed")
	return nil
}

func isReconnectable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	needles := []string{
		"unable to open database",
		"disk i/o error",
		"i/o error",
		"database disk image is malformed",
		"file is not a database",
		"no such file",
		"stale nfs",
		"connection reset",
		"broken pipe",
		"bad file descriptor",
		"invalid connection",
	}
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}
