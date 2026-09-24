package main

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// tokenSplit's reasoning line was unreachable surface until the Anthropic parser
// learned to read usage.output_tokens_details.thinking_tokens: the line was
// written, gated on KindReasoning, and no producer ever set that bit. These tests
// pin the behaviour now that it can fire, so the next parser regression shows up
// here instead of as a silently missing line in `abctl cost`.
func TestTokenSplit_RendersReasoningAsSubsetOfOutput(t *testing.T) {
	// Captured from a live claude-opus-5 turn at effort "max".
	got := tokenSplit(usage.Counts{
		InputTokens:     56,
		OutputTokens:    1593,
		ReasoningTokens: 948,
		PresentKinds:    uint8(usage.KindInput | usage.KindOutput | usage.KindReasoning),
	})
	if !strings.Contains(got, "948") {
		t.Errorf("tokenSplit = %q, want the 948 reasoning tokens", got)
	}
	// The label must keep saying "of output". Without it the line reads as a fifth
	// sibling tier and invites summing it into the total, which double-counts every
	// thinking token at the output rate.
	if !strings.Contains(got, "of output") {
		t.Errorf("tokenSplit = %q, want the reasoning label to say it is a subset of output", got)
	}
}

// The absent case is the one that matters for honesty: a provider that reports no
// split must produce NO reasoning line, not "reasoning (of output) 0".
func TestTokenSplit_OmitsReasoningWhenUnreported(t *testing.T) {
	got := tokenSplit(usage.Counts{
		InputTokens:  56,
		OutputTokens: 1593,
		PresentKinds: uint8(usage.KindInput | usage.KindOutput),
	})
	if strings.Contains(got, "reasoning") {
		t.Errorf("tokenSplit = %q, want no reasoning line when KindReasoning is clear", got)
	}
	// The kinds that WERE reported must still be there.
	if !strings.Contains(got, "output") {
		t.Errorf("tokenSplit = %q, want the output line", got)
	}
}

// TestTokenSplit_BitAndValueAreBothConsulted pins which of the two decides, because
// the case above cannot: it leaves the bit clear AND the value zero, so a renderer
// gated only on the value behaves identically and the rule it claims to pin is not
// pinned. `add` gates on `bit == 0 && v == 0`, so each half needs a case where the
// two disagree.
func TestTokenSplit_BitAndValueAreBothConsulted(t *testing.T) {
	// REPORTED ZERO: bit set, value 0. Must render — the provider measured the split
	// and it was nothing, which is the observation that says effort reached the model
	// and bought no reasoning. A renderer gated on the VALUE alone would drop this.
	reportedZero := tokenSplit(usage.Counts{
		OutputTokens:    1593,
		ReasoningTokens: 0,
		PresentKinds:    uint8(usage.KindOutput | usage.KindReasoning),
	})
	if !strings.Contains(reportedZero, "reasoning") {
		t.Errorf("tokenSplit = %q, want a reasoning line for a REPORTED zero", reportedZero)
	}

	// LEGACY PRODUCER: bit clear, value non-zero. Must also render — the value is the
	// only evidence available on an event predating PresentKinds, and dropping it would
	// hide a real number. A renderer gated on the BIT alone would drop this.
	legacy := tokenSplit(usage.Counts{
		OutputTokens:    1593,
		ReasoningTokens: 948,
		PresentKinds:    uint8(usage.KindOutput),
	})
	if !strings.Contains(legacy, "948") {
		t.Errorf("tokenSplit = %q, want the 948 from a producer predating PresentKinds", legacy)
	}
}

// `abctl cost --json` must publish the reasoning figure the TUI's drawer draws.
// Without it a scripted consumer can only get that number by reimplementing
// usage.ApportionReasoning, which is the drift costJSON.Tiers exists to prevent.
func TestTiersJSON_PublishesReasoningAndKeepsTheFourTiersSumming(t *testing.T) {
	c := usage.Counts{
		CostMicros:      4_546_200,
		InputCostMicros: 3000, CacheWriteCostMicros: 7500,
		CacheReadCostMicros: 30000, OutputCostMicros: 45000,
		OutputTokens: 1593, ReasoningTokens: 948,
		PresentKinds: uint8(usage.KindOutput | usage.KindReasoning),
	}
	got := tiersJSONOf(c)
	if got == nil {
		t.Fatal("no tiers published for a Counts with a mix")
	}
	if got.Reasoning == nil {
		t.Fatal("reasoningOfOutput is absent despite a reported split; a consumer would have " +
			"to reimplement the apportionment")
	}
	// INSIDE output, not beside it.
	if *got.Reasoning > got.Output {
		t.Errorf("reasoning %d exceeds output %d", *got.Reasoning, got.Output)
	}
	// And the four tiers still reconcile to the total without it.
	if sum := got.Input + got.CacheWrite + got.CacheRead + got.Output; sum != c.CostMicros {
		t.Errorf("the four tiers sum to %d, want %d — reasoning must not be in the sum",
			sum, c.CostMicros)
	}
	// It is the SAME figure the drawer derives, by construction: one call.
	want, ok := c.ApportionReasoning(got.Output)
	if !ok || want != *got.Reasoning {
		t.Errorf("published %d but ApportionReasoning gives %d (ok=%v)", *got.Reasoning, want, ok)
	}
}

// Absent, not zero, when nothing reported a split — so a consumer can tell "no figure"
// from "free".
func TestTiersJSON_OmitsReasoningWhenThereIsNoFigure(t *testing.T) {
	c := usage.Counts{
		CostMicros:      4_546_200,
		InputCostMicros: 3000, OutputCostMicros: 45000,
		OutputTokens: 1593,
		PresentKinds: uint8(usage.KindOutput),
	}
	got := tiersJSONOf(c)
	if got == nil {
		t.Fatal("no tiers published")
	}
	if got.Reasoning != nil {
		t.Errorf("reasoningOfOutput = %d for a provider reporting no split; want absent", *got.Reasoning)
	}
}
