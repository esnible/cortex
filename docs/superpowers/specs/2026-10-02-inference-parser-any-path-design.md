# Inference parser: known dialects at any path

**Date:** 2026-10-02
**Status:** implemented

## Goal

A provider that speaks a dialect `inference-parser` already reads — OpenAI chat
completions or Anthropic Messages — is parsed wherever it mounts the endpoint, with no
code change. Today each such provider needs one.

## The problem

The parser routes by exact path (`OnRequest`'s switch in `plugin.go`, and
`isAnthropicMessagesPath` in `anthropic.go`): seven OpenAI-dialect paths and three
Anthropic ones. Every provider that mounts the same API under a prefix of its own has
needed a new constant:

| Provider | Path | Added in |
|---|---|---|
| IBM Bob | `/inference/v1/chat/completions` | #903 |
| OpenCode Zen | `/zen/v1/chat/completions` | 19aba133 |
| OpenCode Zen, Go plan | `/zen/go/v1/chat/completions` | 5252f000 |
| OpenCode Zen, Anthropic dialect | `/zen/v1/messages`, `/zen/go/v1/messages` | 5252f000 |

Each unlisted one is recorded with its method and path and nothing else: no model and no
tokens, and no cost unless a gateway header reports one. OpenRouter
(`/api/v1/chat/completions`), Groq (`/openai/v1/chat/completions`), Azure OpenAI
(`/openai/deployments/<d>/chat/completions`) and the `/anthropic/v1/messages` gateways
(MiniMax, Kimi, Azure AI Foundry) are all in that state today.

## Non-goals

- **New dialects.** The Responses API (`/v1/responses`: OpenAI's GPT models, Codex,
  Zen's GPT and Grok models), Gemini's native API and Bedrock each have their own
  request and stream shapes, so each needs parser code once. Not here.
- **Tool-prune and context-guru.** Both keep their own `paths` lists. They already match
  by suffix.
- **Pricing.** Parsing yields tokens; a model with no rate stays unpriced.
- **The model named in the path.** Azure and Vertex put it there; the parser does not
  read it, so such a call parses with an empty model and stays unpriced.
- **Configuration.** No config surface is added. See *Alternatives*.

## Design

### The rule

`dialectFor(path)` decides from the end of the path alone, after `endpointPath` has
removed the query string:

| The path ends in | Dialect |
|---|---|
| `/v1/messages` | Anthropic |
| `/completions` | OpenAI |
| anything else | none: not parsed |

The Anthropic rule is checked first. The two cannot overlap, so the order only states
intent.

What it covers, by example:

| Path | Dialect |
|---|---|
| `/v1/messages`, `/zen/v1/messages`, `/zen/go/v1/messages`, `/anthropic/v1/messages` | Anthropic |
| `/v1/chat/completions`, `/chat/completions`, `/v1/completions`, `/completions` | OpenAI |
| `/inference/v1/chat/completions`, `/zen/v1/chat/completions`, `/zen/go/v1/chat/completions` | OpenAI |
| `/api/v1/chat/completions`, `/openai/v1/chat/completions`, `/openai/deployments/<d>/chat/completions`, `/v1beta/openai/chat/completions` | OpenAI |
| `/v1/messages/count_tokens`, `/v1/messages/batches` | none |
| `/v1/embeddings`, `/v1/rerank`, `/v1/responses`, `/inference/v1/model/info` | none |
| `/inference/v1/chat/completions/` (trailing slash) | none |

A trailing slash is not normalised. `TestInferenceParser_BobNonInferencePathsAreIgnored`
pins that case as not inference, and no client is known to send one.

The rule replaces the switch's path list and `isAnthropicMessagesPath`. The constants
`bobPath`, `zenPath`, `zenGoPath`, `zenMessagesPath` and `zenGoMessagesPath` are deleted.
`anthropicMessagesPath` stays, as the suffix the rule matches. The five response-side
call sites that ask `isAnthropicMessagesPath(endpointPath(pctx))` ask
`dialectFor(endpointPath(pctx))` instead. They keep their shape, and request and
response still get their answer from one function.

### The body check

Exact matching was the only thing keeping a non-inference body out of
`pctx.Extensions.Inference`. `parseOpenAIRequest` turns any JSON object into an extension
marked `IsAction: true`, and IBAC and OPA read that extension as fact. A suffix rule
matches more paths, so the parser checks the body as well:

- **OpenAI dialect:** the body is a JSON object with a `messages` array, or with a
  `prompt` field (legacy completions, `TestInferenceParser_LegacyCompletions`).
- **Anthropic dialect:** the body is a JSON object with a `messages` array.

A body that fails leaves the extension nil, which is what a non-JSON body does today, so
the existing path for it is reused unchanged: no request telemetry, and the response is
still priced from the gateway's cost header (`OnResponseFrame`'s nil-extension branch).
The check lives in `parseOpenAIRequest` and `parseAnthropicRequest`, which already return
nil for an empty or non-JSON body. It applies to every path, including the ones matched
exactly today.

