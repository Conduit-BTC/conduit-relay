package relay

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/michmich112/congee/internal/audit"
	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/nip77"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
)

// NegConnAudit is one open NIP-77 session for admin connection audit.
type NegConnAudit struct {
	SubID            string `json:"sub_id"`
	OpenedUnix       int64  `json:"opened_unix"`
	FilterKinds      []int  `json:"filter_kinds,omitempty"`
	RecordCount      int    `json:"record_count"`
	Rounds           int    `json:"rounds"`
	LastActivityUnix int64  `json:"last_activity_unix"`
}

// RegisterNIP77 registers NEG-* handlers and starts the negentropy worker queue.
func RegisterNIP77(s *Server, _ storage.Store) {
	s.negQueue = newNegQueue(s)
	s.negQueue.start()
	s.negLoadSlots = make(chan struct{}, config.EffectiveNIP77MaxConcurrentLoads(s.cfg))
	for i := 0; i < cap(s.negLoadSlots); i++ {
		s.negLoadSlots <- struct{}{}
	}

	s.RegisterMessageHandler("NEG-OPEN", func(ctx context.Context, c *Conn, msg any) error {
		return handleNEGOpen(ctx, s, c, msg.(*nostr.NegOpenMessage))
	})
	s.RegisterMessageHandler("NEG-MSG", func(ctx context.Context, c *Conn, msg any) error {
		return handleNEGMsg(ctx, s, c, msg.(*nostr.NegMsgMessage))
	})
	s.RegisterMessageHandler("NEG-CLOSE", func(ctx context.Context, c *Conn, msg any) error {
		return handleNEGClose(ctx, s, c, msg.(*nostr.NegCloseMessage))
	})
}

func relayNIP77Enabled(cfg *config.Config) bool {
	return config.NIP77Enabled(cfg)
}

// RelayBusyForNeg reports whether inbound NEG-OPEN should be rejected for REQ backpressure.
func (s *Server) RelayBusyForNeg() bool {
	depth := config.EffectiveNIP77BackpressureReqQueueDepth(s.cfg)
	if depth <= 0 || s.readQueue == nil {
		return false
	}
	return s.readQueue.PendingDepth() >= depth
}

func (s *Server) sendNegErr(c *Conn, subID, reason string) error {
	if s.metrics != nil {
		s.metrics.IncNegErr()
	}
	audit.Enqueue(storage.AuditEntry{
		CreatedAt: time.Now().Unix(),
		Action:    audit.ActionNegErr,
		Detail:    fmt.Sprintf("conn_id=%s sub_id=%s reason=%s", c.ID, subID, audit.SanitizeAuditDetailFragment(reason)),
	})
	b, err := nostr.MarshalRelayNegErr(subID, reason)
	if err != nil {
		return err
	}
	return c.enqueue(b)
}

func (s *Server) sendNegBlocked(c *Conn, subID, reason string) error {
	if s.metrics != nil {
		s.metrics.IncNegBlocked()
	}
	audit.Enqueue(storage.AuditEntry{
		CreatedAt: time.Now().Unix(),
		Action:    audit.ActionNegBlocked,
		Detail:    fmt.Sprintf("conn_id=%s sub_id=%s reason=%s", c.ID, subID, audit.SanitizeAuditDetailFragment(reason)),
	})
	return s.sendNegErr(c, subID, reason)
}

func validateNegFilter(cfg *config.Config, c *Conn, f *nostr.Filter) error {
	if f == nil {
		return fmt.Errorf("blocked: missing filter")
	}
	if f.HasSearch() {
		return fmt.Errorf("blocked: search filters not supported")
	}
	if f.Limit != nil {
		return fmt.Errorf("blocked: limit filters not supported for NEG-OPEN")
	}
	// Negentropy reveals event IDs even when subsequent REQs withhold the events.
	// Do not reconcile gift wraps on this public endpoint, including IDs-only and
	// wildcard filters. Explicit non-gift-wrap kinds remain available for sync.
	if len(f.Kinds) == 0 || slices.Contains(f.Kinds, nip17KindGiftWrap) || slices.Contains(f.Kinds, nip59KindEphemeralGiftWrap) {
		return fmt.Errorf("blocked: gift-wrap kinds are not available for negentropy")
	}
	if subscribeAuthRequired(cfg, []nostr.Filter{*f}) && !c.nip42HasAnyAuth() {
		return fmt.Errorf("auth-required: subscription requires authentication")
	}
	return nil
}

