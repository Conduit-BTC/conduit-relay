package relay

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/michmich112/congee/internal/config"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/rs/zerolog"
)

var (
	// ErrSubscriptionIDTooLong is returned when sub id exceeds configured max.
	ErrSubscriptionIDTooLong = errors.New("relay: subscription id too long")
	// ErrTooManySubscriptions is returned when a connection exceeds its sub cap.
	ErrTooManySubscriptions = errors.New("relay: too many subscriptions")
	// ErrTooManyFilters is returned when a REQ carries too many filters.
	ErrTooManyFilters = errors.New("relay: too many filters per REQ")
)

const pendingLiveCap = 256

// subEntry wraps a subscription's filters with an atomic closed flag.
type subEntry struct {
	filters []nostr.Filter
	closed  atomic.Bool

	generation     uint64
	openedUnix     int64
	initialSent    atomic.Uint64
	initialDropped atomic.Uint64
	broadcastOk    atomic.Uint64
	broadcastDrop  atomic.Uint64
	eoseSent       atomic.Uint32

	snapshotDone atomic.Bool
	pendingLive  [][]byte
	overflow     atomic.Bool
}

// SubscriptionManager tracks REQ subscriptions per connection and broadcasts events.
type SubscriptionManager struct {
	mu             sync.RWMutex
	nextGeneration uint64
	// connID -> subID -> *subEntry
	subs map[string]map[string]*subEntry
	// connID -> enqueue outbound JSON
	senders map[string]func([]byte) bool

	maxSubsPerConn int
	maxSubIDLen    int
	maxFilters     int

	relayLog zerolog.Logger
}

// NewSubscriptionManager builds a manager from relay config slices.
func NewSubscriptionManager(cfg *config.Config, relayLog zerolog.Logger) *SubscriptionManager {
	return &SubscriptionManager{
		subs:           make(map[string]map[string]*subEntry),
		senders:        make(map[string]func([]byte) bool),
		maxSubsPerConn: cfg.ConnectionLimits.MaxSubscriptionsPerConnection,
		maxSubIDLen:    cfg.MaxSubscriptionIDLength,
		maxFilters:     cfg.ConnectionLimits.MaxFiltersPerReq,
		relayLog:       relayLog,
	}
}

// RegisterSender records how to deliver EVENT/EOSE/CLOSED to a connection.
func (m *SubscriptionManager) RegisterSender(connID string, send func([]byte) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.senders[connID] = send
}

// UnregisterSender removes send routing and all subs for connID.
func (m *SubscriptionManager) UnregisterSender(connID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.senders, connID)
	cmap := m.subs[connID]
	delete(m.subs, connID)
	if cmap == nil {
		return nil
	}
	out := make([]string, 0, len(cmap))
	for id, entry := range cmap {
		entry.closed.Store(true)
		out = append(out, id)
	}
	return out
}

// Add creates or replaces a subscription for connID.
func (m *SubscriptionManager) Add(connID, subID string, filters []nostr.Filter) error {
	if len(subID) > m.maxSubIDLen {
		return fmt.Errorf("%w: len %d max %d", ErrSubscriptionIDTooLong, len(subID), m.maxSubIDLen)
	}
	if len(filters) > m.maxFilters {
		return fmt.Errorf("%w: got %d max %d", ErrTooManyFilters, len(filters), m.maxFilters)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cmap := m.subs[connID]
	if cmap == nil {
		cmap = make(map[string]*subEntry)
		m.subs[connID] = cmap
	}
	if _, exists := cmap[subID]; !exists && len(cmap) >= m.maxSubsPerConn {
		return ErrTooManySubscriptions
	}
	if previous := cmap[subID]; previous != nil {
		previous.closed.Store(true)
	}
	m.nextGeneration++
	e := &subEntry{
		generation: m.nextGeneration,
		filters:    filters,
		openedUnix: time.Now().Unix(),
	}
	e.snapshotDone.Store(false)
	cmap[subID] = e
	return nil
}

// Remove drops one subscription.
func (m *SubscriptionManager) Remove(connID, subID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeLocked(connID, subID)
}

func (m *SubscriptionManager) removeLocked(connID, subID string) {
	if cmap := m.subs[connID]; cmap != nil {
		if entry := cmap[subID]; entry != nil {
			entry.closed.Store(true)
		}
		delete(cmap, subID)
		if len(cmap) == 0 {
			delete(m.subs, connID)
		}
	}
}

// TotalSubscriptions returns the number of open REQ subscriptions across all connections.
func (m *SubscriptionManager) TotalSubscriptions() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, cmap := range m.subs {
		n += len(cmap)
	}
	return n
}

