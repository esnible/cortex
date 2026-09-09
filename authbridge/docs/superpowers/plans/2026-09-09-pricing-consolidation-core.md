# Consolidated Model Pricing — Core Consolidation (Phases 1-6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **THIS PLAN IS A SKELETON.** Phases 1-6 below carry verbatim scope from the spec but do
> not yet carry tasks. Read *How to complete this plan* before doing anything else.

**Goal:** Replace AuthBridge's ~17 scattered per-plugin rate knobs and 3 hand-measured
host globs with one `authlib/pricing` resolver — a single `pricing:` config section, a
generated bundled price table, and one arithmetic site — so every priced request reports
cost with explicit provenance and every unpriced one is named rather than shown as `$0.00`.

**Architecture:** A new standalone `authlib/pricing` package owns rates, tier resolution,
host-glob matching and `Cost()`. It is built once at startup into a long-lived `Registry`
with an atomically-swappable table, injected into pipelines via `Deps` / `BuildWithDeps`
and a `pricing.ResolverConsumer` interface, and then consumed by `toolprune`,
`litellm_budgettrack`, the `usage` aggregator, and `abctl`. Each consumer's local rate
table and token parser is deleted as it migrates. Phase 0 (already merged as #920) made
the aggregator consume the existing cost event; phase 7 (discovery) is deliberately out.

**Tech Stack:** Go 1.26.5, multi-module workspace (`authbridge/go.work`). Stdlib only —
no new dependencies.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-09-pricing-consolidation-design.md`
(see its *Phasing* section, entries 1-6, and *Implementation surface*)

**Issue:** cortex #910 · **PR:** PR 2 of 3 · **Predecessor:** phase 0 = #920 (merged, `f742c6f8`)

**Worktree / branch:** `.worktrees/pricing-core` on `feat/pricing-core`

---

## How to complete this plan

This skeleton exists because detailing all six phases in one shot exceeds Claude Code's
**block-level stream watchdog**: it aborts a turn when 300 s pass without a content block
completing, and a single `Write` of a ~2,000-line plan takes longer than that. The fix is
to detail **one phase per turn**.

**Protocol — follow it exactly:**

1. Pick the lowest-numbered phase whose **Status** is `⬜ NOT YET DETAILED`.
2. Read that phase's listed spec sections, plus the real source files it names.
3. Replace **only** that phase's `<!-- FILL-IN:PHASE-N -->` … `<!-- /FILL-IN:PHASE-N -->`
   block, using `Edit` — never `Write` the whole file, and never touch another phase's block.
4. Flip that phase's **Status** to `✅ DETAILED`.
5. Commit: `git commit -s -m "docs: detail phase N of pricing consolidation plan"`.
6. Stop. Next turn, next phase.

**What "detailed" means** — inside the block, emit `### Task N.M: <name>` sections
matching the shape used throughout `2026-09-09-pricing-consolidation-phase0.md`:

- `**Files:**` — `Create:` / `Modify: path:line-range` / `Test:` with exact paths
- `**Interfaces:**` — `Consumes:` and `Produces:`, with exact signatures and types
- Numbered `- [ ] **Step N: …**` checkboxes in strict TDD order: write the failing test →
  run it and confirm it fails → minimal implementation → run and confirm it passes → commit
- Real code in every code step. No `TBD`, no "add error handling", no "similar to Task N".

Task numbering is per-phase (`3.1`, `3.2`, …) so phases stay independently editable.

---

## Global Constraints

Every task's requirements implicitly include this section.

- **Go version:** 1.26.5 (`authbridge/go.work`). Do not raise it.
- **Modules:** phases 1-5 live in `github.com/rossoctl/cortex/authbridge/authlib`. **Phase 6
  crosses a module boundary** into `authbridge/cmd/abctl` (its own `go.mod`) — that module
  consumes `authlib` and must build and test separately.
- **No new dependencies.** Stdlib only.
- **Test command:** `cd authbridge/authlib && go test -race ./...`; for phase 6 also
  `cd authbridge/cmd/abctl && go test -race ./...`. CI runs `go test -v -race -cover ./...`
  (`.github/workflows/ci.yaml:67,128`).
- **Lint:** scoped `pre-commit --files <changed>` plus `go vet` and
  `golangci-lint --new-from-rev`. **Do not gate on `make lint`** — it fails on pre-existing
  ruff/E501 in `demos/` and `sparc-service` and rewrites ~10 unrelated files.
- **DCO:** every commit uses `git commit -s`. Required or CI fails.
- **Attribution:** `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. **Never**
  `Co-Authored-By` — a `commit-msg` hook rejects it.
- **Wire compatibility is mandatory.** The four JSON tags `cost_usd`, `source`,
  `daily_total_usd`, `daily_max_usd` must not change. `source` may only be extended
  **additively** (phase 4).
- **Unpriced must never render as `$0.00`.** A zero cost and an unknown cost are different
  answers. House style — see `costevent` and `usage/snapshot.go`.
- **Per-tier invariant** from the spec's *Tier scope* must hold in `Cost()` and be tested.
- **Redirect verbose command output to a file** and report only the exit code, per the
  repo's context-budget rule: `export LOG_DIR=/tmp/rossoctl/tdd/pricing-core && mkdir -p $LOG_DIR`.

---

## File Structure

Copied from the spec's *Implementation surface*; each phase's block narrows this to its own
slice.

**New** — `authlib/pricing/` (rates, resolver, `Cost`, host matching, generated bundled
slice; the discovery client is phase 7, **not** here) · `pricing.ResolverConsumer` ·
`Deps` + `BuildWithDeps`.

**Deleted** — `toolprune/pricing.go` defaults and glob machinery · 12 `toolprune` rate
knobs · `toolprune`'s `modelRates` / `normalize` / `rateFor` / `set` / `ratesFor` /
`rateSource` · `litellm_budgettrack`'s `parseFrameUsage` / `usageJSON` / `frameUsage` /
reconciliation / 4 rate knobs · client-side arithmetic in `abctl`'s `prune_saving.go`.
(`usage.Pricer` / `WithPricer` were already deleted in phase 0.)

**Changed** — `config.Config` (+`Pricing`) · `usage.Snapshot` (+`UnpricedBy`) ·
`usage.foldInto` · `litellm_budgettrack`'s `costEvent.source` values (additive) ·
`toolprune` and `litellm_budgettrack` configure/price paths · `abctl`'s `prune_saving.go`,
`cost_event.go`, `usage_render.go` · `plugin-catalog.md`, `tool-prune-plugin.md`,
`litellm-budgettrack-plugin.md`, `laptop-token-savings.md`.

**Net** — ~17 rate knobs → one `pricing:` section · 3 hand-measured globs → a generated
slice · 2 token parsers → 1 · 5 arithmetic sites → 1.

---

## Phase 1 — `authlib/pricing` standalone

**Status:** ✅ DETAILED · ✅ IMPLEMENTED

**Spec scope (verbatim, §Phasing entry 1):** "**`authlib/pricing` standalone** — types,
resolution, `Cost`, host matching, bundled slice + generator + golden test. No consumers.
Fully unit-tested in isolation."

**Surface:** new package `authbridge/authlib/pricing/`. `Tier`, `Rates` (4 base tiers plus
context thresholds), `Provenance`, `Resolve(endpoint, model, promptTotal)`, `Cost()` with
the per-tier invariant, host-glob matching with port stripping, exact-beats-glob then
longest-glob, a long-lived `Registry` with an atomically-swappable table, plus a generated
bundled price table derived from LiteLLM's public map with a generator and a golden test
pinning the upstream commit. No consumers touched in this phase.

**Read before detailing:** spec `### New package authlib/pricing/` (L240-282),
`### Resolution order` (L283-300), `## Tier scope` (L219-237), `### The bundled slice`
(L499-506), `## Testing & success criteria` (L507-544).

<!-- FILL-IN:PHASE-1 -->

### Task 1.1: Tiers, usage, and rates

The leaf types. No matching, no table, no consumers — just the vocabulary every
later task speaks, and the long-context threshold rule.

**Files:**
- Create: `authbridge/authlib/pricing/rates.go`
- Test: `authbridge/authlib/pricing/rates_test.go`

**Interfaces:**
- Consumes: nothing. New leaf package, stdlib only (`math` arrives in Task 1.2).
- Produces:
  - `type Tier int` with `TierInput`, `TierCacheWrite`, `TierCacheRead`, `TierOutput` and unexported `numTiers = 4`
  - `type Usage struct { Input, CacheWrite, CacheRead, Output int }` with `PromptTotal() int` and unexported `tokens() [numTiers]int`
  - `type ContextThreshold struct { AbovePromptTokens int; Rate [numTiers]float64; Set [numTiers]bool }`
  - `type Rates struct { Base [numTiers]float64; Set [numTiers]bool; Thresholds []ContextThreshold }` with `At(promptTotal int) Rates` and unexported `any() bool`

**Design note — why `Usage` is not `parsercommon.TokenUsage`.** The spec writes
`Cost(r Rates, u parsercommon.TokenUsage)`, but `parsercommon` lives at
`authlib/plugins/internal/parsercommon`, and Go's internal rule makes it importable
only from packages under `authlib/plugins/`. `authlib/pricing` is not one, so that
signature does not compile. Moving `parsercommon` out of `internal` would be worse —
it is the parsers' shared vocabulary and belongs to them. `pricing.Usage` mirrors its
four billable fields (dropping `Reasoning`, a subset of `Output`, and `ReportedTotal`,
which is an aggregate no tier prices) and one conversion helper lands in Task 1.4.

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/pricing/rates_test.go`:

```go
package pricing

import "testing"

// perMillion converts the unit providers publish into the per-token unit the
// package stores. Divided by a constant so the compiler folds it exactly — a
// runtime division lands a ulp low and makes expected values disagree in the
// last digit for no reason (the rule toolprune/pricing.go:45-47 already states).
const perMillion = 1_000_000

func TestUsagePromptTotal(t *testing.T) {
	u := Usage{Input: 10, CacheWrite: 20, CacheRead: 30, Output: 40}
	// Output is NOT prompt-side: a context threshold is measured against the
	// prompt, so folding output in would trip the premium early.
	if got, want := u.PromptTotal(), 60; got != want {
		t.Fatalf("PromptTotal() = %d, want %d", got, want)
	}
}

func TestUsageTokensIndexedByTier(t *testing.T) {
	u := Usage{Input: 1, CacheWrite: 2, CacheRead: 3, Output: 4}
	got := u.tokens()
	for tier, want := range map[Tier]int{
		TierInput: 1, TierCacheWrite: 2, TierCacheRead: 3, TierOutput: 4,
	} {
		if got[tier] != want {
			t.Errorf("tokens()[%d] = %d, want %d", tier, got[tier], want)
		}
	}
}

// base is a fully-priced Rates in the shape a real Claude entry has.
func base() Rates {
	return Rates{
		Base: [numTiers]float64{
			TierInput:      3.80 / perMillion,
			TierCacheWrite: 4.75 / perMillion,
			TierCacheRead:  0.38 / perMillion,
			TierOutput:     19.00 / perMillion,
		},
		Set: [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true},
	}
}

func TestRatesAt_ThresholdIsStrictlyExceeded(t *testing.T) {
	r := base()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}

	// Exactly at the boundary pays the base rate: "$X above 200k tokens" means
	// the premium starts at token 200,001. The spec calls this boundary out for
	// direct cover because off-by-one here misprices every long session.
	if got, want := r.At(200_000).Base[TierInput], 3.80/perMillion; got != want {
		t.Errorf("At(200000) input = %v, want %v", got, want)
	}
	if got, want := r.At(200_001).Base[TierInput], 7.60/perMillion; got != want {
		t.Errorf("At(200001) input = %v, want %v", got, want)
	}
}

