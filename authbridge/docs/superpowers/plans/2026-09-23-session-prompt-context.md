# Session prompt-context figure — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make abctl's `CONTEXT(1M)` gauge a fact about the session rather than about
abctl's uptime, by computing the figure server-side and publishing it on `/v1/sessions`.

**Architecture:** Lift the prompt-context fold from `cmd/abctl/tui` into `authlib/pipeline`
(zero new dependency edges — `authlib/session` already imports it). Reformulate it as a max
over a total order so it is a commutative monoid. Maintain one per session on `Store.Append`,
following the `cost`/`avoided` precedent rather than summing in `ListSessions`. Publish a lossy
mergeable projection on `SessionSummary`, and have abctl merge it with its own figure.

**Tech Stack:** Go 1.26, `charmbracelet/bubbles` table, `encoding/json`. No new dependencies.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-23-session-prompt-context-design.md`

## Global Constraints

- **Worktree:** `/Users/haihuang/works/go/src/github.com/kagenti/kagenti-extensions/.worktrees/promptctx`, branch `feat/session-prompt-context`. All paths below are relative to `authbridge/`.
- **One PR, 8 commits in 4 phases.** Phase A = tasks 1-3 (move), B = 4-5 (monoid), C = 6-7 (server), D = 8 (client).
- **DCO is mandatory:** every commit uses `git commit -s`.
- **Attribution:** end every commit message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. NEVER `Co-Authored-By`, `Generated with`, or `Made-with`.
- **Commit by pathspec:** `git commit -s -F <msgfile> -- <paths>`. A bare `git add` + `commit` can sweep another session's staged files.
- **Network commands need proxies cleared:** prefix `git fetch`/`git push`/`go mod` with `HTTPS_PROXY= HTTP_PROXY= ALL_PROXY=`. A TLS bridge on `:47600` otherwise fails certificate verification.
- **Output discipline:** any command producing >5 lines redirects to `$LOG_DIR` (`export LOG_DIR=/Users/haihuang/.claude/jobs/ecb7387f/tmp/promptctx`), returning only an exit code.
- **gofmt is scoped to changed files only.** Never `gofmt -w .` at a module root — it sweeps pre-existing dirty files into the diff.
- **Lint:** `golangci-lint run --new-from-rev=upstream/main ./<changed>/...`. Do NOT run cortex's `make lint` — it fails on pre-existing ruff errors elsewhere and rewrites ~10 unrelated files.
- **Comment density must match the surrounding file.** `SessionSummary.CostMicros` carries a 19-line doc comment for one field; new exported fields and the moved rule need comparable treatment. Terse code will read as out of place here.
- **Known pre-existing failure:** `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` in `cmd/abctl` fails on this machine (reads a real `~/.cortex/ca/bundle.crt` that does not exist). It fails identically on clean `main`. Ignore it; do not "fix" it.

---

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `authlib/pipeline/promptcontext.go` | the fold: rule, ordering, `Add`/`AddAll`/`PromptContextOf`, `Publish`, `Merge` | **create** |
| `authlib/pipeline/tokens.go` | `PromptTokens(*InferenceExtension) int` | **create** |
| `authlib/pipeline/promptcontext_test.go` | the 13 rule tests + monoid laws | **create** |
| `authlib/pipeline/promptcontext_fixtures_test.go` | duplicated event fixtures | **create** |
| `cmd/abctl/tui/sessions_context.go` | gauge rendering + model-state caching only | shrink (−224) |
| `cmd/abctl/tui/cost_event.go` | loses `promptTokens` | modify |
| `cmd/abctl/tui/events_columns.go:319`, `events_pane.go:1176` | re-qualify to `pipeline.PromptTokens` | modify |
| `cmd/abctl/tui/sessions_context_test.go` | gauge + table tests only | shrink (−327) |
| `authlib/session/store.go` | `entry.context`, `Append` hook, `SessionSummary.PromptContext`, `ListSessions` | modify |
| `authlib/session/store_test.go` | Append maintenance + trim invariance | modify |
| `cmd/abctl/tui/sessions_pane.go:277,318` | pass the summary's projection | modify |

---

# Phase A — the verbatim move

## Task 1: Move `promptTokens` into `pipeline`

Its only parameter is a `pipeline` type, so it belongs there. 3 non-test callers; one of them is
the fold, which is about to move, and duplicating a token-summing function across two packages
is the drift risk this design rejects for the rule itself.

**Files:**
- Create: `authlib/pipeline/tokens.go`
- Modify: `cmd/abctl/tui/cost_event.go` (delete lines 88-96)
- Modify: `cmd/abctl/tui/events_columns.go:319`, `cmd/abctl/tui/events_pane.go:1176`
- Modify: 6 test call sites under `cmd/abctl/tui/`

**Interfaces:**
- Produces: `pipeline.PromptTokens(resp *InferenceExtension) int`

- [ ] **Step 1: Create the new file with the function, verbatim apart from the name**

```go
package pipeline

