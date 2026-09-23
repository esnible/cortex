package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// reasoningCounts is tierCounts plus a reported reasoning split: 948 of 1,593
// generated tokens were reasoning, captured from a live claude-opus-5 turn at
// effort "max".
func reasoningCounts() usage.Counts {
	c := tierCounts()
	c.OutputTokens = 1593
	c.ReasoningTokens = 948
	c.PresentKinds = uint8(usage.KindOutput | usage.KindReasoning)
	return c
}

// indentedRows returns the child rows — the ones this panel uses to say "part of
// the row above" rather than "peer of it".
func indentedRows(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, " ") && strings.Contains(l, "reasoning") {
			out = append(out, l)
		}
	}
	return out
}

// The child row exists, names itself, and sits DIRECTLY under output wherever
// output ranked — the adjacency is what carries "subset" to a reader who does not
// know the indent convention.
func TestRenderTierRows_ReasoningIsAChildOfOutput(t *testing.T) {
	lines := renderTierRows(reasoningCounts(), tierColumnWidth)

	outputAt, reasoningAt := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), "output"):
			outputAt = i
		case strings.Contains(l, "reasoning"):
			reasoningAt = i
		}
	}
	if outputAt < 0 {
		t.Fatal("no output row")
	}
	if reasoningAt < 0 {
		t.Fatal("no reasoning row; a reported reasoning split must be shown")
	}
	if reasoningAt != outputAt+1 {
		t.Errorf("reasoning is at %d and output at %d; the child must directly follow its parent",
			reasoningAt, outputAt)
	}
	if !strings.HasPrefix(lines[reasoningAt], " ") {
		t.Errorf("reasoning row %q is not indented; flush with the tiers it reads as a peer",
			lines[reasoningAt])
	}
}

// THE INVARIANT THIS PANEL EXISTS ON: the rows that sum to the bill still sum to
// the bill. The child is excluded from that sum because its money is already inside
// output's — counting both double-counts every reasoning token at the output rate,
// which is the error usage.Counts warns about in the field's own doc comment.
func TestRenderTierRows_ChildIsExcludedFromTheHundredPercent(t *testing.T) {
	lines := renderTierRows(reasoningCounts(), tierColumnWidth)

	total, counted := 0, 0
	for _, l := range lines {
		if strings.HasPrefix(l, " ") { // the child
			continue
		}
		pct, ok := sharePercent(l)
		if !ok {
			continue
		}
		counted++
		total += pct
	}
	if counted != numTierRows {
		t.Errorf("counted %d tier rows, want %d", counted, numTierRows)
	}
	if total != 100 {
		t.Errorf("the four tier shares sum to %d%%, want 100%% — the child must not be in the sum", total)
	}
}

// Containment, checked as arithmetic rather than left to the label: a child that
// renders a bigger figure than its parent is the one way this layout can lie, and
// it would look authoritative doing it.
func TestRenderTierRows_ReasoningNeverExceedsOutput(t *testing.T) {
	lines := renderTierRows(reasoningCounts(), tierColumnWidth)

	var outputPct, reasoningPct int
	for _, l := range lines {
		pct, ok := sharePercent(l)
		if !ok {
			continue
		}
		switch {
		case strings.Contains(l, "reasoning"):
			reasoningPct = pct
		case strings.HasPrefix(strings.TrimSpace(l), "output"):
			outputPct = pct
		}
	}
	if reasoningPct > outputPct {
		t.Errorf("reasoning is %d%% of the bill but output is only %d%%; a subset cannot exceed its set",
			reasoningPct, outputPct)
	}
}

// An unreported split renders the NOT-KNOWN cell, not $0.00 and not a vanished row.
//
// The row must still be there: the panel's height is reserved from a constant that
// layout() cannot consult, and a height that followed the data is the defect
// TestRenderTierRows_HeightIsConstant exists for. And it must not read $0.00, which
// would assert the model did no reasoning when the truth is that nothing reported
// either way — the same refusal renderTierRows makes for an absent tier.
func TestRenderTierRows_UnreportedSplitIsNotKnownNotZero(t *testing.T) {
	lines := renderTierRows(tierCounts(), tierColumnWidth)
	child := indentedRows(lines)
	if len(child) != 1 {
		t.Fatalf("want exactly one child row even when unreported, got %d", len(child))
	}
	if !strings.Contains(child[0], emptyCell) {
		t.Errorf("child row = %q, want the not-known cell", child[0])
	}
	if strings.Contains(child[0], "$0.00") {
		t.Errorf("child row = %q, want no $0.00 — that asserts the model did no reasoning", child[0])
	}
}

// Reasoning reported but nothing generated: no denominator, so no defensible figure.
// The row stays (height is constant) and says it does not know.
func TestRenderTierRows_NoFigureWithoutOutputTokens(t *testing.T) {
	c := reasoningCounts()
	c.OutputTokens = 0
	child := indentedRows(renderTierRows(c, tierColumnWidth))
	if len(child) != 1 {
		t.Fatalf("want one child row, got %d", len(child))
	}
	if !strings.Contains(child[0], emptyCell) {
		t.Errorf("child row = %q, want the not-known cell with no output to apportion by", child[0])
	}
}

// The panel's height is CONSTANT whether or not a split was reported. This is the
// invariant the drawer's fixed reservation depends on.
func TestRenderTierRows_HeightConstantAcrossReasoningStates(t *testing.T) {
	withSplit := renderTierRows(reasoningCounts(), tierColumnWidth)
	without := renderTierRows(tierCounts(), tierColumnWidth)
	if len(withSplit) != len(without) {
		t.Errorf("panel is %d lines with a split and %d without; height must not follow the data",
			len(withSplit), len(without))
	}
	if len(withSplit) != tierPanelLines {
		t.Errorf("panel is %d lines, want tierPanelLines = %d", len(withSplit), tierPanelLines)
	}
}

// The drawer reserves its height from a constant, so the extra line has to be in
// it — otherwise the child row pushes the footer off the terminal, the defect
// keys.go records for spendDrawerLines.
func TestSpendDrawerLines_AccountsForTheChildRow(t *testing.T) {
	if want := len(renderTierRows(reasoningCounts(), tierColumnWidth)); tierPanelLines < want {
		t.Errorf("tierPanelLines = %d but the panel renders %d lines", tierPanelLines, want)
	}
	if tierPanelLines != numTierRows+1 {
		t.Errorf("tierPanelLines = %d, want numTierRows+1 = %d", tierPanelLines, numTierRows+1)
	}
	if spendDrawerLines < tierPanelLines {
		t.Errorf("spendDrawerLines = %d cannot hold a %d-line tier panel", spendDrawerLines, tierPanelLines)
	}
}
