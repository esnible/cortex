# Directional Body Capabilities and the `tool-prune` Plugin

**Status**: Draft
**Date**: September 2026

This document specifies two changes that together let AuthBridge cut an agent's
token bill by removing tool definitions the agent never calls:

1. **A directional split of the body-write capability.** `PluginCapabilities.WritesBody`
   is renamed to `WritesRequestBody` and joined by `WritesResponseBody`. Response
   streaming is then gated on the response-side flag alone, so a plugin that only
   rewrites requests no longer forfeits incremental server-sent events (SSE).
2. **`tool-prune`**, an outbound plugin that deletes named entries from the `tools`
   array of an inference request. The list is static, produced at setup time by a
   new `abctl tools scan` subcommand that analyses local Claude Code transcripts.

Part 1 is a prerequisite for part 2 but stands on its own merits: it is a
framework correctness fix that any future request-only mutator benefits from.

## Motivation

Claude Code sends the full tool manifest on every request. On the author's machine
that manifest is roughly 20,500 tokens, and it sits at the front of the cached
prompt prefix, so it is billed on every turn of every session. Measurements across
seven developers show most of those definitions are never called even once in a
30-day window.

Removing the dead entries is a pure win: fewer prompt tokens, no behaviour change
for tools the agent actually uses. Doing it in the proxy rather than in each
client's configuration means it works for every agent behind AuthBridge without
per-client setup, and it is measurable centrally.

### Why the list is static

The `tools` array is at the front of the cached prompt prefix. Any change to it
invalidates every cache breakpoint after it, at 1.25x write cost. A plugin that
learned at runtime and revised its verdict would repeatedly bust the prompt cache
and could plausibly destroy more value than it saves. A list fixed at setup time
busts the cache exactly once, then stabilises.

This is the deciding argument for setup-time analysis over runtime learning, and
it removes the need for any persistence layer inside the plugin.

## Part 1: Directional body capabilities

### The defect

`PluginCapabilities.WritesBody` is a single boolean covering both directions.
`Pipeline.WritesBody()` (`authlib/pipeline/pipeline.go:383-390`) is a plain OR
across the chain, with no notion of direction:

```go
func (p *Pipeline) WritesBody() bool {
	for _, plugin := range p.plugins {
		if plugin.Capabilities().Normalize().WritesBody {
			return true
		}
	}
	return false
}
```

Both proxy listeners consult that predicate to decide whether an SSE response may
be relayed incrementally (`forwardproxy/server.go:383-387`,
`reverseproxy/server.go:440-454`). A plugin that rewrites only the **request** body
therefore disables **response** streaming, for a body it never touches:

```go
if isEventStream(resp.Header.Get("Content-Type")) && resp.Body != nil {
	if s.OutboundPipeline.WritesBody() {
		slog.Warn("forward-proxy: text/event-stream response with WritesBody plugin — falling back to buffered path", ...)
```

The cost is latency and feel, not correctness: the buffered path restores the body
verbatim (`reverseproxy/server.go:466`) and the `event:` line is preserved on the
re-framing path (`reverseproxy/server.go:749-754`). But the
user-visible effect is that a long completion arrives in one lump after a silent
wait instead of appearing incrementally, which is the first thing anyone notices.

The fallback is also independent of `ErrorPolicy`: `WritesBody()` asks whether any
plugin *declares* the capability, not whether it is currently *permitted* to
mutate. So a plugin running in `on_error: observe` (measure-only) loses response
streaming before it has gained anything.

Request buffering is not part of this cost. Requests are never streamed — Claude
Code sends one complete `POST /v1/messages` with a `Content-Length` — and the
request body is already read end to end before dispatch whenever any plugin
declares `ReadsBody` (`forwardproxy/server.go:256-266`, `pipeline.go:368-376`).
`inference-parser` declares it, so every request through the demo chain is already
fully buffered. A request-only mutator adds no buffering at all.

### The change

```go
type PluginCapabilities struct {
	ReadsBody bool

	// WritesRequestBody declares the plugin may call pctx.SetBody.
	WritesRequestBody bool

	// WritesResponseBody declares the plugin may call pctx.SetResponseBody.
	// Listeners fall back from incremental SSE relay to the buffered path
	// only when some plugin declares this.
	WritesResponseBody bool

	Requires    []string
	RequiresAny []string
	Description string
}
```

