package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The ring and the ledger must count the same traffic by the same rules
// (#1566). They used to keep private copies of the error and priceable
// predicates, and only the ring's were ever updated: a truncated OpenAI
// stream (200, model, no usage) read "9/9 priced" on today while the 1h
// window named the gap, and a 403 destination_private was an error on one
// surface and not the other. Both rules now live in core/cost/usage and are
// called from the writer; this drives the REAL writer and reads the rows
// back through Query, so the agreement is the shipped code's, not a
// re-implementation of its rules.
func TestLedger_CountsTheRingShapes(t *testing.T) {
	truncated := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "api.openai.com",
		Inference: &pipeline.InferenceExtension{Model: "gpt-5.1"}, // no usage block
	}
	refusedPrivate := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 403, Host: "internal",
		Inference: &pipeline.InferenceExtension{Model: "m", TotalTokens: 10},
		Error:     &pipeline.EventError{Kind: pipeline.ErrorKindDestinationPrivate},
	}
	failed := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 502, Host: "api.example.com",
		Inference: &pipeline.InferenceExtension{Model: "m"}, // failed, no usage
	}
	healthy := &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "api.example.com",
		Inference: &pipeline.InferenceExtension{Model: "m", TotalTokens: 100},
	}

	for _, tc := range []struct {
		name                   string
		e                      *pipeline.SessionEvent
		wantPriceable, wantErr bool
	}{
		{"truncated OpenAI stream is a coverage gap, not parity", truncated, true, false},
		{"egress-guard refusal is policy working, not an error", refusedPrivate, true, false},
		{"failed with no usage is an error and not priceable", failed, false, true},
		{"healthy inference is priceable and no error", healthy, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := func() time.Time { return at }
			w := newTestWriter(t, t.TempDir(), clock)
			w.Record("s", tc.e)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			rows, _, err := w.Query(context.Background(), at.Add(-time.Minute), at.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("query returned %d rows, want 1", len(rows))
			}
			r := rows[0]
			if (r.PriceableRequests == 1) != tc.wantPriceable {
				t.Errorf("PriceableRequests = %d, want priceable=%v (the ring calls this %v)", r.PriceableRequests, tc.wantPriceable, tc.wantPriceable)
			}
			if (r.Errors == 1) != tc.wantErr {
				t.Errorf("Errors = %d, want error=%v", r.Errors, tc.wantErr)
			}
		})
	}
}
