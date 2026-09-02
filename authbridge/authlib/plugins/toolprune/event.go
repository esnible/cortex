package toolprune

import "github.com/rossoctl/cortex/authbridge/authlib/pipeline"

// pruneEvent is the per-request record published under "tool-prune/event", so a
// consumer can show what this one request saved instead of only an aggregate.
//
// It deliberately carries the applicable rates rather than a finished dollar
// figure. The dollar amount depends on which prompt-cache tier the saving came
// out of, and that is only known from the response — so the request-side event
// supplies the inputs and the consumer, which can pair request to response by
// RequestID, does the last step. Carrying the rates also means a consumer needs
// no knowledge of the built-in default table.
//
// No body content: the session store is unauthenticated, so this holds counts,
// tool names the operator themselves configured, and rates.
type pruneEvent struct {
	ToolsRemoved   []string `json:"toolsRemoved,omitempty"`
	BytesRemoved   int      `json:"bytesRemoved"`
	BodyBytesAfter int      `json:"bodyBytesAfter"`
	Model          string   `json:"model,omitempty"`

	// Rates are USD per token for this request's model, already resolved
	// through config → flat fallback → built-in defaults.
	RateInput      float64 `json:"rateInput,omitempty"`
	RateCacheWrite float64 `json:"rateCacheWrite,omitempty"`
	RateCacheRead  float64 `json:"rateCacheRead,omitempty"`
	RateSource     string  `json:"rateSource,omitempty"` // configured | default | none
}

func (p *ToolPrune) publish(pctx *pipeline.Context, ev pruneEvent) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix] = ev
}

// inferenceModel returns the model the parser recorded, or "" when no parser has
// run — in which case rate lookup falls through to the flat fallback.
func inferenceModel(pctx *pipeline.Context) string {
	if pctx.Extensions.Inference == nil {
		return ""
	}
	return pctx.Extensions.Inference.Model
}