func TestRatesAt_UnsetThresholdTierKeepsBase(t *testing.T) {
	r := base()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	eff := r.At(300_000)
	// A table that prices only long-context input must not silently unprice
	// output — that would flip a whole request to unpriced via Cost's invariant.
	if !eff.Set[TierOutput] {
		t.Fatal("output tier lost its rate when a threshold overrode only input")
	}
	if got, want := eff.Base[TierOutput], 19.00/perMillion; got != want {
		t.Errorf("At(300000) output = %v, want %v", got, want)
	}
}

func TestRatesAt_HighestExceededThresholdWins_AnyOrder(t *testing.T) {
	r := base()
	// Deliberately ascending: At must not depend on slice order, so a hand-built
	// Rates in a test or a hand-written config cannot pick the wrong tier.
	r.Thresholds = []ContextThreshold{
		{AbovePromptTokens: 200_000, Rate: [numTiers]float64{TierInput: 7.60 / perMillion}, Set: [numTiers]bool{TierInput: true}},
		{AbovePromptTokens: 500_000, Rate: [numTiers]float64{TierInput: 11.40 / perMillion}, Set: [numTiers]bool{TierInput: true}},
	}
	if got, want := r.At(600_000).Base[TierInput], 11.40/perMillion; got != want {
		t.Errorf("At(600000) input = %v, want %v", got, want)
	}
	if got, want := r.At(300_000).Base[TierInput], 7.60/perMillion; got != want {
		t.Errorf("At(300000) input = %v, want %v", got, want)
	}
}

func TestRatesAt_IsIdempotent(t *testing.T) {
	r := base()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	// Resolve returns already-flattened rates (Task 1.3), and Cost calls At again.
	// Flattening twice must be a no-op or the premium would be applied to a Rates
	// that no longer carries its thresholds and quietly fall back to base.
	once := r.At(300_000)
	twice := once.At(300_000)
	// Rates holds a slice, so it is not comparable with ==; compare the two
	// comparable halves and assert the slice is empty separately.
	if twice.Base != once.Base || twice.Set != once.Set {
		t.Errorf("At not idempotent: %+v then %+v", once, twice)
	}
	if len(once.Thresholds) != 0 {
		t.Error("At returned rates that still carry thresholds")
	}
}

