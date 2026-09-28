package ledger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// A row written before Currency existed decodes as USD.
//
// THIS IS THE ONE TEST THAT PROTECTS THE WHOLE RETENTION WINDOW rather than the current process.
// The package's schema rule is "additive changes keep old files readable because an absent field
// decodes to zero; a RENAME would read every historical row as zero for that column. Add, never
// rename." Currency is additive, so every row already on disk — for as long as
// cost_ledger.retention_days keeps it — arrives with the field absent, and the only reading that
// does not silently relabel history is USD.
//
// ASSERTED ON BYTES THAT CARRY NO currency KEY, not on a Row with the field set to "". The round
// trip through the decoder IS the property; a hand-built struct skips the step that could break
// it, which is exactly the shape of a test that reads as coverage and cannot fail.
func TestRow_AHistoricalRowWithNoCurrencyDecodesAsUSD(t *testing.T) {
	// Byte-for-byte what a pre-Currency writer emitted: no currency key at all.
	const historical = `{"at":"2026-09-01T10:00:00Z","endpoint":"api.anthropic.com",` +
		`"model":"claude-opus-5","agent":"claude-code/2.1.270","provenance":"authoritative",` +
		`"requests":3,"tokens":1000,"costMicros":4170000}`

	var r Row
	if err := json.Unmarshal([]byte(historical), &r); err != nil {
		t.Fatalf("a historical row must stay readable: %v", err)
	}
	if got := r.currencyOrUSD(); got != pricing.CurrencyUSD {
		t.Errorf("historical row reads as %q, want %s — this relabels every retained day",
			got, pricing.CurrencyUSD)
	}
	// And the raw field stays empty rather than being rewritten on read: the file is the record,
	// and "" is the lossless spelling of "written before units existed". Same argument Agent's own
	// comment makes for storing absence as "" rather than as the display string.
	if r.Currency != "" {
		t.Errorf("Currency = %q, want empty; the decoder must not invent a value", r.Currency)
	}
}

// Currency is part of the accumulation key, so two units never fold into one row.
//
// THE WHOLE POINT. The ledger already buckets by (endpoint, model, agent, provenance); adding
// currency to that tuple is what makes "never sum credits into dollars" fall out of folding that
// already exists, rather than needing arithmetic that checks. Two rows identical but for the unit
// must stay two rows — if they merge, the merged CostMicros is a number with no meaning.
func TestRow_CurrencyIsPartOfTheKey(t *testing.T) {
	usd := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "USD"}
	credits := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "credits"}

	if usd.key() == credits.key() {
		t.Fatal("two currencies share one key; their costs would be summed into a figure that " +
			"is neither dollars nor credits")
	}
}

// A row with no currency and a row that says USD land in the SAME bucket.
//
// Otherwise the day a unit is first configured, every endpoint's history splits in two: rows
// written yesterday with the field absent, rows written today saying USD, identical in every
// other respect and reported as separate series. The key has to normalise, which is why it reads
// currencyOrUSD rather than the raw field.
func TestRow_LegacyAndExplicitUSDShareABucket(t *testing.T) {
	legacy := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured"}
	explicit := Row{Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "USD"}

	if legacy.key() != explicit.key() {
		t.Errorf("an absent currency keys apart from an explicit %s, so history splits the day a "+
			"unit is first configured", pricing.CurrencyUSD)
	}
}

// Currency is compared case-insensitively, so "credits" and "Credits" are one bucket.
//
// The config preserves an operator's spelling deliberately — they see back what they typed — which
// means two endpoints can legitimately arrive spelled differently for the same unit. Keying on the
// raw string would report those as two currencies and refuse to combine figures that belong
// together, which is the mirror image of the defect this field prevents.
func TestRow_CurrencyKeyIsCaseInsensitive(t *testing.T) {
	lower := Row{Endpoint: "gw", Model: "m", Currency: "credits"}
	upper := Row{Endpoint: "gw", Model: "m", Currency: "Credits"}

	if lower.key() != upper.key() {
		t.Error("two spellings of one unit key apart, so figures in the same currency would be " +
			"reported as two and never combined")
	}
}

// The overflow row resets currency along with every other axis.
//
// overflowKey exists to bound cardinality, and it names ALL the identity fields for a stated
// reason: keeping any real one would let the overflow row multiply on that axis and defeat the
// bound. A fifth field that kept its value would reintroduce exactly that, one unit per row.
func TestOverflow_ResetsCurrencyToo(t *testing.T) {
	r := overflow(Row{
		Endpoint: "gw", Model: "m", Agent: "a", Provenance: "configured", Currency: "credits",
	})
	if r.Currency != overflowLabel {
		t.Errorf("Currency = %q after overflow, want %q — the row can multiply per unit otherwise",
			r.Currency, overflowLabel)
	}
	if r.key() != overflowKey {
		t.Errorf("overflow row key = %+v, want %+v", r.key(), overflowKey)
	}
}

