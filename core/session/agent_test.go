package session

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func agentOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	for _, sum := range s.ListSessions() {
		if sum.ID == id {
			return sum.Agent
		}
	}
	t.Fatalf("session %q not listed", id)
	return ""
}

func evFrom(c *pipeline.EventClient) pipeline.SessionEvent {
	e := ev()
	e.Client = c
	return e
}

// A session names the coding agent it belongs to: its affinity owner when one claimed it, else
// the first event from a known agent. The first-wins half is what keeps a Claude Code session
// Claude's when a Bob call is filed into it later, and the owner half is what decides when the
// session's first event came from someone else.
func TestListSessions_NamesTheSessionsAgent(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}

	s.Append("claude-1", evFrom(nil))
	s.Append("claude-1", evFrom(claude))
	s.Append("claude-1", evFrom(bob))
	if got := agentOf(t, s, "claude-1"); got != "claude-code" {
		t.Errorf("claude-1 agent = %q, want claude-code (first known agent wins)", got)
	}

	s.Claim("task-1", "bob-shell")
	s.Append("task-1", evFrom(claude))
	if got := agentOf(t, s, "task-1"); got != "bob-shell" {
		t.Errorf("task-1 agent = %q, want bob-shell (the owner that claimed it)", got)
	}

	webFetch := &pipeline.EventClient{Raw: "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"}
	s.Append("fetch-only", evFrom(webFetch))
	if got := agentOf(t, s, "fetch-only"); got != "claude-code" {
		t.Errorf("fetch-only agent = %q, want claude-code (AffinityName reads the comment)", got)
	}

	for _, id := range []string{DefaultSessionID, PendingSessionID("bob-shell")} {
		s.Append(id, evFrom(bob))
		if got := agentOf(t, s, id); got != "" {
			t.Errorf("%s agent = %q, want none: it is not one agent's session", id, got)
		}
	}
}