func TestRatesAny(t *testing.T) {
	if (Rates{}).any() {
		t.Error("empty Rates reported a rate")
	}
	if !base().any() {
		t.Error("fully-priced Rates reported no rate")
	}
	// Threshold-only is still a rate: NewTable (Task 1.3) uses any() to reject
	// rows that price nothing, and a long-context-only row prices something.
	thresholdOnly := Rates{Thresholds: []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}}
	if !thresholdOnly.any() {
		t.Error("threshold-only Rates reported no rate")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

```bash
export LOG_DIR=/tmp/rossoctl/tdd/pricing-core && mkdir -p $LOG_DIR
cd authbridge/authlib && go test ./pricing/ > $LOG_DIR/1.1-red.log 2>&1; echo "EXIT:$?"
```

Expected: non-zero, with build errors — `undefined: Usage`, `undefined: Rates`,
`undefined: numTiers`, `undefined: TierInput`. A build failure is the correct red
here: the package does not exist yet.

- [ ] **Step 3: Write the minimal implementation**

Create `authbridge/authlib/pricing/rates.go`:

```go
// Package pricing owns model rates and the arithmetic that turns tokens into
// dollars. It is the single source of truth for both: before it, rates lived in
// two plugins' configs and dollars were computed in five places, which is how a
// stale "understates by 4x" comment survived a gateway repricing.
//
// The package holds no I/O and no provider knowledge. A rate table is built once
// at startup and swapped atomically; resolution is a pure function of
// (endpoint, model, prompt size).
package pricing

// Tier names which kind of token is being priced.
//
// Four tiers, because that is what every provider on a Claude path bills
// separately and prompt-cache tiers differ by more than 12x: a cache write is
// 1.25x input and a cache read 0.1x, so one flat rate misprices cache-heavy
// traffic (Claude Code) by up to ~10x.
//
// Deliberately absent: service-tier variants (flex / priority / ultrafast) and
// the 1-hour-TTL cache-write premium. Both are real fields in LiteLLM's
// ModelInfoBase, neither is on a path we run, and an unpopulated tier is an
// untested tier. Adding one later is a field plus a threshold row, not a reshape.
type Tier int

const (
	TierInput Tier = iota
	TierCacheWrite
	TierCacheRead
	TierOutput
)

// numTiers sizes the per-tier arrays. Arrays rather than named fields so the
// arithmetic is a loop over every tier: an enumerated form is where a forgotten
// tier hides, and a forgotten tier silently prices at zero.
const numTiers = 4

// Usage is one request's token count, split by tier.
//
// This mirrors parsercommon.TokenUsage, which is what the inference parsers
// already publish — but parsercommon sits under authlib/plugins/internal/, so
// Go's internal rule puts it out of reach here. Reasoning is omitted (a subset of
// Output, already counted) and so is ReportedTotal (an aggregate no tier prices).
type Usage struct {
	Input      int // uncached prompt tokens
	CacheWrite int // prompt tokens written to cache
	CacheRead  int // prompt tokens served from cache
	Output     int // generated completion tokens
}

// PromptTotal is the prompt-side total, which is what a context threshold is
// measured against. Output is excluded: a long-context premium is priced on how
// much prompt was sent, not on what came back.
func (u Usage) PromptTotal() int { return u.Input + u.CacheWrite + u.CacheRead }

// tokens indexes the counts by Tier so Cost can loop over every tier.
func (u Usage) tokens() [numTiers]int {
	return [numTiers]int{
		TierInput:      u.Input,
		TierCacheWrite: u.CacheWrite,
		TierCacheRead:  u.CacheRead,
		TierOutput:     u.Output,
	}
}

// ContextThreshold is a long-context rate override: above AbovePromptTokens the
// tiers it Sets replace the base ones.
//
// Tiers it does not set keep their base rate rather than becoming unset. A table
// that prices only long-context input would otherwise unset output above the
// threshold, and Cost's per-tier invariant would flip the whole request to
// unpriced — turning a partial price list into a coverage hole.
type ContextThreshold struct {
	AbovePromptTokens int
	Rate              [numTiers]float64
	Set               [numTiers]bool
}

// Rates is one (endpoint, model) pair's rates, in USD per token.
//
// Set exists because a zero rate and an absent rate are different answers. Zero
// means free; absent means unknown, and Cost refuses to price a request whose
// tokens land on an absent tier (see Cost).
type Rates struct {
	Base       [numTiers]float64
	Set        [numTiers]bool
	Thresholds []ContextThreshold
}

// At flattens Rates for a prompt of promptTotal tokens: the highest threshold
// strictly exceeded, overlaid on Base, with Thresholds cleared.
//
// Strictly exceeded, not met — "$X above 200k tokens" means the premium starts at
// token 200,001, so a request of exactly 200k pays base.
//
// The scan takes the maximum exceeded threshold rather than the first, so At does
// not depend on slice order. Order-dependence here would be a footgun with no
// visible symptom: a config or test that happened to list thresholds ascending
// would price long context at the lower premium and look plausible.
//
// Idempotent: the result carries no thresholds, so flattening it again returns it
// unchanged. Resolve hands out flattened rates and Cost flattens again.
func (r Rates) At(promptTotal int) Rates {
	out := Rates{Base: r.Base, Set: r.Set}
	best := -1
	var pick *ContextThreshold
	for i := range r.Thresholds {
		t := &r.Thresholds[i]
		if promptTotal > t.AbovePromptTokens && t.AbovePromptTokens > best {
			best, pick = t.AbovePromptTokens, t
		}
	}
	if pick == nil {
		return out
	}
	for i := range out.Base {
		if pick.Set[i] {
			out.Base[i], out.Set[i] = pick.Rate[i], true
		}
	}
	return out
}

// any reports whether these rates price anything at all, at any prompt size.
// NewTable uses it to reject a row that prices nothing: such a row would match
// traffic and then resolve it as unpriced, which is indistinguishable from having
// no row and hides the typo that produced it.
func (r Rates) any() bool {
	for _, s := range r.Set {
		if s {
			return true
		}
	}
	for _, t := range r.Thresholds {
		for _, s := range t.Set {
			if s {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 4: Run the test and confirm it passes**

```bash
cd authbridge/authlib && go test -race ./pricing/ > $LOG_DIR/1.1-green.log 2>&1; echo "EXIT:$?"
```

Expected: `EXIT:0`. If it fails, read only the failing assertion out of the log —
do not cat the whole file.

- [ ] **Step 5: Commit**

```bash
git add authbridge/authlib/pricing/rates.go authbridge/authlib/pricing/rates_test.go
git commit -s -m "feat: Add pricing tiers, usage split and context-threshold rates

The leaf vocabulary for authlib/pricing: four billable tiers, a per-tier
token split, and rates whose long-context thresholds flatten by strict
exceedance so a request exactly at the boundary pays the base rate.

Rates.Set distinguishes a zero rate from an absent one, which is what lets
Cost refuse to under-price rather than charge a tier zero.

Uses its own Usage rather than the spec's parsercommon.TokenUsage: that type
lives under plugins/internal/ and is unreachable from this package.

Refs #910

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 1.2: `Cost` and the per-tier invariant

The one place tokens become dollars. Five sites collapse into this function.

**Files:**
- Create: `authbridge/authlib/pricing/cost.go`
- Test: `authbridge/authlib/pricing/cost_test.go`

**Interfaces:**
- Consumes: `Rates`, `Rates.At`, `Usage`, `Usage.tokens`, `Usage.PromptTotal`, `numTiers`, the `Tier` constants (Task 1.1)
- Produces: `func Cost(r Rates, u Usage) (micros int64, ok bool)`

**The invariant, stated once:** a request is priced only if every tier that
carried tokens had a rate. Otherwise `ok` is false and the request counts as
*unpriced*, never as under-priced. This is not new policy — it is
`toolprune`'s `rateFor` rule (`plugins/toolprune/plugin.go:155-174`) promoted, and
that function's own comment says why: a model configured with only
`cache_read_cost_per_token` "used to resolve as 'priced' and then return 0 for a
cache-write request — pricing it at zero while still counting toward the priced
denominator, so the saving silently vanished with no `requests unpriced` row to
show it had."

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/pricing/cost_test.go`:

```go
package pricing

import "testing"

// claudeOpus is a fully-priced entry in the published unit.
func claudeOpus() Rates {
	return Rates{
		Base: [numTiers]float64{
			TierInput:      3.80 / perMillion,
			TierCacheWrite: 4.75 / perMillion,
			TierCacheRead:  0.38 / perMillion,
			TierOutput:     19.00 / perMillion,
		},
		Set: [numTiers]bool{TierInput: true, TierCacheWrite: true, TierCacheRead: true, TierOutput: true},
	}
}

func TestCost_AllFourTiers(t *testing.T) {
	// 1000*3.80 + 2000*4.75 + 10000*0.38 + 500*19.00, per million
	//   = 3800 + 9500 + 3800 + 9500 micros
	u := Usage{Input: 1000, CacheWrite: 2000, CacheRead: 10_000, Output: 500}
	micros, ok := Cost(claudeOpus(), u)
	if !ok {
		t.Fatal("Cost reported unpriced for a fully-priced model")
	}
	if want := int64(26_600); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

// TestCost_PerTierInvariant is the regression this function exists to hold. A
// model priced only for cache reads must not price a cache-write request at zero.
func TestCost_PerTierInvariant(t *testing.T) {
	cacheReadOnly := Rates{
		Base: [numTiers]float64{TierCacheRead: 0.38 / perMillion},
		Set:  [numTiers]bool{TierCacheRead: true},
	}

	if _, ok := Cost(cacheReadOnly, Usage{CacheWrite: 5000}); ok {
		t.Error("a cache-write request priced against a cache-read-only model reported priced")
	}
	// The mirror: the tier it does price still prices.
	micros, ok := Cost(cacheReadOnly, Usage{CacheRead: 10_000})
	if !ok {
		t.Fatal("a cache-read request against a cache-read rate reported unpriced")
	}
	if want := int64(3_800); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

func TestCost_UnsetTierWithNoTokensStillPrices(t *testing.T) {
	// Absent output rate, zero output tokens. The invariant is about tiers that
	// CARRIED tokens; refusing here would unprice every request on a prompt-only
	// rate table (which is exactly what toolprune ships, having no output rate).
	noOutput := Rates{
		Base: [numTiers]float64{TierInput: 3.80 / perMillion},
		Set:  [numTiers]bool{TierInput: true},
	}
	micros, ok := Cost(noOutput, Usage{Input: 1000})
	if !ok {
		t.Fatal("reported unpriced when the only unset tier carried no tokens")
	}
	if want := int64(3_800); micros != want {
		t.Errorf("Cost = %d micros, want %d", micros, want)
	}
}

func TestCost_ZeroUsageIsUnpriced(t *testing.T) {
	// No tokens reported at all is unknown usage, not a free request. Calling it
	// "priced $0" would put it in the priced denominator and dilute coverage.
	if _, ok := Cost(claudeOpus(), Usage{}); ok {
		t.Error("empty usage reported priced")
	}
}

func TestCost_NegativeTokensRefused(t *testing.T) {
	// A negative count is a parser bug or a hostile body. Refuse rather than emit
	// a negative cost, which would corrode a running total that nothing re-derives.
	if _, ok := Cost(claudeOpus(), Usage{Input: -1, Output: 100}); ok {
		t.Error("negative token count reported priced")
	}
}

func TestCost_UnpricedModelIsUnpriced(t *testing.T) {
	if _, ok := Cost(Rates{}, Usage{Input: 1000}); ok {
		t.Error("empty Rates reported priced")
	}
}

func TestCost_AppliesContextThreshold(t *testing.T) {
	r := claudeOpus()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}

	// 300k prompt tokens, all uncached: 300000 * 7.60/1e6 = 2.28 USD.
	micros, ok := Cost(r, Usage{Input: 300_000})
	if !ok {
		t.Fatal("long-context request reported unpriced")
	}
	if want := int64(2_280_000); micros != want {
		t.Errorf("Cost above threshold = %d micros, want %d", micros, want)
	}

	// And below it, the base rate: 100000 * 3.80/1e6 = 0.38 USD.
	micros, ok = Cost(r, Usage{Input: 100_000})
	if !ok {
		t.Fatal("short request reported unpriced")
	}
	if want := int64(380_000); micros != want {
		t.Errorf("Cost below threshold = %d micros, want %d", micros, want)
	}
}

func TestCost_ThresholdMeasuredOnPromptNotOutput(t *testing.T) {
	r := claudeOpus()
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	// 100k prompt with 500k output must NOT trip a prompt-size threshold.
	micros, ok := Cost(r, Usage{Input: 100_000, Output: 1})
	if !ok {
		t.Fatal("reported unpriced")
	}
	if want := int64(380_000 + 19); micros != want {
		t.Errorf("Cost = %d micros, want %d (threshold tripped on output)", micros, want)
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

```bash
cd authbridge/authlib && go test ./pricing/ > $LOG_DIR/1.2-red.log 2>&1; echo "EXIT:$?"
```

Expected: non-zero, `undefined: Cost`.

- [ ] **Step 3: Write the minimal implementation**

Create `authbridge/authlib/pricing/cost.go`:

```go
package pricing

import "math"

// Cost prices u at r, returning integer micros — millionths of a dollar.
//
// Micros because usage.Counts.CostMicros is already that unit, and integer
// addition across ring buckets is exact where repeated float addition is not.
//
// ok is false when the request is UNPRICED, which is a different answer from a
// cost of zero. Four ways to be unpriced:
//
//   - A tier that carried tokens has no rate. The invariant: a request is priced
//     only if every tier it used had a rate, so a partial table produces a named
//     gap rather than a total that is quietly too low. This is toolprune's
//     rateFor rule (plugin.go:155-174) promoted — see the plan for the failure it
//     was written against.
//   - No tokens were reported at all. That is unknown usage, not a free request;
//     counting it as priced-zero would dilute the coverage denominator.
//   - A negative count, which is a parser bug or a hostile body. A negative cost
//     would corrode a running total that nothing re-derives.
//   - No rates at all, which is the ProvNone case reaching here directly.
//
// A tier with no rate but no tokens is fine: toolprune's table has no output rate
// at all, and refusing there would unprice every request it measures.
func Cost(r Rates, u Usage) (micros int64, ok bool) {
	if u == (Usage{}) {
		return 0, false
	}
	eff := r.At(u.PromptTotal())
	var usd float64
	for i, n := range u.tokens() {
		if n < 0 {
			return 0, false
		}
		if n == 0 {
			continue
		}
		if !eff.Set[i] {
			return 0, false
		}
		usd += float64(n) * eff.Base[i]
	}
	return int64(math.Round(usd * 1e6)), true
}
```

- [ ] **Step 4: Run the test and confirm it passes**

```bash
cd authbridge/authlib && go test -race ./pricing/ > $LOG_DIR/1.2-green.log 2>&1; echo "EXIT:$?"
```

Expected: `EXIT:0`.

- [ ] **Step 5: Prove the invariant test is load-bearing**

A test that passes against a broken implementation is not cover. Mutate `Cost` to
charge an absent tier zero instead of refusing — the pre-consolidation behaviour —
and confirm `TestCost_PerTierInvariant` is what catches it.

```bash
cd authbridge/authlib
python3 - <<'PY'
import pathlib
p = pathlib.Path("pricing/cost.go")
s = p.read_text()
p.with_suffix(".go.bak").write_text(s)
p.write_text(s.replace("\t\tif !eff.Set[i] {\n\t\t\treturn 0, false\n\t\t}\n", "\t\tif !eff.Set[i] {\n\t\t\tcontinue\n\t\t}\n"))
PY
go test ./pricing/ -run TestCost > $LOG_DIR/1.2-mutant.log 2>&1; echo "MUTANT_EXIT:$?"
mv pricing/cost.go.bak pricing/cost.go
go test ./pricing/ -run TestCost > $LOG_DIR/1.2-restored.log 2>&1; echo "RESTORED_EXIT:$?"
```

Expected: `MUTANT_EXIT:1` with `TestCost_PerTierInvariant` and
`TestCost_UnpricedModelIsUnpriced` both named in the failure, and
`RESTORED_EXIT:0`. If the mutant passes, the test is not testing the invariant —
fix the test before continuing. Confirm `git status --porcelain` shows no `.bak`
left behind.

- [ ] **Step 6: Commit**

```bash
git add authbridge/authlib/pricing/cost.go authbridge/authlib/pricing/cost_test.go
git commit -s -m "feat: Add pricing.Cost, the single tokens-to-dollars site

One function replaces the five places that multiplied tokens by rates.
Returns integer micros to match usage.Counts.CostMicros, so bucket
addition stays exact.

Holds the per-tier invariant: a request is priced only if every tier that
carried tokens had a rate. Otherwise it is unpriced, never under-priced.
Promoted from toolprune's rateFor, whose comment records the failure it
was written against — a cache-read-only rate charging a cache-write
request zero while still counting as priced.

Zero usage, negative counts and an empty table are all unpriced rather
than \$0.00, per the house rule that a zero cost and an unknown cost are
different answers.

Refs #910

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 1.3: `Provenance`, `Table` and resolution precedence

Rates gain an origin, and a table resolves `(endpoint, model)` to exactly one row.

**Files:**
- Create: `authbridge/authlib/pricing/provenance.go`
- Create: `authbridge/authlib/pricing/table.go`
- Test: `authbridge/authlib/pricing/table_test.go`

**Interfaces:**
- Consumes: `Rates`, `Rates.At`, `Rates.any` (Task 1.1)
- Produces:
  - `type Provenance int` with `ProvNone`, `ProvBundled`, `ProvDiscovered`, `ProvConfigured`, `ProvAuthoritative`, and `String() string`
  - `type Resolver interface { Resolve(endpoint, model string, promptTotal int) (Rates, Provenance) }`
  - `type Entry struct { Host, Model string; Rates Rates; Prov Provenance }`
  - `type Table struct{ ... }`, `func NewTable(entries []Entry) (*Table, error)`, `func (t *Table) Resolve(endpoint, model string, promptTotal int) (Rates, Provenance)`

**Scope boundary — host matching is exact here.** `Resolve` in this task matches an
`Entry.Host` by case-insensitive equality, with `""` and `"*"` meaning any
endpoint. Task 1.4 widens that to host globs with the port stripped (`path.Match`,
the idiom already established at `plugins/sparc/collect.go:148-156`) and adds its own
cases. Model globbing is *not* deferred — it is what makes "exact beats glob"
meaningful, so it lands here via `gobwas/glob` (already an `authlib` dependency,
`authlib/go.mod:9`).

**Why `promptTotal` is on `Resolve`.** `Resolve` returns rates already flattened
through `Rates.At(promptTotal)`, so the caller never has to remember to apply the
long-context threshold — forgetting would silently price long sessions at base.
`Cost` flattens again, which `At`'s idempotence makes free.

**Two precedence rules, and why that order.** Provenance decides first —
configured beats discovered beats bundled, because an override is an override.
Specificity breaks ties *within* a level, and there the endpoint outranks the
model: a named host beats a catch-all host before an exact model beats a model
glob. The endpoint is the more consequential axis, since rates differ more between
a discounted gateway and vendor list than between two models on one endpoint, so a
deployment-specific host row must not be shadowed by a broader model glob. Within
each axis, exact beats glob and longer beats shorter — `toolprune`'s existing rule
(`plugin.go:190-209`, `pricing.go:80-85`).

- [ ] **Step 1: Write the failing test**

Create `authbridge/authlib/pricing/table_test.go`:

```go
package pricing

import (
	"strings"
	"testing"
)

// rate builds a Rates priced only on input, at the given USD per million. Enough
// to tell resolved rows apart by their value.
func rate(perM float64) Rates {
	return Rates{
		Base: [numTiers]float64{TierInput: perM / perMillion},
		Set:  [numTiers]bool{TierInput: true},
	}
}

// inputPerMillion is the inverse of rate, for readable assertions.
func inputPerMillion(r Rates) float64 { return r.Base[TierInput] * perMillion }

func mustTable(t *testing.T, entries ...Entry) *Table {
	t.Helper()
	tab, err := NewTable(entries)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tab
}

func TestProvenanceString(t *testing.T) {
	for p, want := range map[Provenance]string{
		ProvNone:          "none",
		ProvBundled:       "bundled",
		ProvDiscovered:    "discovered",
		ProvConfigured:    "configured",
		ProvAuthoritative: "authoritative",
	} {
		if got := p.String(); got != want {
			t.Errorf("Provenance(%d).String() = %q, want %q", p, got, want)
		}
	}
}

func TestResolve_ProvenanceOutranksSpecificity(t *testing.T) {
	// A configured catch-all beats a bundled exact match. An override is an
	// override: an operator who priced their gateway must not be silently
	// overruled by the shipped table just because it names the model precisely.
	tab := mustTable(t,
		Entry{Host: "*", Model: "*", Rates: rate(1.00), Prov: ProvConfigured},
		Entry{Host: "*", Model: "claude-opus-5", Rates: rate(5.00), Prov: ProvBundled},
	)
	r, p := tab.Resolve("gw.internal", "claude-opus-5", 0)
	if p != ProvConfigured {
		t.Errorf("provenance = %s, want configured", p)
	}
	if got := inputPerMillion(r); got != 1.00 {
		t.Errorf("input rate = %v, want 1.00", got)
	}
}

func TestResolve_ConfiguredBeatsDiscoveredBeatsBundled(t *testing.T) {
	all := []Entry{
		{Host: "*", Model: "claude-opus-5", Rates: rate(3.00), Prov: ProvBundled},
		{Host: "*", Model: "claude-opus-5", Rates: rate(2.00), Prov: ProvDiscovered},
		{Host: "*", Model: "claude-opus-5", Rates: rate(1.00), Prov: ProvConfigured},
	}
	for _, tc := range []struct {
		name    string
		entries []Entry
		want    Provenance
		perM    float64
	}{
		{"all three", all, ProvConfigured, 1.00},
		{"discovered and bundled", all[:2], ProvDiscovered, 2.00},
		{"bundled only", all[:1], ProvBundled, 3.00},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := mustTable(t, tc.entries...).Resolve("gw.internal", "claude-opus-5", 0)
			if p != tc.want {
				t.Errorf("provenance = %s, want %s", p, tc.want)
			}
			if got := inputPerMillion(r); got != tc.perM {
				t.Errorf("input rate = %v, want %v", got, tc.perM)
			}
		})
	}
}

func TestResolve_ExactModelBeatsGlobWithinLevel(t *testing.T) {
	// This is the whole point of the bundled slice: it distinguishes
	// claude-opus-4-1 from claude-opus-5, which a family glob cannot.
	tab := mustTable(t,
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(5.00), Prov: ProvBundled},
		Entry{Host: "*", Model: "claude-opus-4-1", Rates: rate(15.00), Prov: ProvBundled},
	)
	if got := inputPerMillion(first(tab.Resolve("api.anthropic.com", "claude-opus-4-1", 0))); got != 15.00 {
		t.Errorf("exact model rate = %v, want 15.00", got)
	}
	// And a version the exact rows do not name still lands on the family glob.
	if got := inputPerMillion(first(tab.Resolve("api.anthropic.com", "claude-opus-6", 0))); got != 5.00 {
		t.Errorf("glob fallback rate = %v, want 5.00", got)
	}
}

func TestResolve_LongerGlobWins(t *testing.T) {
	tab := mustTable(t,
		Entry{Host: "*", Model: "*claude-*", Rates: rate(1.00), Prov: ProvBundled},
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(5.00), Prov: ProvBundled},
	)
	if got := inputPerMillion(first(tab.Resolve("h", "claude-opus-5", 0))); got != 5.00 {
		t.Errorf("rate = %v, want 5.00 (longer glob)", got)
	}
	if got := inputPerMillion(first(tab.Resolve("h", "claude-haiku-4-5", 0))); got != 1.00 {
		t.Errorf("rate = %v, want 1.00 (shorter glob)", got)
	}
}

