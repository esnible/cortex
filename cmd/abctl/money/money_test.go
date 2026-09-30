package money

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// A default unit leaves every dollar rendering untouched, whatever its shape.
func TestRelabel_DollarsPassThroughUnchanged(t *testing.T) {
	for _, unit := range []string{"", "USD", "usd"} {
		for _, in := range []string{"$12.40", "<$0.01", "$1.2k", ">$1G", "--"} {
			if got := Relabel(in, unit, 4); got != in {
				t.Errorf("Relabel(%q, %q) = %q, want it unchanged", in, unit, got)
			}
		}
	}
}

// A foreign unit keeps the dollar formatter's figure and trails its own name, shortening the name
// before the figure, and never keeping the "$".
func TestRelabel_ForeignUnitsFitTheirBudgetWithoutADollarSign(t *testing.T) {
	for _, tc := range []struct {
		in     string
		budget int
		want   string
	}{
		{"$12.40", 0, "12.40 Bobcoins"},
		{"<$0.01", 0, "<0.01 Bobcoins"},
		{"$12.40", 14, "12.40 Bobcoins"},
		{"$12.40", 10, "12.40 Bob…"},
		{"$12.40", 8, "12.40 B…"},
		{"$12.40", 7, "12.40¤"},
		{"$12.40", 5, ""},
	} {
		got := Relabel(tc.in, "Bobcoins", tc.budget)
		if got != tc.want {
			t.Errorf("Relabel(%q, budget %d) = %q, want %q", tc.in, tc.budget, got, tc.want)
		}
		if strings.Contains(got, "$") {
			t.Errorf("Relabel(%q, budget %d) = %q: a Bobcoins figure printed with a dollar sign", tc.in, tc.budget, got)
		}
		if tc.budget > 0 && lipgloss.Width(got) > tc.budget {
			t.Errorf("Relabel(%q, budget %d) = %q is %d wide", tc.in, tc.budget, got, lipgloss.Width(got))
		}
	}
}

// A unit name from the wire is reduced to the configured charset before it reaches a terminal.
func TestIn_StripsWhatAConfiguredUnitCannotContain(t *testing.T) {
	if got := In(0.25, "cr\x1b[31medits"); got != "0.25 cr31medits" {
		t.Errorf("In = %q, want the escape sequence's control and punctuation dropped", got)
	}
	if got := In(0.25, "\x1b[]"); got != "0.25 ¤" {
		t.Errorf("In = %q, want the generic sign for a name with nothing printable", got)
	}
	if got := In(0.25, ""); got != "$0.25" {
		t.Errorf("In(default) = %q, want $0.25", got)
	}
}

// A series' own units win over the window's, and an older server's absent field falls back to them.
func TestSeriesUnit_PrefersTheSeriesOwnUnits(t *testing.T) {
	snap := &usage.Snapshot{
		Currencies:       []string{"Bobcoins", "USD"},
		SeriesCurrencies: map[string][]string{"bob-shell/2.0.5": {"Bobcoins"}},
	}
	if u, ok := SeriesUnit(snap, "bob-shell/2.0.5"); !ok || u != "Bobcoins" {
		t.Errorf("bob = %q, %v; want Bobcoins", u, ok)
	}
	if _, ok := SeriesUnit(snap, "claude-code/2.1.284"); ok {
		t.Error("a series with no entry of its own took a unit from a mixed window")
	}
	if u, ok := SeriesUnit(&usage.Snapshot{}, "anything"); !ok || u != "USD" {
		t.Errorf("an older server = %q, %v; want USD, the only unit there was", u, ok)
	}
}
