package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// The detail pane is where the EXACT reasoning figure lives.
//
// The events table shows one total per row and the spend drawer shows a proportion,
// deliberately: a table column tight enough to be dropped on a 150-column terminal is
// the wrong home for a fifth figure. So this pane is the only place the number itself
// appears per event, and filterForDetail dropping it would leave it nowhere.
func TestFilterForDetail_ResponseKeepsReasoningTokens(t *testing.T) {
	ext := &pipeline.InferenceExtension{
		Model: "claude-opus-5", OutputTokens: 1593, CompletionTokens: 1593,
		ReasoningTokens: 948, PromptTokens: 56, TotalTokens: 1649,
	}
	raw, err := json.Marshal(map[string]any{"inference": ext})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(filterForDetail(raw, pipeline.SessionResponse))

	if !strings.Contains(got, "reasoningTokens") {
		t.Errorf("response detail dropped reasoningTokens, so the figure appears nowhere:\n%s", got)
	}
	if !strings.Contains(got, "948") {
		t.Errorf("response detail dropped the reasoning count:\n%s", got)
	}
	// It must sit beside the figure it is a subset of, not replace it.
	if !strings.Contains(got, "completionTokens") {
		t.Errorf("response detail lost completionTokens:\n%s", got)
	}
}

// A REQUEST row has no reasoning figure to show — reasoning is reported with the
// response — so the key must not leak onto the request side, where it would read as
// a request-time budget rather than a measurement.
func TestFilterForDetail_RequestDropsReasoningTokens(t *testing.T) {
	ext := &pipeline.InferenceExtension{Model: "claude-opus-5", ReasoningTokens: 948}
	raw, err := json.Marshal(map[string]any{"inference": ext})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(filterForDetail(raw, pipeline.SessionRequest)); strings.Contains(got, "reasoningTokens") {
		t.Errorf("request detail carries reasoningTokens:\n%s", got)
	}
}

// Unreported reasoning must not render as a zero. The field is omitempty, so an
// absent split leaves the key out entirely rather than claiming the model did none.
func TestFilterForDetail_UnreportedReasoningIsAbsentNotZero(t *testing.T) {
	ext := &pipeline.InferenceExtension{
		Model: "gpt-oss-120b", OutputTokens: 244, CompletionTokens: 244,
	}
	raw, err := json.Marshal(map[string]any{"inference": ext})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(filterForDetail(raw, pipeline.SessionResponse)); strings.Contains(got, "reasoningTokens") {
		t.Errorf("a provider reporting no split still shows reasoningTokens:\n%s", got)
	}
}