func handleNEGOpen(ctx context.Context, s *Server, c *Conn, msg *nostr.NegOpenMessage) error {
	log := relayLogger(c, ctx)
	subID := msg.SubID

	if !relayNIP77Enabled(s.cfg) {
		return s.sendNegBlocked(c, subID, "blocked: NIP-77 is not enabled")
	}
	if len(subID) > s.cfg.MaxSubscriptionIDLength {
		return s.sendNegBlocked(c, subID, fmt.Sprintf("blocked: %v", ErrSubscriptionIDTooLong))
	}
	if s.RelayBusyForNeg() {
		return s.sendNegBlocked(c, subID, "blocked: relay busy")
	}
	if err := validateNegFilter(s.cfg, c, &msg.Filter); err != nil {
		return s.sendNegBlocked(c, subID, err.Error())
	}

	sess, ok := c.negSessions.reserve(subID, &s.negActiveSessions, config.EffectiveNIP77MaxConcurrentSessions(s.cfg))
	if !ok {
		return s.sendNegBlocked(c, subID, "blocked: too many sync sessions")
	}
	job := &negOpenJob{ctx: ctx, c: c, msg: msg, sess: sess}
	if !s.negQueue.Enqueue(job) {
		c.negSessions.removeIf(subID, sess)
		return s.sendNegBlocked(c, subID, "blocked: sync queue full")
	}
	log.Debug().Str("sub_id", subID).Msg("nip77 neg-open enqueued")
	return nil
}

