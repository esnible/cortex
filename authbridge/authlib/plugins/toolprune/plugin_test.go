package toolprune

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// anthropicBody is deliberately awkward: unsorted keys, odd indentation, a
// trailing field after tools. Byte-exactness assertions below depend on it
// staying awkward, because the whole safety claim is "every byte outside the
// deleted elements is unchanged".
const anthropicBody = `{"model":"claude-opus-5",
  "tools":[
    {"name":"Read","description":"read a file","input_schema":{"type":"object"}},
    {"name":"NotebookEdit","description":"edit a notebook","input_schema":{"type":"object"}},
    {"name":"Bash","description":"run a command","input_schema":{"type":"object"}}
  ],
  "max_tokens":1024,"stream":true}`

func configured(t *testing.T, remove ...string) *ToolPrune {
	t.Helper()
	p := New()
	raw, err := json.Marshal(map[string]any{"remove": remove})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func inferenceCtx(path, body string, toolNames ...string) *pipeline.Context {
	pctx := &pipeline.Context{Path: path, Body: []byte(body)}
	tools := make([]pipeline.InferenceTool, 0, len(toolNames))
	for _, n := range toolNames {
		tools = append(tools, pipeline.InferenceTool{Name: n})
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Tools: tools}
	return pctx
}

func run(t *testing.T, p *ToolPrune, pctx *pipeline.Context, policies ...pipeline.ErrorPolicy) {
	t.Helper()
	var opts []pipeline.Option
	if len(policies) > 0 {
		opts = append(opts, pipeline.WithPolicies(policies...))
	}
	pipe, err := pipeline.New([]pipeline.Plugin{p}, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v, want Continue — tool-prune must never block a request", act.Type)
	}
}

// TestPrune_LeavesEveryOtherByteIntact is the core safety claim. Deleting a
// tool must not reformat the document, reorder keys, or disturb whitespace: the
// request that reaches the model has to be the one the client sent, minus
// exactly the elements named.
func TestPrune_LeavesEveryOtherByteIntact(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, pctx)

	if !pctx.BodyMutated() {
		t.Fatal("expected the body to be rewritten")
	}
	got := string(pctx.Body)
	if strings.Contains(got, "NotebookEdit") {
		t.Errorf("removed tool still present:\n%s", got)
	}
	for _, keep := range []string{
		`"model":"claude-opus-5"`,
		`"name":"Read"`,
		`"name":"Bash"`,
		`"max_tokens":1024`,
		`"stream":true`,
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("expected %s to survive verbatim:\n%s", keep, got)
		}
	}
	// The only difference from the original must be the removed element.
	if len(got) >= len(anthropicBody) {
		t.Errorf("body did not shrink: %d -> %d", len(anthropicBody), len(got))
	}
}

// TestPrune_DescendingDeletion: removing several tools by index only works if
// the deletions run high-to-low. An ascending loop would shift the array under
// itself and delete the wrong elements — here it would leave "Bash" and remove
// something else, so the assertion catches exactly that bug.
func TestPrune_DescendingDeletion(t *testing.T) {
	body := `{"tools":[{"name":"A"},{"name":"B"},{"name":"C"},{"name":"D"},{"name":"E"}]}`
	p := configured(t, "A", "B", "D")
	pctx := inferenceCtx("/v1/messages", body, "A", "B", "C", "D", "E")
	run(t, p, pctx)

	got := string(pctx.Body)
	for _, gone := range []string{`"A"`, `"B"`, `"D"`} {
		if strings.Contains(got, gone) {
			t.Errorf("tool %s should be gone: %s", gone, got)
		}
	}
	for _, kept := range []string{`"C"`, `"E"`} {
		if !strings.Contains(got, kept) {
			t.Errorf("tool %s should remain: %s", kept, got)
		}
	}
}

// TestPrune_OpenAIDialect: OpenAI nests the name under function, Anthropic puts
// it at the top level. Both must resolve, since the plugin reads names out of
// the raw bytes rather than trusting manifest ordering.
func TestPrune_OpenAIDialect(t *testing.T) {
	body := `{"tools":[{"type":"function","function":{"name":"Read"}},` +
		`{"type":"function","function":{"name":"NotebookEdit"}}]}`
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/chat/completions", body, "Read", "NotebookEdit")
	run(t, p, pctx)

	got := string(pctx.Body)
	if strings.Contains(got, "NotebookEdit") {
		t.Errorf("removed tool still present: %s", got)
	}
	if !strings.Contains(got, "Read") {
		t.Errorf("kept tool missing: %s", got)
	}
}

