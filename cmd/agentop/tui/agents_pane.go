package tui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/money"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// agentRow is one coding agent's totals for the whole window, as the AGENTS pane shows them.
//
// The label is pipeline.AgentName of pipeline.EventClient.Label — "claude-code", every release
// together — or the raw User-Agent for an agent the parser did not recognise. CLIENT-ASSERTED AND SPOOFABLE, like every other use
// of that field: a display and scoping key, never an authorization subject.
type agentRow struct {
	label string
	usage.Counts
	// units is what this agent's figures are in, as Snapshot.SeriesCurrencies (or, from an older
	// server, the window's Currencies) reports it; nil is dollars. See agentCostCellIn.
	units []string
}

// agentRowsFromBuckets folds a snapshot's per-agent series into one row per agent.
//
// The snapshot carries per-agent figures PER BUCKET, while this pane shows one row per agent
// for the window, so this fold is the pane's whole data path. Callers pass the buckets from a
// snapshot fetched with usage.GroupAgent; any other grouping yields rows labelled by that
// grouping's keys instead, which is the caller's error to avoid and not something this can
// detect — Bucket.Series does not record which axis produced it.
//
// THE FOLD ITSELF IS usage.FoldSeriesAcrossWindow, not a loop here. It saturates through
// Counts.Add rather than wrapping, and `agentop cost --agent` needs the identical answer — two
// copies would be two definitions of what a window total means. The drawer's rankSeriesByCost
// records what the alternative cost when it did write its own: a wrapped total ranks BELOW a
// ten-micro series, which here would sort the busiest agent to the bottom of the table.
//
// ORDERED BY usage.SortSeriesLabels, not by a comparison written here. `agentop cost --by` ranks
// the same series for the same reason, so the rule has one definition in core and both surfaces
// call it — the tie-break on the label matters more than it sounds, because every unpriced agent
// has CostMicros 0 and until billing units land the label is the entire order for all of them.
// That function's godoc carries why it takes a slice rather than the map.
// agentRowsFromSnapshot is agentRowsFromBuckets with each row's units attached: the agent's own
// SeriesCurrencies entry when the server sent one, else the window's list, so an older server's
// mixed window withholds every row rather than labelling one agent with another's units.
func agentRowsFromSnapshot(snap *usage.Snapshot) []agentRow {
	rows := agentRowsFromBuckets(snap.Buckets)
	for i := range rows {
		units, ok := snap.SeriesCurrencies[rows[i].label]
		if !ok {
			units = snap.Currencies
		}
		rows[i].units = units
	}
	return rows
}

func agentRowsFromBuckets(buckets []usage.Bucket) []agentRow {
	totals := usage.FoldSeriesAcrossWindow(buckets)
	labels := make([]string, 0, len(totals))
	for label := range totals {
		labels = append(labels, label)
	}
	usage.SortSeriesLabels(labels, totals)
	out := make([]agentRow, 0, len(labels))
	for _, label := range labels {
		out = append(out, agentRow{label: label, Counts: totals[label]})
	}
	return out
}

// agentsPaneApplies reports whether the AGENTS pane is worth entering.
//
// FEWER THAN TWO AGENTS SKIPS THE PANE, and this is the property that keeps the feature from
// costing every existing user something. A picker offering one row is a keystroke that cannot
// change what is displayed, and one agent is EVERY deployment today — so entering
// unconditionally would put a new mandatory step in front of everyone to no purpose. Mirrors
// the Namespaces → Pods picker, which is likewise conditional.
//
// ZERO SKIPS TOO, and that case is reachable rather than theoretical: a proxy that has served
// no inference yet reports no agent series, and an empty picker with nothing to select is a
// worse answer than going straight to the view.
//
// A RULE OVER DATA rather than a branch inside the navigation code, so it is assertable
// without driving the TUI and cannot be satisfied by an incidental detail of how panes happen
// to be entered.
func agentsPaneApplies(rows []agentRow) bool {
	return len(rows) >= 2
}