// filtersMatch is true if any filter matches the event (NIP-01 OR semantics).
func filtersMatch(filters []nostr.Filter, ev *nostr.Event) bool {
	for i := range filters {
		if filters[i].Matches(ev) {
			return true
		}
	}
	return false
}

// Broadcast delivers EVENT to every matching subscription.
// If visible is nil, all connections receive matching events. Otherwise visible(connID, ev) must be true.
func (m *SubscriptionManager) Broadcast(ev *nostr.Event, visible func(connID string, ev *nostr.Event) bool) {
	// Match first, then perform potentially expensive visibility checks without the
	// manager lock. Entry identity prevents delivery to a replacement subscription.
	type match struct {
		subID string
		entry *subEntry
	}
	matches := make(map[string][]match)
	m.mu.RLock()
	for connID, cmap := range m.subs {
		if m.senders[connID] == nil {
			continue
		}
		for subID, entry := range cmap {
			if !entry.closed.Load() && filtersMatch(entry.filters, ev) {
				matches[connID] = append(matches[connID], match{subID, entry})
			}
		}
	}
	m.mu.RUnlock()
	for connID, subs := range matches {
		if visible != nil && !visible(connID, ev) {
			continue
		}
		for _, sub := range subs {
			b, err := nostr.MarshalRelayEvent(sub.subID, ev)
			if err != nil {
				m.relayLog.Error().Err(err).Str("conn_id", connID).Str("sub_id", sub.subID).
					Str("event_id", ev.ID).Int("kind", ev.Kind).Msg("broadcast marshal relay event failed")
				continue
			}
			m.mu.Lock()
			entry := m.subs[connID][sub.subID]
			send := m.senders[connID]
			if entry != sub.entry || entry.closed.Load() || send == nil {
				m.mu.Unlock()
				continue
			}
			if !entry.snapshotDone.Load() {
				if len(entry.pendingLive) >= pendingLiveCap {
					entry.overflow.Store(true)
				} else {
					entry.pendingLive = append(entry.pendingLive, b)
				}
			} else if send(b) {
				entry.broadcastOk.Add(1)
			} else {
				entry.broadcastDrop.Add(1)
				m.relayLog.Debug().Str("conn_id", connID).Str("sub_id", sub.subID).
					Str("event_id", ev.ID).Msg("broadcast send queue full or closed")
			}
			m.mu.Unlock()
		}
	}
}

// SubGeneration identifies one subscription instance, including replacements in the same second.
func (m *SubscriptionManager) SubGeneration(connID, subID string) (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.subs[connID][subID]
	if e == nil || e.closed.Load() {
		return 0, false
	}
	return e.generation, true
}

// SubOpenedUnix returns the opened_unix stamp for an active subscription.
func (m *SubscriptionManager) SubOpenedUnix(connID, subID string) (int64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cmap := m.subs[connID]
	if cmap == nil {
		return 0, false
	}
	e := cmap[subID]
	if e == nil || e.closed.Load() {
		return 0, false
	}
	return e.openedUnix, true
}

// IsSameSnapshot reports whether connID/subID is still the same subscription generation.
func (m *SubscriptionManager) IsSameSnapshot(connID, subID string, generation uint64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.senders[connID] == nil {
		return false
	}
	cmap := m.subs[connID]
	if cmap == nil {
		return false
	}
	e := cmap[subID]
	if e == nil || e.closed.Load() {
		return false
	}
	return e.generation == generation
}

