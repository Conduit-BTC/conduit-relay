package storage

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/michmich112/congee/internal/nostr"
)

// Migrate copies events (with tags) from src to dst.
// Events already present on dst (same event id) are skipped.
// Source counts and rows use one snapshot. The destination may be pre-populated;
// every accepted event is read back, and all source events and tags are counted.
// This copy does not include writes made after the snapshot starts.
// progress is optional; it may be called from the caller's goroutine frequently during the run.
// debug is optional; when non-nil it receives short milestone strings (not per-row).
// On success, the returned summary describes rows copied and final destination totals.
func Migrate(ctx context.Context, src, dst MigrationSource, progress func(MigrationProgress), debug func(string)) (MigrationSummary, error) {
	if progress == nil {
		progress = func(MigrationProgress) {}
	}
	if debug == nil {
		debug = func(string) {}
	}

	ctx = WithBulkMigration(ctx)
	snapshot, err := src.BeginMigrationSnapshot(ctx)
	if err != nil {
		return MigrationSummary{}, fmt.Errorf("migration: source snapshot: %w", err)
	}
	defer snapshot.Close()

	progress(MigrationProgress{Percent: 0, Message: "counting source rows"})
	srcCounts, err := snapshot.MigrationRowCounts(ctx)
	if err != nil {
		return MigrationSummary{}, fmt.Errorf("migration: source counts: %w", err)
	}
	dstStart, err := dst.MigrationRowCounts(ctx)
	if err != nil {
		return MigrationSummary{}, fmt.Errorf("migration: destination baseline counts: %w", err)
	}
	debug(fmt.Sprintf("source_row_counts events=%d tags=%d", srcCounts.Events, srcCounts.Tags))
	debug(fmt.Sprintf("dst_start_counts events=%d tags=%d", dstStart.Events, dstStart.Tags))

	totalSteps := srcCounts.Events
	if totalSteps == 0 {
		totalSteps = 1
	}
	var done int64

	report := func(msg string) {
		p := float64(done) / float64(totalSteps) * 100
		if p > 100 {
			p = 100
		}
		progress(MigrationProgress{Percent: p, Message: msg})
	}

	progress(MigrationProgress{Percent: 1, Message: fmt.Sprintf("copying events (0 / %d)", srcCounts.Events)})
	var evInserted, evSkipped int64
	var tagsAdded int64
	var tagsScanned int64
	verifyEvent := func(ev *nostr.Event) error {
		stored, err := dst.QueryEvents(ctx, []nostr.Filter{{IDs: []string{ev.ID}}})
		if err != nil {
			return fmt.Errorf("migration: verify stored event: %w", err)
		}
		if len(stored) != 1 || !reflect.DeepEqual(stored[0], ev) {
			return fmt.Errorf("migration: destination event verification failed")
		}
		return nil
	}
	err = snapshot.ScanEventsForMigration(ctx, func(ev *nostr.Event) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		tagsScanned += int64(len(ev.Tags))
		exists, err := dst.HasEventID(ctx, ev.ID)
		if err != nil {
			return fmt.Errorf("migration: check event %s: %w", ev.ID, err)
		}
		if exists {
			if err := verifyEvent(ev); err != nil {
				return err
			}
			evSkipped++
			done++
			if srcCounts.Events > 0 {
				report(fmt.Sprintf("events %d / %d (%d skipped)", evInserted+evSkipped, srcCounts.Events, evSkipped))
			}
			return nil
		}
		if err := dst.SaveEvent(ctx, ev); err != nil {
			if !errors.Is(err, ErrStaleReplaceable) {
				return fmt.Errorf("migration: save event %s: %w", ev.ID, err)
			}
			evSkipped++
			done++
			if srcCounts.Events > 0 {
				report(fmt.Sprintf("events %d / %d (%d skipped)", evInserted+evSkipped, srcCounts.Events, evSkipped))
			}
			return nil
		}
		if err := verifyEvent(ev); err != nil {
			return err
		}
		evInserted++
		tagsAdded += int64(len(ev.Tags))
		done++
		if srcCounts.Events > 0 {
			report(fmt.Sprintf("events %d / %d (%d skipped)", evInserted+evSkipped, srcCounts.Events, evSkipped))
		}
		return nil
	})
	if err != nil {
		return MigrationSummary{}, err
	}
	if done != srcCounts.Events || tagsScanned != srcCounts.Tags {
		return MigrationSummary{}, fmt.Errorf("migration: source snapshot totals do not match scanned events and tags")
	}
	debug(fmt.Sprintf("events_copy_done inserted=%d skipped=%d", evInserted, evSkipped))

	progress(MigrationProgress{Percent: 92, Message: "verifying row counts"})
	dstCounts, err := dst.MigrationRowCounts(ctx)
	if err != nil {
		return MigrationSummary{}, fmt.Errorf("migration: destination counts: %w", err)
	}
	debug(fmt.Sprintf("verify_ok dst events=%d tags=%d (tags_added=%d from inserted events)",
		dstCounts.Events, dstCounts.Tags, tagsAdded))

	progress(MigrationProgress{Percent: 100, Message: "done"})
	return MigrationSummary{
		Source:           srcCounts,
		DestinationFinal: dstCounts,
		EventsInserted:   evInserted,
		EventsSkipped:    evSkipped,
		TagsAdded:        tagsAdded,
	}, nil
}