// agentsPaneRefusal is why the pane will not open, or "" when it will.
//
// EXACTLY THE INVERSE OF agentsPaneApplies, pinned by
// TestAgentsPaneRefusal_AgreesWithAgentsPaneApplies. Two functions answering one question is
// how the spend drawer came to print the wrong refusal on two panes: the decision and the
// sentence explaining it drifted. They are separate here only because one is a branch and the
// other is prose, and the test makes the agreement the compiler's-equivalent of enforced.
//
// A REFUSAL MAY NEVER BE SILENT, which is the contract spendDrawerHostPane keeps and which
// this package has twice been bitten by breaking. `A` doing nothing looks like a broken
// binding, and a reader cannot tell that from a pane deciding it had nothing worth showing.
//
// The two cases get DIFFERENT SENTENCES because they are different situations: no agents means
// nothing has been observed yet and waiting may fix it; one agent means the breakdown would
// restate a total the reader already has, and waiting will not change that until a second
// agent appears.
func agentsPaneRefusal(rows []agentRow) string {
	switch len(rows) {
	case 0:
		return "agents: no agent traffic seen in this window yet"
	case 1:
		// Names the agent, so it is visible that a per-agent breakdown would be one row
		// repeating the figure already on screen.
		return "agents: only " + rows[0].label + " has been seen — a breakdown would be one row"
	}
	return ""
}

// agentsFetchTimeout bounds the one request this pane makes. The same 5s the usage pane
// allows itself, matched rather than chosen again: both call GetUsage against the same
// endpoint, and two different bounds on one call would be two different answers to "how long
// before we give up".
const agentsFetchTimeout = 5 * time.Second

// agentsWindow is the span the breakdown covers.
//
// A SYMBOLIC WINDOW, so the figures come from the durable cost ledger rather than the
// six-hour ring. "Which agents have spent what" is a question about a day, and the ring cannot
// answer it — a reader comparing this against `agentop cost` (which defaults to the same window)
// must not find two different denominators. Where the ledger is off the proxy serves the
// longest window it holds and says so in the response, which is the same degradation
// `agentop cost` documents.
const agentsWindow = usage.WindowToday

// agentsOpen says what the reply to a rows fetch is allowed to do with them.
//
// AN ENUM RATHER THAN A BOOL because there are three answers, not two, and the third differs
// from the second only in whether it may speak. `A` is owed an answer either way — a key that
// appears to do nothing is the defect agentsPaneRefusal exists to prevent. The startup gate is
// owed the opposite: nobody asked for it, so it enters or it stays quiet.
type agentsOpen int

const (
	// agentsOpenNever is a background refresh: update the rows, enter nothing, say nothing.
	agentsOpenNever agentsOpen = iota
	// agentsOpenOnPress is an `A` press. It enters, or it flashes the reason it will not.
	agentsOpenOnPress
	// agentsOpenAtStartup is the gate run once per connection. It enters when
	// agentsPaneApplies, and otherwise does nothing AND says nothing — see
	// startupAgentsGateCmd.
	agentsOpenAtStartup
)

// agentRowsLoadedMsg carries a fetched per-agent breakdown back to Update.
//
// NOT agentsLoadedMsg, which is TAKEN — by the Kubernetes namespace picker, whose
// Lister.ListAgents lists agent WORKLOADS. Same word, unrelated meaning; see paneAgents.
type agentRowsLoadedMsg struct {
	rows []agentRow
	err  error
	// open records who asked, so the reply knows whether it may enter the pane and whether it
	// may complain. A field rather than something inferred from the current pane: by the time a
	// reply lands the reader may have moved.
	open agentsOpen
	// from is the pane the `A` press came from, captured AT PRESS TIME and carried here for
	// exactly the reason the field above gives: by the time this reply lands the reader may have
	// moved, so reading m.pane then records a caller the press never had. Meaningless under
	// agentsOpenNever, which enters nothing, and unread on the startup path — see that arm in
	// Update for why it does not consult this field.
	from paneID
}

// fetchAgentRowsCmd requests the per-agent breakdown off the render loop.
//
// group=agent and no agent filter: the pane lists every agent, so the per-agent split arrives
// as Bucket.Series and is folded here.
func (m *model) fetchAgentRowsCmd(open agentsOpen, from paneID) tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), agentsFetchTimeout)
		defer cancel()
		snap, err := client.GetUsageWindow(ctx, agentsWindow, 0, "", usage.GroupAgent)
		if err != nil {
			return agentRowsLoadedMsg{err: err, open: open, from: from}
		}
		return agentRowsLoadedMsg{rows: agentRowsFromSnapshot(snap), open: open, from: from}
	}
}

// agentsColumns is the table's declared layout, at the width it wants on a wide terminal.
//
// A FUNCTION RATHER THAN A LITERAL INSIDE newAgentsTable, matching pipelineColumns and
// catalogColumns, because layout() has to re-fit these on every resize and must do it from
// THESE definitions rather than from the live table's columns — refitting the live ones
// compounds each narrowing, so widening the terminal back up never restores what it took away.
//
// COST is widest because it is the column the pane exists for, and it holds "—" for an agent
// nothing could price, which is every Bob row until the billing-unit work lands.
func agentsColumns() []table.Column {
	return []table.Column{
		{Title: "AGENT", Width: 34},
		{Title: "REQUESTS", Width: 10},
		{Title: "TOKENS", Width: 10},
		{Title: "COST", Width: agentsCostWidth},
	}
}

