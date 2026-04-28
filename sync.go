package conduitl2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

const (
	defaultBackfillWindow = 7 * 24 * time.Hour
	defaultBackfillSince  = 365 * 24 * time.Hour
	minBackfillWindow     = time.Second
)

var defaultSyncRelays = []string{
	"wss://relay.damus.io",
	"wss://nos.lol",
	"wss://relay.primal.net",
	"wss://relay.plebeian.market",
}

type SyncConfig struct {
	Enabled          bool
	Relays           []string
	StatePath        string
	BackfillSince    time.Time
	BackfillWindow   time.Duration
	LiveLookback     time.Duration
	FetchLimit       int
	ReconnectDelay   time.Duration
	VerifySignatures bool
	Logger           *log.Logger
}

type SyncStats struct {
	Imported   int
	Rejected   int
	Duplicates int
	Ignored    int
}

type syncState struct {
	SyncedUntil int64 `json:"synced_until"`
}

type RelaySyncer struct {
	relay *khatru.Relay
	pool  syncPool
	cfg   SyncConfig

	mu    sync.Mutex
	state syncState
	stats SyncStats
}

type syncPool interface {
	FetchMany(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent
	SubscribeMany(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent
}

func DefaultSyncConfig(statePath string) SyncConfig {
	return SyncConfig{
		Enabled:          false,
		Relays:           slices.Clone(defaultSyncRelays),
		StatePath:        statePath,
		BackfillSince:    time.Now().Add(-defaultBackfillSince).UTC(),
		BackfillWindow:   defaultBackfillWindow,
		LiveLookback:     10 * time.Minute,
		FetchLimit:       500,
		ReconnectDelay:   5 * time.Second,
		VerifySignatures: true,
	}
}

func (cfg *SyncConfig) withDefaults() {
	if cfg.FetchLimit <= 0 {
		cfg.FetchLimit = 500
	}
	if cfg.BackfillWindow <= 0 {
		cfg.BackfillWindow = defaultBackfillWindow
	}
	if cfg.LiveLookback <= 0 {
		cfg.LiveLookback = 10 * time.Minute
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = 5 * time.Second
	}
	if cfg.Relays == nil {
		cfg.Relays = slices.Clone(defaultSyncRelays)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "[conduitl2-sync] ", log.LstdFlags)
	}
}

func StartRelaySync(ctx context.Context, relay *khatru.Relay, cfg SyncConfig) (*RelaySyncer, error) {
	cfg.withDefaults()
	if !cfg.Enabled {
		return nil, nil
	}
	if relay == nil {
		return nil, errors.New("relay sync requires a relay")
	}
	if len(cfg.Relays) == 0 {
		return nil, errors.New("relay sync requires at least one source relay")
	}

	syncer := &RelaySyncer{
		relay: relay,
		pool:  nostr.NewPool(nostr.PoolOptions{PenaltyBox: true}),
		cfg:   cfg,
	}

	if err := syncer.loadState(); err != nil {
		return nil, err
	}

	cfg.Logger.Printf("sync enabled relays=%d backfill_since=%s window=%s live_lookback=%s limit=%d state_path=%s", len(cfg.Relays), syncer.initialBackfillStart().Format(time.RFC3339), cfg.BackfillWindow, cfg.LiveLookback, cfg.FetchLimit, cfg.StatePath)

	go syncer.run(ctx)
	return syncer, nil
}

func (s *RelaySyncer) Stats() SyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *RelaySyncer) run(ctx context.Context) {
	go func() {
		if err := s.backfill(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Printf("backfill stopped: %v", err)
		}
	}()

	if err := s.streamLive(ctx); err != nil && ctx.Err() == nil {
		s.cfg.Logger.Printf("live sync stopped: %v", err)
	}
}

