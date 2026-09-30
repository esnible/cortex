package usage

import (
	"testing"
	"time"
)

// TestRekeyed_MovesTheSessionsFiguresToTheAdoptingSession pins what session.Store.Adopt
// relies on: after a pending bucket is renamed, /v1/usage?session=<new> answers with its
// figures and the all-sessions breakdown names the new id, not the pending one.
func TestRekeyed_MovesTheSessionsFiguresToTheAdoptingSession(t *testing.T) {
	a := New()
	a.Record("pending:bob-shell", inferenceEvent("m", 13, 0, 0, 5, 0, 0b1001))
	a.Record("other", inferenceEvent("m", 29, 0, 0, 7, 0, 0b1001))

	a.Rekeyed("pending:bob-shell", "task-1")

	all := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)
	if _, ok := all["pending:bob-shell"]; ok {
		t.Error("all-sessions breakdown still names the adopted pending bucket")
	}
	if got := all["task-1"].InputTokens; got != 13 {
		t.Errorf("task-1 InputTokens = %d, want 13", got)
	}
	if got := all["other"].InputTokens; got != 29 {
		t.Errorf("other InputTokens = %d, want 29 (untouched)", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "task-1", GroupNone).Totals.InputTokens; got != 13 {
		t.Errorf("session=task-1 InputTokens = %d, want 13 (the per-session ring must follow)", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "pending:bob-shell", GroupNone).Totals.InputTokens; got != 0 {
		t.Errorf("session=pending:bob-shell InputTokens = %d, want 0 after the move", got)
	}
}