func TestResolve_NamedHostBeatsCatchAllHost(t *testing.T) {
	// The endpoint axis outranks the model axis: a gateway priced by the operator
	// must not be overruled by a broader model glob on "*".
	tab := mustTable(t,
		Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(15.00), Prov: ProvConfigured},
		Entry{Host: "gw.internal", Model: "*", Rates: rate(3.80), Prov: ProvConfigured},
	)
	if got := inputPerMillion(first(tab.Resolve("gw.internal", "claude-opus-5", 0))); got != 3.80 {
		t.Errorf("rate on named host = %v, want 3.80", got)
	}
	// Traffic to any other endpoint still gets the catch-all row.
	if got := inputPerMillion(first(tab.Resolve("api.anthropic.com", "claude-opus-5", 0))); got != 15.00 {
		t.Errorf("rate on other host = %v, want 15.00", got)
	}
}

func TestResolve_HostScopingExcludes(t *testing.T) {
	// The requirement this dimension exists for: one laptop, several endpoints,
	// each priced differently. A row for one gateway must not price another.
	tab := mustTable(t,
		Entry{Host: "gw-a.internal", Model: "*", Rates: rate(3.80), Prov: ProvConfigured},
		Entry{Host: "gw-b.internal", Model: "*", Rates: rate(7.60), Prov: ProvConfigured},
	)
	if got := inputPerMillion(first(tab.Resolve("gw-a.internal", "m", 0))); got != 3.80 {
		t.Errorf("gw-a rate = %v, want 3.80", got)
	}
	if got := inputPerMillion(first(tab.Resolve("gw-b.internal", "m", 0))); got != 7.60 {
		t.Errorf("gw-b rate = %v, want 7.60", got)
	}
	if _, p := tab.Resolve("api.anthropic.com", "m", 0); p != ProvNone {
		t.Errorf("unlisted endpoint resolved as %s, want none", p)
	}
}

