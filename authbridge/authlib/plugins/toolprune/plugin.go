// Package toolprune removes unused tool definitions from outbound inference
// requests.
//
// A Claude Code request carries the full tool manifest on every turn — tens of
// thousands of tokens of JSON schema, billed each time and largely for tools
// the agent will never call in a given deployment. The manifest is assembled by
// the client, so the only place to trim it without touching every client is in
// the proxy.
//
// The verdict is entirely configuration: `remove` names the tools to drop.
// There is no learning, no state and no storage dependency. `abctl tools scan`
// produces a candidate list from local transcripts, but the plugin itself only
// ever does what it was told.
//
// Safety is one-directional. Removing a tool the model needs is the harmful
// failure; carrying a few extra definitions is not. So every error path fails
// open, forwarding the original bytes untouched, and a tool named by a forced
// tool_choice is never removed — the manifest and tool_choice have to agree or
// the request is invalid.
//
// That is a promise about this plugin's own failure modes, not a claim that
// pruning is always safe: whether a provider or gateway accepts a validly
// pruned manifest is outside what the plugin can observe. on_error: observe
// exists to establish that empirically before any request changes.
package toolprune

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
)

// defaultPaths are the inference endpoints the plugin acts on, matched by
// suffix as context-guru does.
var defaultPaths = []string{"/v1/chat/completions", "/v1/completions", "/v1/messages"}

type config struct {
	// Remove names the tools to delete from the manifest. Names not present
	// in a given request are ignored; names the plugin never observes are
	// reported as drift rather than failing.
	Remove []string `json:"remove" description:"Tool names to remove from the outbound manifest."`

	// Paths are the request paths this plugin acts on, matched exactly or by
	// suffix. Defaults to the three inference endpoints.
	Paths []string `json:"paths" description:"Request paths to act on (exact or suffix match)."`

	// Pricing gives per-token rates per model. Rates are per model because they
	// differ enormously: across the Claude family the input rate spans roughly
	// 5x (opus 1.0x, sonnet ~0.4x, haiku ~0.2x), so one flat rate misprices by
	// that factor depending on which model served the request.
	// Keys match the model name the parser records
	// (pctx.Extensions.Inference.Model), matched case-insensitively.
	Pricing map[string]modelRates `json:"pricing" description:"Per-token rates keyed by model name."`

	// pricing is Pricing with keys lower-cased; built by applyDefaults.
	pricing map[string]modelRates `json:"-"`

	// The flat fields are the fallback for models absent from Pricing. Names and
	// semantics match litellm-budget-track. All optional; with nothing set no
	// cost is reported rather than a price being assumed.
	//
	// There is deliberately no output rate: pruning only ever shrinks the
	// prompt, so attributing output cost to it would be false.
	InputCostPerToken      float64 `json:"input_cost_per_token" description:"Fallback USD per uncached input token, for models absent from pricing."`
	CacheWriteCostPerToken float64 `json:"cache_write_cost_per_token" description:"Fallback USD per cache-write token; defaults to input_cost_per_token."`
	CacheReadCostPerToken  float64 `json:"cache_read_cost_per_token" description:"Fallback USD per cache-read token; defaults to input_cost_per_token."`
}

// modelRates is one model's prompt-tier pricing. Cache rates fall back to the
// input rate, matching litellm-budget-track — though on Anthropic-family models
// that fallback is poor (a real cache read is 0.1x input), so set them when known.
type modelRates struct {
	InputCostPerToken      float64 `json:"input_cost_per_token" description:"USD per uncached input token."`
	CacheWriteCostPerToken float64 `json:"cache_write_cost_per_token" description:"USD per cache-write token; defaults to input_cost_per_token."`
	CacheReadCostPerToken  float64 `json:"cache_read_cost_per_token" description:"USD per cache-read token; defaults to input_cost_per_token."`
}

func (r modelRates) rateFor(t tier) float64 {
	switch t {
	case tierCacheWrite:
		if r.CacheWriteCostPerToken > 0 {
			return r.CacheWriteCostPerToken
		}
	case tierCacheRead:
		if r.CacheReadCostPerToken > 0 {
			return r.CacheReadCostPerToken
		}
	}
	return r.InputCostPerToken
}

func (r modelRates) set() bool {
	return r.InputCostPerToken > 0 || r.CacheWriteCostPerToken > 0 || r.CacheReadCostPerToken > 0
}