// CurrenciesIn reports every distinct unit a window's rows carry, sorted.
//
// IT IS WHAT MAKES THE REFUSAL POSSIBLE. A total is only meaningful when the rows behind it share
// a unit, and nothing else on a snapshot can say whether they do: Counts is a flat sum by design,
// and the spec that introduced units is explicit that it must stay that way — "a unit belongs to
// the grouping key, not to the numbers". So the fact travels beside the numbers instead.
//
// NORMALISED AND DEDUPLICATED on the same rule key() uses, or the answer would disagree with the
// bucketing: a row with no currency and one saying USD are ONE unit, and "credits" and "Credits"
// are one unit spelled twice.
func TestCurrenciesIn_ReportsDistinctUnitsSorted(t *testing.T) {
	rows := []Row{
		{Endpoint: "bob", Currency: "credits"},
		{Endpoint: "anthropic"},                 // absent: USD
		{Endpoint: "litellm", Currency: "USD"},  // explicit: the same USD
		{Endpoint: "bob2", Currency: "Credits"}, // a second spelling of one unit
	}

	got := CurrenciesIn(rows)

	want := []string{"USD", "credits"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted, so a caller's message is stable between reads)", got, want)
		}
	}
}

// One currency, or none at all, is reported as such — the single-unit answer must stay singular.
//
// This is the case every deployment is in today, and it is what keeps the refusal downstream from
// firing on traffic that is perfectly summable. An empty window reports nothing rather than
// inventing USD: there is no figure to label.
func TestCurrenciesIn_SingleUnitAndEmpty(t *testing.T) {
	if got := CurrenciesIn(nil); len(got) != 0 {
		t.Errorf("an empty window reported %v, want nothing", got)
	}
	rows := []Row{{Endpoint: "a"}, {Endpoint: "b", Currency: "USD"}}
	if got := CurrenciesIn(rows); len(got) != 1 || got[0] != "USD" {
		t.Errorf("a USD-only window reported %v, want [USD]", got)
	}
}

// group=currency is served by the ledger, and folds every row including the legacy ones.
//
// It is the axis the cross-unit refusal POINTS AT, so it has to work — `abctl cost` tells a reader
// to use --by currency when it withholds a combined total, and a key that does nothing is the
// failure this repo has been bitten by more than once.
//
// The legacy row is the case worth the fixture: written before Currency existed, it has to land in
// the USD bucket rather than a nameless one, or a per-currency table would not reconcile against
// the total printed beside it.
func TestFold_GroupCurrencySeparatesUnitsAndKeepsLegacyRowsInUSD(t *testing.T) {
	if !Groupable(usage.GroupCurrency) {
		t.Fatal("the ledger does not serve group=currency, which the cost command points readers at")
	}
	rows := []Row{
		{Endpoint: "bob", Currency: "credits", Counts: usage.Counts{Requests: 4, CostMicros: 77_400}},
		{Endpoint: "anthropic", Counts: usage.Counts{Requests: 6, CostMicros: 2_000_000}},
		{Endpoint: "litellm", Currency: "USD", Counts: usage.Counts{Requests: 1, CostMicros: 500_000}},
	}

	_, series, _, _ := Fold(rows, usage.GroupCurrency)

	if len(series) != 2 {
		t.Fatalf("got %d series, want 2 (USD and credits): %v", len(series), series)
	}
	// The legacy row and the explicit one share the USD bucket.
	if got := series[pricing.CurrencyUSD]; got.Requests != 7 || got.CostMicros != 2_500_000 {
		t.Errorf("USD = %+v, want Requests 7 and CostMicros 2500000 — the legacy row must fold in here",
			got)
	}
	if got := series["credits"]; got.Requests != 4 || got.CostMicros != 77_400 {
		t.Errorf("credits = %+v, want Requests 4 and CostMicros 77400", got)
	}
}

