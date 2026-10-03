package relay

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/michmich112/congee/internal/nostr"
	"github.com/nbd-wtf/go-nostr/nip77/negentropy"
)

type negSession struct {
	subID       string
	filter      nostr.Filter
	filterKinds []int
	recordCount int
	openedUnix  int64
	rounds      int
	lastActUnix int64
	neg         *negentropy.Negentropy
	idleCancel  func()
	active      *atomic.Int32 // owned reservation; released only under the map lock
}

type negSessionMap struct {
	mu     sync.Mutex
	byID   map[string]*negSession
	closed bool
}

func newNegSessionMap() *negSessionMap {
	return &negSessionMap{byID: make(map[string]*negSession)}
}

func (m *negSessionMap) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for id, s := range m.byID {
		s.close()
		delete(m.byID, id)
	}
}

// reserve closes the previous subscription, then atomically acquires a global
// slot. Pending loads live in the map so replacements and closes release them.
func (m *negSessionMap) reserve(subID string, active *atomic.Int32, max int) (*negSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false
	}
	if old, ok := m.byID[subID]; ok {
		old.close()
		delete(m.byID, subID)
	}
	for {
		n := active.Load()
		if int(n) >= max {
			return nil, false
		}
		if active.CompareAndSwap(n, n+1) {
			break
		}
	}
	s := &negSession{subID: subID, active: active}
	m.byID[subID] = s
	return s, true
}

func (s *negSession) close() {
	if s.idleCancel != nil {
		s.idleCancel()
	}
	if s.active != nil {
		s.active.Add(-1)
		s.active = nil
	}
}

func (m *negSessionMap) get(subID string) (*negSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[subID]
	return s, ok
}

func (m *negSessionMap) getReady(subID string) (*negSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[subID]
	return s, ok && s.neg != nil
}

func (m *negSessionMap) remove(subID string) (*negSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[subID]
	if ok {
		s.close()
		delete(m.byID, subID)
	}
	return s, ok
}

// removeIf removes the entry for subID only when it is still the same
// session instance s. This prevents a stale goroutine (idle timer) or a
// reconcile error path from deleting a newer session that reused the subID
// and releasing that session's counter.
func (m *negSessionMap) removeIf(subID string, s *negSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.byID[subID]
	if !ok || cur != s {
		return false
	}
	s.close()
	delete(m.byID, subID)
	return true
}

func (m *negSessionMap) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

func (m *negSessionMap) auditList() []NegConnAudit {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]NegConnAudit, 0, len(m.byID))
	for subID, s := range m.byID {
		out = append(out, NegConnAudit{
			SubID:            subID,
			OpenedUnix:       s.openedUnix,
			FilterKinds:      append([]int(nil), s.filterKinds...),
			RecordCount:      s.recordCount,
			Rounds:           s.rounds,
			LastActivityUnix: s.lastActUnix,
		})
	}
	return out
}

func (s *negSession) touchActivity() {
	s.lastActUnix = time.Now().Unix()
	s.rounds++
}
