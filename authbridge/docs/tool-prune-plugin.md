# `tool-prune` plugin

Removes unused tool definitions from outbound inference requests.

A Claude Code turn carries the full tool manifest on every request — tens of
thousands of tokens of JSON schema, billed each time, largely for tools the
agent will never call in a given deployment. The manifest is assembled by the
client, so the proxy is the only place to trim it without changing every client.

The verdict is entirely configuration. `remove` names the tools to drop; there
is no learning, no state and no storage dependency. `abctl tools scan` proposes
a list, but the plugin only ever does what it was told.

## Configuration

```yaml
pipeline:
  outbound:
    plugins:
      - inference-parser
      - mcp-parser
      - name: tool-prune
        on_error: observe        # measure only; switch to enforce when trusted
        config:
          remove: [NotebookEdit, ScheduleWakeup, TaskOutput]
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `remove` | `[]string` | `[]` | Tool names to delete. Names absent from a given request are ignored. |
| `paths` | `[]string` | `/v1/chat/completions`, `/v1/completions`, `/v1/messages` | Request paths to act on, matched exactly or by suffix. |

**Placement matters.** `tool-prune` requires `inference-parser` earlier in the
chain, and because it rewrites the request body it must sit *after* every
body-reading plugin — readers have to see the original bytes. `pipeline.New`
enforces both and fails at startup rather than misbehaving quietly.

It declares `WritesRequestBody` only, never `WritesResponseBody`, so responses
still stream incrementally. See
[`plugin-reference.md`](./plugin-reference.md#capability-fields).

## Turning it on

**The empty `remove` list is the off switch.** With no tool named the plugin does
nothing, whatever the policy, so filling the list is the single act that enables
it:

```sh
abctl tools scan --write ./cortex-ca/demo.yaml
```

The config is hot-reloaded, so no restart. A reload does rebuild the plugin and
therefore **resets its counters** — the same as a process restart.

### Measure instead of enforce, when you want to

`on_error: observe` turns the plugin into a projection: it computes exactly what
it would remove and counts it, while every byte on the wire stays untouched.
Nothing in the plugin differs between the modes — under observe `SetBody` is a
no-op on bytes and leaves `BodyMutated()` false, which is how it knows which
counter to increment.

Two occasions worth it:

- **Sizing the change** before it affects anything: read `bytes removed` and
  `tokens saved / request`, decide, then remove the line.
- **Clearing the plugin of suspicion.** If requests start failing and you are
  not sure whether this is the cause, set `observe` and watch: the bytes are then
  provably unmodified, so a failure that persists is not this plugin. That is
  faster than reasoning about it, and it costs no configuration.

## Reading the metrics

`abctl`'s plugin detail pane shows a `Metrics:` section (source:
`GET /v1/pipeline`):

```
Metrics:
  requests seen                      2  count
  requests pruned                    2  count
  tools removed                     22  count
  bytes removed                 57,136  bytes
  bytes removed / request       28,568  bytes
  tokens saved: cache write     13,044  tokens   estimate, n=2
  tokens saved: cache read      13,064  tokens   estimate, n=2
  $ saved                       0.2642  usd      estimate, n=2
  $ saved / request             0.1321  usd      estimate, n=2
  removed: NotebookEdit              2  count
