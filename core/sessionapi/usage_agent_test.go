package sessionapi

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

func agentResponse(t *testing.T, at time.Time, c *pipeline.EventClient, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(event.Event{CostUSD: costUSD, Settled: true, Source: event.SourceUsageFallback, Provenance: "bundled"})
	if err != nil {
		t.Fatal(err)
	}
	return &pipeline.SessionEvent{At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw", Client: c,
		Inference: &pipeline.InferenceExtension{Model: "opus", InputTokens: 100, OutputTokens: 50, TotalTokens: 150, PresentKinds: 0b1001},
		Plugins:   map[string]json.RawMessage{event.Key: rec}}
}

// agent= narrows both producers to one agent and echoes it; without it the response is unchanged
// and carries no agent key, which is how a client tells an old server that ignored the parameter.
func TestHandleUsage_AgentNarrowsLedgerAndRingWindows(t *testing.T) {
	now := time.Now()
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	led := newTestLedger(t, now)
	agg := usage.New()
	for _, e := range []*pipeline.SessionEvent{agentResponse(t, now, bob, 0.01), agentResponse(t, now, claude, 0.25)} {
		led.Record("s1", e)
		agg.Record("s1", e)
	}
	ts, _ := newTestServer(t, WithUsage(agg), WithCostLedger(led))

	for _, window := range []string{"today", "1h"} {
		_, body := fetchUsage(t, ts.URL, "?window="+window+"&agent="+url.QueryEscape(bob.Label()))
		var snap usage.Snapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("%s: decode: %v (%s)", window, err, body)
		}
		if snap.Agent != bob.Label() || snap.Totals.CostMicros != 10_000 {
			t.Errorf("window=%s agent=bob: Agent=%q CostMicros=%d, want the echo and Bob's 10000 alone",
				window, snap.Agent, snap.Totals.CostMicros)
		}
		if _, plain := fetchUsage(t, ts.URL, "?window="+window); strings.Contains(plain, `"agent"`) {
			t.Errorf("window=%s without agent= carries an agent key: %s", window, plain)
		}
	}
}