// PromptTokens is what a response's prompt cost, in tokens: input plus both cache tiers.
//
// OUTPUT IS DELIBERATELY EXCLUDED. Measured across six live sessions it is 0.003%-2.2% of the
// prompt and 0.2% on conversations near the context limit — a fifth of one eighth-block at 1M,
// so including it would change no rendered pixel while making the figure mean something else.
//
// The three-way sum FIRST, PromptTokens second, because zero on the sum means "this provider
// reported the breakdown" and a provider that reports only a total sets the scalar instead.
// Taking the scalar first would silently discard the cache tiers, which on a cached
// conversation are ~99% of the prompt.
func PromptTokens(resp *InferenceExtension) int {
	if resp == nil {
		return 0
	}
	if n := resp.InputTokens + resp.CacheReadTokens + resp.CacheWriteTokens; n > 0 {
		return n
	}
	return resp.PromptTokens
}
```

- [ ] **Step 2: Delete the original and re-qualify its callers**

Delete `func promptTokens` from `cmd/abctl/tui/cost_event.go` (lines 88-96, through the closing
brace, leaving the `savingSign` comment that follows intact).

Then replace `promptTokens(` with `pipeline.PromptTokens(` at:
- `cmd/abctl/tui/events_columns.go:319`
- `cmd/abctl/tui/events_pane.go:1176`
- `cmd/abctl/tui/sessions_context.go:182` (moves away in Task 2; re-qualify now so the tree compiles)
- the 6 test call sites

Find them all:

```bash
grep -rn "promptTokens(" --include=*.go cmd/abctl/ | grep -v "pipeline.PromptTokens"
```

Each touched file needs `"github.com/rossoctl/cortex/authbridge/authlib/pipeline"` imported; most already have it.

- [ ] **Step 3: Verify the tree builds and every existing test still passes**

```bash
export LOG_DIR=/Users/haihuang/.claude/jobs/ecb7387f/tmp/promptctx
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go build ./... > $LOG_DIR/t1-build.log 2>&1; echo "BUILD:$?"
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./cmd/abctl/tui/ ./authlib/pipeline/ > $LOG_DIR/t1-test.log 2>&1; echo "TEST:$?"
```

Expected: both `0`. **No new test in this task** — it is a pure move, and the existing suite
passing unchanged is the assertion. Adding a test for `PromptTokens` here would be testing the
move rather than the behaviour.

- [ ] **Step 4: Format, lint, commit**

```bash
gofmt -l authlib/pipeline/tokens.go cmd/abctl/tui/cost_event.go cmd/abctl/tui/events_columns.go cmd/abctl/tui/events_pane.go
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= golangci-lint run --new-from-rev=upstream/main ./authlib/pipeline/... ./cmd/abctl/tui/... > $LOG_DIR/t1-lint.log 2>&1; echo "LINT:$?"
```

`gofmt -l` must print nothing. Commit message:

```
refactor(pipeline): Move promptTokens to the package that owns its type

Its only parameter is *pipeline.InferenceExtension, and the fold that is about
to move into pipeline is one of its three callers. Duplicating a token-summing
function across two packages is the drift risk this work rejects for the rule
itself, so it moves once and the two rendering callers re-qualify.

Pure move; no behaviour change. The existing suite passing unchanged is the
assertion.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
```

---

## Task 2: Move the fold into `pipeline`, verbatim

**Files:**
- Create: `authlib/pipeline/promptcontext.go`
- Modify: `cmd/abctl/tui/sessions_context.go` (delete lines 12-235; adapt `sessionContextFor` and `rebaseSessionContext`)

**Interfaces:**
- Consumes: `pipeline.PromptTokens` (Task 1)
- Produces:
  - `type PromptContextFold struct` — unexported fields `n, tokens, msgs int; at time.Time; stated bool`
  - `func (f *PromptContextFold) Add(e *SessionEvent)`
  - `func (f *PromptContextFold) AddAll(events []SessionEvent)`
  - `func (f PromptContextFold) Tokens() int`
  - `func (f PromptContextFold) Folded() int` — exposes `n` for abctl's slice-length check
  - `func PromptContextOf(events []SessionEvent) int`

- [ ] **Step 1: Copy lines 12-235 of `sessions_context.go` into the new file unchanged, then apply exactly these renames**

**Copy the doc comments verbatim.** They carry ~200 lines of measured justification (the
1491/830k interleaving table, the 73-vs-42 manifest split, the 555-turn measurement) that is the
reason this rule is trusted. Losing it is the one unrecoverable part of this move.

| was | becomes |
|---|---|
| `contextRun` | `PromptContextFold` |
| `foldSessionContext(events, run) contextRun` | `(*PromptContextFold).AddAll(events)` |
| `sessionContext(events) int` | `PromptContextOf(events) int` |
| `toolCount` | `toolCount` (unexported, unchanged) |
| `messageCount` | `messageCount` (unexported, unchanged) |
| `promptTokens(...)` | `PromptTokens(...)` (same package now — drop the qualifier) |
| `pipeline.SessionResponse` | `SessionResponse` |
| `pipeline.AgentRoleSubagent` | `AgentRoleSubagent` |
| `pipeline.InferenceExtension` | `InferenceExtension` |
| `pipeline.SessionEvent` | `SessionEvent` |

`AddAll` is the existing loop body with `run` replaced by the receiver:

```go
func (f *PromptContextFold) AddAll(events []SessionEvent) {
	for i := range events {
		f.Add(&events[i])
	}
	f.n += len(events)
}
```

`Add` is one iteration of that loop — the `continue` statements become `return`:

```go
func (f *PromptContextFold) Add(e *SessionEvent) {
	if e.Phase != SessionResponse || e.Inference == nil {
		return
	}
	if toolCount(e.Inference) == 0 {
		return
	}
	n := PromptTokens(e.Inference)
	if n <= 0 {
		return
	}
	switch role := e.Inference.AgentRole; {
	case role == AgentRoleSubagent:
		return
	case role != "" && !f.stated:
		f.tokens, f.msgs, f.at, f.stated = n, messageCount(e.Inference), e.At, true
	case role != "":
		if !e.At.Before(f.at) {
			f.tokens, f.at = n, e.At
		}
	case f.stated:
		return
	default:
		if msgs := messageCount(e.Inference); msgs > f.msgs ||
			(msgs == f.msgs && !e.At.Before(f.at)) {
			f.tokens, f.msgs, f.at = n, msgs, e.At
		}
	}
}
```

**Note the deliberate asymmetry:** `AddAll` advances `n`, `Add` does not. `n` is a cursor into a
caller's slice, and a per-event caller has no slice. Document it — it will otherwise read as a
bug. The store leaves `n` at zero forever.

Accessors:

```go
func (f PromptContextFold) Tokens() int { return f.tokens }
func (f PromptContextFold) Folded() int { return f.n }

func PromptContextOf(events []SessionEvent) int {
	var f PromptContextFold
	f.AddAll(events)
	return f.tokens
}
```

- [ ] **Step 2: Adapt the two model-state functions that stay in tui**

In `sessions_context.go`, `sessionContextFor` and `rebaseSessionContext` change only in how they
name the fold. `m.contextRun` becomes `map[string]pipeline.PromptContextFold`:

```go
func (m *model) sessionContextFor(id string) int {
	events := m.events[id]
	run, ok := m.contextRun[id]
	switch {
	case ok && run.Folded() == len(events):
		return run.Tokens()
	case ok && run.Folded() < len(events):
		run.AddAll(events[run.Folded():])
		m.contextRun[id] = run
	default:
		m.rebaseSessionContext(id, events)
		return m.contextRun[id].Tokens()
	}
	return run.Tokens()
}

func (m *model) rebaseSessionContext(id string, events []pipeline.SessionEvent) {
	prev := m.contextRun[id]
	prev.ResetFolded()
	prev.AddAll(events)
	if m.contextRun == nil {
		m.contextRun = map[string]pipeline.PromptContextFold{}
	}
	m.contextRun[id] = prev
}
```

`ResetFolded()` replaces the old `prev.n = 0`, which is no longer reachable from outside the
package. Add it to `promptcontext.go`, and keep the existing comment explaining why the rebase
resets only the counter rather than rebuilding the struct field-by-field — that literal is how
the `at` tie-break went missing once already:

```go
// ResetFolded zeroes the slice cursor while KEEPING the figure, for a caller whose slice was
// replaced rather than appended to. See abctl's rebaseSessionContext: dropping the figure there
// blanks a live session's gauge, because a view=summary timeline may carry no candidate at all.
func (f *PromptContextFold) ResetFolded() { f.n = 0 }
```

Update the `model.contextRun` field type and the inventory comment on `model.events` to name
`pipeline.PromptContextFold`.

- [ ] **Step 3: Verify — the existing suite is the assertion**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go build ./... > $LOG_DIR/t2-build.log 2>&1; echo "BUILD:$?"
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./cmd/abctl/tui/ ./authlib/pipeline/ > $LOG_DIR/t2-test.log 2>&1; echo "TEST:$?"
```

Expected `0` and `0`. The 13 rule tests are still in `cmd/abctl/tui` at this point, now
exercising the moved code through `pipeline.PromptContextOf` — which is exactly the check this
step wants. They move in Task 3.

- [ ] **Step 4: Format, lint, commit**

```
refactor(pipeline): Move the prompt-context fold out of the TUI

The rule reads InferenceExtension.AgentRole, the tool manifest and the prompt
counts — all pipeline's types — and authlib/session needs it to maintain a
per-session figure. session already imports pipeline and pipeline imports only
authlib/contracts, so this costs no new dependency edge.

Doc comments move verbatim. They carry the measurements the rule is trusted on:
the 1491/830k interleaving, the 73-vs-42 manifest split across 115 responses,
and the 555-turn series that says the column tracks the conversation.

Gauge rendering and the model-state cache stay in the TUI. Behaviour unchanged;
the existing tests pass against the moved code.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
```

---

## Task 3: Duplicate the fixtures, move the 13 rule tests

**Files:**
- Create: `authlib/pipeline/promptcontext_fixtures_test.go`
- Create: `authlib/pipeline/promptcontext_test.go`
- Modify: `cmd/abctl/tui/sessions_context_test.go` (delete lines 95-327)

**Interfaces:**
- Produces (test-only, package `pipeline`): `toolsOf`, `exchange`, `conversation`, `oneShot`, `roled`, `mainAgent`, `subagent`

- [ ] **Step 1: Write the fixtures into the new package**

Copy `sessions_context_test.go` lines 26-94, dropping the `pipeline.` qualifier throughout.
**One deliberate change:** `roled` becomes copy-on-write. In tui it mutates its argument and
returns it, which is safe only because every caller passes a fresh `conversation(...)`; carried
into a second package that invariant is invisible.

```go
// roled states the caller's role on every event of a turn, which is what a proxy that reads
// Claude Code's billing-header line publishes.
//
// COPIES FIRST, unlike the tui original, which mutated its argument and returned it. That reads
// as pure and is not: `base := conversation(...)` followed by `subagent(base)` would restamp
// base too. Safe there only because every caller happens to pass a fresh turn.
func roled(evs []SessionEvent, role AgentRole) []SessionEvent {
	out := make([]SessionEvent, len(evs))
	for i := range evs {
		out[i] = evs[i]
		if evs[i].Inference != nil {
			inf := *evs[i].Inference
			inf.AgentRole = role
			out[i].Inference = &inf
		}
	}
	return out
}
```

Add a header comment pointing at the other copy:

```go
// Fixtures for the prompt-context rule. DELIBERATELY A SECOND COPY of the set in
// cmd/abctl/tui/sessions_context_test.go, not a shared helper package.
//
// authbridge has no exported test-helper package anywhere, and introducing the first one to
// avoid copying ~94 lines would cost ~450 lines of churn across 80 call sites. Drift between
// the copies is loud or harmless, never silently wrong: a manifest that goes missing makes the
// fold return 0 and fails every test that reads it, while 27-tools-versus-1 changes nothing
// because the rule only asks whether a manifest is non-empty.
//
// The tui copy also has a job this one does not: it states NO role, so the tests over there
// keep exercising the unstated fallback rule on purpose.
```

- [ ] **Step 2: Move the 13 rule tests**

Cut `sessions_context_test.go` lines 95-327 into `promptcontext_test.go`. Rename
`sessionContext(` → `PromptContextOf(` and drop `pipeline.` qualifiers. The test names stay
identical so `git log -S` still finds their history.

The 13: `TestSessionContext_IgnoresOneShotsBetweenTurns`, `_SurvivesALongSilence`,
`_UnstatedRoleMostMessagesWins`, `_JudgesTheResponsesOwnManifest`,
`_IgnoresRequestEventsEvenWithCounts`, `_UnstatedRoleHoldsThePreCompactionFigure`,
`_AfterACompactionFollowsTheMainAgent`, `_IgnoresASubagentThatSpokeLast`,
`_AStatedOneShotIsStillAOneShot`, `_AStatedTurnDisplacesAnUnstatedFigure`,
`_AStatedSessionIgnoresUnstatedRows`, `_ZeroWhenNothingCanBeSaid`, and the projection tests that
reference `PromptContextOf` only.

**Leave in tui:** `TestContextGauge` (line 328), `TestSessionsTable_ContextColumnReplacesActive`
(372), `TestSessionsTable_UnknownContextIsADash` (414) — all rendering. Leave the fixtures
there, and leave `sessions_context_fold_test.go`, `sessions_context_wire_test.go` and
`sessions_context_bench_test.go` completely untouched: they test the model's caching, not the
rule.

- [ ] **Step 3: Run both packages**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/pipeline/ ./cmd/abctl/tui/ -count=1 > $LOG_DIR/t3.log 2>&1; echo "TEST:$?"
grep -c "^--- PASS" $LOG_DIR/t3.log
```

Expected: exit `0`. Confirm the 13 now run in `pipeline`:

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/pipeline/ -run TestSessionContext -v 2>&1 | grep -c "^--- PASS"
```

Expected: `12` or more (several have subtests).

- [ ] **Step 4: Format, lint, commit** — message states that the fixtures are a deliberate second copy and why, plus the `roled` copy-on-write change as the one intentional behaviour difference.

---

# Phase B — the monoid

## Task 4: Reformulate the rule as a max over a total order

This is the one **behaviour change** in Phase A+B, and it is why this is its own commit rather
than part of the move.

**Files:**
- Modify: `authlib/pipeline/promptcontext.go`
- Modify: `authlib/pipeline/promptcontext_test.go`

**Interfaces:**
- Produces: `func (f *PromptContextFold) Add(e *SessionEvent)` (same signature, new internals)

- [ ] **Step 1: Write the failing test for the tie-break delta**

```go
// EXACT-TIMESTAMP TIES NOW RESOLVE DETERMINISTICALLY, by the larger context.
//
// The sequential form fell through to arrival order here: two unstated turns with equal message
// counts and the same timestamp gave 100k or 200k depending purely on which was folded first.
// That is the only input on which this reformulation disagrees with its predecessor, and a
// restore that folded a session's events in any other order would have inherited the
// non-determinism.
func TestPromptContextFold_ExactTimestampTiesAreDeterministic(t *testing.T) {
	at := time.Now()
	small := conversation("s", at, 600, 100_000)
	large := conversation("l", at, 600, 200_000)

	var a, b PromptContextFold
	a.AddAll(append(append([]SessionEvent{}, small...), large...))
	b.AddAll(append(append([]SessionEvent{}, large...), small...))

	if a.Tokens() != b.Tokens() {
		t.Errorf("fold order changed the answer: %d vs %d", a.Tokens(), b.Tokens())
	}
	if got := a.Tokens(); got != 200_000 {
		t.Errorf("tie resolved to %d, want 200000 — the larger context wins", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/pipeline/ -run TestPromptContextFold_ExactTimestampTiesAreDeterministic -v > $LOG_DIR/t4-red.log 2>&1; echo "EXIT:$?"
grep -E "fold order|tie resolved" $LOG_DIR/t4-red.log
```

Expected: FAIL, reporting `100000 vs 200000`. **If it passes, stop** — the premise is wrong and
the reformulation needs re-deriving before going further.

- [ ] **Step 3: Replace the switch with a candidate plus a total order**

```go
// candidate is one event's claim on the column: extracted, then compared.
type candidate struct {
	tokens int
	msgs   int
	at     time.Time
	stated bool
}

// candidateOf extracts an event's claim, or a zero candidate if it makes none.
func candidateOf(e *SessionEvent) candidate {
	if e.Phase != SessionResponse || e.Inference == nil {
		return candidate{}
	}
	if toolCount(e.Inference) == 0 {
		return candidate{}
	}
	if e.Inference.AgentRole == AgentRoleSubagent {
		return candidate{}
	}
	n := PromptTokens(e.Inference)
	if n <= 0 {
		return candidate{}
	}
	return candidate{
		tokens: n,
		msgs:   messageCount(e.Inference),
		at:     e.At,
		stated: e.Inference.AgentRole != "",
	}
}

// better reports whether a outranks b under THE rule, expressed as a TOTAL ORDER rather than as
// the sequential latch this replaced.
//
//	stated ≻ unstated                      one stated turn settles it, however much longer an
//	                                       unstated predecessor was — a figure chosen by a rule
//	                                       that cannot see subagents is not evidence
//	  within stated:    (at, tokens, msgs)
//	  within unstated:  (msgs, tokens, at)
//
// A max over a total order is commutative AND associative, which is what makes the fold a
// monoid: replay order cannot change the answer, so a future restore-then-continue needs no
// ordering guarantee. The trailing comparators exist only to make the STORED struct fully
// determined; nothing reads msgs once stated is true, nor at once unstated.
func better(a, b candidate) bool {
	if a.stated != b.stated {
		return a.stated
	}
	if a.stated {
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		if a.tokens != b.tokens {
			return a.tokens > b.tokens
		}
		return a.msgs > b.msgs
	}
	if a.msgs != b.msgs {
		return a.msgs > b.msgs
	}
	if a.tokens != b.tokens {
		return a.tokens > b.tokens
	}
	return a.at.After(b.at)
}

func (f *PromptContextFold) Add(e *SessionEvent) {
	c := candidateOf(e)
	if c.tokens == 0 {
		return
	}
	// The zero fold is the monoid's identity: any candidate beats "nothing known yet", and
	// better() must not be asked to compare against it — an unstated candidate would lose to a
	// zero fold on msgs.
	if f.tokens == 0 || better(c, f.current()) {
		f.tokens, f.msgs, f.at, f.stated = c.tokens, c.msgs, c.at, c.stated
	}
}

func (f PromptContextFold) current() candidate {
	return candidate{tokens: f.tokens, msgs: f.msgs, at: f.at, stated: f.stated}
}
```

- [ ] **Step 4: Add the monoid law property test**

```go
// THE LAWS THE RESTORE PATH WILL RELY ON. Stated as tests because the spec claims them: if the
// fold is not commutative and associative, replay order changes a persisted figure.
func TestPromptContextFold_IsACommutativeMonoid(t *testing.T) {
	base := time.Now()
	turns := [][]SessionEvent{
		conversation("c1", base, 1491, 830_000),
		oneShot("o1", base.Add(time.Minute), 282_000),
		mainAgent("m1", base.Add(2*time.Minute), 108, 217_121),
		subagent("s1", base.Add(3*time.Minute), 186, 198_899),
		conversation("c2", base.Add(4*time.Minute), 1509, 851_000),
	}

	foldOf := func(order []int) PromptContextFold {
		var f PromptContextFold
		for _, i := range order {
			f.AddAll(turns[i])
		}
		f.ResetFolded() // n counts arrivals, not content; it is not part of the answer
		return f
	}

	want := foldOf([]int{0, 1, 2, 3, 4})
	for _, order := range [][]int{
		{4, 3, 2, 1, 0}, {2, 0, 4, 1, 3}, {1, 4, 0, 3, 2}, {3, 2, 4, 0, 1},
	} {
		if got := foldOf(order); got != want {
			t.Errorf("order %v gave %+v, want %+v — the fold is not commutative", order, got, want)
		}
	}

	// Identity.
	var zero PromptContextFold
	if got := foldOf([]int{0}); got == zero {
		t.Fatal("fixture folds to the zero value; this test proves nothing")
	}
	withZero := foldOf([]int{0})
	withZero.AddAll(nil)
	withZero.ResetFolded()
	if withZero != foldOf([]int{0}) {
		t.Error("folding nothing changed the answer; zero is not the identity")
	}
}
```

- [ ] **Step 5: Run the whole package**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/pipeline/ ./cmd/abctl/tui/ -count=1 > $LOG_DIR/t4-green.log 2>&1; echo "EXIT:$?"
```

Expected `0`. **All 13 moved rule tests must still pass** — they are the proof that the
reformulation preserves the rule on every input except the tie. If any fails, the reformulation
is wrong; do not adjust the test.

- [ ] **Step 6: Format, lint, commit** — the message must state the behaviour delta explicitly (exact-timestamp ties among equal-message unstated turns now resolve by larger context, previously by arrival order) and that the 13 rule tests passing unchanged is the evidence it is confined to that.

---

## Task 5: Add `PromptContext`, `Publish`, and `Merge`

**Files:**
- Modify: `authlib/pipeline/promptcontext.go`
- Modify: `authlib/pipeline/promptcontext_test.go`

**Interfaces:**
- Produces:
  - `type PromptContext struct { Tokens int; Stated bool; At time.Time }` with JSON tags `tokens`, `stated`, `at`
  - `func (f PromptContextFold) Publish() *PromptContext` — nil when `Tokens == 0`
  - `func Merge(a, b *PromptContext) *PromptContext`

- [ ] **Step 1: Write the failing tests**

```go
// NIL IS "NOTHING KNOWN", and it must survive the round trip as an ABSENT field rather than as a
// zero object. contextGauge renders 0 as an em dash and a real figure as a track; a
// {"tokens":0} on the wire would assert a figure the server does not have.
func TestPromptContextFold_PublishIsNilWhenNothingIsKnown(t *testing.T) {
	var f PromptContextFold
	if got := f.Publish(); got != nil {
		t.Errorf("a zero fold published %+v, want nil", got)
	}
	f.AddAll(oneShot("o1", time.Now(), 282_000)) // no manifest: makes no claim
	if got := f.Publish(); got != nil {
		t.Errorf("a one-shot-only session published %+v, want nil", got)
	}
	f.AddAll(conversation("c1", time.Now(), 600, 500_000))
	got := f.Publish()
	if got == nil || got.Tokens != 500_000 {
		t.Fatalf("published %+v, want tokens=500000", got)
	}
}

// THE HOLE A BARE max LEFT OPEN, and the reason PromptContext carries Stated at all.
//
// abctl attaches to a proxy predating agentRole, folds unstated turns, and lands on the
// documented stale-fallback figure — 700k held from before a compaction. The proxy is then
// upgraded; abctl keeps running, because contextRun outlives everything but a pod switch. The
// new proxy publishes a STATED 200k, the correct latest main-agent turn. max(700k, 200k) pins
// the unsound figure permanently.
func TestMerge_StatedBeatsUnstatedHoweverLarge(t *testing.T) {
	stated := &PromptContext{Tokens: 200_000, Stated: true, At: time.Now()}
	unstated := &PromptContext{Tokens: 700_000, Stated: false, At: time.Now()}

	for _, tc := range []struct{ name string; a, b *PromptContext }{
		{"stated first", stated, unstated},
		{"unstated first", unstated, stated},
	} {
		if got := Merge(tc.a, tc.b); got.Tokens != 200_000 {
			t.Errorf("%s: merged to %d, want 200000 — a figure from a rule that cannot see "+
				"subagents is not evidence about the conversation", tc.name, got.Tokens)
		}
	}
}

// NIL IS THE IDENTITY, which is what lets the client merge without version detection: an old
// proxy sends no field, and that is a valid operand rather than a case to branch on.
func TestMerge_NilIsTheIdentity(t *testing.T) {
	x := &PromptContext{Tokens: 500_000, Stated: true, At: time.Now()}
	if got := Merge(nil, x); got != x {
		t.Errorf("Merge(nil, x) = %+v, want x", got)
	}
	if got := Merge(x, nil); got != x {
		t.Errorf("Merge(x, nil) = %+v, want x", got)
	}
	if got := Merge(nil, nil); got != nil {
		t.Errorf("Merge(nil, nil) = %+v, want nil", got)
	}
}

// Both stated: the later turn wins, which is the rule the column follows.
func TestMerge_BothStatedTakesTheLater(t *testing.T) {
	early := &PromptContext{Tokens: 900_000, Stated: true, At: time.Now()}
	late := &PromptContext{Tokens: 200_000, Stated: true, At: early.At.Add(time.Minute)}
	if got := Merge(early, late); got.Tokens != 200_000 {
		t.Errorf("merged to %d, want 200000 — latest-wins, not largest", got.Tokens)
	}
}

// THE COARSENING IS CONFINED TO ONE ARM, which is the spec's claim for omitting msgs.
func TestMerge_NeitherStatedFallsBackToLargerTokens(t *testing.T) {
	a := &PromptContext{Tokens: 100_000, At: time.Now()}
	b := &PromptContext{Tokens: 700_000, At: time.Now().Add(-time.Hour)}
	if got := Merge(a, b); got.Tokens != 700_000 {
		t.Errorf("merged to %d, want 700000 — msgs is not published, so this arm is coarse "+
			"by design and takes the larger figure", got.Tokens)
	}
}
```

- [ ] **Step 2: Run them and watch them fail to compile**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/pipeline/ -run 'TestMerge|TestPromptContextFold_Publish' > $LOG_DIR/t5-red.log 2>&1; echo "EXIT:$?"
```

Expected: non-zero, `undefined: Merge`.

- [ ] **Step 3: Implement**

```go
// PromptContext is the fold's PUBLISHABLE state: enough to merge two of them, which a client
// holding its own figure must do, and which a future restore-then-continue would do.
//
// A LOSSY PROJECTION, deliberately. msgs is omitted because it is the FALLBACK rule's
// comparator and is slated for deletion once the supported proxy floor publishes agentRole
// (see PromptContextFold). Omitting it coarsens exactly one arm of Merge —
// unstated-versus-unstated — which a client reaches only against a proxy that publishes this
// type without publishing agentRole. Persistence should store the FOLD, not this.
type PromptContext struct {
	Tokens int       `json:"tokens"`
	Stated bool      `json:"stated"`
	At     time.Time `json:"at"`
}

// Publish projects the fold for the wire, or nil when nothing can be said.
//
// NIL RATHER THAN A ZERO STRUCT, so the field is absent under omitempty. A session with only
// one-shot calls has no conversation to measure and is indistinguishable from one nobody has
// observed; both must render as an em dash rather than as a figure. Same standing rule
// SessionSummary.CostMicros states for its own omitempty.
func (f PromptContextFold) Publish() *PromptContext {
	if f.tokens == 0 {
		return nil
	}
	return &PromptContext{Tokens: f.tokens, Stated: f.stated, At: f.at}
}

// Merge combines two published figures, nil meaning "nothing known".
//
// THE PUBLISHED ORDER IS COARSER THAN THE FOLD'S — see PromptContext — but it is the same shape:
// a max over a total order, so Merge is commutative, associative, and has nil as its identity.
// That is what lets a client merge the server's figure with its own and need no version
// detection: an old proxy sends nothing, and nothing is a valid operand.
//
//	stated ≻ unstated             a figure from a rule that cannot see subagents is not
//	                              evidence, at any size
//	  both stated:    later At wins; tie → larger Tokens
//	  neither stated: larger Tokens (msgs is unpublished, so this arm is coarse)
func Merge(a, b *PromptContext) *PromptContext {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case a.Stated != b.Stated:
		if a.Stated {
			return a
		}
		return b
	case a.Stated:
		if !a.At.Equal(b.At) {
			if a.At.After(b.At) {
				return a
			}
			return b
		}
		if a.Tokens >= b.Tokens {
			return a
		}
		return b
	default:
		if a.Tokens >= b.Tokens {
			return a
		}
		return b
	}
}
```

- [ ] **Step 4: Add the published-order monoid law test**

```go
// THE SECOND MONOID, which the spec claims separately from the fold's. Merge is what the client
// and any future restore call, so its laws are load-bearing independently.
func TestMerge_IsACommutativeMonoid(t *testing.T) {
	at := time.Now()
	vals := []*PromptContext{
		nil,
		{Tokens: 100_000, At: at},
		{Tokens: 700_000, At: at.Add(-time.Hour)},
		{Tokens: 200_000, Stated: true, At: at},
		{Tokens: 900_000, Stated: true, At: at.Add(-time.Minute)},
	}
	for _, x := range vals {
		for _, y := range vals {
			if Merge(x, y) != Merge(y, x) {
				t.Errorf("not commutative for %+v, %+v", x, y)
			}
			for _, z := range vals {
				if Merge(Merge(x, y), z) != Merge(x, Merge(y, z)) {
					t.Errorf("not associative for %+v, %+v, %+v", x, y, z)
				}
			}
		}
	}
}
```

- [ ] **Step 5: Run, format, lint, commit**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/pipeline/ ./cmd/abctl/tui/ -count=1 > $LOG_DIR/t5-green.log 2>&1; echo "EXIT:$?"
```

---

# Phase C — the server

## Task 6: Maintain the fold on `Store.Append`

**Files:**
- Modify: `authlib/session/store.go` (`entry` struct ~line 74; `Append` ~line 300)
- Modify: `authlib/session/store_test.go`

**Interfaces:**
- Consumes: `pipeline.PromptContextFold`, `.Add`, `.Tokens`
- Produces: `entry.context` (unexported)

- [ ] **Step 1: Write the failing trim-invariance test**

This is the **inverse** of `TestAppend_RunningTotalsMatchAFullRecomputation` at
`store_test.go:762`. Read that test first for the construction idiom, then write:

```go
// THE FIGURE OUTLIVES THE EVENTS IT WAS READ FROM, which is the opposite of the invariant
// TestAppend_RunningTotalsMatchAFullRecomputation holds for cost.
//
// cost is a SUM and stays equal to sumCost(Events), shedding whatever a trim evicts — that is
// what scopes the COST column exactly like the TOKENS column beside it. This is a MAXIMUM, and
// a maximum over a trimmed slice does not understate, it reports a small conversation when the
// conversation is large and merely aged out.
//
// THE SECOND ASSERTION FORBIDS A FUTURE "FIX". Making this recomputable from Events — the
// instinct, by analogy to cost — reintroduces exactly the bug this field exists to remove: the
// gauge blanking because the store forgot the turn. That regression must fail here rather than
// look like a consistency improvement.
func TestAppend_PromptContextSurvivesATrim(t *testing.T) {
	const maxEvents = 4
	s := New(time.Hour, maxEvents, 0)
	base := time.Now()

	// The winning agentic turn, then enough one-shots to evict it.
	for _, e := range promptContextTurn("win", base, 600, 27, 500_000) {
		s.Append("sess", e)
	}
	for i := 0; i < maxEvents*2; i++ {
		for _, e := range promptContextTurn(fmt.Sprintf("o%d", i),
			base.Add(time.Duration(i+1)*time.Minute), 3, 0, 7_000) {
			s.Append("sess", e)
		}
	}

	var got *pipeline.PromptContext
	for _, sum := range s.ListSessions() {
		if sum.ID == "sess" {
			got = sum.PromptContext
		}
	}
	if got == nil {
		t.Fatal("no figure after the trim — the gauge went blank for a session that reached 500k")
	}
	if got.Tokens != 500_000 {
		t.Errorf("reported %d, want 500000", got.Tokens)
	}

	// And it is NOT recomputable from what survived.
	var live pipeline.PromptContextFold
	live.AddAll(s.Snapshot("sess"))
	if live.Tokens() == got.Tokens {
		t.Error("a recomputation over surviving events matched the stored figure, so this test " +
			"is not exercising the trim — raise the one-shot count")
	}
}
```

Add the fixture beside it (the store package has no event builders of its own):

```go
// promptContextTurn is a request/response pair as the store records one: manifest and message
// count on both sides, token counts on the response, since the provider is the only party that
// tokenizes. ntools == 0 makes it a one-shot, which the rule excludes.
func promptContextTurn(id string, at time.Time, msgs, ntools, context int) []pipeline.SessionEvent {
	inf := func() *pipeline.InferenceExtension {
		tools := make([]pipeline.InferenceTool, ntools)
		return &pipeline.InferenceExtension{
			Model:     "claude-opus-5",
			Messages:  make([]pipeline.InferenceMessage, msgs),
			Tools:     tools,
			AgentRole: pipeline.AgentRoleMain,
		}
	}
	resp := inf()
	resp.InputTokens, resp.CacheReadTokens = 300, context-300
	return []pipeline.SessionEvent{
		{At: at, RequestID: id, Phase: pipeline.SessionRequest,
			Direction: pipeline.Outbound, Inference: inf()},
		{At: at.Add(time.Second), RequestID: id, Phase: pipeline.SessionResponse,
			Direction: pipeline.Outbound, Inference: resp},
	}
}
```

Confirm `Snapshot` is the right accessor for held events before relying on it:

```bash
grep -n "func (s \*Store) Snapshot" authlib/session/store.go
```

If the exported name differs, use whatever `viewtail_test.go` uses to read a session's events.

- [ ] **Step 2: Run it — it must fail to compile**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/session/ -run TestAppend_PromptContextSurvivesATrim > $LOG_DIR/t6-red.log 2>&1; echo "EXIT:$?"
```

Expected: non-zero, `sum.PromptContext undefined`.

- [ ] **Step 3: Add the field and the hook**

In the `entry` struct, after `cost`/`avoided`/`money`:

```go
	// context is the CONTEXT gauge's answer for this session: the largest main-agent prompt
	// total seen, maintained by Append and read by ListSessions.
	//
	// A REMEMBERED MAXIMUM, NOT A SUM, and that is the whole difference from cost above. It is
	// deliberately NOT maintained in lockstep with Events: a trim does nothing to it, because a
	// maximum over a trimmed slice does not understate the way a partial sum does — it reports a
	// small conversation when the conversation is large and merely aged out. See
	// TestAppend_PromptContextSurvivesATrim, and pipeline.PromptContextFold for the rule.
	//
	// So this needs none of the machinery cost needs: no subtraction on trim, and no parallel
	// per-event slice to make that subtraction decode-free. Fixed size per SESSION.
	context pipeline.PromptContextFold
```

In `Append`, in the lockstep block right after `sess.avoided.Add(money.avoided)`:

```go
	// AND THE PROMPT-CONTEXT FIGURE, which unlike the two above sheds nothing on trim.
	//
	// BEFORE THE TRIM BLOCK BELOW, load-bearing rather than incidental: an event appended and
	// immediately evicted still has to contribute, because the figure outlives the events it was
	// read from.
	//
	// NOT HOISTED ABOVE THE LOCK like moneyOf, and that is not an oversight. moneyOf is a
	// json.Unmarshal and was hoisted because of a measured regression; this is a phase check, a
	// nil check, len(Tools), three int adds and one comparison — tens of nanoseconds, no
	// allocation, NO DECODE. Splitting it to hoist the extraction would add an exported type for
	// plumbing alone and save nothing measurable.
	sess.context.Add(&event)
```

Add `PromptContext *pipeline.PromptContext` to `SessionSummary` and `PromptContext: sess.context.Publish()` to `ListSessions` — Task 7 writes the doc comments; this task needs the field only so the test compiles.

- [ ] **Step 4: Run it green, plus the whole session package**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/session/ -count=1 > $LOG_DIR/t6-green.log 2>&1; echo "EXIT:$?"
```

Expected `0`, **including `TestAppend_RunningTotalsMatchAFullRecomputation`** — cost's invariant must be untouched.

- [ ] **Step 5: Add the positive maintenance test and the eviction test**

```go
// The ordinary path: Append maintains the figure, ListSessions reports it, and a session with
// nothing to say reports nil rather than zero.
func TestAppend_MaintainsThePromptContextFigure(t *testing.T) {
	s := New(time.Hour, 0, 0)
	base := time.Now()
	for _, e := range promptContextTurn("c1", base, 600, 27, 500_000) {
		s.Append("agentic", e)
	}
	for _, e := range promptContextTurn("o1", base, 3, 0, 282_000) {
		s.Append("oneshot", e)
	}

	for _, sum := range s.ListSessions() {
		switch sum.ID {
		case "agentic":
			if sum.PromptContext == nil || sum.PromptContext.Tokens != 500_000 {
				t.Errorf("agentic: %+v, want tokens=500000", sum.PromptContext)
			}
			if sum.PromptContext != nil && !sum.PromptContext.Stated {
				t.Error("agentic: Stated=false, but the fixture declares AgentRoleMain")
			}
		case "oneshot":
			if sum.PromptContext != nil {
				t.Errorf("oneshot: %+v, want nil — no manifest means no conversation to measure",
					sum.PromptContext)
			}
		}
	}
}

// A session the store forgets entirely has no figure: the entry goes, and the fold with it. A
// session recreated under the same id starts fresh, consistent with nextSeq restarting at 1.
func TestPromptContext_WholeEntryEvictionDropsTheFigure(t *testing.T) {
	s := New(time.Hour, 0, 1) // maxSessions=1
	base := time.Now()
	for _, e := range promptContextTurn("c1", base, 600, 27, 500_000) {
		s.Append("first", e)
	}
	for _, e := range promptContextTurn("c2", base.Add(time.Minute), 600, 27, 100_000) {
		s.Append("second", e)
	}
	for _, sum := range s.ListSessions() {
		if sum.ID == "first" {
			t.Error("the evicted session is still listed; this test is not exercising eviction")
		}
	}
	for _, e := range promptContextTurn("c3", base.Add(2*time.Minute), 3, 0, 9_000) {
		s.Append("first", e)
	}
	for _, sum := range s.ListSessions() {
		if sum.ID == "first" && sum.PromptContext != nil {
			t.Errorf("recreated session carried a figure forward: %+v", sum.PromptContext)
		}
	}
}
```

- [ ] **Step 6: Run, format, lint, commit**

---

## Task 7: Document and wire-test `SessionSummary.PromptContext`

**Files:**
- Modify: `authlib/session/store.go` (`SessionSummary` ~line 563, `ListSessions` ~line 627)
- Modify: `authlib/session/store_test.go`

- [ ] **Step 1: Write the failing JSON round-trip test**

```go
// NIL MUST SERIALIZE AS AN ABSENT FIELD, not as {"tokens":0}. A client reading zero would draw
// an empty track where the column's contract is an em dash, and "barely used" and "not known"
// are different answers this column has to keep apart.
func TestSessionSummary_PromptContextOmittedWhenNil(t *testing.T) {
	b, err := json.Marshal(SessionSummary{ID: "s"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "promptContext") {
		t.Errorf("a nil figure serialized as %s, want the field absent", b)
	}

	at := time.Now().UTC().Truncate(time.Second)
	b, err = json.Marshal(SessionSummary{
		ID:            "s",
		PromptContext: &pipeline.PromptContext{Tokens: 851_000, Stated: true, At: at},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back SessionSummary
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PromptContext == nil {
		t.Fatal("the figure did not survive the round trip")
	}
	if back.PromptContext.Tokens != 851_000 || !back.PromptContext.Stated ||
		!back.PromptContext.At.Equal(at) {
		t.Errorf("round-tripped to %+v", back.PromptContext)
	}
}
```

- [ ] **Step 2: Run it, confirm it fails on the missing tag or field**

- [ ] **Step 3: Write the field's doc comment to the density of its neighbour**

`CostMicros` carries 19 lines. This needs comparable treatment:

```go
	// PromptContext is how full this session's conversation got, by the rule in
	// pipeline.PromptContextFold: the largest main-agent request seen, in prompt tokens.
	//
	// LIFETIME-MAX, NOT SCOPED TO WHAT THE STORE HOLDS — unlike TotalTokens and CostMicros
	// above, and the asymmetry is deliberate rather than an inconsistency. Those are sums, and a
	// sum over a trimmed slice understates by a known amount, which is why CostMicros can
	// honestly call itself the cost of the events in this session. This is a maximum, and a
	// maximum over a trimmed slice does not understate — it reports a small conversation when the
	// conversation is large and merely aged out of the store. A client showing the three on one
	// row must not present any of them as a check on another.
	//
	// A POINTER SO ABSENT AND ZERO STAY APART, which is the same standing rule CostMicros states
	// for its omitempty: an unknown figure must never render as a real one. A session with only
	// one-shot completions has no conversation to measure and is indistinguishable here from one
	// nobody observed; both must reach a client as an absent field so it can draw an em dash.
	//
	// RESETS ON PROXY RESTART, like CostMicros and unlike a cost-ledger window, because the store
	// is in-memory per-pod. A client that has been watching longer than this proxy has been up may
	// hold a larger figure legitimately — see pipeline.Merge, which is how the two combine.
	PromptContext *pipeline.PromptContext `json:"promptContext,omitempty"`
```

And in `ListSessions`, beside the existing `// Read, not computed. Append maintains these`:

```go
		// Read, not computed — and unlike TotalTokens above this is NOT a walk. Folding the rule
		// here instead would repeat the mistake entry.cost documents: O(events) per session
		// under the read lock, on abctl's two-second poll, in front of a lock whose writer side
		// is Append on the proxy's request path, with maxEvents unset by default.
		PromptContext: sess.context.Publish(),
```

- [ ] **Step 4: Run the package green, then confirm the endpoint actually carries it**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./authlib/session/ ./authlib/sessionapi/ -count=1 > $LOG_DIR/t7.log 2>&1; echo "EXIT:$?"
```

`sessionapi/server.go:335` serializes `[]session.SessionSummary` directly, so no change is needed
there. Confirm that is still true:

```bash
grep -n "SessionSummary" authlib/sessionapi/server.go
```

- [ ] **Step 5: Format, lint, commit**

---

# Phase D — the client

## Task 8: Merge the server's figure into the gauge

**Files:**
- Modify: `cmd/abctl/tui/sessions_context.go` (`sessionContextFor`)
- Modify: `cmd/abctl/tui/sessions_pane.go:277` and `:318`
- Modify: `cmd/abctl/tui/sessions_context_wire_test.go`

**Interfaces:**
- Consumes: `pipeline.Merge`, `pipeline.PromptContext`, `session.SessionSummary.PromptContext`
- Produces: `(m *model) sessionContextFor(id string, server *pipeline.PromptContext) int`

- [ ] **Step 1: Write the failing headline test — the actual bug, on the rendered row**

Reuse `heldContextCell` and `assertGaugeFilled`, already in this file from PR #1102.

```go
// THE BUG THIS WHOLE CHANGE EXISTS FOR: a session idle since before abctl attached shows a gauge
// on the first /v1/sessions poll, with no Enter, no snapshot and no wait.
//
// Asserted on the RENDERED ROW rather than on sessionContextFor, because a correct figure that
// nothing paints is the defect PR #1102 fixed and this file's own tests missed.
func TestSessionsTable_AnIdleRowShowsTheServersFigureWithoutBeingOpened(t *testing.T) {
	base := time.Now()
	const id = "idle"
	m := &model{width: 200, pane: paneSessions, events: map[string][]pipeline.SessionEvent{}}
	m.sessionsTbl = newSessionsTable()

	// What the poll delivers: a row abctl holds no events for, carrying the server's figure.
	m.Update(sessionsLoadedMsg([]session.SessionSummary{{
		ID: id, UpdatedAt: base, EventCount: 1509,
		PromptContext: &pipeline.PromptContext{Tokens: 851_000, Stated: true, At: base},
	}}))

	assertGaugeFilled(t, heldContextCell(t, m, id), "on the first poll")
}

// AND THE PROXY-UPGRADE REGRESSION, which is why PromptContext carries Stated.
func TestSessionsTable_AStatedServerFigureBeatsAStaleUnstatedLocalOne(t *testing.T) {
	base := time.Now()
	const id = "s"
	// abctl's own figure, folded from a proxy that stated no roles: the documented
	// stale-fallback case, 700k held from before a compaction.
	m := &model{width: 200, pane: paneSessions, events: map[string][]pipeline.SessionEvent{
		id: conversation("pre", base.Add(-time.Hour), 2468, 700_000),
	}}
	m.sessionsTbl = newSessionsTable()
	if got := m.sessionContextFor(id, nil); got != 700_000 {
		t.Fatalf("local figure is %d, want 700000 — the fixture is not exercising the fallback", got)
	}

	server := &pipeline.PromptContext{Tokens: 200_000, Stated: true, At: base}
	if got := m.sessionContextFor(id, server); got != 200_000 {
		t.Errorf("merged to %d, want 200000 — a stated figure beats an unstated one at any "+
			"size, so max() over token counts is not the rule", got)
	}
}

// An old proxy sends nothing, and nothing must not blank a row abctl can answer for itself.
func TestSessionsTable_ANilServerFigureKeepsTheLocalOne(t *testing.T) {
	base := time.Now()
	const id = "s"
	m := &model{width: 200, events: map[string][]pipeline.SessionEvent{
		id: conversation("c1", base, 600, 500_000),
	}}
	m.sessionsTbl = newSessionsTable()
	if got := m.sessionContextFor(id, nil); got != 500_000 {
		t.Errorf("got %d, want 500000", got)
	}
}
```

- [ ] **Step 2: Run them, confirm the first fails on the dash and the second on the signature**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./cmd/abctl/tui/ -run 'TestSessionsTable_(AnIdleRowShows|AStatedServerFigure|ANilServerFigure)' -v > $LOG_DIR/t8-red.log 2>&1; echo "EXIT:$?"
```

- [ ] **Step 3: Thread the server's figure through**

```go
// sessionContextFor is the gauge's figure for one session: abctl's own remembered fold merged
// with whatever the server published for that row.
//
// NEITHER SOURCE DOMINATES, which is why this merges rather than preferring one. The server has
// seen everything since the PROXY started; abctl only since IT attached, which is usually less —
// but abctl's copy survives a proxy restart, and destroying a figure it still holds because the
// server forgot is #870's shape. pipeline.Merge resolves it by the rule rather than by size: a
// stated figure beats an unstated one at any magnitude.
//
// server is nil for a proxy older than the field and for a session with no conversation to
// measure. Both mean "nothing known", both are the merge's identity, and that is what lets this
// need no version detection at all.
func (m *model) sessionContextFor(id string, server *pipeline.PromptContext) int {
	return pipeline.Merge(server, m.localContextFor(id)).TokensOrZero()
}

// localContextFor is the fold abctl maintains itself, unchanged from before the server published
// anything — see the retention inventory on model.events for why it is a remembered maximum.
func (m *model) localContextFor(id string) *pipeline.PromptContext {
	events := m.events[id]
	run, ok := m.contextRun[id]
	switch {
	case ok && run.Folded() == len(events):
		return run.Publish()
	case ok && run.Folded() < len(events):
		run.AddAll(events[run.Folded():])
		m.contextRun[id] = run
	default:
		m.rebaseSessionContext(id, events)
		return m.contextRun[id].Publish()
	}
	return run.Publish()
}
```

Add to `promptcontext.go`, so the call site needs no nil check:

```go
// TokensOrZero is the figure, or zero when nothing is known — which contextGauge renders as an
// em dash. A method on the pointer so a merge result can be read without a nil guard at every
// call site.
func (p *PromptContext) TokensOrZero() int {
	if p == nil {
		return 0
	}
	return p.Tokens
}
```

Update both call sites in `sessions_pane.go`:

```go
// line 277, inside the m.sessions loop — the summary is in hand
row = append(row, padLeft(contextGauge(m.sessionContextFor(s.ID, s.PromptContext), contextW), contextW))

// line 318, the cached-only rows — the server does not list these at all, so there is no
// summary and no published figure; abctl's own copy is the only source
row = append(row, padLeft(contextGauge(m.sessionContextFor(id, nil), contextW), contextW))
```

Then fix the other callers the compiler finds:

```bash
grep -rn "sessionContextFor(" --include=*.go cmd/abctl/tui/ | grep -v "func (m \*model)"
```

Existing tests call it with one argument; pass `nil` for all of them — they predate the server
figure and are about the local fold.

- [ ] **Step 4: Run the full tui package**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./cmd/abctl/tui/ -count=1 > $LOG_DIR/t8-green.log 2>&1; echo "EXIT:$?"
```

Expected `0`. **The three PR #1102 repaint tests must still pass** — this change does not remove
the need for them, since an old proxy still leaves the open-a-session path as the only source.

- [ ] **Step 5: Whole-tree verification**

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go build ./... > $LOG_DIR/final-build.log 2>&1; echo "BUILD:$?"
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= go test ./... > $LOG_DIR/final-test.log 2>&1; echo "TEST:$?"
grep -E "^(FAIL|--- FAIL)" $LOG_DIR/final-test.log
```

Expected: only `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` fails (pre-existing, environmental — see Global Constraints).

- [ ] **Step 6: Format, lint, commit, push, open the PR**

```bash
gofmt -l $(git diff --name-only upstream/main | grep '\.go$')
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= golangci-lint run --new-from-rev=upstream/main ./authlib/... ./cmd/abctl/... > $LOG_DIR/final-lint.log 2>&1; echo "LINT:$?"
git log --grep="^Co-[Aa]uthored-[Bb]y:" --grep="Generated with" upstream/main..HEAD --oneline
```

The last command must print nothing. Then:

Write the PR body to `$LOG_DIR/pr.md` first. It must cover, in this order: the symptom (the
column reports abctl's uptime, not the session); that the data was never missing (`ListSessions`
already walks the events that hold it); the approach and why not `ListSessions` (the regression
`entry.cost` documents); the trim invariance and its inverted test; the reachable
proxy-upgrade hole that a bare `max` left open; what changes on screen; and that PR #1102 is
complementary rather than superseded. End it with the attribution line.

```bash
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= git push -u origin feat/session-prompt-context
HTTPS_PROXY= HTTP_PROXY= ALL_PROXY= gh pr create --repo rossoctl/cortex --base main \
  --head huang195:feat/session-prompt-context \
  --title "Feat: Publish a per-session prompt-context figure on /v1/sessions" \
  --body-file $LOG_DIR/pr.md
```

Three things that will otherwise bite:

- The PR title prefix must be **capitalized** (`Feat:`) — the title check is case-sensitive and
  rejects `feat:`, even though commit messages use lowercase.
- The PR is **cross-repository**: the fork is `huang195/kagenti-extensions` but the base repo is
  `rossoctl/cortex`, hence the explicit `--repo` and `--head owner:branch`.
- Any `gh api` **write** must spell the path `repos/rossoctl/cortex/...`. The old repo name
  307-redirects, and `gh api` does not follow redirects on writes — reads work, so the mismatch
  only surfaces at submit time.

The body ends with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`, never
"Generated with".

---

## Verification checklist

- [ ] `go build ./...` clean
- [ ] `go test ./...` clean except the known environmental failure
- [ ] The 13 moved rule tests pass in `authlib/pipeline`
- [ ] `TestAppend_RunningTotalsMatchAFullRecomputation` still passes — cost's invariant untouched
- [ ] The three PR #1102 repaint tests still pass
- [ ] Both monoid law tests pass
- [ ] `gofmt -l` on changed files prints nothing
- [ ] `golangci-lint --new-from-rev=upstream/main` clean
- [ ] Every commit has `Signed-off-by` and `Assisted-By`; none has `Co-Authored-By`
- [ ] `git log --format='%an <%ae>'` shows a real account, not a placeholder

## Two risks flagged in the spec review, to resolve during implementation

1. **Task 5's coarsening argument.** The spec claims a client never reaches the degraded
   unstated-vs-unstated `Merge` arm, because any proxy publishing `PromptContext` also publishes
   `agentRole`. That is an inference about version coupling, not a verified fact. While in
   Task 6, check whether `agentRole` is populated unconditionally by the inference parser. If it
   can be absent on a proxy new enough to publish this field, `msgs` needs publishing after all —
   stop and report rather than proceeding.
2. **Task 6's trim test.** It must force a trim through the exported API. `maxEvents` is settable
   via `New(ttl, maxEvents, maxSessions)`, so this should work — but if `planTrim`'s intent-pin
   rule keeps more than expected, the one-shot count needs raising. The test's second assertion
   catches that case and says so.