`model` is not required. Azure carries the model in the path and sends a body without
one; requiring it would reject a request the rule exists to accept.

## Effects

Everything keyed on `pctx.Extensions.Inference != nil` now sees the newly parsed traffic —
OpenRouter, Groq, Azure, the `/anthropic/v1/messages` gateways — as inference. Each such
request used to be a bare method and path row.

- **IBAC.** Under `unclassified_policy: judge`, the IBAC demo's setting
  (`demos/ibac/k8s/ibac-patch.yaml`), such a request was unclassified and judged. It is now
  inference and skipped as `skip/inference_bypass` unless `judge_inference: true`
  (`core/plugins/ibac/plugin.go`). With `judge_inference: true` it is judged even under the
  default `passthrough`, which let it through before.
- **The implausible-cost cap.** `implausibleUnparsedCost` (`core/cost/settle/settle.go`)
  refuses an implausible gateway-header cost only on a response with no extension, so it no
  longer applies on these paths. A header there is bounded only by `pricing.MaxCostMicros`,
  as on any parsed path.
- **tool-prune** prunes the requests its `paths` match, where it skipped them with
  `no_inference_extension`.
- **cpex** hands its policies the request messages one part each and can write a redaction
  back into them. Before, the body crossed as one text part and a redaction failed closed.
- **sparc**, in `inference` enforcement, gates their tool calls.
- **session-budget** counts their calls and tokens.
- **`/v1/usage`** names a request with a model and no rate in `unpricedBy`. Traffic with no
  model is left out, as before, so an Azure call still is.
- **lineage** labels their spans `inference` rather than `http`, and names them by model
  where the body sends one.
- **OPA** policies that read `input.inference` see them.
- **Spend figures rise where a rate applies.** A Claude model reached through OpenRouter,
  for example, is now priced at the bundled Anthropic rates where before it cost nothing
  on the record.

None of this is a new attacker capability. An agent picks its own URLs, so
`/v1/chat/completions` on any host already got both the IBAC and the cost-cap exemption, and
`"messages":[]` passes the body check. Nothing here looks at the host; whether a host's cost
header should be believed at all is rossoctl/cortex#1027.

## Alternatives considered

- **An endpoint table in config.** Operators list `{path, dialect}`, today's list the
  default. Every new provider still needs an edit, so the work moves rather than goes.
- **Detecting the dialect from the body.** OpenAI and Anthropic requests share their
  shape (`model` plus `messages`), and the response side would have to guess again from
  the stream.
- **An optional config escape hatch** (`openai_paths` / `anthropic_paths`, added to the
  rule), and **storing the dialect per request** so config could not change between
  request and response. Dropped together: the hatch was the larger part of the change and
  no current endpoint needs it — the one example, Vertex's `:streamRawPredict`, would
  still be unpriced. The stored dialect existed only because of the hatch. Either is a
  self-contained change later, against a real case.
- **A library.** Envoy AI Gateway solves this in Go, but under `internal/`, so it cannot
  be imported. Bifrost (`maximhq/bifrost/core`, already in `core/go.mod` for context-guru)
  exports per-provider wire types and converters. Importing only its OpenAI and Anthropic
  providers grew a minimal binary from 2.0MB to 5.6MB, stripped. It is built to
  translate for a gateway it drives, not to read traffic it does not originate, and none
  of it does the path routing this change is about. It is worth revisiting for a new
  dialect, not for this.

## Testing

- **The rule:** a table test of every path in the coverage table above, both columns.
- **The body check:** no `messages`; `messages` not an array; `prompt` only; a JSON array
  or string body; each for both dialects where it applies.
- **End to end:** an OpenAI-dialect streamed response at `/api/v1/chat/completions` and a
  non-streamed one at `/openai/deployments/d/chat/completions` with no `model`, through
  `OnRequest` then `OnResponseFrame`, asserting tokens and finish reason.
- **Existing tests** pass with only these edits: references to the deleted constants
  become literals, and the comment in `TestInferenceParser_BobNonInferencePathsAreIgnored`
  that credits an exact-match switch is reworded.

## Docs

- `docs/agents/opencode.md`, *Typed inference*: "The path has to match exactly" and the
  list around it become the rule.
- `docs/plugin-catalog.md`, `inference-parser`: say which endpoints it reads.
- `core/pipeline/client.go:271`: a comment names `bobPath`.

## Delivery

One PR. The rule and the body check cannot ship apart: the rule without the check
widens what reaches IBAC and OPA, and the check without the rule changes nothing anyone
asked for.