```

In observe mode `requests projected` replaces `requests pruned`, so a
projection is never mistaken for a realised saving.

### Why tokens are reported per tier and never summed

Byte counts are exact. Tokens are an estimate, and — more importantly — they
are **not fungible**. Providers price prompt tiers very differently: Anthropic
charges 1.25x the input rate for a cache write and 0.1x for a cache read, so the
same pruned bytes are worth more than **12x** more on a cache miss than on a
cache hit.

A single "tokens saved" figure would invite multiplying by one rate, which is
wrong by that factor. So the saving is attributed to the tier it actually came
out of and reported separately. The tool manifest sits inside the cached prefix
(Claude Code puts `cache_control` on the tool block), so a cache-miss request
saves cache-*write* tokens and a hit saves cache-*read* tokens. Traffic that
alternates shows both rows, and the honest headline is a range rather than a
point.

The bytes-to-tokens ratio is calibrated on your own traffic — prompt tokens over
request bytes for the same request, both post-pruning so the two sides agree —
rather than bundling a tokenizer or assuming a constant.

### Costing it

No price is assumed. Set any of these and the `$` rows appear; leave them and
the row says so instead of inventing a figure:

| Field | Meaning |
|---|---|
| `input_cost_per_token` | USD per uncached input token |
| `cache_write_cost_per_token` | USD per cache-write token; defaults to the input rate |
| `cache_read_cost_per_token` | USD per cache-read token; defaults to the input rate |

Field names and semantics match
[`litellm-budget-track`](./plugin-catalog.md#litellm-budget-track), so rates are
configured once in a familiar shape. There is deliberately no output rate:
pruning only ever shrinks the prompt, so attributing output cost to it would be
false.

If your gateway reports authoritative per-request cost (LiteLLM's
`x-litellm-response-cost`), `litellm-budget-track` is the plugin that consumes
it; this one prices from rates because a saving is a counterfactual — the cost
of a request that was never sent.

Counters are in-memory and per-process. That is the right trade for the
single-laptop case this targets and what keeps the plugin free of a storage
dependency; fleet aggregation belongs on the stats server later and would not
change the plugin.

## Where the list comes from

```
abctl tools scan [--days 30] [--keep Name,Name] [--dir PATH] [--write CONFIG]
```

It reads `~/.claude/projects/**/*.jsonl`, deduplicates tool calls by their
unique `tool_use` block id (a transcript is rewritten on every resume, so raw
occurrences would inflate heavily-resumed sessions), and windows to the last
`--days`. Without `--write` it prints the YAML block; with `--write` it patches
the `remove:` list of the `tool-prune` entry in place, idempotently and without
reformatting the rest of the file.

**The offered-set problem.** Transcripts record tools that were *called*, never
tools that were *offered*. This is structural, not a defect: a
configured-but-never-invoked tool leaves no trace. Two consequences:

- The removal candidates are tools abctl knows Claude Code ships that you never
  called — which is also where most of the wasted tokens sit.
- A tool name the scan has never heard of is **kept**. Removing a tool the model
  needs is the harmful direction of failure; carrying a few extra definitions is
  merely expensive. Drift in the known-tool table costs savings, never
  correctness.

A `--keep` flag and a small implies table cover tools whose use is indirect —
`Agent` implying `SendMessage`, say, which a transcript may never show being
called by name. At runtime the plugin also logs, once, any configured name
absent from the first manifest it sees, so a stale list surfaces as a warning
rather than a silent no-op.

## Failure behaviour

Every error path forwards the original bytes unmodified: the plugin fails open on
a malformed or truncated body, an unparseable manifest, a rewrite that does not
shrink the body, a rewrite that produces invalid JSON, an unexpected tool count
afterwards, and any panic.

**What that does and does not promise.** It means the plugin's own failure modes
cannot break a request — a bug or a surprising input forwards the original bytes
rather than a damaged rewrite. It does **not** promise that a validly pruned
manifest is acceptable to every provider or gateway in front of one. Pruning
changes the request, so if a provider rejects a request for a reason the plugin
cannot see, `on_error: observe` is how you find out safely: it counts what it
would remove while sending the bytes untouched.

Three specifics worth knowing:

- **A forced `tool_choice` is never pruned.** `tool_choice: {"type":"tool",
  "name":"X"}` (or OpenAI's `{"type":"function","function":{"name":"X"}}`) makes
  `X` mandatory; a `tool_choice` naming a tool absent from the manifest is an
  invalid request. `X` is kept even when the remove list names it, and the rest
  of the list still applies.

- **Nothing else in the request changes.** Deletions are surgical: every byte
  outside the removed array elements is preserved, including key order and
  whitespace.
- **Removing every tool drops the keys.** An empty `tools: []` is not a safe
  output — OpenAI rejects it, and rejects `tool_choice` without `tools` — so an
  over-broad list removes both keys instead of emptying the array.

## What the saving does and does not change

`/cost` and anything derived from the API response `usage` block **do** move:
the server bills the request it received, so `input_tokens` and
`cache_read_input_tokens` genuinely drop.

Claude Code's `/context` breakdown **does not**. It is a client-side pre-flight
view of what the CLI assembled, and it computes `Free space` itself; the pruning
happens downstream. This is the first place anyone looks, so it is worth stating
plainly: proxy-side pruning saves money but does not return context window. The
client still believes it sent the full manifest, so auto-compact triggers at the
same point. Recovering headroom needs client-side configuration
(`--allowedTools`, disabling unused MCP servers). AuthBridge's advantage is the
complement — it applies to every agent behind it with no per-client change, and
it measures.

One further caveat on the list changing: a new `remove` list invalidates the
prompt-cache prefix once. That is inherent and bounded — the list is static, so
it happens on the change and then the prefix is stable again.

## Build tag

Compiled in by default; exclude with `-tags exclude_plugin_toolprune`. The
`authbridge-lite` image excludes it along with the other non-auth plugins.
