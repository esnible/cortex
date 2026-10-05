package tui

import (
	"strings"

	"github.com/rossoctl/cortex/core/session"
)

// pendingBucket splits a pending bucket's id — pending:<agent>, or pending:<agent>@<pid>.<start>
// for one process's — into the agent and the pid, and reports false for any other id.
//
// A pending bucket is where an agent's requests that name no session collect until one does
// (core/session/affinity.go). The server lists it like any session, with no title and no agent,
// so as a bare row it was "pending:openc…": a truncated id that named nothing and read as
// unfinished. The id stays what every request and header keys on; only its rendering changes.
func pendingBucket(id string) (agent, pid string, ok bool) {
	rest, ok := strings.CutPrefix(id, session.PendingPrefix)
	if !ok {
		return "", "", false
	}
	agent, proc, _ := strings.Cut(rest, "@")
	pid, _, _ = strings.Cut(proc, ".")
	return agent, pid, agent != ""
}

// pendingTitle is what a pending bucket's title says when nothing else names it.
func pendingTitle(pid string) string {
	if pid == "" {
		return "calls outside any session"
	}
	return "calls outside any session · pid " + pid
}

// sessionIDCell is the SESSION cell: the id, or a pending bucket's agent. The agent and not
// agent·pid, because SESSION is 14 columns and "claude-code·89949" would lose the pid to the
// cut anyway; the pid is in the title.
func sessionIDCell(id string, w int) string {
	if agent, _, ok := pendingBucket(id); ok {
		return trunc(sanitizeLabel(agent), w)
	}
	return trunc(id, w)
}

// sessionAgentCell is the AGENT cell: the server's agent, or a pending bucket's from its id.
// The server leaves Agent blank on a bucket so that it is counted as no session's agent; the
// cell can still say whose calls they are.
func sessionAgentCell(s session.SessionSummary, w int) string {
	agent := s.Agent
	if a, _, ok := pendingBucket(s.ID); ok && agent == "" {
		agent = a
	}
	return trunc(sanitizeLabel(agent), w)
}