func TestResolve_HostIsCaseInsensitive(t *testing.T) {
	tab := mustTable(t, Entry{Host: "GW.Internal", Model: "*", Rates: rate(3.80), Prov: ProvConfigured})
	if _, p := tab.Resolve("gw.internal", "m", 0); p != ProvConfigured {
		t.Errorf("host match was case-sensitive: got %s", p)
	}
}

func TestResolve_ModelIsCaseInsensitive(t *testing.T) {
	// Gateways vary in how they echo model names, and a case mismatch would
	// silently unprice the traffic rather than fail visibly.
	tab := mustTable(t, Entry{Host: "*", Model: "Claude-Opus-5", Rates: rate(5.00), Prov: ProvBundled})
	if _, p := tab.Resolve("h", "claude-opus-5", 0); p != ProvBundled {
		t.Errorf("model match was case-sensitive: got %s", p)
	}
}

func TestResolve_ModelGlobSpansDashAndSlash(t *testing.T) {
	// Provider prefixes and dated suffixes must not defeat a family glob, so the
	// model matcher is compiled with no separator (toolprune/pricing.go:65-67).
	tab := mustTable(t, Entry{Host: "*", Model: "*claude-opus-*", Rates: rate(5.00), Prov: ProvBundled})
	for _, model := range []string{
		"claude-opus-5",
		"aws/claude-opus-4-1-20250805",
		"anthropic/claude-opus-4-8",
	} {
		if _, p := tab.Resolve("h", model, 0); p != ProvBundled {
			t.Errorf("model %q did not match the family glob", model)
		}
	}
}

func TestResolve_NoMatchIsProvNone(t *testing.T) {
	tab := mustTable(t, Entry{Host: "*", Model: "*claude-*", Rates: rate(5.00), Prov: ProvBundled})
	r, p := tab.Resolve("h", "gpt-5", 0)
	if p != ProvNone {
		t.Errorf("provenance = %s, want none", p)
	}
	if r.any() {
		t.Error("unmatched model returned rates")
	}
}