// startupAgentsGateCmd asks, once per connection, whether this proxy has enough agents on it to
// be worth a picker.
//
// ONE FETCH FROM initSessionView, which is the single place every entry point converges on:
// `--endpoint` mode's Init, the pod picker's portForwardReadyMsg, and `[l]`'s local endpoint all
// call it, and each replaces m.client first. Hooking it there rather than in Init is what makes
// the gate run again when the operator backs out to the pod picker and enters a DIFFERENT pod —
// a different proxy has different agents on it, and the answer from the previous one is not an
// answer about this one. The spend strip's chain is started from the same place for the same
// reason.
//
// NO MEMORY OF PREVIOUS ANSWERS, deliberately: the decision is a pure function of what the
// window currently shows. The Namespaces → Pods picker remembers nothing either, and a
// remembered dismissal would go stale exactly when it mattered — the moment a second agent
// appears is the moment the picker becomes worth showing.
//
// THE FIRST FRAME IS NOT BLOCKED. This returns a tea.Cmd like every other fetch, so the sessions
// pane paints and streams while the answer is in flight; entering AGENTS is something that
// happens a beat later, if it happens. A gate that waited would add its own latency to every
// startup, including the majority that it declines.
func (m *model) startupAgentsGateCmd() tea.Cmd {
	// paneNone: this gate has no caller pane. It interrupts the sessions view before the
	// operator has pressed anything, so there is no press-time pane to record — and the esc
	// arm's paneNone fallback already lands on Sessions, which that arm documents as the one
	// pane always defensible to land on. The reply handler does not read this field on the
	// startup path at all; see the agentsOpenAtStartup case in Update for why not.
	return m.fetchAgentRowsCmd(agentsOpenAtStartup, paneNone)
}

