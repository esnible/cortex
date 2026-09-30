package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A server that predates agent= ignores it and answers for every agent; the fetch then narrows
// client-side, from the group=agent series. A server that applied it is used as it answered, in
// one request.
func TestFetchUsageScoped_NarrowsWhateverTheServerDid(t *testing.T) {
	var hits atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(usageSnapshotJSON))
	}))
	defer old.Close()
	snap, err := fetchUsageScoped(context.Background(), apiclient.New(old.URL), "today", 0, "bob-shell/2.0.5", usage.GroupNone)
	if err != nil || snap.Totals.Requests != 8 {
		t.Fatalf("old server: requests = %v (err %v), want Bob's 8 narrowed client-side", snap, err)
	}

	hits.Store(0)
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"window":"today","agent":"` + r.URL.Query().Get("agent") + `","totals":{"requests":3}}`))
	}))
	defer current.Close()
	snap, err = fetchUsageScoped(context.Background(), apiclient.New(current.URL), "today", 0, "bob-shell/2.0.5", usage.GroupNone)
	if err != nil || snap.Totals.Requests != 3 || hits.Load() != 1 {
		t.Errorf("current server: requests = %v in %d requests (err %v), want its own 3 in one", snap, hits.Load(), err)
	}
}

// The band asks for the scoped agent, and asks exactly as before without one.
func TestFetchSpendSpan_SendsTheScopeAndNothingWithoutOne(t *testing.T) {
	var query atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"window":"today","agent":"` + r.URL.Query().Get("agent") + `","totals":{}}`))
	}))
	defer ts.Close()
	for _, scope := range []string{"", "bob-shell/2.0.5"} {
		m := &model{client: apiclient.New(ts.URL), agentScope: scope}
		m.fetchSpendSpan(spanToday)()
		got, _ := query.Load().(string)
		if has := strings.Contains(got, "agent="); has != (scope != "") {
			t.Errorf("scope %q: query %q", scope, got)
		}
	}
}

func sessionsFixture() []session.SessionSummary {
	now := time.Now()
	return []session.SessionSummary{
		{ID: "claude-1", UpdatedAt: now, Agent: "claude-code"},
		{ID: "task-1", UpdatedAt: now, Agent: "bob-shell"},
		{ID: session.DefaultSessionID, UpdatedAt: now},
	}
}

// Scoped, the sessions pane lists that agent's sessions only; the buckets that belong to no one
// agent go too.
func TestSessionsPane_ScopeListsOnlyThatAgentsSessions(t *testing.T) {
	m := &model{width: 200, sessions: sessionsFixture(), agentScope: "bob-shell/2.0.5",
		events: map[string][]pipeline.SessionEvent{"evicted-1": {{}}}}
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "task-1" {
		t.Errorf("scoped to bob: rows %q, want task-1 alone", got)
	}
	m.agentScope = ""
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "claude-1,task-1,default,evicted-1" {
		t.Errorf("unscoped: rows %q, want every session", got)
	}
}

// AGENT appears only when two agents are listed, so a single-agent layout is today's.
func TestSessionsPane_AgentColumnOnlyWhenTwoAgentsAreListed(t *testing.T) {
	m := &model{width: 200, sessions: sessionsFixture()}
	m.rebuildSessionsTable()
	if sessionsColumnWidth(m.sessionsTbl.Columns(), "AGENT") == 0 {
		t.Errorf("two agents listed and no AGENT column: %v", m.sessionsTbl.Columns())
	}
	one := &model{width: 200, sessions: sessionsFixture()[:1]}
	one.rebuildSessionsTable()
	if want := alignSessionsHeaders(fitTableColumns(sessionsColumnsFor(200), 200)); !sameColumns(one.sessionsTbl.Columns(), want) {
		t.Errorf("one agent: columns %v, want today's %v", one.sessionsTbl.Columns(), want)
	}
}

