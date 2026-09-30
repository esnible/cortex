package usage

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// agent= narrows the ring to one agent: its totals, its units, and the echo that tells a client
// the filter was applied. A grouping the narrowed ring cannot carry is downgraded and says so.
func TestAgentSnapshot_NarrowsTheRingToOneAgent(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := twoUnitRing(t, now)
	const bob = "bob-shell/2.0.5"

	snap := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", bob)
	if snap.Agent != bob {
		t.Errorf("Agent = %q, want the %q echo", snap.Agent, bob)
	}
	if snap.Totals.CostMicros != 7_800 {
		t.Errorf("Totals.CostMicros = %d, want Bob's 7800 alone", snap.Totals.CostMicros)
	}
	if !slices.Equal(snap.Currencies, []string{"Bobcoins"}) {
		t.Errorf("Currencies = %v, want [Bobcoins]: Claude's dollars are not Bob's", snap.Currencies)
	}

	if snap.Group != GroupNone {
		t.Errorf("Group = %q, want none: the narrowed buckets carry no series", snap.Group)
	}

	idle := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", "nobody/1.0")
	if idle.Agent != "nobody/1.0" || idle.Totals.Requests != 0 || idle.Currencies != nil || len(idle.Buckets) != len(snap.Buckets) {
		t.Errorf("an agent with no traffic = %+v, want zeroed buckets, no units, and the echo", idle)
	}
}

// The agent axis is read uncapped, so an agent the response cap would fold into (other) is found.
func TestAgentSnapshot_FindsAnAgentPastTheSeriesCap(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	for i := range MaxSeriesInResponse + 4 {
		e := withCost(t, respEvent(now, 200, time.Second, "m", 100), float64(MaxSeriesInResponse+4-i))
		e.Client = &pipeline.EventClient{Name: fmt.Sprintf("agent%02d", i), Version: "1"}
		a.Record("s", e)
	}
	const last = "agent19/1"
	if got := a.AgentSnapshot(10*BucketWidth, BucketWidth, "", last); got.Totals.Requests != 1 {
		t.Errorf("%s past the cap: requests = %d, want its 1", last, got.Totals.Requests)
	}
}
