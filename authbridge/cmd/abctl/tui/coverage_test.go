package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// The three coverage states a cost total can be in. Conflating any two of them is
// how a partial figure gets read as a complete one.
func TestRenderCostSummary_ThreeCoverageStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap usage.Snapshot
		want []string
		deny []string
	}{{
		name: "nothing priced says so instead of showing zero",
		snap: usage.Snapshot{Totals: usage.Counts{Requests: 5}},
		want: []string{"unavailable"},
		// $0.00 would read as "this traffic was free", which is a different claim.
		deny: []string{"$0.0000"},
	}, {
		name: "fully priced shows a bare total",
		snap: usage.Snapshot{
			Priced: true,
			Totals: usage.Counts{Requests: 5, PricedRequests: 5, CostMicros: 1_250_000},
		},
		want: []string{"$1.2500"},
		deny: []string{"priced", "unpriced"},
	}, {
		name: "partially priced discloses the gap and names it",
		snap: usage.Snapshot{
			Priced:     true,
			Totals:     usage.Counts{Requests: 10, PricedRequests: 4, CostMicros: 500_000},
			UnpricedBy: map[string]int64{"api.openai.com gpt-5": 6},
		},
		want: []string{"$0.5000", "4/10", "api.openai.com gpt-5"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCostSummary(&tc.snap)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("rendered %q, missing %q", got, w)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("rendered %q, should not contain %q", got, d)
				}
			}
		})
	}
}

// TestRenderCostSummary_NamesTheBiggestGapFirst: an operator fixing coverage wants
// the entry that buys the most, not an alphabetical list.
func TestRenderCostSummary_NamesTheBiggestGapFirst(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{Requests: 100, PricedRequests: 10, CostMicros: 1},
		UnpricedBy: map[string]int64{
			"a.example small": 2,
			"z.example huge":  87,
			"m.example mid":   1,
		},
	}
	got := renderCostSummary(&snap)
	hi, lo := strings.Index(got, "z.example huge"), strings.Index(got, "a.example small")
	if hi < 0 {
		t.Fatalf("the largest gap was not named: %q", got)
	}
	if lo >= 0 && lo < hi {
		t.Errorf("smaller gap listed before the largest: %q", got)
	}
}

// TestRenderCostSummary_CapsTheGapList keeps one pathological deployment from
// pushing everything else off the line.
func TestRenderCostSummary_CapsTheGapList(t *testing.T) {
	by := map[string]int64{}
	for i := 0; i < 20; i++ {
		by[string(rune('a'+i))+".example m"] = int64(20 - i)
	}
	snap := usage.Snapshot{
		Priced:     true,
		Totals:     usage.Counts{Requests: 300, PricedRequests: 1, CostMicros: 1},
		UnpricedBy: by,
	}
	got := renderCostSummary(&snap)
	if n := strings.Count(got, ".example"); n > 3 {
		t.Errorf("named %d gaps, want at most 3: %q", n, got)
	}
	if !strings.Contains(got, "+17 more") {
		t.Errorf("did not disclose the elided gaps: %q", got)
	}
}
