package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/abctl/money"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// moneySweep is a spread of figures across every rung of the money ladders: sub-cent, cents,
// dollars, hundreds, thousands and a million, so a relabelling that only handled one magnitude
// fails somewhere.
var moneySweep = []int64{1, 4_999, 30_000, 7_800_000, 123_450_000, 9_876_000_000, 1_234_567_000_000}

// A session with no Currencies — every dollars-only deployment, and every older server — renders
// exactly what it rendered before units existed, at every width the pane uses.
func TestSessionMoneyCellIn_DollarsAreByteIdentical(t *testing.T) {
	for _, micros := range moneySweep {
		for _, budget := range []int{6, 7, 8, 10, 14} {
			for _, sat := range []bool{false, true} {
				want := sessionMoneyCell(micros, sat, budget)
				for _, cur := range [][]string{nil, {"USD"}} {
					if got := sessionMoneyCellIn(micros, sat, budget, cur); got != want {
						t.Errorf("micros %d budget %d sat %v cur %v: got %q, want %q",
							micros, budget, sat, cur, got, want)
					}
				}
			}
		}
	}
}

// A session priced in one foreign unit shows its figure in that unit, never with "$", and never
// wider than the column.
func TestSessionMoneyCellIn_AForeignUnitNeverWearsADollarSign(t *testing.T) {
	for _, micros := range moneySweep {
		for _, budget := range []int{8, 10, 14} {
			for _, sat := range []bool{false, true} {
				got := sessionMoneyCellIn(micros, sat, budget, []string{"Bobcoins"})
				if strings.Contains(got, "$") {
					t.Errorf("micros %d budget %d: %q is a Bobcoins figure printed as dollars", micros, budget, got)
				}
				if w := lipgloss.Width(got); w > budget {
					t.Errorf("micros %d budget %d: %q is %d wide", micros, budget, got, w)
				}
				if got == emptyCell {
					t.Errorf("micros %d budget %d: a real charge rendered as no figure", micros, budget)
				}
			}
		}
	}
	if got := sessionMoneyCellIn(30_000, false, 14, []string{"Bobcoins"}); got != "0.03 Bobcoins" {
		t.Errorf("wide cell = %q, want 0.03 Bobcoins", got)
	}
}

// A session whose figure spans two units has no amount to show.
func TestSessionMoneyCellIn_AMixedSessionShowsNoAmount(t *testing.T) {
	if got := sessionMoneyCellIn(7_800_000, false, 10, []string{"Bobcoins", "USD"}); got != "(mixed)" {
		t.Errorf("mixed session = %q, want (mixed)", got)
	}
}

// An agent's COST cell is in its own units: dollars as before, Bob's credits labelled as credits,
// and an agent that itself spans units withheld.
func TestAgentCostCellIn_UsesTheAgentsOwnUnits(t *testing.T) {
	c := usage.Counts{PricedRequests: 2, CostMicros: 7_800}
	if got, want := agentCostCellIn(c, nil, agentsCostWidth), agentCostCell(c); got != want {
		t.Errorf("dollars = %q, want %q unchanged", got, want)
	}
	if got := agentCostCellIn(c, []string{"Bobcoins"}, agentsCostWidth); got != "0.01 Bobcoi…" {
		t.Errorf("Bobcoins = %q, want 0.01 Bobcoi…", got)
	}
	if got := agentCostCellIn(c, []string{"Bobcoins", "USD"}, agentsCostWidth); got != "(mixed)" {
		t.Errorf("mixed agent = %q, want (mixed)", got)
	}
	if got := agentCostCellIn(usage.Counts{}, []string{"Bobcoins"}, agentsCostWidth); got != emptyCell {
		t.Errorf("unpriced = %q, want the empty cell whatever the unit", got)
	}
}

// Rows take their units from the server's per-agent cross-tabulation, so Claude Code's dollars are
// not withheld because Bob billed in credits in the same window.
func TestAgentRowsFromSnapshot_LabelsEachAgentWithItsOwnUnits(t *testing.T) {
	snap := &usage.Snapshot{
		Currencies: []string{"Bobcoins", "USD"},
		SeriesCurrencies: map[string][]string{
			"bob-shell/2.0.5": {"Bobcoins"}, "claude-code/2.1.284": {"USD"},
		},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"bob-shell/2.0.5":     {Requests: 3, PricedRequests: 3, CostMicros: 7_800},
			"claude-code/2.1.284": {Requests: 9, PricedRequests: 9, CostMicros: 6_200_000},
		}}},
	}
	for _, r := range agentRowsFromSnapshot(snap) {
		cell := agentCostCellIn(r.Counts, r.units, agentsCostWidth)
		switch r.label {
		case "claude-code/2.1.284":
			if cell != "$6.20" {
				t.Errorf("claude-code = %q, want $6.20", cell)
			}
		case "bob-shell/2.0.5":
			if strings.Contains(cell, "$") || cell == money.Mixed {
				t.Errorf("bob-shell = %q, want a Bobcoins figure", cell)
			}
		}
	}
	// An older server sends no cross-tabulation: a mixed window withholds every row.
	snap.SeriesCurrencies = nil
	for _, r := range agentRowsFromSnapshot(snap) {
		if cell := agentCostCellIn(r.Counts, r.units, agentsCostWidth); cell != money.Mixed {
			t.Errorf("%s from an older server = %q, want (mixed)", r.label, cell)
		}
	}
}