// ratesFor resolves rates for a model: its own entry when present, else the flat
// fallback. Reports false when neither is configured, so the caller counts the
// request as unpriced rather than charging it at another model's rate — which on
// a 5x spread would be worse than reporting nothing.
func (c *config) ratesFor(model string) (modelRates, bool) {
	if r, ok := c.pricing[strings.ToLower(model)]; ok && r.set() {
		return r, true
	}
	fallback := modelRates{
		InputCostPerToken:      c.InputCostPerToken,
		CacheWriteCostPerToken: c.CacheWriteCostPerToken,
		CacheReadCostPerToken:  c.CacheReadCostPerToken,
	}
	return fallback, fallback.set()
}

// priced reports whether any pricing is configured at all.
func (c *config) priced() bool {
	if c.InputCostPerToken > 0 || c.CacheWriteCostPerToken > 0 || c.CacheReadCostPerToken > 0 {
		return true
	}
	for _, r := range c.Pricing {
		if r.set() {
			return true
		}
	}
	return false
}

func (c *config) applyDefaults() {
	if len(c.Paths) == 0 {
		c.Paths = append([]string(nil), defaultPaths...)
	}
	// Fold model keys to lower case once, so lookup is case-insensitive
	// without allocating per request. Gateways vary in how they echo model
	// names, and a case mismatch would silently unprice the traffic.
	c.pricing = make(map[string]modelRates, len(c.Pricing))
	for k, v := range c.Pricing {
		c.pricing[strings.ToLower(k)] = v
	}
}

// ToolPrune is the plugin. Counters live in metrics, guarded by its own mutex;
// everything else is read-only after Configure.
type ToolPrune struct {
	cfg    config
	raw    json.RawMessage
	remove map[string]struct{}

	m         metrics
	driftOnce sync.Once
}

func New() *ToolPrune { return &ToolPrune{} }

func init() {
	plugins.RegisterPlugin("tool-prune", func() pipeline.Plugin { return New() })
}

func (p *ToolPrune) Name() string { return "tool-prune" }

func (p *ToolPrune) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		// Request-only: the response is never touched, so SSE relay stays
		// incremental. That distinction is the reason WritesResponseBody
		// exists as a separate capability.
		WritesRequestBody: true,
		RequiresAny:       []string{"inference-parser"},
		Description:       "Removes unused tool definitions from inference requests.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider.
func (p *ToolPrune) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(config{})
}

// RawConfig implements pipeline.RawConfigProvider.
func (p *ToolPrune) RawConfig() json.RawMessage { return p.raw }

func (p *ToolPrune) Configure(raw json.RawMessage) error {
	var c config
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("tool-prune config: %w", err)
		}
	}
	c.applyDefaults()

	p.cfg = c
	p.raw = raw
	p.remove = make(map[string]struct{}, len(c.Remove))
	for _, n := range c.Remove {
		if n != "" {
			p.remove[n] = struct{}{}
		}
	}
	if len(p.remove) == 0 {
		slog.Info("tool-prune: configured with an empty remove list — no-op until names are added",
			"hint", "abctl tools scan")
	}
	return nil
}