// A capped minute does not make a single-currency deployment refuse its own total.
//
// overflow() coarsens a row onto overflowKey on every axis at once, currency included, so one
// minute past maxLabelsPerMinute used to put "(other)" into CurrenciesIn beside the real unit — a
// second entry, which is exactly what len(Currencies) > 1 tests for downstream. The result was a
// USD-only deployment withholding its headline and naming "(other)" as a currency it could not add
// to dollars. "(other)" is not a unit an operator can even configure: normaliseUnit rejects "(".
//
// ASSERTED AS THE CONSEQUENCE, not as the mechanism. TestOverflow_ResetsCurrencyToo already pins
// that overflow() rewrites the field, and it stayed green throughout — the bug was never in that
// rewrite, it was in reading the result as a billing unit, so only a test at this layer sees it.
func TestCurrenciesIn_TheOverflowLabelIsNotABillingUnit(t *testing.T) {
	// The shape a capped minute actually produces: real rows, plus the one folded accumulator.
	rows := []Row{
		{Endpoint: "anthropic", Currency: "USD", Counts: usage.Counts{Requests: 1}},
		overflow(Row{Endpoint: "gw", Model: "m", Currency: "USD", Counts: usage.Counts{Requests: 9}}),
	}

	got := CurrenciesIn(rows)

	if len(got) != 1 || got[0] != pricing.CurrencyUSD {
		t.Fatalf("CurrenciesIn = %v, want [%s]: a capped minute must not read as a second unit, or "+
			"every deployment today withholds its total the moment one minute overflows",
			got, pricing.CurrencyUSD)
	}
	for _, c := range got {
		if c == overflowLabel {
			t.Errorf("%q is reported as a billing unit; normaliseUnit could never have accepted it",
				overflowLabel)
		}
	}
}

// A capped row does not hide a REAL second unit that sits beside it.
//
// The complement of the test above, and the direction that would break the feature rather than an
// existing user: skipping the overflow label must not skip the rows around it. A window holding
// credits and dollars still has to refuse, capped or not.
func TestCurrenciesIn_SkippingOverflowStillSeesTheRealUnits(t *testing.T) {
	rows := []Row{
		{Endpoint: "anthropic", Currency: "USD"},
		overflow(Row{Endpoint: "gw", Currency: "credits"}),
		{Endpoint: "bob", Currency: "credits"},
	}

	got := CurrenciesIn(rows)

	if len(got) != 2 {
		t.Fatalf("CurrenciesIn = %v, want two units: the overflow exemption must not swallow the "+
			"real rows beside it", got)
	}
}

// The configured unit reaches the row on disk, and the default writes no field at all.
//
// THE PRODUCER SIDE OF THE WHOLE FEATURE, end to end through the seam the PR body says needed no
// threading: config -> Build -> CurrencyFor -> Record -> the bytes -> decode. Nothing covered it,
// so deleting the writer's entire unit-write block left the suite green, and so did dropping its
// not-the-default condition.
//
// ASSERTED ON THE BYTES, not only on the decoded Row, because both halves of the claim live there:
// a credits deployment must find "credits" in the file, and a dollars deployment must produce a
// file with no currency key in it — which is what "a single-currency deployment produces
// byte-identical files" means and the only form of that claim a test can check.
func TestRecord_ConfiguredUnitReachesTheRowOnDisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string // as written in pricing.endpoints[].unit
		// wantOnDisk is the currency value the decoded row carries, "" when the field is absent.
		wantOnDisk     string
		wantKeyInBytes bool
	}{
		{"absent means USD and writes nothing", "", "", false},
		{"explicit USD writes nothing either", "USD", "", false},
		// The spelling four consumers used to read as a non-default unit. normaliseUnit
		// canonicalises it, so it must be as silent on disk as "USD" is.
		{"a lowercase usd is still the default", "usd", "", false},
		{"a gateway billing in credits", "credits", "credits", true},
		{"an operator's own spelling is preserved", "Bobcoins", "Bobcoins", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
				Hosts: []string{"gw"},
				Unit:  tc.unit,
				Models: map[string]pricing.ModelConfig{
					"m": {TierRates: pricing.TierRates{InputCostPerMillion: 2}},
				},
			}}})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			dir := t.TempDir()
			now := at
			w, err := New(dir, WithClock(func() time.Time { return now }), WithPricing(tbl))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })

			w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
			now = at.Add(time.Minute)
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			if got := string(readAllBytes(t, dir)); strings.Contains(got, `"currency"`) != tc.wantKeyInBytes {
				t.Errorf("currency key present on disk = %v, want %v; line was:\n%s",
					!tc.wantKeyInBytes, tc.wantKeyInBytes, got)
			}
			rows := readAllRows(t, dir)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
			}
			if rows[0].Currency != tc.wantOnDisk {
				t.Errorf("Row.Currency on disk = %q, want %q", rows[0].Currency, tc.wantOnDisk)
			}
			// And the row reads back as the unit a reader will label its figure with, which is the
			// answer the refusal is computed from — the default spelled out, never empty.
			wantRead := tc.wantOnDisk
			if wantRead == "" {
				wantRead = pricing.CurrencyUSD
			}
			if got := rows[0].currencyOrUSD(); got != wantRead {
				t.Errorf("currencyOrUSD() = %q, want %q", got, wantRead)
			}
		})
	}
}
