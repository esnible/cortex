package session

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

func agentOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	return summaryOf(t, s, id).Agent
}

func evFrom(c *pipeline.EventClient) pipeline.SessionEvent {
	e := ev()
	e.Client = c
	return e
}

// A session names the coding agent it belongs to by the label group=agent files that agent's
// traffic under: its affinity owner's when one claimed it, else the first known agent's. The
// first-wins half is what keeps a Claude Code session Claude's when a Bob call is filed into it
// later, and the owner half is what decides when the session's first event came from someone
// else.
func TestListSessions_NamesTheSessionsAgent(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	claudeNext := &pipeline.EventClient{Name: "claude-code", Version: "2.1.285"}
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}

	s.Append("claude-1", evFrom(nil))
	s.Append("claude-1", evFrom(claude))
	s.Append("claude-1", evFrom(bob))
	if got := agentOf(t, s, "claude-1"); got != "claude-code" {
		t.Errorf("claude-1 agent = %q, want claude-code (first known agent wins)", got)
	}
	s.Append("claude-2", evFrom(claudeNext))
	if got := agentOf(t, s, "claude-2"); got != "claude-code" {
		t.Errorf("claude-2 agent = %q, want claude-code: an agent's releases are one agent", got)
	}

	s.Claim("task-1", "bob-shell")
	s.Append("task-1", evFrom(claude))
	s.Append("task-1", evFrom(bob))
	if got := agentOf(t, s, "task-1"); got != "bob-shell" {
		t.Errorf("task-1 agent = %q, want bob-shell (the owner that claimed it)", got)
	}
	s.Claim("task-2", "bob-shell")
	s.Append("task-2", evFrom(claude))
	if got := agentOf(t, s, "task-2"); got != "" {
		t.Errorf("task-2 agent = %q, want none: its owner sent nothing", got)
	}

	webFetch := &pipeline.EventClient{Raw: "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"}
	s.Append("fetch-only", evFrom(webFetch))
	if got := agentOf(t, s, "fetch-only"); got != webFetch.Label() {
		t.Errorf("fetch-only agent = %q, want %q (AffinityName reads the comment)", got, webFetch.Label())
	}

	s.Claim(DefaultSessionID, "bob-shell")
	for _, id := range []string{DefaultSessionID, PendingSessionID("bob-shell")} {
		s.Append(id, evFrom(bob))
		if got := agentOf(t, s, id); got != "" {
			t.Errorf("%s agent = %q, want none: it is not one agent's session", id, got)
		}
	}
}

// An IBM Bob IDE session names its agent from the User-Agents the IDE really sends, the account
// call included. Unrecognised, these left the session's agent absent, so no AGENTS row could
// reach it (#1210).
func TestListSessions_NamesTheIBMBobIDE(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	for _, ua := range []string{
		"IBM Bob/2.2.1",
		"ai-sdk/openai-compatible/3.0.36 ai-sdk/provider-utils/5.0.29 runtime/node.js/v24.15.0 IBM Bob/2.2.1",
	} {
		s.Append("3981fd731b31c9d1aff4e17e6556f5c6", evFrom(pipeline.ParseUserAgent(ua)))
	}
	if got := agentOf(t, s, "3981fd731b31c9d1aff4e17e6556f5c6"); got != "ibm-bob" {
		t.Errorf("agent = %q, want ibm-bob", got)
	}
}

// The agent a session names is a key the usage ring's group=agent series has for the same
// events, so a client can match one against the other — a long version (folded away) included.
func TestListSessions_AgentIsTheGroupAgentLabel(t *testing.T) {
	clients := []*pipeline.EventClient{
		{Name: "claude-code", Version: "2.1.284"},
		{Name: "bob-shell", Version: strings.Repeat("9", 2*usage.MaxLabelLen)},
		{Raw: "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"},
	}
	s := New(0, 0, 0)
	defer s.Close()
	agg := usage.New()
	for i, c := range clients {
		id := string(rune('a' + i))
		e := evFrom(c)
		e.Phase = pipeline.SessionResponse
		s.Append(id, e)
		agg.Record(id, &e)
	}
	series := usage.FoldSeriesAcrossWindow(agg.Snapshot(usage.BucketWidth, usage.BucketWidth, "", usage.GroupAgent).Buckets)
	for i := range clients {
		id := string(rune('a' + i))
		if got := agentOf(t, s, id); got == "" || series[got].Requests != 1 {
			t.Errorf("session %s agent = %q; group=agent has %v", id, got, slices.Sorted(maps.Keys(series)))
		}
	}
}

// A session that adopted a pending bucket says so, which is how a client tells that bucket's
// cached history — now this session's head — from a bucket that was evicted.
func TestListSessions_NamesTheBucketsASessionAdopted(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	pending := PendingSessionID("bob-shell")
	s.Append(pending, evFrom(bob))
	s.Claim("task-1", "bob-shell")
	s.Append("task-1", evFrom(bob))
	s.Append("claude-1", evFrom(nil))

	if got := summaryOf(t, s, "task-1").Adopted; !slices.Equal(got, []string{pending}) {
		t.Errorf("task-1 adopted %v, want [%s]", got, pending)
	}
	if got := summaryOf(t, s, "claude-1").Adopted; got != nil {
		t.Errorf("claude-1 adopted %v, want none", got)
	}
}