`Normalize()` promotes `ReadsBody` from either write flag.
`Pipeline.WritesResponseBody()` becomes the streaming predicate;
`Pipeline.WritesRequestBody()` keeps gating request propagation.

### Why rename rather than add

Adding `WritesResponseBody` alongside an unchanged `WritesBody` would default the new
field to `false`, so an out-of-tree plugin that rewrites responses would silently
start streaming and then call `SetResponseBody` after bytes had already been sent.
That converts a latency annoyance into a correctness bug, in code the change does
not touch.

Plugins are compiled into the binary — registration is via
`plugins.RegisterPlugin`, with no dynamic loading — so renaming the field gives
every such author a **compile error** instead. That is the safest available
failure: impossible to miss, trivially fixed, and it forces the author to answer
"which body?" rather than inherit a default they never considered. It also retires
the ambiguity permanently, since after the change there is no undirected option to
pick.

The rename is confined to Go source and documentation. `PluginCapabilities` has no
struct tags and is never marshalled directly; the session API defines its own
tagged wire types (`sessionapi.CatalogEntry` at `sessionapi/server.go:55-63`, and
the pipeline view whose `readsBody` field is at `:167`) and neither exposes
`writesBody`. So **no wire key
and no configuration key changes.** Capabilities are not configurable, so
`authlib/config` is untouched.

### Compatibility audit

Every in-tree plugin that declares the capability today, and what it actually does:

| Plugin | Rewrites request | Rewrites response | Evidence | After the change |
|---|---|---|---|---|
| `context-guru` | yes | **no** | `contextguru/plugin.go:160`; no `SetResponseBody` call anywhere | `WritesRequestBody` — **gains** response streaming |
| `sparc` | yes | yes | `sparc/plugin.go:214`; `sparc/respond.go:111,122` | both flags — unchanged |
| `cpex` | yes | yes | `cpex/plugin.go:122`; `cmf_body.go:609`, `cmf_a2a.go:216`, `cmf_inference.go:218` | both flags — unchanged |
| `tool-prune` | yes | no | new | `WritesRequestBody` — streams |

Two of the three genuinely need the buffered path and keep it. Only `context-guru`
changes behaviour, and only by regaining streaming it never needed to lose.

`validateCapabilities` (`pipeline.go:549-573`) becomes direction-aware, but the
**outcome is identical for every configuration that exists today**: all three
current plugins write requests, so they remain mutually exclusive exactly as
before. The reader-ordering rule stays triggered by either write flag, so no
configuration that validates today starts failing and none that fails starts
passing.

### One adjacent fix

`cloneCatalog` (`plugins/registry.go:202-222`) copies capability fields one at a
time, so any field added to `PluginCapabilities` is silently dropped from
`/v1/plugins`:

```go
Capabilities: pipeline.PluginCapabilities{
	ReadsBody:   caps.ReadsBody,
	WritesBody:  caps.WritesBody,
	Description: caps.Description,
	Requires:    append([]string(nil), caps.Requires...),
	RequiresAny: append([]string(nil), caps.RequiresAny...),
},
```

Replace the field-by-field construction with a struct copy plus explicit slice
reallocation, which preserves the deep-copy guarantee and picks up future fields
automatically:

```go
c := caps
c.Requires = append([]string(nil), caps.Requires...)
c.RequiresAny = append([]string(nil), caps.RequiresAny...)
```

### Documented contract fix

`SetBody`'s godoc (`pipeline/context.go:390-396`) states that a plugin without the
write capability which calls `SetBody` mutates only the in-memory context and
leaves the wire unchanged. The code does not do this: `SetBody` sets
`c.bodyMutated = true` unconditionally outside observe mode (`context.go:424`), and
the listeners gate purely on `pctx.BodyMutated()` (`forwardproxy/server.go:335`,
`reverseproxy/server.go:358`). An undeclared mutation therefore does reach the
wire.

This proposal does not add the missing enforcement — doing so silently would break
any plugin currently relying on the actual behaviour. It corrects the comment to
describe what the code does, and notes the divergence so a future change can close
it deliberately. Left as documented, it is a live trap: it makes "just don't
declare the capability" look like a legitimate way to keep streaming.

## Part 2: The `tool-prune` plugin