// TestPrune_RemovingEveryToolDropsTheKeys: an empty tools array is not a safe
// output — OpenAI rejects `tools: []`, and tool_choice without tools. Drop both
// keys instead, so an over-broad remove list still yields a valid request.
func TestPrune_RemovingEveryToolDropsTheKeys(t *testing.T) {
	body := `{"model":"m","tools":[{"name":"A"},{"name":"B"}],"tool_choice":"auto"}`
	p := configured(t, "A", "B")
	pctx := inferenceCtx("/v1/chat/completions", body, "A", "B")
	run(t, p, pctx)

	got := string(pctx.Body)
	if strings.Contains(got, "tools") {
		t.Errorf("tools key should be gone entirely, not left empty: %s", got)
	}
	if strings.Contains(got, "tool_choice") {
		t.Errorf("tool_choice is invalid without tools; should be dropped: %s", got)
	}
	if !strings.Contains(got, `"model":"m"`) {
		t.Errorf("unrelated fields must survive: %s", got)
	}
}

// TestPrune_UnknownNamesIgnored: a name absent from this request's manifest is
// simply not there — not an error. Drift in the configured list costs savings,
// never correctness.
func TestPrune_UnknownNamesIgnored(t *testing.T) {
	body := `{"tools":[{"name":"Read"}]}`
	p := configured(t, "ToolThatDoesNotExist")
	pctx := inferenceCtx("/v1/messages", body, "Read")
	run(t, p, pctx)

	if pctx.BodyMutated() {
		t.Error("no configured tool was present; body must be untouched")
	}
	if string(pctx.Body) != body {
		t.Errorf("body = %s, want unchanged", pctx.Body)
	}
}

