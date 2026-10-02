package tui

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// `/` on the sessions pane matches SESSION, TITLE (harvested or served) and AGENT,
// case-insensitively, on listed and cached-only rows alike (#867).
func TestSessionsFilter_MatchesSessionTitleAndAgent(t *testing.T) {
	newModel := func() *model {
		m := newTitleModel(t, map[string]SessionMetadata{
			"aaa-111":    {Title: "Refactor the parser"},
			"cached-333": {Title: "an old cached session"},
		})
		m.sessions = []session.SessionSummary{
			{ID: "aaa-111", Agent: "claude-code"},
			{ID: "bbb-222", Title: "served name", Agent: "weather-agent"},
		}
		m.events["cached-333"] = []pipeline.SessionEvent{{Host: "api.example.com"}}
		return m
	}
	cases := []struct {
		filter string
		want   []string
	}{
		{"", []string{"aaa-111", "bbb-222", "cached-333"}},
		{"bbb", []string{"bbb-222"}},
		{"PARSER", []string{"aaa-111"}},
		{"served", []string{"bbb-222"}},
		{"weather", []string{"bbb-222"}},
		{"Claude", []string{"aaa-111"}},
		{"cached session", []string{"cached-333"}},
		{"nomatch", []string{}},
	}
	for _, tc := range cases {
		m := newModel()
		m.filter = tc.filter
		m.rebuildSessionsTable()
		if !slices.Equal(m.sessionRowIDs, tc.want) {
			t.Errorf("filter %q listed %v, want %v", tc.filter, m.sessionRowIDs, tc.want)
		}
	}
}

// The shared filter input names what it matches on the pane it opened on.
func TestFilterPlaceholder_PerPane(t *testing.T) {
	m := newTestEventsModel(t)
	m.filterInput = newTestFilterInput()
	resetSettingsForTest(t)

	for _, p := range []paneID{paneSessions, paneEvents, panePipeline} {
		m.pane = p
		m.filtering = false
		m.handleKey(keyRune('/'))
		if got, want := m.filterInput.Placeholder, filterPlaceholder(p); got != want {
			t.Errorf("pane %v: placeholder %q, want %q", p, got, want)
		}
	}
	if filterPlaceholder(paneSessions) == filterPlaceholder(paneEvents) {
		t.Error("sessions and events share a placeholder; each should name its own fields")
	}
}