### Behaviour

One registration, `plugins.RegisterPlugin("tool-prune", ...)`:

```go
pipeline.PluginCapabilities{
	WritesRequestBody: true,
	RequiresAny:       []string{"inference-parser"},
	Description:       "Removes unused tool definitions from inference requests",
}
```

On each outbound request:

1. Skip unless the path matches `paths` (default `/v1/chat/completions`,
   `/v1/completions`, `/v1/messages`), matched by suffix as `context-guru` does.
2. Read the parsed manifest from `pctx.Extensions.Inference.Tools`.
3. For each configured name present in the manifest, delete its element from the
   `tools` array of the **original** request bytes with `sjson.DeleteBytes`,
   iterating indices in descending order so earlier deletions do not shift later
   ones.
4. Call `pctx.SetBody` once with the result.

Every byte outside the deleted array elements is unchanged. `gjson`/`sjson` are
already in `authlib/go.mod` (currently indirect), so no new dependency.

Any error or panic fails open: the original body is forwarded unmodified. A
cost optimisation must never be able to break a request.

### Configuration

```yaml
pipeline:
  outbound:
    plugins:
      - inference-parser
      - mcp-parser
      - a2a-parser
      - name: tool-prune
        on_error: observe        # measure only; switch to enforce when trusted
        config:
          remove: [NotebookEdit, ScheduleWakeup, TaskOutput]
```

`remove` is the complete verdict. There is no learning, no state, and no storage
dependency.

### Measure-only mode comes from the framework

`on_error` is a per-plugin policy already parsed by `authlib/config`
(`config.go:257`, values `enforce | observe | off`). Under `observe`, `SetBody` is
a no-op on bytes but still records a modify `Invocation` with `Shadow=true`
(`context.go:397-421`), so "would have removed" is countable without changing a
single request. `off` skips dispatch entirely.

This is why one registration suffices: the same plugin code serves measure and
enforce, selected by one word of configuration. `context.go` states the intent
directly — "Plugin code therefore looks identical under enforce and observe."

Off-by-default is satisfied structurally: the plugin is absent from the shipped
pipeline list until a user adds it, and the documented first step adds it with
`on_error: observe`.

### Where the list comes from: `abctl tools scan`

A new subcommand ports the discovery core of `claude-tool-audit.py` (about 40 of
its 814 lines) into Go:

- Read `~/.claude/projects/**/*.jsonl`.
- Hot-path line filter on the literal `"tool_use"` before any JSON parsing.
- Deduplicate tool calls by the unique `tool_use` block id.
- Window to the last `--days` (default 30).

`abctl` currently has no subcommand dispatch — `main.go` parses two flags and
launches the terminal UI. The change checks for a non-flag first argument before
`flag.Parse()` and dispatches, falling through to the UI otherwise.

```
abctl tools scan [--days 30] [--keep Name,Name] [--write <config.yaml>]
```

Without `--write` it prints the YAML block. With `--write` it patches the
`remove:` list of the `tool-prune` entry in place, idempotently.

### The offered-set problem, and how the scan stays safe

Transcripts record tools that were **called**, never tools that were **offered**.
This is structural, not a defect: a configured-but-never-invoked tool leaves no
trace. Two consequences:

- Tools never called in the window but bundled in the known Claude Code tool set
  are the removal candidates, and they are where most of the 20,500 tokens sit.
- A tool name the scan has never heard of is **kept**. Removing a tool the model
  needs is the harmful direction of failure; carrying a few extra definitions is
  not.

The bundled set is version-sensitive: developers on newer Claude Code releases
produced tool calls the current table does not recognise. Two mitigations:

1. Unknown names are always kept, so drift costs savings, never correctness.
2. At startup the plugin compares its configured `remove` list against the names
   it observes in `ext.Tools` and logs any configured name that never appears, so
   a stale list surfaces as a warning rather than a silent no-op.

A `--keep` flag and a small "implies" table cover tools whose use is indirect —
for example `Agent` implying `SendMessage`, which a transcript may not show being
called directly.

### Installation flow

`install-demo.sh` already downloads both binaries with checksum verification and
prints next steps. `authbridge-proxy` writes `cortex-ca/demo.yaml` on first run
(`cmd/authbridge-proxy/demo.go`), and that file is hot-reloaded — its own header
says so — so the list can be filled in without a restart.

