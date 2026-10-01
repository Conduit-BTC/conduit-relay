package postgres

import (
	"context"
	"database/sql"

	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
)

var _ storage.MigrationSource = (*Store)(nil)

func (s *Store) BeginMigrationSnapshot(ctx context.Context) (storage.MigrationSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return &storage.SQLMigrationSnapshot{Tx: tx}, nil
}

// MigrationRowCounts returns table row totals for migration verification.
func (s *Store) MigrationRowCounts(ctx context.Context) (storage.MigrationCounts, error) {
	ev, err := s.db.NewSelect().Model((*storage.EventRow)(nil)).Count(ctx)
	if err != nil {
		return storage.MigrationCounts{}, err
	}
	tags, err := s.db.NewSelect().Model((*storage.EventTagRow)(nil)).
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
func (s *Store) ScanEventsForMigration(ctx context.Context, fn func(*nostr.Event) error) error {
	snapshot, err := s.BeginMigrationSnapshot(ctx)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	return snapshot.ScanEventsForMigration(ctx, fn)
}