func (s *Server) runNegOpenJob(job *negOpenJob) {
	c := job.c
	msg := job.msg
	subID := msg.SubID
	log := c.log
	if current, ok := c.negSessions.get(subID); !ok || current != job.sess {
		return
	}
	// Only this job's reservation may be released. Replaced or closed jobs
	// must neither release a newer slot nor emit a response for its ID.
	fail := func(reason string, blocked bool) {
		c.negSessions.mu.Lock()
		defer c.negSessions.mu.Unlock()
		if c.negSessions.byID[subID] == job.sess {
			job.sess.close()
			delete(c.negSessions.byID, subID)
			if blocked {
				_ = s.sendNegBlocked(c, subID, reason)
			} else {
				_ = s.sendNegErr(c, subID, reason)
			}
		}
	}

	select {
	case <-s.negLoadSlots:
	default:
		fail("blocked: sync load capacity reached", true)
		return
	}
	defer func() { s.negLoadSlots <- struct{}{} }()

	maxRec := config.EffectiveNIP77MaxRecordsPerQuery(s.cfg)
	if maxRec > 0 {
		n, err := s.store.CountEvents(job.ctx, []nostr.Filter{msg.Filter})
		if err != nil {
			log.Warn().Err(err).Str("sub_id", subID).Msg("nip77 count failed")
			fail("error: count failed", false)
			return
		}
		if n > maxRec {
			// The unfiltered count may include private group records.
			reason := fmt.Sprintf("blocked: this query exceeds the maximum of %d records", maxRec)
			fail(reason, true)
			return
		}
	}

	t0 := time.Now()
	items, err := s.store.QueryEventSyncItems(job.ctx, msg.Filter)
	if err != nil {
		log.Warn().Err(err).Str("sub_id", subID).Msg("nip77 sync query failed")
		fail("error: query failed", false)
		return
	}
	items, err = s.visibleNegSyncItems(job.ctx, c, msg.Filter, items)
	if err != nil {
		log.Warn().Err(err).Str("sub_id", subID).Msg("nip77 visibility query failed")
		fail("error: query failed", false)
		return
	}
	loadDur := time.Since(t0)
	if s.metrics != nil {
		s.metrics.RecordNegLoadLatency(loadDur)
		s.metrics.IncNegOpen()
	}

	vec := nip77.BuildVector(items)
	frameLimit := config.EffectiveNIP77FrameSizeLimit(s.cfg)
	neg := nip77.NewServerNegentropy(vec, frameLimit)

	t1 := time.Now()
	out, err := neg.Reconcile(msg.InitialHex)
	if err != nil {
		log.Warn().Err(err).Str("sub_id", subID).Msg("nip77 reconcile failed")
		fail("error: "+err.Error(), false)
		return
	}
	if s.metrics != nil {
		s.metrics.RecordNegReconcileLatency(time.Since(t1))
	}

	now := time.Now().Unix()
	b, err := nostr.MarshalRelayNegMsg(subID, out)
	if err != nil {
		fail("error: marshal failed", false)
		return
	}
	sess := job.sess
	c.negSessions.mu.Lock()
	if c.negSessions.byID[subID] != sess {
		c.negSessions.mu.Unlock()
		return
	}
	sess.filter = msg.Filter
	sess.filterKinds = slices.Clone(msg.Filter.Kinds)
	sess.recordCount = len(items)
	sess.openedUnix = now
	sess.lastActUnix = now
	sess.neg = neg
	// Order this response before a subsequent replacement of the same ID.
	// enqueue is nonblocking and never takes the session-map lock.
	if err := c.enqueue(b); err != nil || out == "" {
		sess.close()
		delete(c.negSessions.byID, subID)
	}
	c.negSessions.mu.Unlock()
	if out != "" {
		s.scheduleNegIdle(c, sess)
	}

	log.Info().
		Str("sub_id", subID).
		Ints("filter_kinds", sess.filterKinds).
		Int("record_count", len(items)).
		Int64("load_duration_ms", loadDur.Milliseconds()).
		Msg("nip77 neg-open complete")

	audit.Enqueue(storage.AuditEntry{
		CreatedAt: now,
		Action:    audit.ActionNegOpen,
		Detail:    fmt.Sprintf("conn_id=%s sub_id=%s record_count=%d filter_kinds=%s", c.ID, subID, len(items), negFilterKindsDetail(msg.Filter.Kinds)),
	})

}

// Reconciliation must disclose only IDs the connection can read through REQ.
// Load event metadata in bounded batches rather than retaining every payload.
func (s *Server) visibleNegSyncItems(ctx context.Context, c *Conn, filter nostr.Filter, items []storage.SyncItem) ([]storage.SyncItem, error) {
	if !nip29Enabled(s.cfg) {
		return items, nil
	}
	const batchSize = 256
	visible := make([]storage.SyncItem, 0, len(items))
	// Group policy is shared by all matching events in this reconciliation.
	groupVisibility := make(map[string]bool)
	for start := 0; start < len(items); start += batchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch := items[start:min(start+batchSize, len(items))]
		ids := make([]string, len(batch))
		for i, item := range batch {
			ids[i] = item.ID
		}
		events, err := s.store.QueryEvents(ctx, []nostr.Filter{{IDs: ids}})
		if err != nil {
			return nil, err
		}
		byID := make(map[string]*nostr.Event, len(events))
		for _, ev := range events {
			if ev != nil {
				byID[ev.ID] = ev
			}
		}
		for _, item := range batch {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ev := byID[item.ID]
			if ev == nil || ev.CreatedAt != item.CreatedAt || !filter.Matches(ev) {
				continue // Deleted or changed records cannot disclose stale IDs.
			}
			if !nostr.NIP29GroupHTagsValid(ev) {
				continue // An allowed first group cannot authorize another indexed group.
			}
			h := nostr.NIP29GroupHTag(ev)
			allowed, checked := groupVisibility[h]
			if h == "" || !checked {
				allowed = s.eventVisibleToSubscription(ctx, c.ID, ev)
				if h != "" {
					groupVisibility[h] = allowed
				}
			}
			if allowed {
				visible = append(visible, item)
			}
		}
	}
	return visible, ctx.Err()
}

