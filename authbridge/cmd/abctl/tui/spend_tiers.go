package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// numTierRows is the height of the tier column, in every state.
//
// FOUR, FIXED, because the panel lives in a reservation layout() computes from the terminal
// height and paneView pads to. A renderer whose height follows its data is exactly what
// overflowed this surface by five rows and under-filled it by six, so the count is a
// constant and every state returns exactly this many rows — enforced by the return type
// rather than by a guard, see renderTierRows.
//
// Four and not five: reasoning is a subset of output, not a sibling tier, so counting it
// here would double-count the same money at the most expensive rate there is. It IS shown
// — as an indented child of output, see childTierLabel — but it is not a tier, which is
// why this constant stays pinned to the rate count and tierPanelLines carries the
// rendered height.
const numTierRows = pricing.NumTiers

// tierBarWidth is the widest a bar may be. Bars are decoration over a figure that is
// already printed, so they yield their space before the figures do.
const tierBarWidth = 12

// tierLabelWidth is the label column, sized to the longest label so the bars start at
// one column whatever the mix.
//
// 12, not 11: the longest label is no longer "cache-write" (11) but the reasoning
// child's " └ reasoning" (12), whose two leading columns are the indent that says it
// is part of the row above.
const tierLabelWidth = 12

// tierPctWidth is the share column: "100%" at its widest, right-aligned.
//
// A FIGURE, NOT DECORATION, which is why it does not degrade with the bar. It answers the question
// an operator brings to a breakdown — what fraction of the bill is cache? — and at the widths this
// panel actually renders at, tierBarBudget had the bar down to six columns, so the proportion was
// being carried almost entirely by the least precise thing on the row.
const tierPctWidth = 4

// tierMoneyWidth is the money column, right-aligned so the decimal points line up.
//
// Nine columns: "$16740.85" is a month of this proxy's traffic at the top tier and is the widest
// figure the panel can be asked to draw. These figures wear no disclosure markers (see
// renderTierRows), so nothing widens them beyond their digits.
//
// FIXED, NOT FITTED TO THE DATA. A column sized to the widest current figure would move whenever a
// total crossed a digit boundary, and this panel is polled — the same flicker the cost-descending
// sort breaks ties to avoid, arriving through the layout instead of the order.
const tierMoneyWidth = 9

// tierLabels names the rate tiers for a reader.
//
// FIXED, not derived through seriesLetter: that helper exists because model names are
// open-ended and need de-duplication, while the tier set is closed and stable. Deriving
// would also collide cache-read with cache-write and leave the dedupe to pick.
var tierLabels = map[pricing.Tier]string{
	pricing.TierInput:      "input",
	pricing.TierCacheWrite: "cache-write",
	pricing.TierCacheRead:  "cache-read",
	pricing.TierOutput:     "output",
}

// childTierLabel is the reasoning row's label, EXACTLY tierLabelWidth runes so the
// bars still start at one column whatever the mix.
//
// "reasoning", not "thinking", because that is the word every other surface in this
// repo uses for it — usage.Counts.ReasoningTokens, parsercommon KindReasoning, and
// `abctl cost`'s own "reasoning (of output)" line. Anthropic's wire field is
// thinking_tokens, and that name stays where it belongs: on the JSON tag that reads
// it.
//
// Indented and hung off a box-drawing stem rather than flush left, because the label
// has to carry a fact the money column cannot: this row's dollars are already inside
// the row above it. Flush left it reads as a fifth tier and the column stops adding
// up to the bill.
const childTierLabel = " └ reasoning"

// tierPanelLines is the panel's MAXIMUM height: the four tiers plus the optional
// reasoning child. Separate from numTierRows, which stays pinned to
// pricing.NumTiers because that is a count of RATES and reasoning is not one.
const tierPanelLines = numTierRows + 1

// tierOrder is the declaration order, which is deliberately NOT the display order.
var tierOrder = [numTierRows]pricing.Tier{
	pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput,
}

