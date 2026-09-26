package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// THE SELECTED ROW MUST NOT INVERT ITS CELLS, because one of them encodes a value as INK.
//
// bubbles wraps the whole row in Selected.Render, so reverse video swapped foreground and
// background for every cell in it — and the sessions table's CONTEXT(1M) gauge draws its value as
// filled blocks against a blank track. Under reverse the blocks rendered in the background colour
// (reading as empty) and the track in the foreground (reading as filled), so a selected row showed
// 44% as roughly 56%, filling from the wrong side.
//
// Asserted on the style rather than on the emitted escape: an SGR reverse is "7m", and a
// background colour of index 7 would also produce "7m" in the byte stream, so a substring test
// would pass or fail for the wrong reason.
func TestTableStyles_SelectionDoesNotInvertTheRow(t *testing.T) {
	sel := tableStyles().Selected
	if sel.GetReverse() {
		t.Error("the selected row is reverse-video again; the context gauge reads backwards on it")
	}
	// Both markers, asserted separately rather than as "at least one of them".
	//
	// They are not interchangeable: the tint is what replaces reverse video on a colour
	// terminal, and bold is the fallback that survives a profile with no colour at all (the
	// cost this change accepted). An either-or assertion would keep passing if Background were
	// dropped, leaving the decision this PR argued for pinned by nothing.
	if sel.GetBackground() == lipgloss.NoColor(struct{}{}) {
		t.Error("the selected row has no background; on a colour terminal nothing marks it")
	}
	if !sel.GetBold() {
		t.Error("the selected row is not bold; with no colour profile nothing marks it")
	}
}

// The gauge's TEXT is identical on the selected row and an unselected one, so the highlight does
// not redraw the value.
//
// THIS DOES NOT CATCH THE REPORTED BUG, and saying so matters: reverse video changes no
// characters, so a stripANSI comparison is blind to it. Mutation-checked — restoring
// Reverse(true) fails only the GetReverse assertion above, and this test keeps passing. What it
// guards is the neighbouring class: a cell whose content is rewritten or truncated by the row
// style, which is how a styled cell came back as a lone ellipsis and why colouring the gauge is
// impossible here.
func TestSessionsTable_TheGaugeReadsTheSameOnTheSelectedRow(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })

	gauge := padLeft(contextGauge(438_000, 11), 11)
	cols := []table.Column{{Title: "SESSION", Width: 14}, {Title: contextColumnTitle, Width: 11}}
	rows := []table.Row{{"fb07c7cb-ba01…", gauge}, {"c39dae31-2790…", gauge}}

	tbl := table.New(table.WithColumns(cols), table.WithRows(rows),
		table.WithHeight(3), table.WithFocused(true))
	tbl.SetStyles(tableStyles())
	tbl.SetCursor(0) // the first data row is highlighted, the second is not

	var data []string
	for _, ln := range strings.Split(tbl.View(), "\n") {
		plain := strings.TrimRight(stripANSI(ln), " ")
		if plain != "" && !strings.Contains(plain, "SESSION") {
			data = append(data, plain)
		}
	}
	if len(data) < 2 {
		t.Fatalf("expected two data rows, got %d: %q", len(data), data)
	}
	selected, unselected := data[0], data[1]
	if !strings.HasSuffix(selected, gauge) {
		t.Errorf("the selected row's gauge was redrawn:\n  got  %q\n  want suffix %q",
			selected, gauge)
	}
	if !strings.HasSuffix(unselected, gauge) {
		t.Errorf("the unselected row's gauge was redrawn: %q", unselected)
	}
}
