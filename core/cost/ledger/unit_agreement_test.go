package ledger

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/pipeline"
)

// The unit settle stamps on a request's record is the unit the writer puts on that request's row.
//
// TWO PRODUCERS OF ONE FACT. The writer asks the Resolver at write time (see the unit block in
// writer.go); settle stamps the record so per-request and per-session surfaces — which read the
// record, not the ledger — can say what unit a figure is in. Should the two ever disagree, the
// sessions pane and the spend band would label the same charge differently, and neither would be
// visibly wrong on its own. Both go through pricing.UnitOf, and this pins that they still do.
//
// wantUnit is asserted too, so the agreement cannot be the trivial one of both sides saying
// nothing: every case that expects a unit has to find it on BOTH sides.
func TestSettleAndWriter_AgreeOnTheUnit(t *testing.T) {
	tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts: []string{"gw.bob"}, Unit: "credits",
		Models: map[string]pricing.ModelConfig{
			"premium-ide": {TierRates: pricing.TierRates{InputCostPerMillion: 2, OutputCostPerMillion: 2}},
		},
	}, {
		Hosts: []string{"gw.usd"}, Unit: "usd",
		Models: map[string]pricing.ModelConfig{
			"m": {TierRates: pricing.TierRates{InputCostPerMillion: 3, OutputCostPerMillion: 5}},
		},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, tc := range []struct {
		name, host, model, wantUnit string
	}{
		{"configured credits gateway", "gw.bob", "premium-ide", "credits"},
		{"model the parser could not read", "gw.bob", "", "credits"},
		{"explicit usd is the default and is not written", "gw.usd", "m", ""},
		{"bundled vendor rate", "api.anthropic.com", "claude-opus-5", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := &pipeline.Context{
				Host: tc.host,
				Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
					Model: tc.model, InputTokens: 100, OutputTokens: 50,
				}},
			}
			rec := settle.NewRecord(settle.Settle(pctx, tbl), nil)
			if rec.Currency != tc.wantUnit {
				t.Fatalf("settle stamped %q, want %q", rec.Currency, tc.wantUnit)
			}

			raw, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			dir := t.TempDir()
			now := at
			w, err := New(dir, WithClock(func() time.Time { return now }), WithPricing(tbl))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })
			w.Record("s1", &pipeline.SessionEvent{
				At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: tc.host,
				Inference: &pipeline.InferenceExtension{
					Model: tc.model, InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
				},
				Plugins: map[string]json.RawMessage{event.Key: raw},
			})
			now = at.Add(time.Minute)
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			rows := readAllRows(t, dir)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
			}
			if rows[0].Currency != rec.Currency {
				t.Errorf("row says %q, record says %q: one charge, two units", rows[0].Currency, rec.Currency)
			}
		})
	}
}
