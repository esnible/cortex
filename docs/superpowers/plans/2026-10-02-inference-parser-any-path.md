# Inference parser: known dialects at any path — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `inference-parser` parses OpenAI chat-completions and Anthropic Messages traffic under any path prefix, so a provider that speaks either dialect needs no code change.

**Architecture:** A body check lands first, so no commit widens what reaches IBAC and OPA without it: a request is inference only if its body has a `messages` array (or a `prompt`, for legacy completions). Then one function, `dialectFor(path)`, replaces the exact-path switch and `isAnthropicMessagesPath`, matching on how the path ends. The request side and all five response-side call sites ask it.

**Tech Stack:** Go 1.26.5, `encoding/json`, the `core/pipeline` plugin API.

**Spec:** `docs/superpowers/specs/2026-10-02-inference-parser-any-path-design.md`

## Global Constraints

- All code is in `core/plugins/inferenceparser/` (package `inferenceparser`), plus one comment in `core/pipeline/client.go`.
- Run Go commands from `core/`, in workspace mode (no `GOWORK=off`), as CI's core job does.
- No configuration surface: the plugin stays non-`Configurable`.
- A trailing slash is not trimmed. `/inference/v1/chat/completions/` stays not inference.
- `model` is never required in a request body.
- Commits: `git commit -s`, message ending `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- Do not run `make dev-install` or restart the laptop proxy: it cuts every attached agent session.
- Before the final commit: `gofmt -l` on every touched Go file prints nothing; `go vet ./...` in `core` passes.

---

### Task 1: Take a body for inference only if it carries messages or a prompt

**Files:**
- Create: `core/plugins/inferenceparser/bodycheck_test.go`
- Modify: `core/plugins/inferenceparser/plugin.go` (`inferenceRequest` at ~line 800, `parseOpenAIRequest` at ~line 129, the nil-extension comment at ~line 289)
- Modify: `core/plugins/inferenceparser/anthropic.go` (`parseAnthropicRequest` at ~line 97)

**Interfaces:**
- Consumes: nothing new.
- Produces: `parseOpenAIRequest(body []byte) *pipeline.InferenceExtension` and `parseAnthropicRequest(body []byte) *pipeline.InferenceExtension` keep their signatures and now return nil for a body without a `messages` array (OpenAI: and without a `prompt`). New helper `jsonPresent(raw json.RawMessage) bool` in `plugin.go`.

- [ ] **Step 1: Write the failing tests**

Create `core/plugins/inferenceparser/bodycheck_test.go`:

```go
package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A path that ends like an inference endpoint is not enough: the body has to carry what an
// inference request cannot do without. Every case here left an extension, marked as an
// action, before the check existed — except the three non-object bodies, which never
// decoded, and stay here to pin that.
func TestInferenceParser_BodyWithoutMessagesIsNotInference(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"openai empty object", "/v1/chat/completions", `{}`},
		{"openai model only", "/v1/chat/completions", `{"model":"gpt-4o"}`},
		{"openai messages null", "/v1/chat/completions", `{"model":"gpt-4o","messages":null}`},
		{"openai messages not an array", "/v1/chat/completions", `{"model":"gpt-4o","messages":{}}`},
		{"openai prompt null", "/v1/completions", `{"model":"codellama","prompt":null}`},
		{"openai json null", "/v1/chat/completions", `null`},
		{"openai json array", "/v1/chat/completions", `[]`},
		{"openai json string", "/v1/chat/completions", `"hi"`},
		{"anthropic empty object", "/v1/messages", `{}`},
		{"anthropic model only", "/v1/messages", `{"model":"claude-sonnet-4-6","max_tokens":64}`},
		{"anthropic prompt is not enough", "/v1/messages", `{"model":"claude-sonnet-4-6","prompt":"hi"}`},
		{"anthropic json null", "/v1/messages", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := &pipeline.Context{Path: tc.path, Body: []byte(tc.body)}
			if action := NewInferenceParser().OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
				t.Fatalf("expected Continue, got %v", action.Type)
			}
			if pctx.Extensions.Inference != nil {
				t.Errorf("%s %s was taken for inference: %+v", tc.path, tc.body, pctx.Extensions.Inference)
			}
		})
	}
}