// newAgentsTable builds an empty per-agent breakdown table.
func newAgentsTable() table.Model {
	t := table.New(
		table.WithColumns(agentsColumns()),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildAgentsTable rebuilds rows from m.agents.
func (m *model) rebuildAgentsTable() {
	// Columns and rows change together, as in rebuildSessionsTable: SESSIONS appears only once a
	// session names its agent, so a server that names none shows the table unchanged.
	withSessions := m.sessionsNameAgents()
	cursor := m.agentsTbl.Cursor()
	cols := agentsColumns()
	if withSessions {
		cols = append(cols[:1:1], append([]table.Column{{Title: "SESSIONS", Width: 8}}, cols[1:]...)...)
	}
	if want := fitTableColumns(cols, m.width); !sameColumns(m.agentsTbl.Columns(), want) {
		m.agentsTbl.SetRows(nil)
		m.agentsTbl.SetColumns(want)
	}
	rows := make([]table.Row, 0, len(m.agents))
	for _, a := range m.agents {
		rows = append(rows, table.Row{
			// SANITISED AT RENDER TIME. The label is a User-Agent, so it is
			// request-controlled; tui.sanitizeLabel is the package's render-time copy of the
			// rule, named as such in ledger's own comment.
			sanitizeLabel(a.label),
			formatCount(int(a.Requests)),
			humanizeCount(a.Tokens),
			agentCostCellIn(a.Counts, a.units, agentsCostWidth),
		})
		if withSessions {
			r := rows[len(rows)-1]
			rows[len(rows)-1] = append(r[:1:1], append(table.Row{m.agentSessionsCell(a.label)}, r[1:]...)...)
		}
	}
	m.agentsTbl.SetRows(rows)
	setCursorVisible(&m.agentsTbl, cursor)
}

// agentCostCell renders one agent's cost, or "—" when nothing priced it.
//
// "—" AND NEVER "$0.00", which is this codebase's standing rule and the reason
// SessionSummary.CostMicros is omitempty: an agent whose traffic nothing could price is not an
// agent that spent nothing. Bob is exactly that case today — it bills in credits, which the
// cost model cannot yet represent — so every Bob row reads "—" rather than claiming it was
// free.
//
// PricedRequests is the test, not CostMicros, because a genuine zero is possible: a request
// can be priced at a rate of zero. Reading the money field instead would collapse "priced, and
// it cost nothing" into "not priced".
func agentCostCell(c usage.Counts) string {
	if c.PricedRequests == 0 {
		return emptyCell
	}
	// formatUSDTotalMicros, not %.2f over micros/1e6: it does the rounding on the integer, so
	// 1_005_000 micros renders $1.01 rather than the $1.00 a float64 %.2f produces. It also
	// carries the floor that keeps a known sub-cent charge from printing as free.
	return formatUSDTotalMicros(c.CostMicros)
}

// agentsCostWidth is the COST column's declared width, the budget a relabelled figure fits.
const agentsCostWidth = 12

// agentCostCellIn is agentCostCell in the agent's own units: dollars (nil, or USD) exactly as
// before, one foreign unit relabelled to fit the column, and more than one withheld as
// money.Mixed — an agent's figure summed across units is not an amount.
func agentCostCellIn(c usage.Counts, units []string, budget int) string {
	cell := agentCostCell(c)
	if cell == emptyCell {
		return cell
	}
	unit, ok := money.WindowUnit(units)
	if !ok {
		return money.Mixed
	}
	if out := money.Relabel(cell, unit, budget); out != "" {
		return out
	}
	return emptyCell
}

// enterAgentsOrRefuse opens the pane, or returns the reason it will not.
//
// ONE DECISION POINT for every caller, so no two can drift on what counts as available. The
// refusal string is agentsPaneRefusal's, never rephrased here.
//
// `from` IS PASSED IN, NOT READ OFF m.pane. This runs when the reply lands, and by then the
// reader may have moved or may already be standing on AGENTS — reading the current pane here
// recorded `paneAgents` as its own caller on a refetch, which left the first esc silently inert
// against the rule paneCatalog's esc arm states. keys.go's `case "A":` resolves the caller at
// press time, the way `case "C":` does, and agentRowsLoadedMsg.from carries it across the
// round trip.
func (m *model) enterAgentsOrRefuse(from paneID) (entered bool, refusal string) {
	if why := agentsPaneRefusal(m.agents); why != "" {
		return false, why
	}
	m.previousPane = from
	m.pane = paneAgents
	m.rebuildAgentsTable()
	return true, ""
}

// selectedAgentLabel is the label of the row under the cursor, or "" when there is none.
//
// READ OFF m.agents BY CURSOR INDEX, not out of the rendered table cell: the cell is passed
// through sanitizeLabel, which is a display transform — a control character or a long label
// arrives on the wire and leaves that function altered, so scoping to what the cell says could
// scope to a string no agent ever sent. The two are kept in step by rebuildAgentsTable, which
// builds the rows from m.agents in order.
func (m *model) selectedAgentLabel() string {
	i := m.agentsTbl.Cursor()
	if i < 0 || i >= len(m.agents) {
		return ""
	}
	return m.agents[i].label
}

// leaveAgentsPane returns to whichever pane opened the AGENTS pane.
//
// ONE EXIT FOR BOTH KEYS — esc backs out, Enter picks an agent and then backs out — so the two
// cannot drift on where the pane returns to. A key-opened surface owes its caller a way back;
// without an exit at all this pane was a dead end reachable only by `q`.
//
// THE FALLBACK IS SESSIONS, and for paneCatalog's stated reason rather than by imitation:
// Sessions is the one pane that is always a defensible place to land, while the enum's zero
// value is the Kubernetes namespace picker, which would look like the connection had gone away.
// The startup gate leans on this fallback deliberately — it records paneNone because it has no
// caller pane at all.
//
// RETURNING INTO USAGE RESTARTS ITS POLLING CHAIN. This pane holds no ticker of its own, but the
// usage pane's tick was dropped by its `m.pane != paneUsage` guard while this pane was up, so
// without the resume its 20s auto-refresh is silently dead. It matters more now than it did:
// Enter changes what the usage pane is showing, so landing back on a pane that never refetches
// would leave the new scope unapplied until the operator pressed something.
func (m *model) leaveAgentsPane() tea.Cmd {
	if m.previousPane != paneNone {
		m.pane = m.previousPane
		m.previousPane = paneNone
	} else {
		m.pane = paneSessions
	}
	if m.pane == paneUsage {
		return m.resumeUsagePolling()
	}
	return nil
}

// agentSessionsCell counts the listed sessions that belong to the agent a row names, or a dash
// where none does.
func (m *model) agentSessionsCell(label string) string {
	n := 0
	for _, s := range m.sessions {
		if s.Agent == label {
			n++
		}
	}
	if n == 0 {
		return emptyCell
	}
	return formatCount(n)
}

// sessionsNameAgents reports whether any listed session names its agent.
func (m *model) sessionsNameAgents() bool {
	for _, s := range m.sessions {
		if s.Agent != "" {
			return true
		}
	}
	return false
}
