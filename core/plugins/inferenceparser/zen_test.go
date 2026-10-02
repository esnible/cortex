package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// OpenCode Zen (opencode.ai/zen) serves each model on the endpoint of the SDK it speaks:
// its Claude models, and most of its Qwen ones, on an Anthropic Messages endpoint under
// the /zen prefix, and the rest on an OpenAI chat endpoint. OpenCode's Go plan mounts the
// same two under /zen/go. A path this parser does not route falls to the default arm and
// records no telemetry: no model, no tokens, no cost.
func TestInferenceParser_ZenPaths_Request(t *testing.T) {
	const anthropicBody = `{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	const openAIBody = `{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}],"stream":false}`
	for _, tc := range []struct {
		path, body, model string
	}{
		{"/zen/v1/messages", anthropicBody, "claude-sonnet-4-6"},
		{"/zen/go/v1/messages", anthropicBody, "claude-sonnet-4-6"},
		{"/zen/go/v1/chat/completions", openAIBody, "glm-5.1"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			pctx := &pipeline.Context{Path: tc.path, Body: []byte(tc.body)}
			if action := NewInferenceParser().OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
				t.Fatalf("expected Continue, got %v", action.Type)
			}
			ext := pctx.Extensions.Inference
			if ext == nil {
				t.Fatalf("Extensions.Inference is nil for %s", tc.path)
			}
			if ext.Model != tc.model {
				t.Errorf("Model = %q, want %s", ext.Model, tc.model)
			}
			if len(ext.Messages) != 1 || ext.Messages[0].Content != "hi" {
				t.Errorf("Messages = %+v, want one user message \"hi\"", ext.Messages)
			}
		})
	}
}

// The response to a Zen Anthropic-dialect request is an Anthropic Messages response, so
// its usage must be read the Anthropic way. Read as OpenAI it yields no tokens at all.
func TestInferenceParser_ZenMessages_NonStreamingResponse(t *testing.T) {
	pctx := &pipeline.Context{Path: "/zen/v1/messages"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-6", IsAction: true}
	body := []byte(`{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6",
		"content": [{"type": "text", "text": "pong"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 25, "output_tokens": 8, "cache_read_input_tokens": 2}
	}`)
	NewInferenceParser().OnResponseFrame(context.Background(), pctx, body, true)

	ext := pctx.Extensions.Inference
	if ext.Completion != "pong" || ext.FinishReason != "end_turn" {
		t.Errorf("Completion = %q, FinishReason = %q, want pong / end_turn", ext.Completion, ext.FinishReason)
	}
	if ext.PromptTokens != 27 || ext.CompletionTokens != 8 || ext.CacheReadTokens != 2 {
		t.Errorf("tokens = prompt %d / completion %d / cache read %d, want 27/8/2",
			ext.PromptTokens, ext.CompletionTokens, ext.CacheReadTokens)
	}
}

func TestInferenceParser_ZenMessages_StreamFoldsEvents(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/zen/v1/messages"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-6", Stream: true, IsAction: true}
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
		t.Errorf("tokens = prompt %d / completion %d, want 25/15", ext.PromptTokens, ext.CompletionTokens)
	}
}
