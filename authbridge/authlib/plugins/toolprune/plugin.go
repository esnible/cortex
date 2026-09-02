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
}

func (c *config) applyDefaults() {
	if len(c.Paths) == 0 {
		c.Paths = append([]string(nil), defaultPaths...)
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
func (p *ToolPrune) gated(path string) bool {
	for _, s := range p.cfg.Paths {
		if path == s || strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
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
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "path_not_inference"})
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

// OnFinish calibrates the bytes-to-tokens ratio on the operator's own traffic,
// rather than bundling a tokenizer or hardcoding a constant. inference-parser
// is a StreamingResponder, so RunResponse skips its OnResponse — OnFinish is
// the hook where response-derived usage is reliably available.
func (p *ToolPrune) OnFinish(_ context.Context, pctx *pipeline.Context) {
	if pctx.Extensions.Inference == nil {
		return
	}
	prompt := pctx.Extensions.Inference.PromptTokens
	if prompt <= 0 || len(pctx.Body) == 0 {
		return
	}
	p.m.observeUsage(prompt, len(pctx.Body))
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
func (p *ToolPrune) Metrics() []pipeline.Metric { return p.m.snapshot() }