func TestResolve_FlattensThresholdForPromptTotal(t *testing.T) {
	r := rate(3.80)
	r.Thresholds = []ContextThreshold{{
		AbovePromptTokens: 200_000,
		Rate:              [numTiers]float64{TierInput: 7.60 / perMillion},
		Set:               [numTiers]bool{TierInput: true},
	}}
	tab := mustTable(t, Entry{Host: "*", Model: "*", Rates: r, Prov: ProvConfigured})

	// Resolve hands back already-flattened rates, so a caller cannot forget to
	// apply the premium.
	got, _ := tab.Resolve("h", "m", 300_000)
	if inputPerMillion(got) != 7.60 {
		t.Errorf("resolved input rate above threshold = %v, want 7.60", inputPerMillion(got))
	}
	if len(got.Thresholds) != 0 {
		t.Error("Resolve returned rates that still carry thresholds")
	}
	got, _ = tab.Resolve("h", "m", 100_000)
	if inputPerMillion(got) != 3.80 {
		t.Errorf("resolved input rate below threshold = %v, want 3.80", inputPerMillion(got))
	}
}

func TestNilTableResolvesNone(t *testing.T) {
	// A binary built without pricing wiring must report traffic as unpriced, not
	// panic on the response path.
	var tab *Table
	if _, p := tab.Resolve("h", "m", 0); p != ProvNone {
		t.Errorf("nil table resolved as %s, want none", p)
	}
}