func (s *Server) scheduleNegIdle(c *Conn, sess *negSession) {
	c.negSessions.mu.Lock()
	defer c.negSessions.mu.Unlock()
	if c.negSessions.byID[sess.subID] != sess {
		return
	}
	if sess.idleCancel != nil {
		sess.idleCancel()
	}
	timeout := time.Duration(config.EffectiveNIP77SessionIdleTimeout(s.cfg)) * time.Second
	if timeout <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(s.metricsCtx)
	sess.idleCancel = cancel
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(timeout):
			if ok := c.negSessions.removeIf(sess.subID, sess); ok {
				c.log.Info().Str("sub_id", sess.subID).Msg("nip77 session idle closed")
				_ = s.sendNegErr(c, sess.subID, "closed: you took too long to respond!")
			}
		}
	}()
}

func handleNEGMsg(ctx context.Context, s *Server, c *Conn, msg *nostr.NegMsgMessage) error {
	log := relayLogger(c, ctx)
	if !relayNIP77Enabled(s.cfg) {
		return s.sendNegBlocked(c, msg.SubID, "blocked: NIP-77 is not enabled")
	}
	if s.metrics != nil {
		s.metrics.IncNegMsg()
	}

	sess, ok := c.negSessions.getReady(msg.SubID)
	if !ok {
		if sess != nil {
			c.negSessions.removeIf(msg.SubID, sess)
		}
		return s.sendNegErr(c, msg.SubID, "closed: unknown subscription")
	}
	c.negSessions.mu.Lock()
	sess.touchActivity()
	c.negSessions.mu.Unlock()
	s.scheduleNegIdle(c, sess)

	t0 := time.Now()
	out, err := sess.neg.Reconcile(msg.MessageHex)
	if err != nil {
		log.Warn().Err(err).Str("sub_id", msg.SubID).Msg("nip77 neg-msg reconcile failed")
		c.negSessions.removeIf(msg.SubID, sess)
		return s.sendNegErr(c, msg.SubID, "error: "+err.Error())
	}
	if s.metrics != nil {
		s.metrics.RecordNegReconcileLatency(time.Since(t0))
	}

	log.Debug().Str("sub_id", msg.SubID).Int("round", sess.rounds).Msg("nip77 neg-msg")

	if out == "" {
		if c.negSessions.removeIf(msg.SubID, sess) {
			audit.Enqueue(storage.AuditEntry{
				CreatedAt: time.Now().Unix(),
				Action:    audit.ActionNegComplete,
				Detail:    fmt.Sprintf("conn_id=%s sub_id=%s rounds=%d", c.ID, msg.SubID, sess.rounds),
			})
		}
	}

	b, err := nostr.MarshalRelayNegMsg(msg.SubID, out)
	if err != nil {
		return s.sendNegErr(c, msg.SubID, "error: marshal failed")
	}
	return c.enqueue(b)
}

func handleNEGClose(ctx context.Context, s *Server, c *Conn, msg *nostr.NegCloseMessage) error {
	_ = ctx
	if removed, _ := c.negSessions.remove(msg.SubID); removed != nil {
		audit.Enqueue(storage.AuditEntry{
			CreatedAt: time.Now().Unix(),
			Action:    audit.ActionNegComplete,
			Detail:    fmt.Sprintf("conn_id=%s sub_id=%s rounds=%d reason=neg-close", c.ID, msg.SubID, removed.rounds),
		})
	}
	return nil
}

func negFilterKindsDetail(kinds []int) string {
	if len(kinds) == 0 {
		return "any"
	}
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = fmt.Sprintf("%d", k)
	}
	return strings.Join(parts, ",")
}