// The check must not turn away a real request. Azure sends no model — it names the
// deployment in the path — and an empty messages array is still a messages array.
func TestInferenceParser_BodyWithMessagesOrPromptIsInference(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"openai messages", "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`},
		{"openai empty messages", "/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`},
		{"openai no model", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"openai prompt string", "/v1/completions", `{"model":"codellama","prompt":"def f("}`},
		{"openai prompt array", "/v1/completions", `{"model":"codellama","prompt":["a","b"]}`},
		{"anthropic messages", "/v1/messages", `{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := &pipeline.Context{Path: tc.path, Body: []byte(tc.body)}
			NewInferenceParser().OnRequest(context.Background(), pctx)
			if pctx.Extensions.Inference == nil {
				t.Errorf("%s %s was not taken for inference", tc.path, tc.body)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd core && go test ./plugins/inferenceparser/ -run 'TestInferenceParser_Body' -v`
Expected: FAIL in `TestInferenceParser_BodyWithoutMessagesIsNotInference` for the empty-object, model-only, messages-null, prompt-null, json-null and anthropic prompt cases ("was taken for inference"). `TestInferenceParser_BodyWithMessagesOrPromptIsInference` passes already.

- [ ] **Step 3: Add the check to the OpenAI parser**

In `core/plugins/inferenceparser/plugin.go`, add a `Prompt` field to `inferenceRequest`, so it reads:

```go
type inferenceRequest struct {
	Model       string             `json:"model"`
	Messages    []inferenceMessage `json:"messages"`
	Temperature *float64           `json:"temperature"`
	MaxTokens   *int               `json:"max_tokens"`
	TopP        *float64           `json:"top_p"`
	Stream      bool               `json:"stream"`
	Tools       []inferenceTool    `json:"tools"`
	ToolChoice  any                `json:"tool_choice"` // "auto"/"none" or object
	// Prompt is read only to tell a legacy completions request from a body that is not
	// inference at all; see parseOpenAIRequest. It is not surfaced.
	Prompt json.RawMessage `json:"prompt"`
}
```

Replace the doc comment and the opening of `parseOpenAIRequest`, up to and including the `json.Unmarshal` error check, with:

```go
// parseOpenAIRequest builds an InferenceExtension from an OpenAI
// chat/completions (or completions) request body. Returns nil for an empty or
// non-JSON body, and for one that is not an inference request. Every populated
// extension is an outbound LLM call — an agent action (IsAction); the "don't
// judge inference by default" choice is operator policy in IBAC, independent of
// this classification.
func parseOpenAIRequest(body []byte) *pipeline.InferenceExtension {
	if len(body) == 0 {
		return nil
	}
	var req inferenceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	// THE PATH SAID OPENAI; THE BODY HAS TO AGREE. dialectFor matches how a path ends, which
	// reaches every provider's prefix and also any endpoint that merely ends the same way,
	// and every extension built here is read downstream as an agent's LLM call. So one is
	// built only for a body carrying what such a call cannot do without: a messages array,
	// or a legacy completions prompt. A model is not required — Azure names the deployment
	// in the path and sends none.
	if req.Messages == nil && !jsonPresent(req.Prompt) {
		return nil
	}
```

Add this helper directly after `parseOpenAIRequest`:

```go
// jsonPresent reports whether a field was sent with a value. An absent field decodes to an
// empty RawMessage and an explicit null to the bytes "null"; neither counts.
func jsonPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}
```

(`bytes` and `encoding/json` are already imported in `plugin.go`. `req.Messages == nil` is the right test: `encoding/json` decodes `[]` to an empty non-nil slice, and leaves the slice nil for an absent field or `null`.)

- [ ] **Step 4: Add the check to the Anthropic parser**

In `core/plugins/inferenceparser/anthropic.go`, in `parseAnthropicRequest`, directly after

```go
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
```

insert:

```go
	// The path said Anthropic; the body has to agree — see parseOpenAIRequest. A messages
	// array is the one thing every Messages API request carries.
	if req.Messages == nil {
		return nil
	}
```

- [ ] **Step 5: Update the comment that lists what leaves the extension nil**

In `core/plugins/inferenceparser/plugin.go`, in `OnResponseFrame`'s nil-extension branch, replace

```go
		// parsed. OnRequest leaves Extensions.Inference nil for /v1/embeddings, /v1/rerank,
		// anything else the gateway mounts, and any body that was not JSON — and returning here
		// made all of it free, with the gateway's figure sitting unread on the response headers.
```

with

```go
		// parsed. OnRequest leaves Extensions.Inference nil for /v1/embeddings, /v1/rerank,
		// anything else the gateway mounts, and any body it does not take for inference — and
		// returning here made all of it free, with the gateway's figure sitting unread on the
		// response headers.
```

- [ ] **Step 6: Run the package tests**

Run: `cd core && go test ./plugins/inferenceparser/`
Expected: `ok`. If an existing test now fails because its fixture body has no `messages` or `prompt`, that fixture was never an inference request: add `"messages":[]` to its body rather than weakening the check, and say so in the commit message.

- [ ] **Step 7: Commit**

```bash
git add core/plugins/inferenceparser/bodycheck_test.go core/plugins/inferenceparser/plugin.go core/plugins/inferenceparser/anthropic.go
git commit -s -m "fix: Take a body for inference only if it carries messages or a prompt

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 2: Choose the dialect by how the path ends

**Files:**
- Create: `core/plugins/inferenceparser/dialect.go`
- Create: `core/plugins/inferenceparser/dialect_test.go`
- Modify: `core/plugins/inferenceparser/plugin.go` (delete lines 61–79, the `bobPath`/`zenPath`/`zenGoPath` constants; replace the `OnRequest` switch at lines 93–101; five call sites at ~lines 207, 213, 441, 446, 543; the `endpointPath` and `OnRequest` comments)
- Modify: `core/plugins/inferenceparser/anthropic.go` (delete lines 13–37: `anthropicMessagesPath`, which moves to `dialect.go`, the Zen constants, `isAnthropicMessagesPath`)
- Modify: `core/plugins/inferenceparser/plugin_test.go` (references to `bobPath` and `zenPath`; three comments)
- Modify: `core/pipeline/client.go:271`

**Interfaces:**
- Consumes: Task 1's body check (no signature change).
- Produces: `type dialect int` with `dialectNone`, `dialectOpenAI`, `dialectAnthropic`; `func dialectFor(path string) dialect`; `const anthropicMessagesPath = "/v1/messages"` (kept: `incomplete_cost_test.go` and `unparsed_cost_test.go` use it); `const completionsSuffix = "/completions"`.

- [ ] **Step 1: Write the failing tests**

Create `core/plugins/inferenceparser/dialect_test.go`:

```go
package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func TestDialectFor(t *testing.T) {
	for _, tc := range []struct {
		path string
		want dialect
	}{
		// Anthropic Messages at the root and under every prefix seen so far.
		{"/v1/messages", dialectAnthropic},
		{"/zen/v1/messages", dialectAnthropic},
		{"/zen/go/v1/messages", dialectAnthropic},
		{"/anthropic/v1/messages", dialectAnthropic},
		// OpenAI chat completions and legacy completions, likewise.
		{"/v1/chat/completions", dialectOpenAI},
		{"/chat/completions", dialectOpenAI},
		{"/v1/completions", dialectOpenAI},
		{"/completions", dialectOpenAI},
		{"/inference/v1/chat/completions", dialectOpenAI},
		{"/zen/v1/chat/completions", dialectOpenAI},
		{"/zen/go/v1/chat/completions", dialectOpenAI},
		{"/api/v1/chat/completions", dialectOpenAI},
		{"/openai/v1/chat/completions", dialectOpenAI},
		{"/openai/deployments/gpt-4o/chat/completions", dialectOpenAI},
		{"/v1beta/openai/chat/completions", dialectOpenAI},
		// Endpoints beside an inference one, sharing its prefix but not its ending.
		{"/v1/messages/count_tokens", dialectNone},
		{"/v1/messages/batches", dialectNone},
		{"/v1/embeddings", dialectNone},
		{"/v1/rerank", dialectNone},
		{"/v1/responses", dialectNone},
		{"/inference/v1/model/info", dialectNone},
		{"/v1/autocompletions", dialectNone},
		{"/inference/v1/chat/completions/", dialectNone}, // trailing slash: not trimmed
		{"", dialectNone},
		{"/", dialectNone},
	} {
		if got := dialectFor(tc.path); got != tc.want {
			t.Errorf("dialectFor(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}

// A provider mounted under a prefix of its own is parsed, and its stream read as the same
// dialect: OpenRouter's /api prefix.
func TestInferenceParser_PrefixedOpenAIPath_StreamedEndToEnd(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/api/v1/chat/completions",
		Body: []byte(`{"model":"anthropic/claude-sonnet-4.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference == nil {
		t.Fatal("Extensions.Inference is nil for /api/v1/chat/completions")
	}
	for _, f := range []string{
		`{"choices":[{"delta":{"content":"po"}}]}`,
		`{"choices":[{"delta":{"content":"ng"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`,
	} {
		if action := p.OnResponseFrame(context.Background(), pctx, []byte(f), false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "pong" || ext.FinishReason != "stop" {
		t.Errorf("Completion = %q, FinishReason = %q, want pong / stop", ext.Completion, ext.FinishReason)
	}
	if ext.PromptTokens != 9 || ext.CompletionTokens != 2 {
		t.Errorf("tokens = %d/%d, want 9/2", ext.PromptTokens, ext.CompletionTokens)
	}
}

// Azure names the deployment in the path and sends no model. The request is still
// inference, with an empty model, and its response still yields tokens.
func TestInferenceParser_AzureDeploymentPath_NoModel(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/openai/deployments/gpt-4o/chat/completions",
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	p.OnRequest(context.Background(), pctx)
	ext := pctx.Extensions.Inference
	if ext == nil {
		t.Fatal("Extensions.Inference is nil for an Azure deployment path")
	}
	if ext.Model != "" {
		t.Errorf("Model = %q, want empty: the body named none", ext.Model)
	}
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)
	if ext.Completion != "pong" || ext.PromptTokens != 9 || ext.CompletionTokens != 2 {
		t.Errorf("Completion = %q, tokens = %d/%d; want pong, 9/2", ext.Completion, ext.PromptTokens, ext.CompletionTokens)
	}
}

// A gateway's /anthropic prefix routes the response to the Anthropic reader too. Read as
// OpenAI, this stream yields no tokens at all.
func TestInferenceParser_PrefixedAnthropicPath_StreamedEndToEnd(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{
		Path: "/anthropic/v1/messages",
		Body: []byte(`{"model":"claude-sonnet-4-6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}
	p.OnRequest(context.Background(), pctx)
	if pctx.Extensions.Inference == nil {
		t.Fatal("Extensions.Inference is nil for /anthropic/v1/messages")
	}
	for _, f := range []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","usage":{"input_tokens":25,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`,
		`{"type":"message_stop"}`,
	} {
		if action := p.OnResponseFrame(context.Background(), pctx, []byte(f), false); action.Type != pipeline.Continue {
			t.Fatalf("frame action = %v, want Continue", action.Type)
		}
	}
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "pong" || ext.FinishReason != "end_turn" {
		t.Errorf("Completion = %q, FinishReason = %q, want pong / end_turn", ext.Completion, ext.FinishReason)
	}
	if ext.PromptTokens != 25 || ext.CompletionTokens != 15 {
		t.Errorf("tokens = %d/%d, want 25/15", ext.PromptTokens, ext.CompletionTokens)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd core && go test ./plugins/inferenceparser/ -run 'TestDialectFor|Prefixed|AzureDeployment' -v`
Expected: build failure, `undefined: dialect` and `undefined: dialectFor`.

- [ ] **Step 3: Create `dialect.go`**

Create `core/plugins/inferenceparser/dialect.go`:

```go
package inferenceparser

import "strings"

// dialect is the wire format an inference endpoint speaks, which decides how both its
// request and its response are read.
type dialect int

const (
	dialectNone      dialect = iota // not an endpoint this parser reads
	dialectOpenAI                   // OpenAI chat completions, or legacy completions
	dialectAnthropic                // Anthropic Messages
)

// anthropicMessagesPath is the Anthropic Messages endpoint, and the ending that marks the
// dialect under any prefix: Anthropic's own /v1/messages, OpenCode Zen's /zen/v1/messages
// and /zen/go/v1/messages, and the /anthropic/v1/messages that gateways mount. Clients
// such as Claude Code POST here instead of to /v1/chat/completions, so the parser must
// recognise both dialects.
const anthropicMessagesPath = "/v1/messages"

// completionsSuffix ends every OpenAI-dialect inference path: /v1/chat/completions and the
// legacy /v1/completions, at the root or under any prefix — IBM Bob's /inference, OpenCode
// Zen's /zen and /zen/go, OpenRouter's /api, Groq's /openai, Azure's
// /openai/deployments/<d>.
const completionsSuffix = "/completions"

// dialectFor reports which dialect path speaks, from how it ends. One function for the
// request and every response-side call site, so a request and its response cannot be read
// as different dialects.
//
// THE END OF THE PATH, NOT THE WHOLE OF IT. Providers mount these APIs under prefixes of
// their own, and matching whole paths needed a code change for each one. What keeps an
// unrelated endpoint that happens to end the same way out of the inference record is the
// body check in parseOpenAIRequest and parseAnthropicRequest, not this function.
//
// path must already be query-free (endpointPath). A trailing slash is not trimmed: no
// client is known to send one, and TestInferenceParser_BobNonInferencePathsAreIgnored pins
// it as not inference.
func dialectFor(path string) dialect {
	switch {
	case strings.HasSuffix(path, anthropicMessagesPath):
		return dialectAnthropic
	case strings.HasSuffix(path, completionsSuffix):
		return dialectOpenAI
	}
	return dialectNone
}
```

- [ ] **Step 4: Delete the old path constants and predicate**

In `core/plugins/inferenceparser/anthropic.go`, delete everything from the line `// anthropicMessagesPath is the Anthropic Messages API endpoint. Clients` through the closing `}` of `isAnthropicMessagesPath` (lines 13–37): the `anthropicMessagesPath` const and its comment, the `zenMessagesPath`/`zenGoMessagesPath` const block and its comment, and `isAnthropicMessagesPath` and its comment. Keep the `strings` import — the file uses it three more times.

In `core/plugins/inferenceparser/plugin.go`, delete everything from the line `// bobPath is IBM Bob's inference endpoint. It speaks the OPENAI dialect — the` through `const zenGoPath = "/zen/go/v1/chat/completions"` (lines 61–79).

- [ ] **Step 5: Route the request through `dialectFor`**

In `core/plugins/inferenceparser/plugin.go`, `OnRequest`, replace

```go
	var ext *pipeline.InferenceExtension
	switch path := endpointPath(pctx); {
	case isAnthropicMessagesPath(path):
		ext = parseAnthropicRequest(pctx.Body)
	case path == "/v1/chat/completions", path == "/v1/completions", path == "/chat/completions",
		path == "/completions", path == bobPath, path == zenPath, path == zenGoPath:
		ext = parseOpenAIRequest(pctx.Body)
	default:
		return pipeline.Action{Type: pipeline.Continue}
	}
```

with

```go
	var ext *pipeline.InferenceExtension
	switch dialectFor(endpointPath(pctx)) {
	case dialectAnthropic:
		ext = parseAnthropicRequest(pctx.Body)
	case dialectOpenAI:
		ext = parseOpenAIRequest(pctx.Body)
	default:
		return pipeline.Action{Type: pipeline.Continue}
	}
```

In the comment at the top of `OnRequest`, replace

```go
	// Dispatch by endpoint dialect: OpenAI chat/completions vs Anthropic
	// Messages. No Invocation is recorded when the parser doesn't apply
	// (unrecognized path, empty body, or non-JSON body) — operators infer
	// "inference-parser is in this pipeline" from config, not per-event rows.
```

with

```go
	// Dispatch by endpoint dialect: OpenAI chat/completions vs Anthropic
	// Messages. No Invocation is recorded when the parser doesn't apply (a path
	// dialectFor does not recognise, or a body not taken for inference) —
	// operators infer "inference-parser is in this pipeline" from config, not
	// per-event rows.
```

- [ ] **Step 6: Route the response through `dialectFor`**

Replace all five occurrences in `core/plugins/inferenceparser/plugin.go`:

Run: `cd core && sed -i '' 's/isAnthropicMessagesPath(endpointPath(pctx))/dialectFor(endpointPath(pctx)) == dialectAnthropic/g' plugins/inferenceparser/plugin.go && grep -c 'dialectFor(endpointPath(pctx)) == dialectAnthropic' plugins/inferenceparser/plugin.go`
Expected: `5`. (On Linux, `sed -i` takes no `''`.)

In the `endpointPath` doc comment, replace

```go
// silent: Claude Code posts to /v1/messages?beta=true, and with a query
// attached the exact-match dialect dispatch below falls to the default arm and
// records no inference telemetry at all — or worse, sends an Anthropic stream
// to the OpenAI parser.
```

with

```go
// silent: Claude Code posts to /v1/messages?beta=true, and with a query
// attached the path no longer ends the way dialectFor looks for, so it falls to
// the default arm and records no inference telemetry at all — or worse, sends an
// Anthropic stream to the OpenAI parser.
```

- [ ] **Step 7: Update the tests that used the deleted constants**

In `core/plugins/inferenceparser/plugin_test.go`, directly above the comment `// TestInferenceParser_BobPath covers IBM Bob, which mounts an OpenAI-dialect`, add:

```go
// bobInferencePath and zenChatPath are the provider paths the tests below pin.
const (
	bobInferencePath = "/inference/v1/chat/completions"
	zenChatPath      = "/zen/v1/chat/completions"
)

```

Then rename the references. The substitution is case-sensitive, so the `BobPath`/`ZenPath` test names are untouched, and no other identifier in the file contains `bobPath` or `zenPath` (the two new names do not either):

Run: `cd core && sed -i '' -e 's/bobPath/bobInferencePath/g' -e 's/zenPath/zenChatPath/g' plugins/inferenceparser/plugin_test.go && grep -n 'bobPath\|zenPath' plugins/inferenceparser/plugin_test.go`
Expected: no output. (On Linux, `sed -i` takes no `''`.)

Replace the comment above `TestInferenceParser_BobPath_ChatCompletions`:

```go
// TestInferenceParser_BobPath covers IBM Bob, which mounts an OpenAI-dialect
// inference API under an /inference prefix. The dispatch switch is exact-match, so
// without the path the parser falls to the default arm and records no telemetry at
// all — the body is never even looked at.
```

with

```go
// TestInferenceParser_BobPath covers IBM Bob, which mounts an OpenAI-dialect
// inference API under an /inference prefix. A rule that missed the prefix would send
// the request to the default arm, which records no telemetry at all — the body is
// never even looked at.
```

Replace the comment above `TestInferenceParser_ZenPath_ChatCompletions`:

```go
// TestInferenceParser_ZenPath covers OpenCode Zen (opencode.ai/zen), which mounts an
// OpenAI-dialect inference API under a /zen prefix. Same failure mode as BobPath:
// without the path the parser falls to the default arm and records no telemetry.
```

with

```go
// TestInferenceParser_ZenPath covers OpenCode Zen (opencode.ai/zen), which mounts an
// OpenAI-dialect inference API under a /zen prefix. Same failure mode as BobPath: a
// rule that missed the prefix would record no telemetry.
```

Replace the comment above `TestInferenceParser_BobNonInferencePathsAreIgnored`:

```go
// The sibling Bob endpoints seen alongside the inference one are NOT inference and
// must stay unmatched — an exact-match switch is what keeps /admin/v1/profile from
// being mistaken for a chat completion.
```

with

```go
// The sibling Bob endpoints seen alongside the inference one are NOT inference and
// must stay unmatched — matching on how the path ends is what keeps /admin/v1/profile
// and /inference/v1/model/info from being mistaken for a chat completion.
```

- [ ] **Step 8: Update the comment in `core/pipeline/client.go`**

Replace

```go
	// work — the inference endpoint is parsed (inferenceparser's bobPath), its session header
	// is session.BobSessionHeader, and `agentop configure bobshell` sets it up — so this is a
```

with

```go
	// work — inferenceparser parses its inference endpoint, its session header is
	// session.BobSessionHeader, and `agentop configure bobshell` sets it up — so this is a
```

- [ ] **Step 9: Run the package tests**

Run: `cd core && go test ./plugins/inferenceparser/ -v -run 'TestDialectFor|Prefixed|AzureDeployment|BobPath|ZenPath|BobNonInference|Zen|Legacy|VlessPath'`
Expected: PASS for every test listed.

Run: `cd core && go test ./plugins/inferenceparser/ && go build ./...`
Expected: `ok`, and the build prints nothing. `grep -rn 'isAnthropicMessagesPath\|zenGoPath\|zenMessagesPath\|bobPath' --include=*.go .` from `core/` prints nothing.

- [ ] **Step 10: Commit**

```bash
git add core/plugins/inferenceparser/dialect.go core/plugins/inferenceparser/dialect_test.go core/plugins/inferenceparser/plugin.go core/plugins/inferenceparser/anthropic.go core/plugins/inferenceparser/plugin_test.go core/pipeline/client.go
git commit -s -m "fix: Parse known inference dialects under any path prefix

The parser matched ten exact paths, so every provider that mounts the
OpenAI or Anthropic API under a prefix of its own needed a new constant
(Bob, Zen, Zen Go). It now matches how the path ends: /completions is
OpenAI, /v1/messages is Anthropic. OpenRouter, Groq, Azure and the
/anthropic gateways are parsed with no change here.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 3: Document the rule

**Files:**
- Modify: `docs/agents/opencode.md` (the *Typed inference* paragraph, ~lines 219–231)
- Modify: `docs/plugin-catalog.md` (the `inference-parser` section, ~line 112)

**Interfaces:** none.

- [ ] **Step 1: Rewrite *Typed inference* in `docs/agents/opencode.md`**

Replace from `**Typed inference.** OpenCode Zen serves each model on the endpoint of the SDK it speaks,` through `own, are recorded with their method and path but not parsed.` with:

```markdown
**Typed inference.** OpenCode Zen serves each model on the endpoint of the SDK it speaks,
and Cortex parses two of them, under both the `/zen` prefix and the `/zen/go` prefix of
OpenCode's Go plan:

- the OpenAI dialect (model, messages, tools and the response) on
  `/zen/v1/chat/completions`;
- the Anthropic dialect on `/zen/v1/messages`, where Zen serves its Claude models and
  most of its Qwen ones.

Cortex picks the dialect from how the path ends, not from the provider: a path ending in
`/completions` is read as OpenAI and one ending in `/v1/messages` as Anthropic, under any
prefix. A provider OpenCode is pointed at that speaks either one is parsed the same way:
Anthropic's own API, LiteLLM, OpenRouter, Groq, Azure OpenAI. The body must also carry a
`messages` array, or a `prompt` for legacy completions. Zen's `/zen/v1/responses` (its GPT
and Grok models) and `/zen/v1/models/<id>` (its Gemini models) are other dialects, and are
recorded with their method and path but not parsed.
```

- [ ] **Step 2: Say which endpoints it reads in `docs/plugin-catalog.md`**

In the `inference-parser` section, replace the first paragraph

```markdown
Parses outbound OpenAI-compatible LLM inference requests/responses into
`pctx.Extensions.Inference` for downstream policy plugins, **and prices the finished
response** — it is the one place tokens become dollars.
```

with

```markdown
Parses outbound LLM inference requests/responses into `pctx.Extensions.Inference` for
downstream policy plugins, **and prices the finished response** — it is the one place
tokens become dollars.

It reads two dialects, chosen by how the request path ends, under any prefix: a path ending
in `/completions` is OpenAI chat completions (or legacy completions), and one ending in
`/v1/messages` is Anthropic Messages. That covers providers that mount the same API under a
prefix of their own — IBM Bob's `/inference`, OpenCode Zen's `/zen`, OpenRouter's `/api`,
Groq's `/openai`, Azure's `/openai/deployments/<d>` — with no change here. A body is taken
for inference only if it carries a `messages` array, or a `prompt` for legacy completions.
Anything else is recorded with no inference record, like any other path, and is still
priced from a gateway's cost header. Other dialects — the Responses API, Gemini's native
API, Bedrock — are not parsed.
```

- [ ] **Step 3: Commit**

```bash
git add docs/agents/opencode.md docs/plugin-catalog.md
git commit -s -m "docs: Say the inference parser matches how the path ends

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

### Task 4: Verify the whole module

**Files:** none changed, unless a check fails.

- [ ] **Step 1: Run every core test**

Run: `cd core && go test ./... 2>&1 | grep -v '^ok' | grep -v 'no test files'`
Expected: no output. The listener suites (`listener/parity`, `listener/reverseproxy`, `listener/forwardproxy`, `listener/extproc`) drive the parser with real bodies; a failure there whose fixture has no `messages` gets the Task 1, Step 6 treatment, in its own commit.

- [ ] **Step 2: Vet and format**

Run: `cd core && go vet ./... && gofmt -l plugins/inferenceparser pipeline/client.go`
Expected: `go vet` prints nothing; `gofmt -l` prints nothing.

- [ ] **Step 3: Check the module is still tidy**

Run: `cd core && GOWORK=off go mod tidy -diff`
Expected: no output (no dependency was added or dropped).

- [ ] **Step 4: Stop for review**

Report the commits (`git log --oneline refs/base/parser-any-path..HEAD`) and the diff size (`git diff --stat refs/base/parser-any-path..HEAD`). Do not push or open the PR until the user says to; the PR title is `Fix: Parse known inference dialects under any path prefix`.
