package storage_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michmich112/congee/internal/nostr"
	"github.com/michmich112/congee/internal/storage"
	"github.com/michmich112/congee/internal/storage/turso"
	"github.com/rs/zerolog"
)

func TestMigrateUsesOneSnapshotDuringConcurrentWrites(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := turso.Open(ctx, srcPath, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	writer, err := turso.Open(ctx, srcPath, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	dst, err := turso.Open(ctx, filepath.Join(dir, "dst.db"), nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	pk := strings.Repeat("b", 64)
	var original *nostr.Event
	for i := 0; i < 501; i++ {
		ev := &nostr.Event{ID: fmt.Sprintf("%064x", i+1), PubKey: pk, CreatedAt: int64(100 + i), Kind: 1, Tags: [][]string{{"x", "original"}}, Sig: strings.Repeat("c", 128)}
		if i == 500 {
			ev.Kind = 0
			original = ev
		}
		if err := src.SaveEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	wrote := false
	summary, err := storage.Migrate(ctx, src, dst, func(p storage.MigrationProgress) {
		if wrote || !strings.HasPrefix(p.Message, "copying events") {
			return
		}
		wrote = true
		backdated := &nostr.Event{ID: strings.Repeat("e", 64), PubKey: pk, CreatedAt: 1, Kind: 1, Sig: strings.Repeat("c", 128)}
		replacement := *original
		replacement.ID = strings.Repeat("f", 64)
		replacement.CreatedAt++
		replacement.Tags = [][]string{{"x", "new"}, {"p", pk}}
		for _, ev := range []*nostr.Event{backdated, &replacement} {
			if err := writer.SaveEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || summary.Source.Events != 501 || summary.Source.Tags != 501 || summary.EventsInserted != 501 {
		t.Fatalf("inconsistent snapshot: %+v wrote=%v", summary, wrote)
	}
	retained, err := dst.QueryEvents(ctx, []nostr.Filter{{IDs: []string{original.ID}}})
	if err != nil || len(retained) != 1 || retained[0].Tags[0][1] != "original" {
		t.Fatalf("snapshot revision missing: err=%v count=%d", err, len(retained))
	}
	live, err := src.MigrationRowCounts(ctx)
	if err != nil || live.Events != 502 || live.Tags != 502 {
		t.Fatalf("concurrent writes did not persist: %+v err=%v", live, err)
	}
}

type droppingMigrationStore struct{ storage.MigrationSource }

func (s droppingMigrationStore) SaveEvent(context.Context, *nostr.Event) error { return nil }

func TestMigrateRejectsUnstoredDestinationEvent(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	src, err := turso.Open(ctx, filepath.Join(dir, "src.db"), nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := turso.Open(ctx, filepath.Join(dir, "dst.db"), nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	ev := &nostr.Event{ID: strings.Repeat("a", 64), PubKey: strings.Repeat("b", 64), CreatedAt: 5, Kind: 1, Sig: strings.Repeat("c", 128)}
	if err := src.SaveEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	done := false
	_, err = storage.Migrate(ctx, src, droppingMigrationStore{dst}, func(p storage.MigrationProgress) { done = done || p.Message == "done" }, nil)
	if err == nil || !strings.Contains(err.Error(), "destination event verification failed") || done {
		t.Fatalf("unverified copy succeeded: err=%v done=%v", err, done)
	}
}

func TestMigrateTursoToTurso(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "a.db")
	dstPath := filepath.Join(dir, "b.db")

	src, err := turso.Open(ctx, srcPath, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	dst, err := turso.Open(ctx, dstPath, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	pk := strings.Repeat("b", 64)
	ev := &nostr.Event{
		ID:        strings.Repeat("a", 64),
		PubKey:    pk,
		CreatedAt: 5,
		Kind:      1,
		Tags:      [][]string{{"p", pk}},
		Content:   "migrated",
		Sig:       strings.Repeat("c", 128),
	}
	if err := src.SaveEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	sum, err := storage.Migrate(ctx, src, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.EventsInserted != 1 || sum.EventsSkipped != 0 {
		t.Fatalf("summary: %+v", sum)
	}

	out, err := dst.QueryEvents(ctx, []nostr.Filter{{IDs: []string{ev.ID}}})
	if err != nil || len(out) != 1 || out[0].Content != "migrated" {
		t.Fatalf("dst query: %v %+v", err, out)
	}
}

func TestMigrateSkipsExistingRowsOnDestination(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	src, err := turso.Open(ctx, srcPath, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	dst, err := turso.Open(ctx, dstPath, nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	pk := strings.Repeat("b", 64)
	ev := &nostr.Event{
		ID:        strings.Repeat("a", 64),
		PubKey:    pk,
		CreatedAt: 5,
		Kind:      1,
		Tags:      [][]string{{"p", pk}, {"x", "y"}},
		Content:   "body",
		Sig:       strings.Repeat("c", 128),
	}
	if err := src.SaveEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	if err := dst.SaveEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	sum, err := storage.Migrate(ctx, src, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.EventsInserted != 0 || sum.EventsSkipped != 1 {
		t.Fatalf("summary: %+v", sum)
	}

	out, err := dst.QueryEvents(ctx, []nostr.Filter{{IDs: []string{ev.ID}}})
	if err != nil || len(out) != 1 || len(out[0].Tags) != 2 {
		t.Fatalf("dst event: %v tags=%d", err, len(out[0].Tags))
	}
}

func TestMigrateSkipsStaleReplaceableRevision(t *testing.T) {
	if !turso.HasDriver() {
		t.Skip("libsql driver not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	src, err := turso.Open(ctx, filepath.Join(dir, "src.db"), nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := turso.Open(ctx, filepath.Join(dir, "dst.db"), nil, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	pk := strings.Repeat("b", 64)
	old := &nostr.Event{ID: strings.Repeat("a", 64), PubKey: pk, CreatedAt: 5, Kind: 0, Sig: strings.Repeat("c", 128)}
	newer := &nostr.Event{ID: strings.Repeat("d", 64), PubKey: pk, CreatedAt: 6, Kind: 0, Sig: strings.Repeat("c", 128)}
	other := &nostr.Event{ID: strings.Repeat("e", 64), PubKey: strings.Repeat("f", 64), CreatedAt: 7, Kind: 0, Sig: strings.Repeat("c", 128)}
	for _, ev := range []*nostr.Event{old, other} {
		if err := src.SaveEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := dst.SaveEvent(ctx, newer); err != nil {
		t.Fatal(err)
	}
	summary, err := storage.Migrate(ctx, src, dst, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.EventsInserted != 1 || summary.EventsSkipped != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	retained, err := dst.QueryEvents(ctx, []nostr.Filter{{Authors: []string{pk}, Kinds: []int{0}}})
	if err != nil || len(retained) != 1 || retained[0].ID != newer.ID {
		t.Fatalf("stale revision replaced destination: %+v err=%v", retained, err)
	}
}