// gated reports whether the request path is one the plugin acts on.
//
// The query string is stripped first. Providers accept query parameters on
// these endpoints — /v1/messages?beta=true is a real request Claude Code makes —
// and a suffix match against the raw target silently misses every one of them,
// which reads as the plugin doing nothing for no visible reason.
func (p *ToolPrune) gated(path string) bool {
	path = pathOnly(path)
	for _, s := range p.cfg.Paths {
		if path == s || strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// pathOnly drops a query string and any trailing slash, so the configured
// suffixes match the endpoint rather than the exact request target.
func pathOnly(target string) string {
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if len(target) > 1 && strings.HasSuffix(target, "/") {
		target = strings.TrimRight(target, "/")
	}
	return target
}

// toolNameAt extracts a tool's name from raw manifest element i, covering both
// dialects: Anthropic puts it at tools.i.name, OpenAI at tools.i.function.name.
func toolNameAt(body []byte, i int) string {
	if n := gjson.GetBytes(body, fmt.Sprintf("tools.%d.name", i)); n.Exists() {
		return n.String()
	}
	return gjson.GetBytes(body, fmt.Sprintf("tools.%d.function.name", i)).String()
}

// forcedToolName returns the tool a forced tool_choice names, or "" when the
// request does not force one. Anthropic spells it tool_choice.name, OpenAI
// tool_choice.function.name; "auto" / "none" / "any" carry no name.
//
// This tool can never be removed: a tool_choice naming a tool absent from the
// manifest is an invalid request, so pruning it would turn a cost optimisation
// into a 400.
func forcedToolName(body []byte) string {
	tc := gjson.GetBytes(body, "tool_choice")
	if !tc.IsObject() {
		return "" // "auto" / "none" / absent
	}
	if n := tc.Get("name"); n.Exists() {
		return n.String()
	}
	return tc.Get("function.name").String()
}

// OnRequest prunes the manifest. Every failure path returns Continue with the
// body untouched.
func (p *ToolPrune) OnRequest(_ context.Context, pctx *pipeline.Context) (action pipeline.Action) {
	action = pipeline.Action{Type: pipeline.Continue}
	if len(p.remove) == 0 {
		return action
	}
	// A panic here would fail a request to save tokens. Never worth it.
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("tool-prune: recovered, forwarding original body", "panic", r)
			action = pipeline.Action{Type: pipeline.Continue}
		}
	}()

	if !p.gated(pctx.Path) {
		// Distinguish "this is not an HTTP request at all" from "the path did
		// not match". A CONNECT tunnel has no path, and reporting it as a path
		// mismatch sends an operator hunting for a routing problem when the
		// real answer is that TLS is not being decrypted — so the client does
		// not trust the bridge CA and nothing downstream can see the request.
		reason := "path_not_inference"
		if pctx.Path == "" {
			reason = "no_path_tunnelled"
		}
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionSkip,
			Reason: reason,
			Path:   pctx.Path,
		})
		return action
	}
	// inference-parser establishes that this is an inference call at all. Its
	// absence means the chain is misconfigured; RequiresAny catches that at
	// build time, so treat it as a skip rather than an error.
	if pctx.Extensions.Inference == nil {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_inference_extension"})
		return action
	}
	body := pctx.Body
	if len(body) == 0 {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_body"})
		return action
	}
	// gjson parses leniently: on a truncated document it still resolves
	// fields, and sjson then rewrites the fragment into garbage. Refuse to
	// touch anything that is not well-formed JSON to begin with.
	if !gjson.ValidBytes(body) {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "invalid_json"})
		return action
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_tool_manifest"})
		return action
	}

	raw := tools.Array()
	p.noteDrift(pctx.Extensions.Inference.Tools)
	p.m.seen()

	// Resolve indices from the raw bytes rather than from the parsed manifest:
	// inference-parser drops unnamed tools, so manifest position does not
	// reliably map back to array position.
	forced := forcedToolName(body)
	var victims []int
	names := make([]string, 0, len(raw))
	for i := range raw {
		name := toolNameAt(body, i)
		if name == "" {
			continue
		}
		if name == forced {
			// Removing the tool tool_choice forces would make the request
			// invalid. Keep it and prune the rest.
			slog.Debug("tool-prune: keeping tool forced by tool_choice", "tool", name)
			continue
		}
		if _, ok := p.remove[name]; ok {
			victims = append(victims, i)
			names = append(names, name)
		}
	}
	if len(victims) == 0 {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_configured_tool_present"})
		return action
	}

	out := body
	var err error
	if len(victims) == len(raw) {
		// Emptying the array is not safe — OpenAI rejects `tools: []`, and
		// tool_choice without tools. Drop both keys instead.
		if out, err = sjson.DeleteBytes(out, "tools"); err != nil {
			slog.Warn("tool-prune: delete tools failed, forwarding original", "err", err)
			return action
		}
		if gjson.GetBytes(out, "tool_choice").Exists() {
			if out, err = sjson.DeleteBytes(out, "tool_choice"); err != nil {
				slog.Warn("tool-prune: delete tool_choice failed, forwarding original", "err", err)
				return action
			}
		}
	} else {
		// Descending, so an earlier deletion never shifts a later index.
		for i := len(victims) - 1; i >= 0; i-- {
			if out, err = sjson.DeleteBytes(out, fmt.Sprintf("tools.%d", victims[i])); err != nil {
				slog.Warn("tool-prune: delete failed, forwarding original", "index", victims[i], "err", err)
				return action
			}
		}
	}
	if len(out) >= len(body) {
		// Nothing shrank: treat as a no-op rather than emitting a rewrite.
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_bytes_removed"})
		return action
	}
	// Post-conditions. The edit is surgical, so verify it actually did what
	// was intended before putting it on the wire: still valid JSON, and
	// exactly the intended number of tools left standing.
	if !gjson.ValidBytes(out) {
		slog.Warn("tool-prune: rewrite produced invalid JSON, forwarding original")
		return action
	}
	want := len(raw) - len(victims)
	if got := len(gjson.GetBytes(out, "tools").Array()); got != want {
		slog.Warn("tool-prune: unexpected tool count after rewrite, forwarding original",
			"got", got, "want", want)
		return action
	}

	removedBytes := len(body) - len(out)
	// Carry the saving to OnFinish, where the response reveals which token tier
	// it came out of. SetState keeps it private to this plugin, unlike
	// Extensions.Custom which is shared.
	pipeline.SetState(pctx, p.Name(), &requestState{bytesRemoved: removedBytes})
	pctx.SetBody(out)
	// Under ErrorPolicyObserve, SetBody is a no-op on bytes and leaves
	// bodyMutated false — so this same code path measures without enforcing,
	// and the counter it lands in is what distinguishes the two.
	if pctx.BodyMutated() {
		p.m.pruned(names, removedBytes)
	} else {
		p.m.projected(names, removedBytes)
	}
	return action
}