// renderTierRows is the "where the money went" column: one row per rate tier, ranked by what
// it cost, with a proportional bar and the apportioned figure.
//
// SOLID BLOCKS, not the letter marks usage_stacked.go uses. That file rejected shaded blocks
// because adjacent segments of ONE stacked bar were indistinguishable at a glance; here each
// bar occupies its own line beside its own label, so there is no neighbour to disambiguate
// from and a block is both unambiguous and legible. Colour stays decoration — the label
// carries the identity, so this survives a monochrome terminal and a screenshot.
//
// NO FIGURE WEARS inexactMarker, and it is worth saying what was given up. Every figure here IS
// inexact — the mix is the rate table's while the total may be the gateway's — and each one used
// to carry the glyph saying so. It was dropped deliberately: unlike the sessions table and the
// band, this panel has no money column HEADER to move the caveat onto ("WHERE IT WENT" names the
// column, not the figures), so the choice was a glyph on every row or nothing, and nothing won.
//
// What is left to carry it is this comment and the README. If a reader needs to know these are
// apportioned rather than measured, a header for the money column is the thing to add — not the
// per-row marker back.
//
// Figures read in CENTS, like every other scanned money surface; see the precision rule beside
// formatUSDTotal.
func renderTierRows(c usage.Counts, width int) []string {
	tiers, ok := c.ApportionTiers()

	// Ranked by cost, descending, ties broken on the label so two equal tiers do not swap
	// places between polls and make the panel flicker under a reader comparing rows — the
	// same rule rankSeriesByCost states for the model column.
	order := tierOrder
	sort.SliceStable(order[:], func(i, j int) bool {
		a, b := tiers[order[i]], tiers[order[j]]
		if a != b {
			return a > b
		}
		return tierLabels[order[i]] < tierLabels[order[j]]
	})

	var peak int64
	for _, v := range tiers {
		if v > peak {
			peak = v
		}
	}
	budget := tierBarBudget(width)
	// A FIXED-SIZE ARRAY, so the height is guaranteed by the type rather than by a runtime
	// guard. The first version built a slice, padded it to length, and capped it with
	// out[:numTierRows] — and the pad was dead code: the cap re-extended the slice within
	// its preallocated capacity, filling the gap with empty strings on its own. That is a
	// trap rather than a guarantee, because dropping the preallocation turns the cap into a
	// panic. Indexed assignment into an array of the declared length cannot be wrong.
	// Shares alongside the money, from the same apportioned figures, so a row cannot state a
	// percentage of one total beside a figure from another.
	shares := tierShares(tiers)
	var out [numTierRows]string
	for i, tier := range order {
		label := tierLabels[tier]
		var row string
		switch {
		case !ok, tiers[tier] == 0:
			// NOT KNOWN HERE, which is what emptyCell means — never $0.00, and never a figure
			// apportioned from a mix that does not exist.
			//
			// THE ZERO CASE IS THE SAME CASE. A tier carrying real money always apportions to
			// at least one micro, so zero means this tier is absent from the modelled mix —
			// and formatUSDCell(0) prints "$0.00", which asserts the tier was FREE. That is
			// the "$0.00 for a figure that might be unknown" lie this package refuses in
			// sessionMoneyCell and in `abctl cost`'s headline, arriving through a third door.
			// Found by rendering the panel rather than by a test: the fixture populated all
			// four tiers, so no assertion could see it.
			row = fmt.Sprintf("%-*s %s", tierLabelWidth, label, emptyCell)
		case budget > 0:
			row = fmt.Sprintf("%-*s %s %-*s %s", tierLabelWidth, label,
				tierShareCell(shares[tier], tiers[tier]),
				budget, tierBar(tiers[tier], peak, budget),
				tierMoneyCell(tiers[tier]))
		default:
			// The bar is gone and the two figures remain — see tierBarBudget.
			row = fmt.Sprintf("%-*s %s %s", tierLabelWidth, label,
				tierShareCell(shares[tier], tiers[tier]), tierMoneyCell(tiers[tier]))
		}
		out[i] = clipRow(row, width)
	}
	// The child row is appended into a SLICE rather than written into the array,
	// because it is optional: the array's length is the guarantee that every tier got
	// a row, and a fifth slot in it would be an empty string on the common path.
	// ALWAYS APPENDED, never conditional. The panel's height must not follow its data —
	// layout() reserves it from a constant it cannot consult, and a renderer whose
	// height varied is what TestRenderTierRows_HeightIsConstant records as having
	// overflowed this pane by five rows and under-filled it by six. When no reasoning
	// split was reported the child renders the not-known cell, which is exactly what an
	// absent TIER does two branches above.
	return insertAfterOutput(out[:], order,
		reasoningChildRow(c, tiers, ok, peak, budget, width))
}