func TestNewTable_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry Entry
		want  string
	}{{
		// Authoritative is a settled per-request figure, not a rate. A table row
		// claiming it would outrank every real rate and never be corrected.
		name:  "authoritative provenance",
		entry: Entry{Host: "*", Model: "*", Rates: rate(1), Prov: ProvAuthoritative},
		want:  "authoritative",
	}, {
		name:  "no provenance",
		entry: Entry{Host: "*", Model: "*", Rates: rate(1)},
		want:  "provenance",
	}, {
		// A row that prices nothing matches traffic and then resolves it as
		// unpriced — indistinguishable from having no row, and it hides the typo.
		name:  "prices nothing",
		entry: Entry{Host: "*", Model: "*", Prov: ProvConfigured},
		want:  "no rate",
	}, {
		name:  "bad model glob",
		entry: Entry{Host: "*", Model: "claude-[", Rates: rate(1), Prov: ProvConfigured},
		want:  "model pattern",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewTable([]Entry{tc.entry})
			if err == nil {
				t.Fatal("NewTable accepted an invalid entry")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestNewTable_EmptyIsValid(t *testing.T) {
	// An operator with no pricing config and the bundled slice disabled is a
	// legitimate deployment; it prices nothing and says so.
	tab, err := NewTable(nil)
	if err != nil {
		t.Fatalf("NewTable(nil): %v", err)
	}
	if _, p := tab.Resolve("h", "m", 0); p != ProvNone {
		t.Errorf("empty table resolved as %s, want none", p)
	}
}

func TestTableImplementsResolver(t *testing.T) {
	var _ Resolver = (*Table)(nil)
}

// first drops the provenance from a Resolve result, for assertions that only
// look at the rates.
func first(r Rates, _ Provenance) Rates { return r }
```

- [ ] **Step 2: Run the test and confirm it fails**

```bash
cd authbridge/authlib && go test ./pricing/ > $LOG_DIR/1.3-red.log 2>&1; echo "EXIT:$?"
```

Expected: non-zero, `undefined: Entry`, `undefined: NewTable`, `undefined: Provenance`,
`undefined: Resolver`.

- [ ] **Step 3: Write `provenance.go`**

Create `authbridge/authlib/pricing/provenance.go`:

```go
package pricing

// Provenance says where a rate came from, so every reported figure carries its
// own credibility instead of looking equally authoritative either way.
//
// Ordered by precedence: a higher value wins a resolution. That is the whole
// reason these are ranked rather than named — see Table.Resolve.
type Provenance int

const (
	// ProvNone means no rate was found. The request is unpriced, which is not the
	// same as free.
	ProvNone Provenance = iota
	// ProvBundled is the price table shipped in the binary. A starting point that
	// needs no configuration, not a fact about the operator's account.
	ProvBundled
	// ProvDiscovered is a rate fetched from the gateway's own /model/info. Phase 7
	// produces these; nothing in this PR does.
	ProvDiscovered
	// ProvConfigured is an explicit pricing.endpoints[].models entry. An override
	// is an override, so it outranks a fetched value.
	ProvConfigured
	// ProvAuthoritative is a real observed cost for one request — LiteLLM's
	// X-Litellm-Response-Cost or its -Original variant. Not a rate but a settled
	// figure, so it never appears in a rate table and bypasses Cost entirely.
	// NewTable rejects it.
	ProvAuthoritative
)

func (p Provenance) String() string {
	switch p {
	case ProvBundled:
		return "bundled"
	case ProvDiscovered:
		return "discovered"
	case ProvConfigured:
		return "configured"
	case ProvAuthoritative:
		return "authoritative"
	default:
		return "none"
	}
}

// Resolver hands out rates for a request. An interface so a plugin can be tested
// against a fixed table, and so discovery (phase 7) can wrap a Table rather than
// reach inside it.
//
// promptTotal is the request's prompt-side token count: the returned Rates are
// already flattened through Rates.At for that size, so a caller cannot forget to
// apply a long-context premium.
type Resolver interface {
	Resolve(endpoint, model string, promptTotal int) (Rates, Provenance)
}
```

- [ ] **Step 4: Write `table.go`**

Create `authbridge/authlib/pricing/table.go`:

```go
package pricing

import (
	"fmt"
	"strings"

	"github.com/gobwas/glob"
)

// Entry is one row of a rate table: rates for the models matching Model on the
// endpoints matching Host.
//
// Host "" and "*" both mean any endpoint — how the bundled slice is scoped, since
// a shipped table cannot know an operator's gateway names. Model is a glob
// matched case-insensitively.
type Entry struct {
	Host  string
	Model string
	Rates Rates
	Prov  Provenance
}

// Table is an immutable resolved rate table. Build one with NewTable; never
// mutate one that is live, because a Registry hands the same pointer to every
// concurrent reader.
type Table struct{ rows []row }

type row struct {
	host  string // lower-cased; "" or "*" means any
	model modelMatcher
	spec  specificity
	rates Rates
	prov  Provenance
}

// modelMatcher is one compiled model pattern.
//
// Compiled with NO separator passed to glob.Compile, so "*" spans both "-" and
// "/" — "*claude-opus-*" has to match "aws/claude-opus-4-1-20250805". That is
// toolprune/pricing.go:65-67's rule, and it differs from the "."-delimited host
// globs elsewhere in authlib, which is why the two dimensions do not share a
// matcher.
type modelMatcher struct {
	pattern string
	g       glob.Glob
	exact   bool // no metacharacters: names exactly one model
}

// globMeta are the characters that make a pattern a glob rather than a literal.
const globMeta = `*?[]{}!\`

func compileModel(pattern string) (modelMatcher, error) {
	lower := strings.ToLower(pattern)
	g, err := glob.Compile(lower)
	if err != nil {
		return modelMatcher{}, fmt.Errorf("pricing: model pattern %q: %w", pattern, err)
	}
	return modelMatcher{
		pattern: lower,
		g:       g,
		exact:   !strings.ContainsAny(lower, globMeta),
	}, nil
}

// match lower-cases the subject because gateways vary in how they echo model
// names, and a case mismatch would silently unprice the traffic rather than fail
// visibly (toolprune/plugin.go:224-226).
func (m modelMatcher) match(model string) bool { return m.g.Match(strings.ToLower(model)) }

// specificity ranks how tightly a row names its target, for tie-breaks WITHIN one
// provenance level.
//
// The endpoint axis is compared before the model axis. Rates differ more between
// a discounted gateway and vendor list than between two models on one endpoint,
// so a deployment-specific host row must not be shadowed by a broader model glob.
// Within each axis: exact beats glob, then longer beats shorter — toolprune's rule
// (plugin.go:190-209, pricing.go:80-85). The pattern string is the final tie-break
// so two equally specific rows resolve the same way across restarts instead of
// whichever map iteration reached first.
type specificity struct {
	namedHost  bool
	hostLen    int
	exactModel bool
	modelLen   int
	pattern    string
}

func (s specificity) beats(o specificity) bool {
	switch {
	case s.namedHost != o.namedHost:
		return s.namedHost
	case s.hostLen != o.hostLen:
		return s.hostLen > o.hostLen
	case s.exactModel != o.exactModel:
		return s.exactModel
	case s.modelLen != o.modelLen:
		return s.modelLen > o.modelLen
	default:
		return s.pattern < o.pattern
	}
}

// anyHost reports whether a host pattern matches every endpoint.
func anyHost(pattern string) bool { return pattern == "" || pattern == "*" }

// matchHost reports whether endpoint matches pattern.
//
// Exact, case-insensitive equality for now. Task 1.4 widens this to host globs
// with the port stripped, reusing sparc/collect.go:148-156's path.Match idiom.
func matchHost(pattern, endpoint string) bool {
	if anyHost(pattern) {
		return true
	}
	return pattern == strings.ToLower(endpoint)
}

// NewTable compiles entries into a table, rejecting rows that cannot mean
// anything useful.
func NewTable(entries []Entry) (*Table, error) {
	t := &Table{rows: make([]row, 0, len(entries))}
	for _, e := range entries {
		switch e.Prov {
		case ProvAuthoritative:
			return nil, fmt.Errorf("pricing: entry %q/%q claims authoritative provenance, which is a settled per-request figure and not a table rate", e.Host, e.Model)
		case ProvNone:
			return nil, fmt.Errorf("pricing: entry %q/%q has no provenance", e.Host, e.Model)
		}
		if !e.Rates.any() {
			return nil, fmt.Errorf("pricing: entry %q/%q sets no rate for any tier", e.Host, e.Model)
		}
		m, err := compileModel(e.Model)
		if err != nil {
			return nil, err
		}
		host := strings.ToLower(e.Host)
		t.rows = append(t.rows, row{
			host:  host,
			model: m,
			rates: e.Rates,
			prov:  e.Prov,
			spec: specificity{
				namedHost:  !anyHost(host),
				hostLen:    len(host),
				exactModel: m.exact,
				modelLen:   len(m.pattern),
				pattern:    host + "\x00" + m.pattern,
			},
		})
	}
	return t, nil
}

// Resolve returns the rates for one (endpoint, model) pair and where they came
// from, already flattened for a prompt of promptTotal tokens.
//
// Provenance decides first, specificity only within a level — see the
// specificity type for why the endpoint axis outranks the model axis. A nil Table
// resolves to ProvNone rather than panicking: a binary built without pricing
// wiring must report traffic as unpriced, not crash on the response path.
func (t *Table) Resolve(endpoint, model string, promptTotal int) (Rates, Provenance) {
	if t == nil {
		return Rates{}, ProvNone
	}
	var best *row
	for i := range t.rows {
		r := &t.rows[i]
		if !matchHost(r.host, endpoint) || !r.model.match(model) {
			continue
		}
		if best == nil || r.prov > best.prov || (r.prov == best.prov && r.spec.beats(best.spec)) {
			best = r
		}
	}
	if best == nil {
		return Rates{}, ProvNone
	}
	return best.rates.At(promptTotal), best.prov
}

var _ Resolver = (*Table)(nil)
```

- [ ] **Step 5: Run the test and confirm it passes**

```bash
cd authbridge/authlib && go test -race ./pricing/ > $LOG_DIR/1.3-green.log 2>&1; echo "EXIT:$?"
go vet ./pricing/ > $LOG_DIR/1.3-vet.log 2>&1; echo "VET:$?"
```

Expected: `EXIT:0` and `VET:0`.

- [ ] **Step 6: Commit**

```bash
git add authbridge/authlib/pricing/provenance.go authbridge/authlib/pricing/table.go \
        authbridge/authlib/pricing/table_test.go
git commit -s -m "feat: Add pricing provenance and table resolution

Rates now carry where they came from, ranked so precedence is the type's
own ordering: configured beats discovered beats bundled, because an
override is an override. Authoritative is rejected from a table — it is a
settled per-request figure, not a rate.

Specificity breaks ties within a level, endpoint axis first: a
deployment-specific host row must not be shadowed by a broader model
glob, since rates differ more between a discounted gateway and vendor
list than between two models on one endpoint. Within each axis, exact
beats glob then longer beats shorter, which is toolprune's existing rule.

Resolve returns rates already flattened for the request's prompt size, so
no caller can forget the long-context premium.

Host matching is exact equality here; task 1.4 widens it to globs with
the port stripped. Model globbing lands now because it is what makes
'exact beats glob' mean anything.

Refs #910

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Tasks 1.4-1.6 (implemented; recorded as built)

Detailed at implementation time rather than in advance, since they were written and
executed in the same pass. Each followed the same TDD order as 1.1-1.3.

**Task 1.4 — host globs and the `Usage` conversion** · `pricing/host.go`,
`pricing/inference.go` (+ tests), `pricing/table.go` modified

- `hostKey` strips the port; `matchHost` uses `path.Match` (sparc's idiom, not the
  model matcher's `gobwas/glob`, so a host glob carried over from a sparc config
  behaves the same); `validHostPattern` rejects malformed patterns at build time,
  because `path.Match` reports `ErrBadPattern` only when called and a bad pattern
  would otherwise silently unprice its endpoint.
- **Bug the test caught:** `path.Match` reads `[` as a character class, so `[::1]`
  parsed as "one of ':' or '1'" and never matched the address it names. Bracketed
  IPv6 literals are now compared literally (`isIPv6Literal`).
- `UsageFromInference` is the single parser-vocabulary conversion. Split counters
  beat the legacy `PromptTokens` / `CompletionTokens` aggregates — the aggregate is
  *derived from* the split, so reading it would fold cache tiers into uncached input
  and over-price cache-heavy traffic ~10x. The aggregates remain the fallback for
  providers reporting only totals, since zero usage is unpriced in `Cost` and such
  traffic would otherwise vanish from the total rather than be priced approximately.

**Task 1.5 — `Registry`** · `pricing/registry.go` (+ test)

- `atomic.Pointer[Table]` behind a pointer that never changes, because
  `buildPipelines` is a reloader-invoked closure while the usage aggregator sharing
  the rates is created once outside it and outlives every rebuild. Covered by a test
  that captures a `Resolver` *before* the swap — the aggregator's exact position —
  plus a concurrent resolve/swap test under `-race`.
- `Registry.Cost` collapses provenance to `ProvNone` whenever the request is
  unpriced, including when a row matched but its tiers do not cover the request:
  reporting `bundled` beside a figure of zero would label an absent answer as a
  sourced one.
- A nil `Registry` resolves to `ProvNone` rather than panicking.

**Task 1.6 — bundled slice, generator, `make` target, golden test** ·
`pricing/bundled.go` (generated), `pricing/internal/pricegen/`,
`pricing/internal/gen/`, `pricing/testdata/model_prices.snapshot.json`,
`pricing/bundled_test.go`, root `Makefile`

- 33 rows pinned to litellm `ee7c7e14f3dd7c4c3930a423440ec26427e2c554`: 28 exact
  models plus 5 family globs carrying the newest member's rates, so an unreleased
  version still prices while a known one always wins on specificity.
- **Success criterion 2 met from real data:** `claude-opus-4-1` $15.00/Mtok input vs
  `claude-opus-5` $5.00 — exactly 3x, which one `*claude-opus-*` glob could not
  express. Four sonnet models carry a genuine 200k threshold, so that path is
  exercised by upstream data.
- **Bug found in the first generated output:** rates rendered as Go *integer*
  constant expressions. `15 / 1000000` is integer division and compiles to **zero**,
  so every whole-dollar rate silently unpriced its model while the fractional rate
  beside it worked. `floatLiteral` now guarantees a decimal point, and the golden
  test fails by name when the hazard is reintroduced (verified by mutation).
- The transform lives in `internal/pricegen`, not in the generator command, so the
  golden test runs the same code against the committed snapshot with no network.
- Only `litellm_provider == "anthropic"` is kept: the `vertex_ai` and `bedrock`
  mirrors carry their own rates, and mixing them under one `*` host scope would make
  the table depend on map iteration order.
- Bundled rates are **vendor list**. A gateway billing below list is overstated; the
  generated file says so, and a host-scoped `pricing:` entry outranks it. Phase 7
  discovery is the real fix.
- `Rates.For(Tier) (float64, bool)` added — the per-tier invariant in accessor form,
  which phase 3's `toolprune` event needs.

**Phase 1 gate:** `go build ./...` `BUILD:0` · `go vet ./...` `VET:0` ·
`go test -race ./...` (all of `authlib`) `TEST:0` ·
`golangci-lint --new-from-rev=f742c6f8 ./pricing/...` `0 issues` · `gofmt -l` clean.

<!-- /FILL-IN:PHASE-1 -->

---

## Phase 2 — Injection

**Status:** ✅ IMPLEMENTED

**Spec scope (verbatim, §Phasing entry 2):** "**Injection** — `Deps` / `BuildWithDeps`,
`ResolverConsumer`, consumer probe. Behaviour-neutral."

**Surface:** top-level `pricing:` config section plus validation; `Deps` +
`BuildWithDeps` + `pricing.ResolverConsumer` + a consumer probe; wiring in
`cmd/authbridge-proxy/main.go` — create the registry before `buildPipelines`, inject per
build, swap on reload, and pass it to `usage.New`. No plugin behaviour changes yet.

**Read before detailing:** spec `### Injection` (L317-348), `## Config` (L463-498).

<!-- FILL-IN:PHASE-2 -->

### Implemented (recorded as built)

**`pricing.Config` + `Build`** · `pricing/config.go`, `pricing/consumer.go` (+ tests)

- One `pricing:` section replaces the 17 rate fields previously spread across two
  plugin configs. Endpoint-scoped, so one process prices several gateways and the
  vendor endpoint differently — the choice is per *request*, not per deployment.
- `bundled: true` by default; `Build(nil)` yields the bundled table alone.
- Both units per tier; **both set for one tier is a startup error naming the tier**,
  since they differ by 10⁶ and the readout could not say which was honoured.
- Also rejected at startup: negative/non-finite rates, endpoints with no models,
  models pricing nothing, thresholds with no `prompt_tokens`, thresholds overriding
  nothing. Errors name endpoint and model; faults reported in sorted order so a
  config with several reports the same one across restarts.
- `ResolverConsumer` mirrors `spiffe.ProviderConsumer` — plugin factories take no
  construction arguments, so process-wide deps arrive by injection.

**`Deps` + `BuildWithDeps`** · `plugins/deps.go` (+ test), `plugins/registry.go`

- `Build` and `BuildWithSPIFFE` were two copies of the same 40 lines differing in one
  injection; adding pricing would have made a third. Both now delegate:
  **78 lines deleted, 9 added.**
- Injection happens **before `Configure`** — the contract, since a plugin's
  `Configure` may build rate-dependent state. Verified by mutation: moving injection
  after `Configure` fails `TestBuildWithDeps_InjectsResolverBeforeConfigure` by name.
- A nil dep is *not* injected rather than injected as nil, or the plugin's own
  `!= nil` guard would be true while every call through it panicked.
- `PricingConsumerPlugins()` probe mirrors `SPIFFEConsumerPlugins()`.

**Config + proxy wiring** · `config/config.go`, `config/validate.go`,
`cmd/authbridge-proxy/main.go`

- `Validate` builds the table and discards it, so pricing faults fail at startup
  beside every other config error instead of becoming silently unpriced traffic.
- The `Registry` is created **once, outside `buildPipelines`** — that closure is
  reloader-invoked while the usage aggregator sharing the rates is created later and
  outlives every rebuild. On reload the table is rebuilt and swapped *in place*,
  before pipelines build.
- **No reloader change needed:** `validateReloadable` (`reloader.go:303-325`) is a
  deny-list naming only `mode` and `listener.*`, so `pricing:` is reloadable already.
- Behaviour-neutral — no plugin consumes the resolver yet.

**Phase 2 gate:** all 6 buildable workspace modules `build:OK test:OK` ·
`go vet` clean · `gofmt -l` clean on touched files (4 pre-existing unformatted files
under `plugins/` left alone — unformatted at the merge base too) ·
`golangci-lint --new-from-rev=f742c6f8` 0 issues across `config`, `plugins`,
`pricing` and `cmd/authbridge-proxy`.

<!-- /FILL-IN:PHASE-2 -->

---

## Phase 3 — `toolprune` migration

**Status:** ⬜ NOT YET DETAILED

**Spec scope (verbatim, §Phasing entry 3):** "**`toolprune` migration** — swap to the
resolver, delete its table. Existing `$ saved` tests are the oracle; the bundled slice
changes some expected figures, which the diff must state per model rather than bulk-update."

**Surface:** delete `toolprune/pricing.go` defaults, the 12 rate knobs, and
`modelRates` / `normalize` / `rateFor` / `set` / `ratesFor` / `rateSource`. Consume the
resolver at `plugins/toolprune/plugin.go:628` and `:718`. **Keep publishing rates on the
plugin's event** — the counterfactual `$ saved` calculation still needs them.

**Read before detailing:** spec `### Plugin changes` (L349-385). Source:
`authlib/plugins/toolprune/pricing.go` (4,292 B) and `plugin.go` around `:628` / `:718`.

**Care:** every changed `$ saved` expectation must be justified per model in the commit
message, not bulk-updated.

<!-- FILL-IN:PHASE-3 -->
*Tasks not yet written.*
<!-- /FILL-IN:PHASE-3 -->

---

## Phase 4 — `litellm_budgettrack` migration

**Status:** ⬜ NOT YET DETAILED

**Spec scope (verbatim, §Phasing entry 4):** "**`litellm_budgettrack` migration** — SSE
equivalence test written *first*, then drop the parser and rate knobs, add `Requires`,
refine `source` into provenance."

**Surface:** write the SSE equivalence test **first**, then delete `parseFrameUsage` /
`usageJSON` / `frameUsage` / reconciliation and the 4 rate knobs; read
`Extensions.Inference` instead; add `Requires: ["inference-parser"]` — a **breaking change
that needs a release note**; refine `source` into provenance **additively** so the existing
wire values keep decoding.

**Read before detailing:** spec `### Plugin changes` (L349-385),
`### Aggregator and wire format` (L386-437).

<!-- FILL-IN:PHASE-4 -->
*Tasks not yet written.*
<!-- /FILL-IN:PHASE-4 -->

---

## Phase 5 — Aggregator resolver fallback

**Status:** ⬜ NOT YET DETAILED

**Spec scope (verbatim, §Phasing entry 5):** "**Aggregator resolver fallback** — pricing
for requests with no cost event (no `litellm-budget-track` in the pipeline), plus
`UnpricedBy`."

**Surface:** `usage.WithPricing(resolver)` and a fallback in `foldInto` for requests that
carry no cost event, plus `UnpricedBy map[string]int64` on the snapshot so unpriced
endpoint/model pairs are counted and nameable rather than silently zero.

**Read before detailing:** spec `### Aggregator and wire format` (L386-437). Source:
`authlib/usage/usage.go` `foldInto`, `authlib/usage/snapshot.go`.

<!-- FILL-IN:PHASE-5 -->
*Tasks not yet written.*
<!-- /FILL-IN:PHASE-5 -->

---

## Phase 6 — `abctl`

**Status:** ⬜ NOT YET DETAILED

**Spec scope (verbatim, §Phasing entry 6):** "**`abctl`** — render provenance on both
sides and the three coverage states. Must follow phase 3, which is what makes rendering
provenance consistent rather than asymmetric (see the `cost_event.go:25-27` note above)."

**Surface:** `abctl` renders provenance and names unpriced endpoint/model pairs; delete the
client-side arithmetic `savedTokensAndCost` and `promptCost` from `prune_saving.go`.
Touches `cmd/abctl/tui/cost_event.go`, `usage_render.go`, `prune_saving.go`.

**Read before detailing:** spec `### abctl` (L438-462), and the `cost_event.go:25-27` note
referenced in the scope above.

**Care:** separate module (`cmd/abctl/go.mod`) — build and test it on its own. Hard
ordering dependency on phase 3.

<!-- FILL-IN:PHASE-6 -->
*Tasks not yet written.*
<!-- /FILL-IN:PHASE-6 -->

---

## Definition of done

- All six phases show **Status: ✅ DETAILED** and every task's steps are checked off.
- `cd authbridge/authlib && go test -race ./...` passes; `cd authbridge/cmd/abctl && go test -race ./...` passes.
- `authlib/pricing` is the only place model rates or host globs are defined. No rate knobs
  remain in `toolprune` or `litellm_budgettrack`.
- The four JSON tags `cost_usd`, `source`, `daily_total_usd`, `daily_max_usd` decode
  unchanged; `source` gained values additively only.
- No unpriced request renders as `$0.00` anywhere; unpriced pairs are named via `UnpricedBy`.
- The golden test pins the LiteLLM upstream commit the bundled slice was generated from.
- `Requires: ["inference-parser"]` on `litellm-budget-track` is called out in a release note.
- Docs updated: `plugin-catalog.md`, `tool-prune-plugin.md`,
  `litellm-budgettrack-plugin.md`, `laptop-token-savings.md`.

## Not in this PR

- **Phase 7 — discovery** (`GET /model/info`, refresh loop, fail-soft, status endpoint).
  Deferred to PR 3: it is the only phase with an outbound dependency and a credential, so it
  wants its own review lens.
- **Phase 0** already shipped as #920.
