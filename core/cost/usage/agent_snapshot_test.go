package usage

import (
	"slices"
	"testing"
	"time"
)

// agent= narrows the ring to one agent: its totals, its units, and the echo that tells a client
// the filter was applied. A grouping the narrowed ring cannot carry is downgraded and says so.
func TestAgentSnapshot_NarrowsTheRingToOneAgent(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := twoUnitRing(t, now)
	const bob = "bob-shell/2.0.5"

	snap := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", bob, GroupNone)
	if snap.Agent != bob {
		t.Errorf("Agent = %q, want the %q echo", snap.Agent, bob)
	}
	if snap.Totals.CostMicros != 7_800 {
		t.Errorf("Totals.CostMicros = %d, want Bob's 7800 alone", snap.Totals.CostMicros)
	}
	if !slices.Equal(snap.Currencies, []string{"Bobcoins"}) {
		t.Errorf("Currencies = %v, want [Bobcoins]: Claude's dollars are not Bob's", snap.Currencies)
	}

	byUnit := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", bob, GroupCurrency)
	if byUnit.Group != GroupCurrency || FoldSeriesAcrossWindow(byUnit.Buckets)["Bobcoins"].CostMicros != 7_800 {
		t.Errorf("group=currency = %q %v, want one Bobcoins series of 7800", byUnit.Group, FoldSeriesAcrossWindow(byUnit.Buckets))
	}
	if byModel := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", bob, GroupModel); byModel.Group != GroupNone {
		t.Errorf("group=model under agent= on the ring = %q, want the downgrade to none reported", byModel.Group)
	}

	idle := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", "nobody/1.0", GroupNone)
	if idle.Agent != "nobody/1.0" || idle.Totals.Requests != 0 || idle.Currencies != nil || len(idle.Buckets) != len(snap.Buckets) {
		t.Errorf("an agent with no traffic = %+v, want zeroed buckets, no units, and the echo", idle)
	}
}
