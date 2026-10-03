package inferenceparser

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A path that ends like an inference endpoint is not enough: the body has to carry what an
// inference request cannot do without. Every case here left an extension, marked as an
// action, before the check existed, JSON null included: it decodes into an empty request
// with no error. The exceptions are the bodies that never decoded — `{"messages":{}}`,
// `[]` and `"hi"`, in both dialects — which stay here to pin that.
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
		{"anthropic messages not an array", "/v1/messages", `{"model":"claude-sonnet-4-6","messages":{}}`},
		{"anthropic prompt is not enough", "/v1/messages", `{"model":"claude-sonnet-4-6","prompt":"hi"}`},
		{"anthropic json null", "/v1/messages", `null`},
		{"anthropic json array", "/v1/messages", `[]`},
		{"anthropic json string", "/v1/messages", `"hi"`},
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
