package ledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The past-day decode cache (#1566): a month window on the 31st decoded 31
// files per poll for days that never change. A cached day is served without
// re-reading; a file that changed under the cache decodes fresh; today's
// file, always changing, effectively always re-reads.
func TestDayCache_ServesPastDaysAndInvalidatesOnChange(t *testing.T) {
	clock := func() time.Time { return at }
	dir := t.TempDir()
	w := newTestWriter(t, dir, clock)

	// Two rows on the pinned day — distinct hosts, so they stay distinct
	// rows rather than folding into one.
	w.Record("s", costedEvent(t, "api.one.example.com", "m", 0.01, 10, 10))
	w.Record("s", costedEvent(t, "api.two.example.com", "m", 0.01, 10, 10))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	q := func() int {
		rows, _, err := w.Query(context.Background(), at.Add(-time.Minute), at.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if got := q(); got != 2 {
		t.Fatalf("first query: %d rows, want 2", got)
	}

	// The cache holds the day after one query.
	day := w.store.dayOf(at)
	key := w.store.path(day)
	e, ok := w.store.dayCache[key]
	if !ok {
		t.Fatal("the day was not cached after a query")
	}
	if len(e.rows) != 2 {
		t.Fatalf("cached entry holds %d rows, want 2", len(e.rows))
	}

	// Mark the cached entry as read from disk WITHOUT re-reading: rewrite the
	// file's mtime so the (size, mtime) key no longer matches, and confirm the
	// next query decodes fresh — a repair or manual edit must not be served
	// from the cache.
	path := filepath.Join(dir, filepath.Base(key))
	if err := os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := q(); got != 2 {
		t.Fatalf("query after mtime change: %d rows, want 2 (decoded fresh)", got)
	}
	e2, ok := w.store.dayCache[key]
	if !ok || e2.mtimeNs == e.mtimeNs {
		t.Fatal("the cache entry was not refreshed after the file changed")
	}

	// And appending to the file (today's shape) also invalidates: size moves.
	// The appended line copies a real one with the endpoint swapped, so its
	// timestamp is inside the query window in whatever zone the ledger wrote
	// it — a hand-written UTC instant falls outside a local-time window.
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	lines := strings.SplitAfter(string(b), "\n")
	appended := strings.Replace(lines[len(lines)-2], "api.two.example.com", "api.three.example.com", 1)
	f, aerr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if _, err := f.WriteString(appended); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if got := q(); got != 3 {
		// Show what the fresh decode actually saw, so a wrong hand-written
		// line says so instead of looking like a cache bug.
		b, _ := os.ReadFile(path)
		t.Fatalf("query after append: %d rows, want 3 (size change decodes fresh); file now holds:\n%s", got, b)
	}
}