func (s *RelaySyncer) backfill(ctx context.Context) error {
	s.cfg.withDefaults()
	start := s.initialBackfillStart()
	end := time.Now().UTC()
	if !start.Before(end) {
		s.cfg.Logger.Printf("backfill skipped start=%s end=%s", start.Format(time.RFC3339), end.Format(time.RFC3339))
		return nil
	}

	s.cfg.Logger.Printf("backfill starting start=%s end=%s window=%s", start.Format(time.RFC3339), end.Format(time.RFC3339), s.cfg.BackfillWindow)

	window := s.cfg.BackfillWindow
	for start.Before(end) {
		if err := ctx.Err(); err != nil {
			return err
		}

		next := start.Add(window)
		if next.After(end) {
			next = end
		}

		windowImportedBefore := s.Stats().Imported
		windowDuplicateBefore := s.Stats().Duplicates
		windowRejectedBefore := s.Stats().Rejected
		full, err := s.fetchWindow(ctx, start, next)
		if err != nil {
			return err
		}
		stats := s.Stats()
		importedDelta := stats.Imported - windowImportedBefore
		duplicateDelta := stats.Duplicates - windowDuplicateBefore
		rejectedDelta := stats.Rejected - windowRejectedBefore
		full = importedDelta >= s.cfg.FetchLimit
		s.cfg.Logger.Printf("backfill window start=%s end=%s imported=%d duplicates=%d rejected=%d full=%t", start.Format(time.RFC3339), next.Format(time.RFC3339), importedDelta, duplicateDelta, rejectedDelta, full)
		if full && next.Sub(start) > minBackfillWindow {
			window /= 2
			if window < minBackfillWindow {
				window = minBackfillWindow
			}
			s.cfg.Logger.Printf("backfill window saturated; reducing window to %s", window)
			continue
		}

		if err := s.advanceState(next); err != nil {
			return err
		}
		start = next
	}

	s.cfg.Logger.Printf("backfill completed watermark=%s imported=%d duplicates=%d rejected=%d", s.currentWatermark().Format(time.RFC3339), s.Stats().Imported, s.Stats().Duplicates, s.Stats().Rejected)

	return nil
}

func (s *RelaySyncer) streamLive(ctx context.Context) error {
	s.cfg.withDefaults()
	since := time.Now().Add(-s.cfg.LiveLookback)
	if watermark := s.currentWatermark(); !watermark.IsZero() {
		candidate := watermark.Add(-s.cfg.LiveLookback)
		if candidate.After(since) {
			since = candidate
		}
	}

	filter := nostr.Filter{
		Kinds: []nostr.Kind{30402, 5},
		Since: nostr.Timestamp(since.Unix()),
	}
	s.cfg.Logger.Printf("live sync starting since=%s relays=%d", since.UTC().Format(time.RFC3339), len(s.cfg.Relays))

	for ie := range s.pool.SubscribeMany(ctx, s.cfg.Relays, filter, nostr.SubscriptionOptions{Label: "conduitl2-live"}) {
		s.handleIncomingEvent(ctx, ie.Event)
	}

	return ctx.Err()
}

func (s *RelaySyncer) currentWatermark() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SyncedUntil <= 0 {
		return time.Time{}
	}
	return time.Unix(s.state.SyncedUntil, 0).UTC()
}

func (s *RelaySyncer) fetchWindow(ctx context.Context, start, end time.Time) (bool, error) {
	filter := nostr.Filter{
		Kinds: []nostr.Kind{30402, 5},
		Since: nostr.Timestamp(start.Unix()),
		Until: nostr.Timestamp(end.Unix() - 1),
		Limit: s.cfg.FetchLimit,
	}

	count := 0
	for ie := range s.pool.FetchMany(ctx, s.cfg.Relays, filter, nostr.SubscriptionOptions{Label: "conduitl2-backfill"}) {
		count++
		s.handleIncomingEvent(ctx, ie.Event)
	}

	return count >= s.cfg.FetchLimit, nil
}

