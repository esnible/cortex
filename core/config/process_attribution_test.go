package config

import (
	"strings"
	"testing"
)

func TestProcessAttributionEnabled(t *testing.T) {
	for _, tc := range []struct {
		value        string
		loopbackOnly bool
		want         bool
	}{
		{"", true, true}, {"", false, false},
		{ProcessAttributionAuto, true, true}, {ProcessAttributionAuto, false, false},
		{ProcessAttributionOn, false, true}, {ProcessAttributionOff, true, false},
	} {
		got := SessionConfig{ProcessAttribution: tc.value}.ProcessAttributionEnabled(tc.loopbackOnly)
		if got != tc.want {
			t.Errorf("process_attribution %q, bind_loopback_only %v: %v, want %v", tc.value, tc.loopbackOnly, got, tc.want)
		}
	}
}

// A typo must not quietly mean auto: it would leave attribution on, or off, by accident.
func TestValidate_RejectsAnUnknownProcessAttribution(t *testing.T) {
	c := &Config{Mode: ModeProxySidecar, Listener: forwardOnlyListener(), Session: SessionConfig{ProcessAttribution: "maybe"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "process_attribution") {
		t.Errorf("Validate = %v, want an error naming session.process_attribution", err)
	}
	c.Session.ProcessAttribution = ProcessAttributionOn
	if err := c.Validate(); err != nil {
		t.Errorf("Validate with %q: %v", ProcessAttributionOn, err)
	}
}
