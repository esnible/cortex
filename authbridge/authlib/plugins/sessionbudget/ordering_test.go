package sessionbudget

import (
	"encoding/json"
	"testing"
)

// TestRequiresLater_OnlyWhenALimitNeedsTheParser guards against the over-reach an
// unconditional requirement would be.
//
// A token or call budget is counted from what inference-parser publishes on the
// response, and the response pass runs in reverse, so the parser must sit LATER in
// the chain. But max_duration_seconds is wall-clock, enforced on the request path,
// and touches Extensions.Inference not at all — so a duration-only budget runs
// correctly with no parser, and demanding one would refuse a configuration that
// works today.
func TestRequiresLater_OnlyWhenALimitNeedsTheParser(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want bool
	}{
		{"tokens", map[string]any{"max_tokens": 1000}, true},
		{"calls", map[string]any{"max_calls": 10}, true},
		{"output tokens", map[string]any{"max_output_tokens": 500}, true},
		{"duration only", map[string]any{"max_duration_seconds": 60}, false},
		{"duration plus tokens", map[string]any{"max_duration_seconds": 60, "max_tokens": 10}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg["redis_url"] = "redis://localhost:6379"
			if _, ok := tc.cfg["max_duration_seconds"]; ok {
				tc.cfg["session_ttl_seconds"] = 7200
			}
			raw, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			p := &SessionBudget{}
			if err := p.Configure(raw); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			got := len(p.Capabilities().RequiresLater) > 0
			if got != tc.want {
				t.Errorf("RequiresLater present = %v, want %v (%+v)", got, tc.want, p.Capabilities().RequiresLater)
			}
		})
	}
}
