package pricing

import "math"

// Cost prices u at r, returning integer micros — millionths of a dollar.
//
// Micros because usage.Counts.CostMicros is already that unit, and integer
// addition across ring buckets is exact where repeated float addition is not.
//
// ok is false when the request is UNPRICED, which is a different answer from a
// cost of zero. Four ways to be unpriced:
//
//   - A tier that carried tokens has no rate. The invariant: a request is priced
//     only if every tier it used had a rate, so a partial table produces a named
//     gap rather than a total that is quietly too low. This is toolprune's
//     rateFor rule (plugin.go:155-174) promoted — see the plan for the failure it
//     was written against.
//   - No tokens were reported at all. That is unknown usage, not a free request;
//     counting it as priced-zero would dilute the coverage denominator.
//   - A negative count, which is a parser bug or a hostile body. A negative cost
//     would corrode a running total that nothing re-derives.
//   - No rates at all, which is the ProvNone case reaching here directly.
//
// A tier with no rate but no tokens is fine: toolprune's table has no output rate
// at all, and refusing there would unprice every request it measures.
func Cost(r Rates, u Usage) (micros int64, ok bool) {
	if u == (Usage{}) {
		return 0, false
	}
	eff := r.At(u.PromptTotal())
	var usd float64
	for i, n := range u.tokens() {
		if n < 0 {
			return 0, false
		}
		if n == 0 {
			continue
		}
		if !eff.Set[i] {
			return 0, false
		}
		usd += float64(n) * eff.Base[i]
	}
	return int64(math.Round(usd * 1e6)), true
}
