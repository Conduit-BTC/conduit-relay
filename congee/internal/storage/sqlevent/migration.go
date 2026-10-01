package sqlevent

import (
	"context"
	"net/url"

	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/michmich112/congee/internal/storage/sqlitewriter"
	"github.com/rs/zerolog"
)

var _ storage.MigrationSource = (*Store)(nil)

func (s *Store) BeginMigrationSnapshot(ctx context.Context) (storage.MigrationSnapshot, error) {
	// Use a private-cache connection so a long snapshot does not hold the
	// writer queue's only connection or shared-cache table locks.
	dsn := (&url.URL{Scheme: "file", Path: s.dbPath, RawQuery: "cache=private"}).String()
	_, db, err := sqlitewriter.OpenLibsqlHandles(ctx, dsn, zerolog.Nop())
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &storage.SQLMigrationSnapshot{Tx: tx, CloseDB: db.Close}, nil
}

// MigrationRowCounts returns table row totals for migration verification.
func (s *Store) MigrationRowCounts(ctx context.Context) (storage.MigrationCounts, error) {
	ev, err := s.db().NewSelect().Model((*storage.EventRow)(nil)).Count(ctx)
	if err != nil {
		return storage.MigrationCounts{}, err
	}
	tags, err := s.db().NewSelect().Model((*storage.EventTagRow)(nil)).
		Where("event_id IN (SELECT id FROM events)").
		Count(ctx)
	if err != nil {
		return storage.MigrationCounts{}, err
	}
	return storage.MigrationCounts{
		Events: int64(ev),
		Tags:   int64(tags),
	}, nil
}

// ScanEventsForMigration iterates all events in stable order.
func (s *Store) ScanEventsForMigration(ctx context.Context, fn func(ev *nostr.Event) error) error {
	snapshot, err := s.BeginMigrationSnapshot(ctx)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	return snapshot.ScanEventsForMigration(ctx, fn)
}