// Under a scope the drawer's agent axis would be one row, so `a` never lands on it.
func TestCycleSpendAxis_SkipsTheAgentAxisUnderAScope(t *testing.T) {
	m := &model{agentScope: "bob-shell/2.0.5"}
	if slices.Contains(m.spendAxes(), usage.GroupAgent) {
		t.Errorf("the hint's axis cycle %v lists the agent axis under a scope", m.spendAxes())
	}
	m.spend.groupIdx = slices.Index(spendDrawerAxes, usage.GroupAgent)
	if m.spendAxis() == usage.GroupAgent {
		t.Error("a scope set while the drawer was on the agent axis still asks for it")
	}
	for range 2 * len(spendDrawerAxes) {
		m.cycleSpendAxis()
		if m.spend.axis() == usage.GroupAgent {
			t.Fatalf("the axis cycle reached %q under a scope", usage.GroupAgent)
		}
	}
}

// Every pane under the scoped band says it is scoped, and a scope no session belongs to says so
// rather than drawing an empty table.
func TestSessionsPane_ScopedTitleAndTheEmptyScope(t *testing.T) {
	m := fitModel(t, paneSessions, 120, 40, nil)
	m.sessions = sessionsFixture()
	m.agentScope = "Claude-User (claude-code/2.1.284; +https://support.anthropic.com/)"
	m.rebuildSessionsTable()
	view := m.paneView()
	if title := strings.SplitN(view, "\n", 2)[0]; !strings.Contains(title, "agent=Claude-User") {
		t.Errorf("title %q does not name the scope", title)
	}
	if !strings.Contains(view, "no session belongs to") {
		t.Errorf("a raw User-Agent scope matched sessions or said nothing:\n%s", view)
	}
}

// SESSIONS joins the AGENTS pane only once a session names its agent, and counts per agent.
func TestAgentsPane_SessionsColumnOnlyOnceSessionsNameAgents(t *testing.T) {
	m := &model{agentsTbl: newAgentsTable(), agents: []agentRow{{label: "bob-shell/2.0.5"}, {label: "node"}}}
	m.rebuildAgentsTable()
	if !sameColumns(m.agentsTbl.Columns(), fitTableColumns(agentsColumns(), 0)) {
		t.Errorf("no session names an agent and the columns changed: %v", m.agentsTbl.Columns())
	}
	m.sessions = sessionsFixture()
	m.rebuildAgentsTable()
	if rows := m.agentsTbl.Rows(); len(rows) != 2 || rows[0][1] != "1" || rows[1][1] != "–" {
		t.Errorf("SESSIONS cells = %v, want bob's 1 and node's dash", rows)
	}
}

// Enter on an agent restarts the band under the new scope: the next band request names it.
func TestAgentsPane_EnterRestartsTheBandUnderTheScope(t *testing.T) {
	var sawAgent atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") == "bob-shell/2.0.5" {
			sawAgent.Store(true)
		}
		_, _ = w.Write([]byte(`{"window":"today","totals":{}}`))
	}))
	defer ts.Close()
	m := fitModel(t, paneAgents, 120, 40, nil)
	m.client = apiclient.New(ts.URL)
	m.agents = []agentRow{{label: "bob-shell/2.0.5"}, {label: "claude-code/2.1.284"}}
	m.rebuildAgentsTable()
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.agentScope != "bob-shell/2.0.5" || cmd == nil {
		t.Fatalf("scope %q, cmd %v", m.agentScope, cmd)
	}
	runBatchNoWait(cmd)
	if !sawAgent.Load() {
		t.Error("no band request named the new scope")
	}
}

// runBatchNoWait is runBatch for a batch holding ticks: each member runs on its own goroutine and
// the test waits a moment rather than for a tick that blocks for seconds.
func runBatchNoWait(cmd tea.Cmd) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c == nil {
					continue
				}
				go c()
			}
			time.Sleep(200 * time.Millisecond)
		}
	case <-time.After(time.Second):
	}
}

// A pending bucket the server no longer lists was adopted; its cached events live on under the
// session that adopted it, so it is not drawn as a session of its own.
func TestSessionsPane_HidesAnAdoptedPendingBucket(t *testing.T) {
	m := &model{width: 200, sessions: sessionsFixture()[:1], events: map[string][]pipeline.SessionEvent{
		session.PendingSessionID("bob-shell"): {{}},
		"evicted-1":                           {{}},
	}}
	m.rebuildSessionsTable()
	if got := strings.Join(m.sessionRowIDs, ","); got != "claude-1,evicted-1" {
		t.Errorf("rows %q, want the cached eviction but not the adopted pending bucket", got)
	}
}
