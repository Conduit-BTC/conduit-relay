package storage

import (
	"context"
	"github.com/michmich112/congee/internal/nostr"
	"github.com/uptrace/bun"
)

// SQLMigrationSnapshot exports a stable database view without changing the source.
type SQLMigrationSnapshot struct {
	Tx      bun.Tx
	CloseDB func() error
}

func (s *SQLMigrationSnapshot) Close() error {
	err := s.Tx.Rollback()
	if s.CloseDB != nil {
		closeErr := s.CloseDB()
		if err == nil {
			err = closeErr
		}
	}
	return err
}

func (s *SQLMigrationSnapshot) MigrationRowCounts(ctx context.Context) (MigrationCounts, error) {
	events, err := s.Tx.NewSelect().Model((*EventRow)(nil)).Count(ctx)
	if err != nil {
		return MigrationCounts{}, err
	}
	tags, err := s.Tx.NewSelect().Model((*EventTagRow)(nil)).Where("event_id IN (SELECT id FROM events)").Count(ctx)
	return MigrationCounts{Events: int64(events), Tags: int64(tags)}, err
}

func (s *SQLMigrationSnapshot) ScanEventsForMigration(ctx context.Context, fn func(*nostr.Event) error) error {
	var lastCA int64
	var lastID string
	started := false
	for {
		var rows []EventRow
		q := s.Tx.NewSelect().Model(&rows).Order("created_at ASC", "id ASC").Limit(500)
		if started {
			q = q.Where("(created_at > ?) OR (created_at = ? AND id > ?)", lastCA, lastCA, lastID)
		}
		if err := q.Scan(ctx); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		var tagRows []EventTagRow
		if err := s.Tx.NewSelect().Model(&tagRows).Where("event_id IN (?)", bun.In(ids)).Order("event_id ASC", "pos ASC").Scan(ctx); err != nil {
			return err
		}
		tags, err := GroupTagRows(tagRows)
		if err != nil {
			return err
		}
		for _, row := range rows {
			eventTags := tags[row.ID]
			if eventTags == nil {
				eventTags = [][]string{}
			}
			ev := &nostr.Event{ID: row.ID, PubKey: row.Pubkey, CreatedAt: row.CreatedAt, Kind: row.Kind, Tags: eventTags, Content: row.Content, Sig: row.Sig}
			if err := fn(ev); err != nil {
				return err
			}
		}
		last := rows[len(rows)-1]
		lastCA, lastID, started = last.CreatedAt, last.ID, true
	}
}