- `demoConfigYAML()` gains the `tool-prune` entry with `on_error: observe` and an
  empty `remove: []`.
- `install-demo.sh` runs `abctl tools scan --write` when the config already
  exists, and otherwise prints the block in its next-steps output alongside the
  existing "Watch traffic" hint.

### What the user sees in-session

Claude Code's `/context` breakdown has `System tools`, `Tool schemas`, `MCP tools`,
`Custom agents`, `Memory files` and `Free space` line items — confirmed by string
inspection of the installed 2.1.257 binary. **It will not show this saving.** It is
a client-side pre-flight breakdown of what the CLI assembled; it necessarily
computes `Free space` itself, and the pruning happens downstream. This is the
first place a user would look, and it must be documented as unaffected.

What does move is `/cost` and any figure derived from the API response `usage`
block: the server bills the request it received, so `input_tokens` and
`cache_read_input_tokens` genuinely drop.

The honest limit: proxy-side pruning saves money but does **not** return context
window to the user. The client still believes it sent the full manifest, so
auto-compact triggers at the same point. Recovering headroom requires client-side
configuration (`--allowedTools`, disabling unused MCP servers). AuthBridge's
advantage is the complement — it applies to every agent behind it with no
per-client change, and it measures.

## Delivery

Three commits, sequenced so the regression argument survives review.

1. **Mechanical rename.** `WritesBody` to `WritesRequestBody` across 107
   references in 28 files (Go and documentation), with no semantic change.
   Reviewable as a single token substitution, and every existing test passing
   still carries meaning because nothing but the name moved.
2. **The split.** Add `WritesResponseBody`; declare it on `sparc` and `cpex`;
   point both listener branches at `Pipeline.WritesResponseBody()`; convert
   `cloneCatalog` to a struct copy; correct the `SetBody` godoc; add tests.
3. **`tool-prune`.** Plugin, `abctl tools scan`, `demoConfigYAML()` entry,
   `install-demo.sh` wiring, and documentation.

### Testing

For part 1, the primary regression argument is that the existing body-capability
tests pass with only the identifier renamed. On top of that:

- A truth table for `Pipeline.WritesResponseBody()` across the four plugin shapes
  (request-only, response-only, both, neither).
- Listener tests: an SSE response with a request-only writer in the chain relays
  incrementally; with a `sparc`-shaped or `cpex`-shaped chain it still buffers.
- A reflection-based `cloneCatalog` round-trip that fails if any future
  capability field is dropped.
- `validateCapabilities` table assertions covering the current plugin
  combinations, to show acceptance and rejection are unchanged.

For part 2:

- Byte-level assertions that pruning a manifest leaves every other byte of the
  request identical, including key order and whitespace.
- Descending-index deletion verified against a manifest where a naive ascending
  loop would delete the wrong elements.
- Names absent from the manifest are ignored without error.
- Malformed and truncated bodies fail open, forwarding the original bytes.
- Under `on_error: observe`, the body is unchanged and a `Shadow=true` invocation
  is recorded.
- Scanner tests over fixture transcripts: window boundaries, `tool_use` block
  deduplication, unknown names retained, `--keep` honoured.

### Risks

| Risk | Mitigation |
|---|---|
| Removing a tool the agent needs | Unknown names always kept; `--keep` override; ship with `on_error: observe`; fail open on any error |
| Stale bundled tool set as Claude Code evolves | Drift reduces savings only; plugin warns on configured names never observed in `ext.Tools` |
| One-off prompt-cache invalidation when the list changes | Inherent and bounded: static list means it happens once, then the prefix is stable |
| Commit 1 conflicts with in-flight branches declaring `WritesBody` | One-line fix per branch; the compile error makes it self-evident |
| `context-guru` regaining response streaming exposes a latent bug in that path | Covered by the listener tests above; the path is already exercised by chains with no body writer |

## Open questions

None blocking. Two items deliberately deferred:

- Adding the missing enforcement so `SetBody` matches its documented contract.
  Needs its own compatibility review.
- Surfacing per-plugin savings in `abctl`. The in-session `/cost` readout covers
  the first cut; a `Metrics:` section in the existing plugin detail pane is a
  natural follow-up if operators want it centrally.