func (s *RelaySyncer) handleIncomingEvent(ctx context.Context, evt nostr.Event) {
	if evt.Kind == 5 && !targetsProductDeletion(evt) {
		s.recordIgnored()
		return
	}
	if evt.Kind != 30402 && evt.Kind != 5 {
		return
	}
	if !evt.CheckID() {
		s.recordRejected()
		return
	}
	if s.cfg.VerifySignatures && !evt.VerifySignature() {
		s.recordRejected()
		return
	}

	if evt.Kind.IsRegular() && s.hasStoredID(ctx, evt.ID) {
		s.recordDuplicate()
		return
	}
	if evt.Kind.IsReplaceable() || evt.Kind.IsAddressable() {
		current, ok := s.lookupCurrentReplaceable(ctx, evt)
		if ok && !nostr.IsOlder(current, evt) {
			s.recordDuplicate()
			return
		}
	}

	skipBroadcast, err := s.relay.AddEvent(ctx, evt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			s.recordDuplicate()
			return
		}
		s.recordRejected()
		return
	}

	if skipBroadcast {
		s.recordDuplicate()
		return
	}

	s.relay.BroadcastEvent(evt)
	s.recordImported()
}

func (s *RelaySyncer) hasStoredID(ctx context.Context, id nostr.ID) bool {
	if s.relay.QueryStored == nil {
		return false
	}
	for range s.relay.QueryStored(ctx, nostr.Filter{IDs: []nostr.ID{id}, Limit: 1}) {
		return true
	}
	return false
}

func (s *RelaySyncer) lookupCurrentReplaceable(ctx context.Context, evt nostr.Event) (nostr.Event, bool) {
	if s.relay.QueryStored == nil {
		return nostr.Event{}, false
	}

	filter := nostr.Filter{Kinds: []nostr.Kind{evt.Kind}, Authors: []nostr.PubKey{evt.PubKey}, Limit: 16}
	if evt.Kind.IsAddressable() {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	var latest nostr.Event
	found := false
	for current := range s.relay.QueryStored(ctx, filter) {
		if !found || nostr.IsOlder(latest, current) {
			latest = current
			found = true
		}
	}
	return latest, found
}

func (s *RelaySyncer) initialBackfillStart() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	configuredStart := time.Now().Add(-defaultBackfillSince).UTC()
	if !s.cfg.BackfillSince.IsZero() {
		configuredStart = s.cfg.BackfillSince.UTC()
	}
	if s.state.SyncedUntil <= 0 {
		return configuredStart
	}
	persistedStart := time.Unix(s.state.SyncedUntil, 0).UTC()
	if configuredStart.After(persistedStart) {
		return configuredStart
	}
	return persistedStart
}

func (s *RelaySyncer) advanceState(ts time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts.Unix() <= s.state.SyncedUntil {
		return nil
	}
	s.state.SyncedUntil = ts.Unix()
	return writeSyncState(s.cfg.StatePath, s.state)
}

func (s *RelaySyncer) loadState() error {
	state, err := readSyncState(s.cfg.StatePath)
	if err != nil {
		return err
	}
	s.state = state
	return nil
}

func (s *RelaySyncer) recordImported() {
	s.mu.Lock()
	s.stats.Imported++
	s.mu.Unlock()
}

func (s *RelaySyncer) recordRejected() {
	s.mu.Lock()
	s.stats.Rejected++
	s.mu.Unlock()
}

func (s *RelaySyncer) recordDuplicate() {
	s.mu.Lock()
	s.stats.Duplicates++
	s.mu.Unlock()
}

func (s *RelaySyncer) recordIgnored() {
	s.mu.Lock()
	s.stats.Ignored++
	s.mu.Unlock()
}

func targetsProductDeletion(evt nostr.Event) bool {
	for _, tag := range evt.Tags {
		if len(tag) < 2 {
			continue
		}
		if tag[0] == "a" && strings.HasPrefix(tag[1], "30402:") {
			return true
		}
		if tag[0] == "k" && tag[1] == "30402" {
			return true
		}
	}
	return false
}

func readSyncState(path string) (syncState, error) {
	if path == "" {
		return syncState{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return syncState{}, nil
		}
		return syncState{}, fmt.Errorf("read sync state: %w", err)
	}

	var state syncState
	if err := json.Unmarshal(b, &state); err != nil {
		return syncState{}, fmt.Errorf("decode sync state: %w", err)
	}
	return state, nil
}

func writeSyncState(path string, state syncState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create sync state dir: %w", err)
	}
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode sync state: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write sync state: %w", err)
	}
	return nil
}
