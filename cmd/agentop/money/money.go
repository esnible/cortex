// Package money renders cost figures in the unit they are denominated in.
//
// SHARED BY `agentop cost` AND THE TUI, which used to disagree: the command learned billing units
// and the TUI printed "$" over every figure, so the same Bobcoins charge read as credits in one
// and dollars in the other. The rules live here once so the two cannot drift again.
//
// DOLLARS RENDER EXACTLY AS BEFORE. Every function takes the unit and hands a default one straight
// to the dollar path, so a deployment with no pricing unit configured — and any server too old to
// report one — sees byte-identical output.
package money

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// Mixed is what a figure spanning more than one unit renders as: there is no amount to print,
// since credits summed into dollars is a number that is neither. The same marker `agentop cost --by`
// prints for a mixed row.
const Mixed = "(mixed)"

// unknownUnit stands in for a unit name that does not fit or cannot be printed: the generic
// currency sign, which claims "not dollars" and nothing more.
const unknownUnit = "¤"

// IsDefault reports whether a unit off the wire means the default, USD.
//
// FOLDED, though core canonicalises before it serialises: agentop is a client of whatever server it
// is pointed at, including one older than itself, and a server sending "usd" must not have every
// surface call dollars a foreign unit.
func IsDefault(unit string) bool {
	return unit == "" || strings.EqualFold(unit, pricing.CurrencyUSD)
}

// USD formats a dollar figure for a headline: two decimals, with a real charge below half a cent
// rendered as "<$0.01" so a small non-zero figure is never printed as "$0.00".
func USD(v float64) string {
	if v > 0 && v < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

// In formats a headline figure in its own unit. The unit name TRAILS the figure: it is an
// operator's own word with no agreed prefix form, and "0.08 credits" reads as the quantity it is.
func In(v float64, unit string) string {
	if IsDefault(unit) {
		return USD(v)
	}
	unit = printable(unit)
	if v > 0 && v < 0.005 {
		return "<0.01 " + unit
	}
	return fmt.Sprintf("%.2f %s", v, unit)
}

// WindowUnit reads a Currencies list: the one unit every figure is in, or ok false when there are
// two or more and nothing may be labelled. An absent list is the default — an idle window, or a
// server older than the field.
func WindowUnit(currencies []string) (unit string, ok bool) {
	switch len(currencies) {
	case 0:
		return pricing.CurrencyUSD, true
	case 1:
		return currencies[0], true
	default:
		return "", false
	}
}

// SeriesUnit is the unit one series of snap is in: its own SeriesCurrencies entry when the server
// sent one, else the window's (WindowUnit). ok false means the series itself spans units.
func SeriesUnit(snap *usage.Snapshot, label string) (unit string, ok bool) {
	if snap == nil {
		return pricing.CurrencyUSD, true
	}
	if units, found := snap.SeriesCurrencies[label]; found {
		return WindowUnit(units)
	}
	return WindowUnit(snap.Currencies)
}

// Relabel rewrites a dollar rendering into unit, keeping every other character the dollar
// formatter chose — its rounding, its floor, its magnitude suffix — so the two units cannot
// follow different precision rules. "$12.40" becomes "12.40 Bobcoins", "<$0.01" "<0.01 Bobcoins",
// and a composite such as a figure with its saving, "$0.26(−$0.01)", names the unit once:
// "0.26(−0.01) Bobcoins".
//
// FITTED TO budget when budget > 0: the unit name is shortened with an ellipsis ("12.40 Bobco…"),
// and then replaced by "¤" ("12.40¤"). "" when not even that fits, so a caller with a ladder of
// shorter renderings can try its next rung. A figure in a foreign unit NEVER carries "$".
func Relabel(dollars, unit string, budget int) string {
	if IsDefault(unit) {
		return dollars
	}
	amount := strings.ReplaceAll(dollars, "$", "")
	fits := func(s string) bool { return budget <= 0 || lipgloss.Width(s) <= budget }
	name := []rune(printable(unit))
	if full := amount + " " + string(name); fits(full) {
		return full
	}
	for k := len(name) - 1; k >= 1; k-- {
		if short := amount + " " + string(name[:k]) + "…"; fits(short) {
			return short
		}
	}
	if bare := amount + unknownUnit; fits(bare) {
		return bare
	}
	return ""
}

// UnitName is unit as a caption: printable, and shortened with an ellipsis to fit budget columns
// ("Bobc…" in five). Dollars are "USD".
func UnitName(unit string, budget int) string {
	if IsDefault(unit) {
		return pricing.CurrencyUSD
	}
	name := []rune(printable(unit))
	if budget <= 0 || lipgloss.Width(string(name)) <= budget {
		return string(name)
	}
	for k := len(name) - 1; k >= 1; k-- {
		if short := string(name[:k]) + "…"; lipgloss.Width(short) <= budget {
			return short
		}
	}
	return unknownUnit
}

// printable is unit restricted to the characters a configured unit may contain — letters, digits,
// '-' and '_', the rule core's config enforces — because the name reaches a terminal from a
// server agentop does not control. Nothing left means "¤".
func printable(unit string) string {
	var b strings.Builder
	for _, r := range unit {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return unknownUnit
	}
	return b.String()
}