// reasoningChildRow renders the reasoning row that hangs under output.
//
// NOT A FIFTH TIER, and the whole shape of this function follows from that. Reasoning
// has no rate of its own: it is a subset of output, billed at the output rate, which
// pricing.Usage and usage.Counts both state in their own words and which is why
// ApportionTiers returns exactly pricing.NumTiers figures that sum to the bill. So
// this row is derived here rather than apportioned there, it is excluded from
// tierShares, and it is indented so a reader does not add it to the column.
//
// THE MONEY IS APPORTIONED FROM OUTPUT'S DISPLAYED FIGURE, not from
// c.OutputCostMicros. The displayed figure is already scaled to the gateway's
// authoritative total, so deriving from the raw mix would put a child on screen that
// does not divide into the parent printed directly above it.
//
// The share is denominated in the TOTAL, like every other row, even though the share
// of OUTPUT (56% here, against 15% of the bill) is the more interesting number. Two
// denominators in one column is the defect renderTierRows already refuses — "a row
// cannot state a percentage of one total beside a figure from another" — and the
// containment reads from the indent anyway: 15% under 27% is visibly a part of it.
func reasoningChildRow(c usage.Counts, tiers [pricing.NumTiers]int64, ok bool,
	peak int64, budget, width int) string {
	notKnown := clipRow(fmt.Sprintf("%-*s %s", tierLabelWidth, childTierLabel, emptyCell), width)

	// THE ARITHMETIC IS usage.ApportionReasoning'S, not this file's. It lives beside
	// ApportionTiers for the reason that function states about itself — one place, so the
	// drawer, `abctl cost` and the JSON cannot disagree about a figure derived three
	// times. It was written here first, which left --json unable to publish the number
	// this panel draws.
	//
	// ok from ApportionTiers gates first: with no mix to apportion by there is no output
	// figure to take a share of.
	micros, hasFigure := c.ApportionReasoning(tiers[pricing.TierOutput])
	if !ok || !hasFigure {
		return notKnown
	}
	// Floored against the same total the tier rows use, so the child is comparable down
	// the column. Deliberately NOT tierShares, which must keep summing to 100 across
	// exactly the four tiers.
	//
	// NO CLAMP ON THE SHARE, because it is already bounded by the parent's: it derives
	// from micros AFTER ApportionReasoning's clamp, and
	//
	//	floor(micros*100/total) <= floor(tiers[output]*100/total) <= shares[output]
	//
	// the right-hand step holding because tierShares only ever ADDS its rounding
	// remainder to the largest share, never subtracts.
	pct := 0
	if c.CostMicros > 0 {
		pct = int(micros * 100 / c.CostMicros)
	}
	label := childTierLabel
	var row string
	switch {
	case budget > 0:
		row = fmt.Sprintf("%-*s %s %-*s %s", tierLabelWidth, label,
			tierShareCell(pct, micros), budget, tierBar(micros, peak, budget),
			tierMoneyCell(micros))
	default:
		row = fmt.Sprintf("%-*s %s %s", tierLabelWidth, label,
			tierShareCell(pct, micros), tierMoneyCell(micros))
	}
	return clipRow(row, width)
}

// insertAfterOutput places the child directly beneath output, wherever the cost
// ranking put it. Adjacency is what carries "part of the row above" to a reader who
// has not learned the indent convention, so it has to follow the rank rather than
// sit at a fixed line.
func insertAfterOutput(rows []string, order [numTierRows]pricing.Tier, child string) []string {
	at := len(rows) // fall back to last, so a missing output row cannot drop the child
	for i, tier := range order {
		if tier == pricing.TierOutput {
			at = i + 1
			break
		}
	}
	out := make([]string, 0, len(rows)+1)
	out = append(out, rows[:at]...)
	out = append(out, child)
	return append(out, rows[at:]...)
}

// tierShareCell is one tier's share of the window, right-aligned.
//
// "<1%" FOR A TIER THAT ROUNDS TO NOTHING BUT HOLDS SOMETHING. A 0% beside a non-zero figure is the
// one claim this panel refuses — renderTierRows spells out the rule for the money column ("never
// $0.00 ... which asserts the tier was FREE") and a share floored to zero asserts the same thing
// about the same tier, one column to the left. Measured: $0.20 of a $99.20 window rendered
// "input  0%  $0.20", which contradicts itself on one row.
//
// Callers pass a floored percentage, so this cannot tell "rounds to zero" from "is zero" on its own
// — hence `micros`, which is the figure the row is about to print beside it. A tier absent from the
// mix never reaches here at all: its row is the not-known cell, see renderTierRows.
//
// NOT A DECIMAL PLACE, which was the alternative. "0.2%" claims a precision the modelled mix does
// not have and costs two more columns; humanizeDurationMs already settled this spelling for the same
// situation, where "<1ms" says a duration is too small to state and too real to call zero.
//
// padLeft, not Fprintf("%*s"): fmt pads to a RUNE count and this package measures in display
// columns — the rule footer.go records the cost of breaking. ASCII either way today, and the point
// is that the whole panel obeys one vocabulary.
func tierShareCell(pct int, micros int64) string {
	if pct == 0 && micros > 0 {
		return padLeft("<1%", tierPctWidth)
	}
	return padLeft(strconv.Itoa(pct)+"%", tierPctWidth)
}

// tierMoneyCell is one tier's apportioned figure, right-aligned so the decimal points line up
// down the panel. Same padding rule as tierShareCell, for the same reason.
func tierMoneyCell(micros int64) string {
	return padLeft(formatUSDTotalMicros(micros), tierMoneyWidth)
}

