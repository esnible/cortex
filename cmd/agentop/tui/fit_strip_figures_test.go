package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// fitStripFigures IS LIVE — the drawer's series rows and its hint line both go through it
// (renderSpendDrawer) — but its width contract lost every one of its tests when renderSpendStrip
// went, because the tests were named for the strip rather than for the function.
//
// DELETED COVERAGE, restored here: never exceeding the width, never clipping a figure, the
// wide-character case where a display column is not a rune, the label-drop branch, and the
// return "" branch. Its own doc promises "never clips" and "lipgloss.Width throughout, never
// len()", and nothing was holding it to either.

// fitFigures is a set of readings with distinct full and compact forms, so the ladder has
// something to give up at each step.
func fitFigures() []stripFigure {
	return []stripFigure{
		{full: "$30.93 today (3 inexact)", compact: "~$30.93"},
		{full: "saved ~$0.18", compact: "~$0.18"},
		{full: "1h: $2.91 (12 of 318 unpriced)", compact: "$2.91+"},
		{full: "cache 81%", compact: "cache 81%"},
	}
}

// NEVER WIDER THAN THE BUDGET, at every width from 1 up. The function exists to guarantee this:
// the band and the drawer both live in reservations computed from the terminal, and a line one
// column too wide wraps — which costs a row of the table below and breaks the height budget,
// because the budget counts rows while the terminal counts lines.
func TestFitStripFigures_NeverExceedsTheWidth(t *testing.T) {
	for _, label := range []string{"", " ", "SPEND"} {
		for w := 1; w <= 120; w++ {
			got := fitStripFigures(label, fitFigures(), w)
			if n := lipgloss.Width(got); n > w {
				t.Errorf("label %q width %d: rendered %d columns: %q", label, w, n, got)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("label %q width %d: contains a newline, which costs a row: %q",
					label, w, got)
			}
		}
	}
}

// AND IT DROPS WHOLE FIGURES RATHER THAN CLIPPING ONE. A half-rendered dollar amount is worse
// than a missing one: it reads as a real, smaller figure.
//
// ASSERTED AS MEMBERSHIP rather than by probing for half-figures, because that is the actual
// contract: the fitter chooses among a closed set of candidates — the first n figures in their
// full or compact forms, then figure 0 alone in either form, then "" — and never edits a
// candidate. Anything outside that set IS a clip, whatever shape it takes, so this catches forms
// of truncation a hand-written probe would not think to look for.
//
// (My first attempt did probe by prefix and was wrong in a way worth recording: it required each
// figure's money to appear whole if its "$N." prefix did, checking BOTH forms — but only one
// form is chosen per figure, and their money differs ("$2.91" against "$2.91+"), so it reported
// a clip on correct output.)
func TestFitStripFigures_ReturnsOnlyWholeCandidates(t *testing.T) {
	figs := fitFigures()
	const label = " "

	legal := map[string]bool{"": true}
	for n := 1; n <= len(figs); n++ {
		for _, compact := range []bool{false, true} {
			parts := make([]string, 0, n)
			for _, f := range figs[:n] {
				if compact {
					parts = append(parts, f.compact)
				} else {
					parts = append(parts, f.full)
				}
			}
			legal[label+"  "+strings.Join(parts, stripGap)] = true
		}
	}
	// The label-dropped forms.
	legal[figs[0].full] = true
	legal[figs[0].compact] = true

	for w := 1; w <= 140; w++ {
		got := fitStripFigures(label, figs, w)
		if !legal[got] {
			t.Errorf("width %d: %q is not one of the candidate forms — the fitter edited a "+
				"figure instead of dropping one", w, got)
		}
	}
}

// A DISPLAY COLUMN IS NOT A RUNE, which is why the doc says lipgloss.Width throughout and never
// len(). A CJK label occupies two columns per rune, so a fitter measuring bytes or runes would
// overflow by up to the label's length — invisible in an ASCII-only test suite, and the exact
// reason the deleted …_WideCharacterSafety case existed.
func TestFitStripFigures_CountsDisplayColumnsNotRunes(t *testing.T) {
	wide := []stripFigure{
		{full: "過去一時間: $2.91", compact: "$2.91"},
		{full: "節約 ~$0.18", compact: "~$0.18"},
	}
	for w := 1; w <= 80; w++ {
		got := fitStripFigures("使用量", wide, w)
		if n := lipgloss.Width(got); n > w {
			t.Errorf("width %d: rendered %d display columns: %q — a rune count would have "+
				"passed here and overflowed the terminal", w, n, got)
		}
	}
	// And at a width that fits the wide form, it is actually chosen rather than skipped.
	full := "使用量" + "  " + wide[0].full + stripGap + wide[1].full
	if got := fitStripFigures("使用量", wide, lipgloss.Width(full)); got != full {
		t.Errorf("at exactly the full width the fitter returned %q, want %q", got, full)
	}
}

// THE LABEL GOES BEFORE THE NUMBER DOES. Below the width that holds the label plus one figure,
// the label is dropped and the figure kept: the figure is the information and the label is
// decoration, and a dollar amount alone is still unambiguous in the chrome.
//
// This branch was unreached by any test after the strip's deletion.
func TestFitStripFigures_DropsTheLabelBeforeTheFigure(t *testing.T) {
	figs := []stripFigure{{full: "1h: $2.91", compact: "$2.91"}}
	label := "SPEND"

	// Wide enough for label + figure: both present.
	withLabel := fitStripFigures(label, figs, 40)
	if !strings.Contains(withLabel, label) || !strings.Contains(withLabel, "$2.91") {
		t.Fatalf("at width 40 = %q, want both the label and the figure", withLabel)
	}
	// Narrow enough that the label cannot come too: the figure survives alone.
	only := fitStripFigures(label, figs, lipgloss.Width(figs[0].full))
	if strings.Contains(only, label) {
		t.Errorf("at the figure's own width = %q, the label was kept", only)
	}
	if !strings.Contains(only, "$2.91") {
		t.Errorf("at the figure's own width = %q, the figure was dropped instead of the label", only)
	}
	// Narrower still: the compact form, then nothing at all — never a clipped figure.
	if got := fitStripFigures(label, figs, lipgloss.Width(figs[0].compact)); got != figs[0].compact {
		t.Errorf("at the compact width = %q, want %q", got, figs[0].compact)
	}
	if got := fitStripFigures(label, figs, lipgloss.Width(figs[0].compact)-1); got != "" {
		t.Errorf("below the compact width = %q, want empty: a clipped figure is a wrong figure", got)
	}
}

// No figures is empty, not a bare label: a label with nothing to label reads as a rendering
// fault rather than as an answer.
func TestFitStripFigures_NoFiguresIsEmpty(t *testing.T) {
	if got := fitStripFigures("SPEND", nil, 100); got != "" {
		t.Errorf("no figures rendered %q, want empty", got)
	}
}
