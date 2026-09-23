# Session prompt-context figure — design

Date: 2026-09-23
Status: approved, not yet implemented

## Problem

abctl's `CONTEXT(1M)` column presents itself as a fact about a session — how full that
conversation's context is. It actually reports **whether abctl happened to be watching when
that session last spoke.**

The gauge folds off `m.events`, which has exactly two writers: the live `/v1/events` stream
(only traffic since this abctl process attached) and a per-session snapshot issued only when
the operator presses Enter on a row. `/v1/sessions` carries no per-request prompt-token
field, so a listed row cannot answer on its own. Consequences:

- every session idle since before abctl started shows an em dash
- restarting abctl silently blanks the column for every row
- the same session, same server-side data, reads as a gauge or a dash depending on when a
  client was launched

This has been reported twice as a rendering bug. It is not one: the column answers a
different question than its heading asks, and the answer changes when a client restarts.

### The data is not missing

`Store.ListSessions` already walks `sess.Events` end to end for every row, to compute
`TotalTokens`, and discards everything else it passes. The events that hold the answer are
already traversed on every poll. The gap is in `SessionSummary`, not in the server.

### What PR #1102 did and did not fix

[#1102](https://github.com/rossoctl/cortex/pull/1102) fixed a real defect: three handlers
that reach `rebaseSessionContext` updated `contextRun` and repainted nothing, so even the one
path that *does* fill a gauge took up to 2s (`refreshInterval`) to show it. That stands and is
not superseded here — `rebuildSessionsTable` is what paints this figure too, and the three
repaint sites still cover the open-a-session paths against proxies that predate this change.

But it made the workaround prompt; it did not remove the need for a workaround. This design
does.

## Approach

Compute the figure **server-side, maintained incrementally on `Store.Append`**, and publish it
on `SessionSummary`. Rejected alternative: a client-side prefetch of timelines for listed
sessions, which costs N round trips, grows `m.events` without bound, and still shows nothing
until the prefetch lands.

### Not in `ListSessions`

Folding inside `ListSessions` is the mistake this file already made and wrote up. `cost` and
`avoided` were originally summed on demand there; the `entry` doc records why that was a
regression — `O(events)` per session under the read lock, on abctl's 2s timer, in front of a
lock whose writer side is `Store.Append` on the proxy's request path, with `maxEvents` unset by
default so the event list is uncapped.

`sumTokens` surviving there is tolerance for an existing walk, not licence to add a second.

## Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | Figure resets on proxy restart | Matches `CostMicros`; needs no new storage. abctl restarts many times a day, the proxy rarely, so this removes the dashes actually seen. |
| 2 | Client merges rather than preferring one source | Neither source dominates: the server has seen everything since the proxy started, abctl only since it attached — but abctl's copy survives a proxy restart, which is the whole of #870. |
| 3 | One PR, four ordered commits | Reviewability without the process cost of separate PRs; `git bisect` stays useful. |
| 4 | Duplicate test fixtures, no shared helper package | See "Fixtures" below. |
| 5 | Publish a LOSSLESS mergeable object, not a bare int | A bare `max` cannot honour "stated beats unstated"; see "The hole in max". |
| 6 | Make the fold commutative | Replay order stops mattering; `MergePromptContext` becomes associative. |
| 7 | Design for persistence, document it, build none | No serialization code, no disk format, no flags. |

## §1 Where the fold lives

**`authlib/pipeline`, at zero new dependency cost.** Measured:

- `authlib/session/store.go:14` already imports `authlib/pipeline`
- `cmd/abctl/tui` already imports it
- `pipeline` imports only `authlib/contracts`, so no cycle is possible

A separate `authlib/contextfold` would add a dependency edge from `session` and buy nothing.
The rule *is* inference semantics: it reads `InferenceExtension.AgentRole`, the tool manifest,
and the prompt counts, all of which `pipeline` already owns.

### What moves

From `cmd/abctl/tui/sessions_context.go` lines 12-235 (~224 lines):

| today | becomes |
|---|---|
| `contextRun` | `pipeline.PromptContextFold` |
| `foldSessionContext(events, run)` | `(*PromptContextFold).Add(*SessionEvent)` and `.AddAll([]SessionEvent)` |
| `sessionContext(events) int` | `pipeline.PromptContextOf([]SessionEvent) int` |
| `toolCount` / `messageCount` | unexported in `pipeline`, unchanged |

The per-event `Add` is what the server needs: `Append` has one event and no slice. abctl's
`rebaseSessionContext` still wants whole-slice, so `AddAll` stays.

### What is new, in commit 2 rather than the move

| symbol | purpose |
|---|---|
| `PromptContext` | the publishable projection — §4 |
| `(*PromptContextFold).Publish() *PromptContext` | project for the wire; **nil when `Tokens == 0`**, so "nothing can be said" stays distinct from a real figure |
| `MergePromptContext(a, b *PromptContext) *PromptContext` | combine two published values — §5 |

`Publish` returning a pointer is what makes `omitempty` behave: a zero fold must serialize as an
absent field, not as `{"tokens":0,...}`.

### What stays in tui

`contextGauge` (rendering), `sessionContextFor` / `rebaseSessionContext` (model-state caching),
`cachedMarker`, the gauge width tests, the two table tests, and **all 94 lines of fixtures with
all 80 call sites untouched**.

`sessions_context_bench_test.go` stays too: `BenchmarkSessionContextPerEvent` measures the
*model's* caching, not the rule.

### Naming

`PromptContextFold`, not `ContextFold` — "context" in a Go codebase reads as `context.Context`,
and the figure is specifically prompt tokens.

## §2 `entry.context` and the `Append` hook

```go
// context is the CONTEXT gauge's answer for this session: the largest main-agent prompt
// total seen, maintained by Append and read by ListSessions. See
// pipeline.PromptContextFold for the rule, and why a REMEMBERED MAXIMUM rather than a sum
// over Events.
context pipeline.PromptContextFold
```

48 bytes per session (`tokens int`, `msgs int`, `at time.Time`, `stated bool`, padded) — fixed,
**not per event**.

One line inside the lock, in the lockstep block at `store.go:296-300`:

```go
sess.Events = append(sess.Events, event)
sess.money = append(sess.money, money)
sess.cost.Add(money.cost)
sess.avoided.Add(money.avoided)
sess.context.Add(&event)    // <- here
```

### No pre-lock hoist

`moneyOf` is hoisted above the lock because it is a `json.Unmarshal` — that hoist exists
because of a measured regression. The fold is a phase check, a nil check, `len(Tools)`, three
int adds, a string read and one comparison: tens of nanoseconds, no allocation, **no decode**.
Splitting it into a pre-lock candidate extraction plus an in-lock merge would add an exported
type to `pipeline` for plumbing alone and save nothing measurable.

This needs a comment saying so. The `moneyOf` comment is categorical — "it touches no store
state, so it has no business inside a critical section" — and invites the reader to assume the
pattern was forgotten here.

### Placement is load-bearing

The hook **must precede the trim block** (line 315). An event appended and immediately trimmed
still has to contribute, because the figure is a remembered maximum that outlives the events it
was read from. Line 300 gets that for free, but by placement rather than by accident, so it
gets a comment.

### Ordering against the interner is safe

`InternEvent` runs at line 293 and mutates strings in place. The fold reads `len(Tools)`, token
ints, `AgentRole` and `At`. The interner clones the `Tools` array per event specifically to
avoid aliasing the response event, and a clone preserves length. Folding after interning reads
the same values.

### Rejected: a `Recorder`

The store has an extension point for side-channel aggregation that runs under the write lock.
Wrong fit — `ListSessions` needs this figure *per session, at read time*, so a `Recorder` would
need its own `map[string]fold` plus its own locking, duplicating what `entry` gives free.

## §3 Trim invariance

**A trim does nothing to `sess.context`.** Zero lines added to the trim block at
`store.go:315-332`. No subtraction, no parallel slice, no `applyTrim` participation.

This is principled, and the reason is **max versus sum**. `cost` is a sum, and a sum over a
trimmed slice understates by a known amount — honestly describable as "the cost of the events
in this session". Context is a maximum, and a maximum over a trimmed slice does not
understate, it **lies about the thing it names**: it reports a small conversation when the
conversation is large and merely aged out.

### What that saves

`cost` needed the entire `money []eventMoney` sidecar — 16 bytes per event, held forever —
solely to make a trim decode-free. The context figure needs **no per-event storage at all**.

### The test is the inverse of `cost`'s invariant

`TestAppend_RunningTotalsMatchAFullRecomputation` asserts running totals equal a full
recomputation over `Events`. The analogous assertion here would be **actively wrong**: after a
trim, the figure may legitimately exceed anything recomputable from the surviving events.

```
TestAppend_PromptContextSurvivesATrim
  maxEvents small enough to force a trim
  append the winning main-agent turn, then enough one-shots to evict it
  assert: the figure still reports the winning turn
  assert explicitly: it does NOT equal a recomputation over surviving Events
```

The second assertion forbids a specific future "fix". The instinct to match `cost` — making the
figure recomputable from `Events` — would reintroduce exactly the bug this design removes. The
test must make that fail loudly rather than look like a consistency improvement.

### Whole-entry removal is correct as-is

`evictOldestLocked` (`maxSessions`) and `cleanupLocked` (TTL) delete the entry, so the fold goes
with it — a forgotten session has no figure. A session recreated under the same ID starts a
fresh entry and a fresh fold, consistent with `nextSeq` restarting at 1 in that case. See §5
for how the client resolves that against its own retained figure.

## §4 The wire representation

`SessionSummary` is declared once at `store.go:563` and **reused** by `sessionapi/server.go:335`
and `apiclient/client.go:156`. No DTO duplication: one edit, both ends.

```go
// PromptContext is the fold's publishable state: enough to MERGE two of them, which is what a
// client holding its own figure must do, and what a future replay-then-continue restore would
// do. See pipeline.MergePromptContext.
type PromptContext struct {
	Tokens int       `json:"tokens"`
	Stated bool      `json:"stated"` // which rule produced it; stated always beats unstated
	At     time.Time `json:"at"`     // the winning turn's arrival, for latest-wins
	Msgs   int       `json:"msgs"`   // the unstated rule's comparator; see below
}
```

**LOSSLESS**, carrying every field the fold's ordering reads. An earlier draft of this design
dropped `Msgs`; Task 6 disproved the reason for dropping it — see "Msgs is published" below.

On `SessionSummary`:

```go
// PromptContext is how full this session's conversation got. Nil when nothing can be said.
//
// LIFETIME-MAX, NOT SCOPED TO WHAT THE STORE HOLDS — unlike TotalTokens and CostMicros above,
// and deliberately. Those are sums; a sum over a trimmed slice understates by a known amount.
// This is a maximum; a maximum over a trimmed slice reports a small conversation when the
// conversation is large and merely aged out. A client showing them on one row must not present
// any of the three as a check on another.
//
// A POINTER, so absent and zero stay distinguishable: a session with only one-shot calls has
// no conversation to measure and must render as "—" rather than as a real figure. Same
// standing rule CostMicros states for omitempty.
PromptContext *PromptContext `json:"promptContext,omitempty"`
```

`ListSessions` gains one line — `PromptContext: sess.context.Publish()` — and stays `O(1)` in
this dimension.

### `Msgs` is published, and the reason it was not is false

The first draft omitted `msgs` on two grounds, both of which Task 6 disproved by reading
`authlib/plugins/inferenceparser/subagent.go`:

> *agentRole reports which caller under one client session made a request... **Empty when the
> request says nothing, which is every client that is not Claude Code.***

`agentRole()` returns `""` when there is no system message at all (`/v1/completions`), when the
first system line lacks the required `x-anthropic-billing-header:` prefix, and on an unparseable
body. So:

1. **"That path exists only for proxies that do not send this field" was wrong.** Unstated figures
   are a **live, current-proxy state** for any non-Claude-Code client. The unstated-versus-unstated
   merge arm is a real path, not a version-skew relic.
2. **"`msgs` is slated for deletion once the proxy floor publishes agentRole" was wrong.** `Stated`
   depends on the **client**, not the proxy version, so the fallback comparator is permanent. (That
   claim is inherited from the pre-existing `sessions_context.go` doc comment, so it is a wrong
   statement already in the tree rather than one this design introduced — but it is wrong, and
   anything relying on it should stop.)

With the field permanent, omitting it bought nothing and cost exactness. Publishing it makes the
projection **order-equivalent to the fold** instead of a coarsening, which:

- collapses the "two combines over two different total orders" construct into one — a construct
  that had already produced two reasoning errors in this design
- makes the projection lossless, so persistence can restore from it directly rather than needing
  the private fold (see *Future: persistence*)

Cost: one int on the wire.

### The client needs no version detection

| proxy | session | wire | client sees |
|---|---|---|---|
| old | anything | field not in struct | `nil` |
| new | only one-shot calls | omitted | `nil` |
| new | has agentic turns | `promptContext: {...}` | the object |
| new, just restarted | had turns pre-restart | omitted | `nil` |

Rows 1, 2 and 4 are indistinguishable and **all want identical handling**: the server has said
nothing, so the client's own figure stands. `nil` is a valid `MergePromptContext` operand, so this needs no
capability probe and no version compare — unlike `m.serverProjects`, which abctl must learn by
echo for the timeline projection.

## §5 The hole in `max`, and the merge that closes it

The rule is explicit: *"one stated turn settles it. The first one takes the column outright,
**however much longer an unstated predecessor was**, because a figure chosen by a rule that
cannot see subagents is not evidence about the conversation."*

A bare `max` over token counts cannot honour that, and the bad case is **reachable**:

1. abctl attaches to a proxy predating `agentRole`, folds unstated turns, lands on the
   documented stale-fallback figure — 700k held from before a compaction
2. the proxy is upgraded; abctl keeps running (`contextRun` outlives everything but a pod
   switch)
3. the new proxy publishes a *stated* 200k — the correct latest main-agent turn
4. `max` → **700k**, the unsound figure, permanently

Narrow, but it survives to production precisely because it needs a version transition to
trigger. Hence the object.

### One order, two views

An earlier draft had **two** combining operations over two different total orders, because the
projection dropped `msgs`. Publishing `Msgs` (§4) removed the difference, and with it a construct
that had already caused two reasoning errors in this design.

There is now **one** total order:

```
stated ≻ unstated                      a figure from a rule that cannot see subagents is not
                                       evidence about the conversation, at any magnitude
  within stated:    order by (At, Tokens, Msgs)
  within unstated:  order by (Msgs, At, Tokens)
```

`MergePromptContext` does not re-implement it. It projects each wire value back onto the fold's
comparison type and defers to the same `better()` the fold uses:

```go
func MergePromptContext(a, b *PromptContext) *PromptContext {
	// nil is the identity, in either position
	...
	if better(b.candidate(), a.candidate()) {
		return b
	}
	return a
}
```

**Two hand-written copies of one total order is the drift risk this design rejects elsewhere**, so
they are not two copies. `TestMergePromptContext_IsTheSameOrderAsTheFold` pins the equivalence
over an exhaustive 7×7 cross-product — that `MergePromptContext` agrees with `better` on every
pair, and that `Publish().candidate() == f.current()` — so it is checked rather than trusted.

### It is a monoid

Ties resolve by an appended comparator instead of by arrival order, which turns the rule from a
sequential latch into a **max over a total order**. A max over a total order is commutative and
associative, so the combine is a monoid with `nil` (and the zero fold) as its identity, and replay
order never matters. The `stated` latch becomes a dominance relation rather than a one-way switch.

`Add(e)` is that same combine against a candidate extracted from one event.

The two formulations agree on every input **except ties that the old form resolved by arrival
order** — a stated pair sharing a timestamp, or an unstated pair equal on both message count and
timestamp. In both arms the new comparator is *appended* to the old one, not substituted for it.

That distinction was wrong in the first draft of this spec and Task 4 caught it: the draft gave
the unstated order as `(msgs, Tokens)`, replacing the original's timestamp comparator instead of
appending to it. The consequence was not hypothetical. A `view=summary` timeline that projects
without `MessageCount` ties **every** candidate at `msgs == 0`, so the second comparator decides
the entire answer — and "largest context wins" there pins the pre-compaction figure, which is
precisely the stale-figure failure this column exists to avoid. Corrected to `(msgs, At, Tokens)`,
which also restores the mutation coverage that
`TestSessionContextFor_ATieAcrossARebaseKeepsTheLaterTurn` provides for the `at` comparator.

The remaining delta is confined and tested, and it is why the reformulation is its own commit
rather than part of the move.

### Client wiring

`rebuildSessionsTable` already holds the summary at the call site (`sessions_pane.go:277`), so:

```go
m.sessionContextFor(s.ID, s.PromptContext)   // returns Merge(server, contextRun[id]).Tokens
```

Cached-only rows (line 318) pass `nil` — no summary exists, so abctl's figure stands alone.

**What changes on screen:** a row idle since before abctl attached shows a gauge on the first
`/v1/sessions` poll. No Enter, no snapshot, no wait.

The §3 eviction case now resolves principledly: the server evicts under `maxSessions` pressure
and recreates with a fresh fold; `MergePromptContext` sees `Stated=false, Tokens=0` against abctl's retained
stated figure and takes abctl's. Correct — the conversation reached that size, only the store
forgot.

## §6 Fixtures

The 13 rule tests use all seven fixture builders, so moving them needs those builders reachable
from both packages. **Duplicate them; do not build a shared helper package.**

`find` turns up **no exported test-helper package anywhere in `authbridge`** — `pipelinetest`
would be the first, introduced solely to avoid copying 94 lines, and maintained forever.

Drift between two copies is **loud or harmless, never silently wrong**:

| drift | effect |
|---|---|
| tui's `conversation` loses its manifest | fold returns 0, every wiring test fails loudly |
| `oneShot` gains tools | one-shot-exclusion tests fail loudly |
| 27 tools → 1 tool | nothing breaks, and nothing should — the rule tests non-empty only |
| `exchange` stops stamping the request side | nothing breaks — "THE REQUEST SIDE IS NOT CONSULTED" |
| input/cacheRead split changes | `promptTokens` sums them, total unchanged |

The fixtures are not a contract between packages. Each suite is self-consistent and the
**assertions** carry the measured numbers, which move with the tests they live in.

There is also a positive reason to keep them separate, already recorded: *"`conversation` and
`oneShot` above state NOTHING, so every test written before this field existed exercises the
fallback rule — and that is deliberate rather than an oversight."* The tui set should keep
exercising the unstated fallback while the shared set covers the stated arms.

Cost: ~94 lines duplicated, versus ~450 extra lines and 80 re-qualified call sites for the
shared-package route. The new copy is written copy-on-write from the start, so `roled`'s
in-place mutation is not carried over. Each header points at the other and says why they are
separate.

## §7 Testing

**Shared package**

- the 13 moved rule tests, against fresh fixtures
- **monoid laws for BOTH combines** as property tests — commutativity, associativity, identity —
  since §5 now claims them separately for the internal order and the published one
- `Publish()` returns nil for a zero fold, and a non-nil projection preserves `Tokens`,
  `Stated` and `At`
- stated beats unstated regardless of size, in both combines
- the tie-break delta: exact-timestamp ties now resolve by larger `Tokens`, deterministically
- the coarsening is confined as claimed: the published combine differs from the internal one
  **only** on unstated-versus-unstated

**Store**

- `Append` maintains the figure
- **`TestAppend_PromptContextSurvivesATrim`** — unchanged by a trim, and explicitly *not* equal
  to a recomputation over surviving events
- `ListSessions` reports it and stays `O(1)`
- whole-entry eviction drops it; recreation starts fresh

**Wire**

- nil → field absent from JSON
- round-trip through `encoding/json`

**Client**

- the three merge arms
- **the proxy-upgrade regression**: stated 200k must beat abctl's unstated 700k
- the headline case asserted on the *rendered row* — `heldContextCell` / `assertGaugeFilled`
  from #1102, because a correct figure that nothing paints is the bug #1102 fixed

## Commits

One PR, four ordered commits:

| # | Content | Reviewable as |
|---|---|---|
| 1 | move the fold to `authlib/pipeline`, verbatim | "confirm nothing changed" |
| 2 | reformulate as a monoid; add `MergePromptContext`, `PromptContext` | behaviour delta confined to exact-timestamp ties, pinned by test |
| 3 | `entry.context`, `Append`, `ListSessions`, wire field | server observable via `curl /v1/sessions` |
| 4 | client merge and gauge | what appears on screen |

Estimated ~1,785 lines changed, of which ~1,070 is movement in commit 1.

## Future: persistence

Not built here. Three things recorded now because they are cheap to write and expensive to
rediscover.

**Persist the fold, not the published projection.** `PromptContext` drops `msgs` (§4), so
restoring from it would permanently coarsen the session's ordering. The full
`PromptContextFold` is 48 bytes including `msgs`, so there is no reason to persist the lossy
form: restore fold-to-fold using the **internal** combine, then keep folding live events.

Nothing else needs reconstructing — contrast `cost`, whose invariant is
lockstep-with-`Events`-including-on-trim and which needs its `money` sidecar rebuilt and its
trims replayed.

This qualifies a slogan from an earlier draft: the fold is one representation for **memory and
disk**, and `PromptContext` is a lossy projection for **the wire**. Three uses, two shapes.

**Restore order is free**, because the fold is commutative. This was a constraint in an earlier
draft of this design; decision 6 removed it.

**Rule versioning is the open problem, though a smaller one than this section first claimed.**
The original argument was that the rule is slated to change — the `msgs` fallback dying once the
supported proxy floor publishes `agentRole` — so a persisted figure could outlive the rule that
produced it. Task 6 removed that particular instance: `Stated` depends on the **client**, not the
proxy version, so the fallback never retires and this rule is not scheduled to change at all.

The general problem survives the specific one. Any future change to the ordering leaves persisted
figures computed under the old one, and **a figure cannot be recomputed once its events are gone**
— that is the price of a remembered maximum. It is the class of problem
`costledger/reprice.go` already solves after the fact (rate-card changes applied to historical
rows), so there is precedent to follow, but it would be a new instance of it.

Mild in practice: the figure is a token count under every rule version, and a rule change
alters *which turn wins*, not the units.

**What this design does not make easier:** the hard part of session persistence is the events —
volume, the interner (conversations re-send themselves every turn, so naive per-event JSONL
defeats it), and I/O on the proxy's request path. That problem is the same size after this
change as before.

**One field-by-field note for a summary-only persistence tier**, should that be the first step:

| field | survives without events? |
|---|---|
| `PromptContext` | **yes, by design** — lifetime max |
| `CostMicros` / `AvoidedMicros` | restorable as numbers, but their contract says "scoped to what the store holds" — silently violated |
| `TotalTokens`, `EventCount` | **no** — computed from `Events` on demand |

## Out of scope

- **Persisting sessions.** Decision 7.
- **A rule-version field on `PromptContext`.** Dead weight until something writes to disk; the
  ledger solved the same problem after the fact.
- **Client-side prefetch of timelines.** Superseded by the server field.
- **Deleting the `msgs` fallback.** Not deletable at all, and that is a finding rather than a
  deferral: `agentRole` is empty for every client that is not Claude Code, so `Stated` is a
  property of the client and no proxy-version floor retires the fallback. The pre-existing claim
  to the contrary — inherited from `sessions_context.go`'s doc comment and repeated in
  `sessions_context_test.go:72` — is wrong, and anything relying on it should stop.
- **Anything in PR #1102.** Merged; complementary, not superseded.