// tierShares turns the apportioned figures into whole percentages that sum to 100.
//
// WHOLE PERCENTAGES, because a decimal place on a figure this panel already marks as modelled
// would claim a precision the mix does not have — and because four two-character numbers fit in a
// column where four five-character ones do not.
//
// THE REMAINDER GOES TO THE LARGEST SHARE, which is the rule usage.Counts.ApportionTiers applies to
// its own micro and for the same reason: that is where a point is proportionally smallest and cannot
// flip a rank, and reordering the bars to make the arithmetic add up would be a visible lie in the
// service of an invisible one. Four independent roundings genuinely do not sum to 100 — the live
// mix this was built against floors to 98 — so without this the panel states a breakdown that does
// not add up to the whole it is breaking down.
//
// A TIER ABSENT FROM THE MIX GETS NOTHING, not a rounded-down zero. Its row is the not-known cell
// rather than a figure (see renderTierRows), so a 0% beside it would be the one claim this panel
// refuses: that the tier was free.
func tierShares(tiers [pricing.NumTiers]int64) [pricing.NumTiers]int {
	var out [pricing.NumTiers]int
	var total int64
	for _, v := range tiers {
		if v > 0 {
			total += v
		}
	}
	if total <= 0 {
		return out
	}
	sum, largest := 0, -1
	for i, v := range tiers {
		if v <= 0 {
			continue
		}
		out[i] = int(v * 100 / total)
		sum += out[i]
		if largest < 0 || v > tiers[largest] {
			largest = i
		}
	}
	if r := 100 - sum; r != 0 && largest >= 0 {
		out[largest] += r
	}
	return out
}

// tierBarBudget is how many cells a bar may use at this width: the full bar on a roomy
// terminal, half on a tight one, none when the figures themselves are all that fit.
//
// Bars go before figures because a bar without its number says only "this one is bigger",
// which the row ORDER already says. The share column counts as a FIGURE here and so is never
// what yields — see tierPctWidth.
//
// THE FIXED COST IS NAMED RATHER THAN A LITERAL 12. It was `tierLabelWidth+tierBarWidth+12`, where
// the 12 stood for a money column nothing declared; with two right-aligned figures on the row the
// number would have had to be re-derived by hand, which is how a budget drifts from the layout it
// is supposed to bound.
func tierBarBudget(width int) int {
	fixed := tierLabelWidth + 1 + tierPctWidth + 1 + tierMoneyWidth
	switch {
	case width >= fixed+tierBarWidth+1:
		return tierBarWidth
	case width >= fixed+tierBarWidth/2+1:
		return tierBarWidth / 2
	default:
		return 0
	}
}

// tierBar is a proportional bar with an eighth-block tail, so a small non-zero share stays
// visible rather than rounding away to nothing — the reason sessionMoneyCell skips a rung
// that would render a real charge as zero.
func tierBar(v, peak int64, budget int) string {
	if budget <= 0 || peak <= 0 || v <= 0 {
		return ""
	}
	eighths := int(float64(v) / float64(peak) * float64(budget) * 8)
	full, rem := eighths/8, eighths%8
	if full > budget {
		full, rem = budget, 0
	}
	bar := strings.Repeat("█", full)
	if full < budget && rem > 0 {
		bar += string([]rune("▏▎▍▌▋▊▉")[rem-1])
	}
	if bar == "" {
		// A tier with real money in it gets at least a sliver: an empty bar beside a
		// non-zero figure reads as a rendering fault.
		bar = "▏"
	}
	return bar
}

// clipRow is the last resort at a width narrower than one label. Rows here are built from
// fixed-width parts rather than the strip's droppable figures, so there is nothing to give
// up whole — and a reservation that overflows its terminal is the worse failure.
//
// IN DISPLAY COLUMNS, and it was in runes: `len([]rune(row)) <= width` passes for a string of wide
// characters that renders at twice that, and slicing by rune index then produces a line wider than
// the budget it was clipped to. That is precisely the bug footer.go records — a budget computed in
// columns and sliced by rune index rendering 55 columns for a 40-column one — and it reached a
// SERVER-SUPPLIED string: the drawer's error row carries err.Error() verbatim, so a remote message
// of wide runes overflowed the reservation, wrapped, and pushed the footer off the bottom.
// sanitizeLabel does not help, because a wide rune is not a control character.
//
// Plain text only, which is what every caller passes. An ANSI-styled row would be cut mid-sequence
// here; this package keeps escapes out of these rows deliberately — see the table's own rule.
func clipRow(row string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(row) <= width {
		return row
	}
	var b strings.Builder
	used := 0
	for _, r := range row {
		w := lipgloss.Width(string(r))
		if used+w > width {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}
