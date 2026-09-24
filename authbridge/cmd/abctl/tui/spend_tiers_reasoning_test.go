package tui

import (
	"strconv"
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

// childRows returns the reasoning child rows.
//
// Matched on childTierLabel for the reason tierRowsOnly is: "indented and mentions
// reasoning" is a description of today's output, while the label is the thing
// actually meant. Renamed from indentedRows to stop the predicate drifting back.
func childRows(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, childTierLabel) {
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
		case strings.HasPrefix(l, childTierLabel):
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
	// The label itself carries the indent, so matching it above is what proves the
	// row is a child rather than a peer; this pins the indent has not been flattened
	// out of childTierLabel while the tests kept passing.
	if !strings.HasPrefix(childTierLabel, " ") {
		t.Errorf("childTierLabel %q lost its indent; flush with the tiers it reads as a peer",
			childTierLabel)
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
		if strings.HasPrefix(l, childTierLabel) { // the child, not a tier
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
//
// THE MALFORMED FIXTURE IS THE POINT. A well-formed one (948 of 1,593) cannot
// violate containment whatever the renderer does, so asserting it proves only that
// the arithmetic is not wildly broken — the clamps that actually protect the
// invariant never execute. Reasoning exceeding output should be impossible on the
// wire, which is exactly why a gateway that reports it that way would go unnoticed
// until the panel drew a child longer than its parent.
func TestRenderTierRows_ReasoningNeverExceedsOutput(t *testing.T) {
	sane := reasoningCounts()

	// Reasoning reported ABOVE output: the shape the clamps exist for.
	inverted := reasoningCounts()
	inverted.ReasoningTokens = 4_000 // > OutputTokens (1,593)

	// Reasoning equal to output: the boundary. Every comparison in the loop below is
	// `>`, so "the clamp must not overshoot and make the child SMALLER" needs its own
	// check — asserted at the end of the subtest rather than named here and left
	// untested.
	equal := reasoningCounts()
	equal.ReasoningTokens = equal.OutputTokens

	for _, tc := range []struct {
		name string
		c    usage.Counts
	}{
		{"well-formed", sane},
		{"reasoning reported above output", inverted},
		{"reasoning equal to output", equal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outputPct, reasoningPct int
			var outputRow, reasoningRow string
			for _, l := range renderTierRows(tc.c, tierColumnWidth) {
				pct, ok := sharePercent(l)
				if !ok {
					continue
				}
				switch {
				case strings.HasPrefix(l, childTierLabel):
					reasoningPct, reasoningRow = pct, l
				case strings.HasPrefix(strings.TrimSpace(l), "output"):
					outputPct, outputRow = pct, l
				}
			}
			if reasoningRow == "" {
				t.Fatal("no reasoning row rendered")
			}
			if reasoningPct > outputPct {
				t.Errorf("reasoning is %d%% of the bill but output is only %d%%; a subset cannot "+
					"exceed its set\n  %s\n  %s", reasoningPct, outputPct, outputRow, reasoningRow)
			}
			// THE MONEY CELL NEEDS ITS OWN ASSERTION, because the share cannot stand in
			// for it. pct is DERIVED from micros, so a share that looks sane does not
			// witness a sane figure: floor(micros*100/total) collapses a range of micros
			// onto the same percentage, and on this fixture an unclamped child rendered
			// ~2.5x output while the share check stayed green.
			//
			// (An earlier version of this comment blamed a second clamp on the share.
			// That clamp was removed as unreachable — see reasoningChildRow — so the
			// reason is the floor division, not a second guard.)
			//
			// rOK/oOK are asserted rather than used as a filter: a `&&` over them would
			// let this assertion skip itself the moment the child renders not-known,
			// which is exactly how a guard goes quiet without failing.
			rMoney, rOK := rowMoney(reasoningRow)
			oMoney, oOK := rowMoney(outputRow)
			if !rOK || !oOK {
				t.Fatalf("no figure to compare (child ok=%v, parent ok=%v); this assertion "+
					"cannot fail:\n  %s\n  %s", rOK, oOK, outputRow, reasoningRow)
			}
			if rMoney > oMoney {
				t.Errorf("the child's figure $%.4f exceeds its parent's $%.4f:\n  %s\n  %s",
					rMoney, oMoney, outputRow, reasoningRow)
			}
			// THE COUNTER MUST COUNT, asserted before it is trusted. The first version
			// of drawnBarGlyphs used an inverted rune range and returned 0 for every
			// row, so the comparison below was 0 > 0 and could not fail while the
			// commit message claimed it checked the bar. A dead assertion is worse than
			// an absent one: it reads as coverage. This guard makes that class of
			// mistake fail loudly instead of silently passing.
			if drawnBarGlyphs(outputRow) == 0 {
				t.Fatalf("drawnBarGlyphs counted no glyphs in %q; the bar assertion below "+
					"cannot fail", outputRow)
			}
			if drawnBarGlyphs(reasoningRow) > drawnBarGlyphs(outputRow) {
				t.Errorf("the child's bar is longer than its parent's:\n  %s\n  %s",
					outputRow, reasoningRow)
			}
			// THE OTHER DIRECTION, which only the equal case can witness: all of the
			// output was reasoning, so the child must render its parent's figure and not
			// a clamped-down one. Without this the clamp could subtract and every `>`
			// above would still pass.
			if tc.name == "reasoning equal to output" && rMoney != oMoney {
				t.Errorf("all output was reasoning, so the child should equal its parent, "+
					"got $%.4f against $%.4f:\n  %s\n  %s", rMoney, oMoney, outputRow, reasoningRow)
			}
		})
	}
}

// barGlyphs are the eight block glyphs tierBar draws with, U+2588 through U+258F.
//
// A SET, NOT A RANGE, and the reason is worth keeping: the glyphs run BACKWARDS
// against visual width. '█' (full) is U+2588, the LOWEST code point, and '▏' (one
// eighth) is U+258F, the highest. Written as `r >= '▏' && r <= '█'` — which reads
// correctly as "from thinnest to fullest" — it compiles, vets clean, and is
// unsatisfiable: the counter returned 0 for every row, so the assertion using it
// could not fail. A set cannot be ordered wrongly.
const barGlyphs = "█▉▊▋▌▍▎▏"

// drawnBarGlyphs counts the block glyphs in a rendered row, which is the bar's drawn
// length. Counted rather than measured off an index because the bar sits between
// two variable-width cells.
func drawnBarGlyphs(row string) int {
	n := 0
	for _, r := range row {
		if strings.ContainsRune(barGlyphs, r) {
			n++
		}
	}
	return n
}

// rowMoney reads the dollar figure a rendered row ends with.
//
// Returns false for the not-known cell, which carries no figure — a row without one
// is not a row whose figure is zero, which is the distinction this panel is built on.
func rowMoney(row string) (float64, bool) {
	i := strings.LastIndex(row, "$")
	if i < 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(row[i+1:]), 64)
	if err != nil {
		return 0, false
	}
	return v, true
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
	child := childRows(lines)
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

// A REPORTED SPLIT TOO SMALL TO APPORTION MUST NOT RENDER $0.00.
//
// The apportionment multiply truncates, so a real reasoning count whose share of the
// window falls below one micro yields micros == 0 — reachable on a small window,
// around a hundred output tokens at opus-5 rates. Printing that as "$0.00" asserts
// the reasoning was FREE, which is the claim renderTierRows refuses for a tier
// (tiers[tier] == 0 takes the not-known cell); the child needs the same escape.
//
// Not "<$0.01" either: that form means "too small to state", while what is true here
// is that the apportionment resolved no figure at all.
func TestRenderTierRows_TinyShareIsNotKnownNotFree(t *testing.T) {
	// 1 reasoning token of 900 output, against 300 apportioned output micros:
	// 300 * 1/900 = 0.333, which truncates to zero.
	c := usage.Counts{
		Requests: 3, CostMicros: 4_000,
		InputCostMicros: 900, CacheReadCostMicros: 2_800, OutputCostMicros: 300,
		OutputTokens: 900, ReasoningTokens: 1,
		PresentKinds: uint8(usage.KindInput | usage.KindCacheRead | usage.KindOutput | usage.KindReasoning),
	}
	child := childRows(renderTierRows(c, tierColumnWidth))
	if len(child) != 1 {
		t.Fatalf("want one child row, got %d", len(child))
	}
	if strings.Contains(child[0], "$0.00") {
		t.Errorf("child row = %q renders $0.00 for a REPORTED split; that asserts the "+
			"reasoning was free", child[0])
	}
	if !strings.Contains(child[0], emptyCell) {
		t.Errorf("child row = %q, want the not-known cell when the share apportions to "+
			"nothing", child[0])
	}
}

// Reasoning reported but nothing generated: no denominator, so no defensible figure.
// The row stays (height is constant) and says it does not know.
func TestRenderTierRows_NoFigureWithoutOutputTokens(t *testing.T) {
	c := reasoningCounts()
	c.OutputTokens = 0
	child := childRows(renderTierRows(c, tierColumnWidth))
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
	// THE ONLY LIVE COMPARISON HERE: a constant against what the renderer actually
	// produces. `tierPanelLines != numTierRows+1` and `spendDrawerLines < tierPanelLines`
	// were also asserted and both were tautologies — the first restates the const
	// definition, and spendDrawerLines is max(tierPanelLines, ...)+2 so the second cannot
	// fail. Comparing a constant to its own definition reads as coverage and is none.
	if want := len(renderTierRows(reasoningCounts(), tierColumnWidth)); tierPanelLines < want {
		t.Errorf("tierPanelLines = %d but the panel renders %d lines", tierPanelLines, want)
	}
	// The drawer must actually emit them, which is a property of renderSpendDrawer
	// rather than of the constants. Asserted against a real render.
	if got := len(renderSpendDrawer(reasoningSnap(), nil, usage.GroupModel, "1h", 100)); got > spendDrawerLines {
		t.Errorf("the drawer emitted %d lines but reserves %d; the footer will be pushed off",
			got, spendDrawerLines)
	}
}