// TestPrune_FailsOpen: malformed, truncated and manifest-less bodies all
// forward the original bytes. A cost optimisation must never break a request.
func TestPrune_FailsOpen(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"tools":[{"name":"NotebookEdit"}`},
		{"truncated mid-string", `{"tools":[{"name":"Notebook`},
		{"tools is not an array", `{"tools":"NotebookEdit"}`},
		{"tools absent", `{"model":"m"}`},
		{"empty body", ``},
		{"empty tools array", `{"tools":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := configured(t, "NotebookEdit")
			pctx := inferenceCtx("/v1/messages", tc.body, "NotebookEdit")
			run(t, p, pctx)
			if pctx.BodyMutated() {
				t.Errorf("body was mutated; must fail open on %s", tc.name)
			}
			if string(pctx.Body) != tc.body {
				t.Errorf("body = %q, want original %q", pctx.Body, tc.body)
			}
		})
	}
}

// TestPrune_PathGate: only inference paths are touched, so an unrelated POST
// through the same proxy is never rewritten.
func TestPrune_PathGate(t *testing.T) {
	body := `{"tools":[{"name":"NotebookEdit"}]}`
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/some/other/api", body, "NotebookEdit")
	run(t, p, pctx)
	if pctx.BodyMutated() {
		t.Error("non-inference path must not be pruned")
	}
}

// TestPrune_EmptyRemoveListIsNoop: the shipped default is an empty list, so the
// plugin must be inert until an operator fills it in.
func TestPrune_EmptyRemoveListIsNoop(t *testing.T) {
	p := configured(t)
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit")
	run(t, p, pctx)
	if pctx.BodyMutated() {
		t.Error("empty remove list must not touch the body")
	}
	if p.Metrics() != nil {
		t.Errorf("no requests acted on; Metrics should be nil, got %+v", p.Metrics())
	}
}

// TestPrune_ObserveModeIsProjection: under on_error: observe the plugin computes
// exactly what it would remove and counts it, while the bytes on the wire stay
// untouched and the invocation is marked Shadow. That is what makes measure-only
// mode possible with one registration and no separate code path.
func TestPrune_ObserveModeIsProjection(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, pctx, pipeline.ErrorPolicyObserve)

	if pctx.BodyMutated() {
		t.Error("observe mode must leave the wire untouched")
	}
	if string(pctx.Body) != anthropicBody {
		t.Errorf("body changed under observe:\n%s", pctx.Body)
	}
	if pctx.Extensions.Invocations == nil {
		t.Fatal("expected invocations to be recorded")
	}
	var sawShadowModify bool
	for _, inv := range pctx.Extensions.Invocations.Inbound {
		if inv.Shadow && inv.Reason == "body_rewritten" {
			sawShadowModify = true
		}
	}
	if !sawShadowModify {
		t.Errorf("expected a Shadow=true body_rewritten invocation, got %+v",
			pctx.Extensions.Invocations.Inbound)
	}

	// The projection must still be countable, and must be reported as a
	// projection rather than a realised saving.
	if p.m.requestsProjected != 1 {
		t.Errorf("requestsProjected = %d, want 1", p.m.requestsProjected)
	}
	if p.m.requestsPruned != 0 {
		t.Errorf("requestsPruned = %d, want 0 under observe", p.m.requestsPruned)
	}
	if p.m.bytesRemoved == 0 {
		t.Error("bytesRemoved must accumulate under observe — that is the projection")
	}
	if !hasMetric(p.Metrics(), "requests projected") {
		t.Errorf("readout should say 'requests projected': %+v", p.Metrics())
	}
}

// TestPrune_EnforceCountsPruned is the enforce-mode counterpart.
func TestPrune_EnforceCountsPruned(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit", "Bash")
	run(t, p, pctx, pipeline.ErrorPolicyEnforce)

	if p.m.requestsPruned != 1 {
		t.Errorf("requestsPruned = %d, want 1", p.m.requestsPruned)
	}
	if p.m.requestsProjected != 0 {
		t.Errorf("requestsProjected = %d, want 0 under enforce", p.m.requestsProjected)
	}
	if p.m.toolsRemoved != 1 {
		t.Errorf("toolsRemoved = %d, want 1", p.m.toolsRemoved)
	}
	if !hasMetric(p.Metrics(), "removed: NotebookEdit") {
		t.Errorf("per-tool attribution missing: %+v", p.Metrics())
	}
}

// TestMetrics_NoUsageSampleReportsZeroNotNaN: the bytes-to-tokens ratio divides
// by a sample that starts empty. Report a zero-valued estimate with a note
// rather than NaN or a panic.
func TestMetrics_NoUsageSampleReportsZeroNotNaN(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit")
	run(t, p, pctx)

	m := findMetric(t, p.Metrics(), "tokens saved / request")
	if m.Value != 0 {
		t.Errorf("value = %v, want 0 with no usage sample", m.Value)
	}
	if m.Note != "no usage sample yet" {
		t.Errorf("note = %q, want the missing-sample caveat", m.Note)
	}
}

// TestMetrics_TokenEstimateCalibratesOnObservedUsage: once OnFinish has seen a
// response usage block, the estimate is derived from the operator's own
// traffic and labelled with its sample size.
func TestMetrics_TokenEstimateCalibratesOnObservedUsage(t *testing.T) {
	p := configured(t, "NotebookEdit")
	pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit")
	run(t, p, pctx)

	// 1 prompt token per 4 body bytes.
	pctx.Extensions.Inference.PromptTokens = len(pctx.Body) / 4
	p.OnFinish(context.Background(), pctx)

	m := findMetric(t, p.Metrics(), "tokens saved / request")
	if m.Value <= 0 {
		t.Errorf("value = %v, want a positive estimate", m.Value)
	}
	if !strings.HasPrefix(m.Note, "estimate, n=") {
		t.Errorf("note = %q, want it labelled an estimate with its sample size", m.Note)
	}
	perReq := findMetric(t, p.Metrics(), "bytes removed / request")
	if want := perReq.Value / 4; m.Value < want*0.9 || m.Value > want*1.1 {
		t.Errorf("estimate %v not within 10%% of calibrated %v", m.Value, want)
	}
}

// TestMetrics_ConcurrentAccess exercises Metrics() against live counter updates.
// describePipeline calls it from the HTTP handler while requests are in flight,
// so it must be safe under -race.
func TestMetrics_ConcurrentAccess(t *testing.T) {
	p := configured(t, "NotebookEdit")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				pctx := inferenceCtx("/v1/messages", anthropicBody, "Read", "NotebookEdit")
				pipe, err := pipeline.New([]pipeline.Plugin{p})
				if err != nil {
					t.Error(err)
					return
				}
				pipe.Run(context.Background(), pctx)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = p.Metrics()
			}
		}()
	}
	wg.Wait()
	if p.m.requestsPruned != 8*50 {
		t.Errorf("requestsPruned = %d, want %d", p.m.requestsPruned, 8*50)
	}
}

func TestConfigure_RejectsUnknownFields(t *testing.T) {
	p := New()
	err := p.Configure(json.RawMessage(`{"remove":["A"],"nope":1}`))
	if err == nil {
		t.Fatal("expected an error for an unknown config field")
	}
	if !strings.Contains(err.Error(), "tool-prune config") {
		t.Errorf("error should name the plugin: %v", err)
	}
}

func TestCapabilities_RequestOnlySoStreamingSurvives(t *testing.T) {
	caps := New().Capabilities()
	if !caps.WritesRequestBody {
		t.Error("must declare WritesRequestBody")
	}
	if caps.WritesResponseBody {
		t.Error("must NOT declare WritesResponseBody — it would cost SSE streaming for nothing")
	}
	if len(caps.RequiresAny) != 1 || caps.RequiresAny[0] != "inference-parser" {
		t.Errorf("RequiresAny = %v, want [inference-parser]", caps.RequiresAny)
	}
}

func hasMetric(ms []pipeline.Metric, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

func findMetric(t *testing.T, ms []pipeline.Metric, name string) pipeline.Metric {
	t.Helper()
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("metric %q not found in %+v", name, ms)
	return pipeline.Metric{}
}