// withSnapshot serializes a bounded enqueue or completion with subscription replacement.
// The callback must not block or perform storage work.
func (m *SubscriptionManager) withSnapshot(connID, subID string, generation uint64, fn func(*subEntry)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.subs[connID][subID]
	if e == nil || e.closed.Load() || e.generation != generation || m.senders[connID] == nil {
		return false
	}
	fn(e)
	return true
}

// IsOpen reports whether connID/subID is an active subscription with a registered sender.
func (m *SubscriptionManager) IsOpen(connID, subID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.senders[connID] == nil {
		return false
	}
	cmap := m.subs[connID]
	if cmap == nil {
		return false
	}
	e := cmap[subID]
	if e == nil {
		return false
	}
	return !e.closed.Load()
}

// SubCount returns how many active subscriptions a connection has (for tests).
func (m *SubscriptionManager) SubCount(connID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.subs[connID])
}

// NoteSubInitialDelivery records one initial REQ query EVENT delivery attempt for admin audit.
func (m *SubscriptionManager) NoteSubInitialDelivery(connID, subID string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cmap := m.subs[connID]
	if cmap == nil {
		return
	}
	e := cmap[subID]
	if e == nil {
		return
	}
	if ok {
		e.initialSent.Add(1)
	} else {
		e.initialDropped.Add(1)
	}
}

// FinishSnapshot marks the initial REQ snapshot complete and flushes buffered live events.
func (m *SubscriptionManager) FinishSnapshot(connID, subID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.subs[connID][subID]; e != nil && !e.closed.Load() {
		m.finishSnapshotLocked(connID, subID, e)
	}
}

func (m *SubscriptionManager) finishSnapshotLocked(connID, subID string, e *subEntry) {
	e.snapshotDone.Store(true)
	pending := e.pendingLive
	e.pendingLive = nil
	if e.overflow.Load() {
		m.relayLog.Warn().Str("conn_id", connID).Str("sub_id", subID).
			Int("cap", pendingLiveCap).
			Msg("live events dropped during REQ snapshot: pending buffer overflow")
	}
	send := m.senders[connID]
	if send == nil {
		return
	}
	for _, b := range pending {
		if send(b) {
			e.broadcastOk.Add(1)
		} else {
			e.broadcastDrop.Add(1)
			m.relayLog.Debug().Str("conn_id", connID).Str("sub_id", subID).
				Msg("buffered live event send queue full or closed")
		}
	}
}

// NoteSubEOSE records one EOSE sent for a subscription (admin audit).
func (m *SubscriptionManager) NoteSubEOSE(connID, subID string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cmap := m.subs[connID]
	if cmap == nil {
		return
	}
	e := cmap[subID]
	if e == nil {
		return
	}
	e.eoseSent.Add(1)
}

// AuditSubscriptionsForConn returns a snapshot of open subscriptions for admin (connID must be live).
func (m *SubscriptionManager) AuditSubscriptionsForConn(connID string) []SubConnAudit {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cmap := m.subs[connID]
	if cmap == nil {
		return nil
	}
	out := make([]SubConnAudit, 0, len(cmap))
	for sid, e := range cmap {
		out = append(out, e.toAudit(sid))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubID < out[j].SubID })
	return out
}

func kindHintsFromFilters(filters []nostr.Filter) []int {
	seen := make(map[int]struct{})
	var out []int
	for _, f := range filters {
		for _, k := range f.Kinds {
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
			if len(out) >= 16 {
				sort.Ints(out)
				return out
			}
		}
	}
	sort.Ints(out)
	return out
}

func (e *subEntry) toAudit(subID string) SubConnAudit {
	return SubConnAudit{
		SubID:             subID,
		OpenedUnix:        e.openedUnix,
		FilterCount:       len(e.filters),
		Kinds:             kindHintsFromFilters(e.filters),
		InitialSent:       e.initialSent.Load(),
		InitialDropped:    e.initialDropped.Load(),
		BroadcastEnqueued: e.broadcastOk.Load(),
		BroadcastDropped:  e.broadcastDrop.Load(),
		EOSESent:          e.eoseSent.Load(),
	}
}