func (p *ToolPrune) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// requestState carries the per-request byte saving from OnRequest to OnFinish.
type requestState struct{ bytesRemoved int }

// OnFinish converts the request's byte saving into tokens and attributes it to
// the token tier it actually came out of.
//
// Two things make a single "tokens saved" number wrong, which is why this is
// per-tier. First, the ratio: rather than bundling a tokenizer or assuming
// bytes-per-token, it is calibrated on this request — prompt tokens over request
// bytes, both post-pruning, so the two sides are consistent. Second, and larger:
// providers price prompt tiers very differently. Anthropic charges 1.25x the
// input rate for a cache write and 0.1x for a cache read, so identical saved
// bytes are worth more than 12x more on a cache miss than on a hit. Reporting
// one blended figure would hide a factor of twelve.
//
// The tool manifest sits inside the cached prefix — Claude Code puts
// cache_control on the tool block — so on a cache-miss request the saving comes
// out of cache writes, and on a hit out of cache reads. That is the assumption
// this attribution rests on; it is stated here because it is the one thing that
// would need revisiting for a client that lays out its prompt differently.
func (p *ToolPrune) OnFinish(_ context.Context, pctx *pipeline.Context) {
	st := pipeline.GetState[requestState](pctx, p.Name())
	if st == nil || st.bytesRemoved <= 0 {
		return
	}
	inf := pctx.Extensions.Inference
	if inf == nil || len(pctx.Body) == 0 {
		return
	}
	promptTotal := inf.InputTokens + inf.CacheReadTokens + inf.CacheWriteTokens
	if promptTotal <= 0 {
		// Fall back to the aggregate when a provider reports only a total.
		promptTotal = inf.PromptTokens
	}
	if promptTotal <= 0 {
		return
	}
	tokens := float64(st.bytesRemoved) * float64(promptTotal) / float64(len(pctx.Body))
	if tokens <= 0 {
		return
	}
	t := tierOf(inf)
	rates, priced := p.cfg.ratesFor(inf.Model)
	p.m.observeSaving(tokens, t, tokens*rates.rateFor(t), priced, inf.Model)
}

// tier names which prompt token tier a request's saving came out of.
type tier int

const (
	tierInput tier = iota
	tierCacheWrite
	tierCacheRead
)

// tierOf picks the tier the pruned manifest belonged to. The manifest is in the
// cached prefix, so a write-dominant request wrote it and a read-dominant one
// read it; with no cache tokens reported at all it was plain input.
func tierOf(inf *pipeline.InferenceExtension) tier {
	switch {
	case inf.CacheWriteTokens > inf.CacheReadTokens && inf.CacheWriteTokens > 0:
		return tierCacheWrite
	case inf.CacheReadTokens > 0:
		return tierCacheRead
	default:
		return tierInput
	}
}

// noteDrift logs, once, any configured name absent from the first manifest the
// plugin actually sees. A stale list costs savings rather than correctness, so
// it surfaces as a warning instead of a failure.
func (p *ToolPrune) noteDrift(observed []pipeline.InferenceTool) {
	p.driftOnce.Do(func() {
		if len(observed) == 0 {
			return
		}
		present := make(map[string]struct{}, len(observed))
		for _, t := range observed {
			present[t.Name] = struct{}{}
		}
		var missing []string
		for _, n := range p.cfg.Remove {
			if _, ok := present[n]; !ok {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			slog.Warn("tool-prune: configured tools not present in the observed manifest — list may be stale",
				"missing", strings.Join(missing, ","),
				"hint", "re-run abctl tools scan")
		}
	})
}

// Metrics implements pipeline.MetricsProvider.
func (p *ToolPrune) Metrics() []pipeline.Metric { return p.m.snapshot(&p.cfg) }
