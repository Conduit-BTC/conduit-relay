package relay

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/michmich112/congee/internal/nostr"
)

const (
	readerQueueCapacity = 1024
	readerPageTimeout   = 15 * time.Second
)

type reqPageJob struct {
	connID        string
	subID         string
	generation    uint64
	state         *reqQueryState
	searchEnabled bool
	pageSize      int
}

// ReaderQueue serializes paginated REQ reads so long snapshots do not block the read loop.
type ReaderQueue struct {
	srv    *Server
	jobs   chan *reqPageJob
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newReaderQueue(srv *Server) *ReaderQueue {
	ctx, cancel := context.WithCancel(srv.metricsCtx)
	return &ReaderQueue{
		srv:    srv,
		jobs:   make(chan *reqPageJob, readerQueueCapacity),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (q *ReaderQueue) start() {
	q.wg.Add(1)
	go q.worker()
}

func (q *ReaderQueue) stop() {
	q.cancel()
	q.wg.Wait()
}

// Enqueue schedules remaining REQ pages. Returns false when the queue is full (caller drains sync).
func (q *ReaderQueue) Enqueue(job *reqPageJob) bool {
	if job == nil {
		return true
	}
	select {
	case q.jobs <- job:
		return true
	default:
		return false
	}
}

// PendingDepth returns the number of REQ page jobs waiting in the queue.
func (q *ReaderQueue) PendingDepth() int {
	return len(q.jobs)
}

func (q *ReaderQueue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case job, ok := <-q.jobs:
			if !ok {
				return
			}
			q.runJob(job)
		}
	}
}

func (q *ReaderQueue) runJob(job *reqPageJob) {
	if job == nil {
		return
	}
	s := q.srv
	v, ok := s.conns.Load(job.connID)
	if !ok {
		return
	}
	c := v.(*Conn)
	if !s.subs.IsSameSnapshot(job.connID, job.subID, job.generation) {
		return
	}

	qctx, cancel := context.WithTimeout(q.ctx, readerPageTimeout)
	t0 := time.Now()
	events, hasMore, err := fetchREQPage(qctx, s.store, job.state, job.pageSize)
	cancel()
	if s.metrics != nil {
		s.metrics.RecordQueryLatency(time.Since(t0))
	}
	if err != nil {
		log := relayLogger(c, q.ctx)
		LogStoreErr(log, zerolog.ErrorLevel, "REQ.QueryPage", err, "req page query failed", func(e *zerolog.Event) {
			e.Str("sub_id", job.subID)
		})
		closeSnapshotOnQueryError(s, c, job.subID, job.generation)
		return
	}

	if !sendSnapshotEvents(q.ctx, s, c, job.subID, job.generation, events) {
		return
	}

	if hasMore {
		if q.Enqueue(job) {
			return
		}
		if !drainRemainingPages(q.ctx, s, c, job.subID, job.state, job.pageSize, job.generation) {
			return
		}
	}

	completeSnapshot(q.ctx, s, c, job.subID, job.generation)
}

// sendSnapshotEvents checks visibility outside the manager lock, then atomically
// checks the generation and enqueues. A replacement invalidates the entire old job.
func sendSnapshotEvents(ctx context.Context, s *Server, c *Conn, subID string, generation uint64, events []*nostr.Event) bool {
	if snapshotCanceled(ctx, c) || !s.subs.IsSameSnapshot(c.ID, subID, generation) {
		return false
	}
	for _, ev := range events {
		if snapshotCanceled(ctx, c) {
			return false
		}
		if !s.EventVisibleToSubscription(c.ID, ev) {
			continue
		}
		var sendErr error
		if !s.subs.withSnapshot(c.ID, subID, generation, func(e *subEntry) {
			if snapshotCanceled(ctx, c) {
				return
			}
			sendErr = c.sendEvent(subID, ev)
			if sendErr == nil {
				e.initialSent.Add(1)
			} else {
				e.initialDropped.Add(1)
			}
		}) {
			return false
		}
		if errors.Is(sendErr, ErrSlowConsumer) {
			log := relayLogger(c, ctx)
			log.Warn().Err(sendErr).Str("sub_id", subID).Str("event_id", ev.ID).Msg("send buffer full: initial event skipped")
		}
	}
	return !snapshotCanceled(ctx, c) && s.subs.IsSameSnapshot(c.ID, subID, generation)
}

func snapshotCanceled(ctx context.Context, c *Conn) bool {
	return ctx.Err() != nil || (c.ctx != nil && c.ctx.Err() != nil)
}

func completeSnapshot(ctx context.Context, s *Server, c *Conn, subID string, generation uint64) error {
	var sendErr error
	s.subs.withSnapshot(c.ID, subID, generation, func(e *subEntry) {
		if ctx.Err() != nil {
			sendErr = ctx.Err()
			return
		}
		if c.ctx != nil && c.ctx.Err() != nil {
			sendErr = c.ctx.Err()
			return
		}
		sendErr = c.sendEOSE(subID)
		if sendErr == nil {
			e.eoseSent.Add(1)
			s.subs.finishSnapshotLocked(c.ID, subID, e)
		}
	})
	return sendErr
}

func closeSnapshotOnQueryError(s *Server, c *Conn, subID string, generation uint64) error {
	var sendErr error
	s.subs.withSnapshot(c.ID, subID, generation, func(*subEntry) {
		sendErr = c.sendClosed(subID, "internal error")
		s.subs.removeLocked(c.ID, subID)
	})
	return sendErr
}

// drainRemainingPages fetches and sends all remaining REQ pages synchronously.
func drainRemainingPages(ctx context.Context, s *Server, c *Conn, subID string, state *reqQueryState, pageSize int, generation uint64) bool {
	for {
		if !s.subs.IsSameSnapshot(c.ID, subID, generation) {
			return false
		}
		qctx, cancel := context.WithTimeout(ctx, readerPageTimeout)
		t0 := time.Now()
		events, hasMore, err := fetchREQPage(qctx, s.store, state, pageSize)
		cancel()
		if s.metrics != nil {
			s.metrics.RecordQueryLatency(time.Since(t0))
		}
		if err != nil {
			log := relayLogger(c, ctx)
			LogStoreErr(log, zerolog.ErrorLevel, "REQ.QueryPage", err, "req page query failed", func(e *zerolog.Event) {
				e.Str("sub_id", subID)
			})
			closeSnapshotOnQueryError(s, c, subID, generation)
			return false
		}
		if !sendSnapshotEvents(ctx, s, c, subID, generation, events) {
			return false
		}

		if !hasMore {
			return true
		}
	}
}
