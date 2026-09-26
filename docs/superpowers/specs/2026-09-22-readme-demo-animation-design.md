# README Demo Animation — Design

**Date:** 2026-09-22
**Status:** Converged design — ready for implementation plan.
**Issue:** none filed.
**Repos touched:** `cortex` only (`authbridge/scripts/`, `docs/assets/`, `README.md`, CI).

> **Terminology.** *Act* = one of the four narrative segments. *State* = one full
> screen of text the emitter renders as an SVG group. *Reveal* = an animation
> within a state that uncovers text progressively (typing, row-by-row fill).
> *Frame* is deliberately avoided: the emitter does not emit one screen per tick.

## Problem

Cortex has no demo asset. The README opens with a strong claim — "See what your
coding agent actually sends — and pay less for it" — and then asks the reader to
install a binary to find out whether that's true. Nothing shows the product.

The seed for this work is the hero animation on <https://agentexecutor.io/>. It
is worth copying because of *how* it is built, not how it looks.

### What AX actually does

Read from the page source, not inferred:

- The hero's right column holds a `<pre class="term">` containing the **entire
  session pre-rendered as static HTML**. That is what a reader gets with JS
  disabled, with `prefers-reduced-motion: reduce`, and what search engines index.
- An inline ~120-line vanilla-JS IIFE wipes that `<pre>` and replays a
  hand-authored array: `steps = [{cmd, out: [...], slow: [...]}]`.
- Commands type character-by-character at 35–80ms with jitter (90ms on spaces),
  then a 350ms beat, then output lines at 120ms each.
- `slow` overrides per line. `ax watch task test` carries `slow: [0, 100, 4000, 150]`,
  so `Phase: Pending` hangs for **four real seconds** before `Phase: Running`.
- A blinking cursor span, auto-scroll-to-bottom, infinite loop with a 4.5s rest,
  and a Pause/Play button wired into the fake window's title bar.

**The load-bearing insight is that none of it is a recording.** It is fabricated,
which is why it weighs nothing, stays crisp at any zoom, is editable in a text
editor, and can compress a 30-second operation into four. This design keeps that
property and discards the delivery mechanism, which does not survive GitHub.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Surface | The GitHub README | Highest-traffic surface cortex has today. |
| Format | Animated SVG, referenced as an image | GitHub strips JS, so AX's approach cannot port directly. SVG keeps text as text: crisp at any zoom, a fraction of a GIF's bytes. |
| Frame source | Fabricated, never recorded | See above. Also: a real recording of a live proxy would spend real tokens and risk baking actual prompts and costs into a public asset. |
| abctl screens | Rendered by the **real** TUI from synthetic data | Hand-drawn ASCII of `abctl` would be tedious *and* subtly wrong. The real renderer is the cheaper path and it cannot drift. |
| Runtime | ~50s, looping, one asset | |
| Theme | One dark asset | Light/dark variants double the generation and review surface for little gain; terminal demos are conventionally dark. |

### Why SVG works on GitHub, and the one thing to verify

GitHub blocks *inline* `<svg>` in markdown. An SVG referenced as an image
(`<img src="docs/assets/cortex-demo.svg">`) is proxied through camo and rendered
by the browser **as an image**, which runs declarative CSS `@keyframes` and SMIL
while blocking scripts. So the animation must be expressed declaratively.

This is the design's only unverified assumption. **Step 0 of implementation is a
throwaway spike** (below) that proves it on a real rendered README before any
generator code is written.

## Storyboard

Total ~50s. Act 4 gets 60% of the runtime, because it is the only act that shows
the product.

| Act | Runtime | Content |
|---|---|---|
| 1. Install | 9s | The real one-liner and the real installer narrative. |
| 2. Claude Code setup | 5s | The installer's handoff to `abctl claude-code enable` and its consent prompt. Nothing typed. |
| 3. Three sessions | 6s | Three stacked mini-windows, each running `claude`. |
| 4. Insights | 30s | The observability arc. |

### Act 1 — install (9s)

Types the one-liner from the README, then the installer's genuine output,
compressed. Exact strings live in `authbridge/install.sh`; copy them rather than
paraphrase:

| Line | Source |
|---|---|
| `Resolving newest release...` | `install.sh:376` |
| `Downloading v0.9.3 for darwin/arm64...` | `install.sh:892` |
| `Installed abctl and authbridge-proxy to ~/.cortex/bin` | `install.sh:1048` |
| `Setting up the launchd service...` | `install.sh:1157` |

AX's timing trick belongs here: a ~2.5s hang on `Downloading` so the act reads as
work being done rather than a text dump.

### Act 2 — Claude Code setup (5s)

**Nothing is typed in this act.** `--claude-code` does not merely suggest the next
command — the installer *runs* `abctl claude-code enable` as a subprocess
(`install.sh:1227-1233`), and prints the "run this yourself" hint only when it is
**not** doing it for you (`install.sh:1219`). So with the README's one-liner, act 2
is the continuous tail of act 1's output, not a second command:

1. `Adds to the "env" block of ~/.claude/settings.json:` (`cmd_claudecode.go:462`)
2. the env block
3. `Nothing else in the file changes; a copy is kept as settings.json.bak` (`:464`)
4. `Apply? [y/N] ` (`:724`) — the cursor pauses, then types `y`
5. `Enabled — run \`claude\` as usual.` (`:495`)

Showing the consent prompt is deliberate. It is a trust beat: cortex asks before
touching your config. Keeping it as a handoff rather than a typed command is what
makes the act cost 5s instead of 10.

### Act 3 — three Claude Code sessions (6s)

Three stacked mini-windows, each with its own title bar (`~/work/api`,
`~/work/web`, `~/work/infra`), all visible at once, lighting up in turn at ~2s
each: `claude` starts and prints a line or two of work.

This act is not decoration: abctl names sessions from Claude Code metadata, so
these three working directories become the three named rows act 4 opens on. The
reader should recognise them.

### Act 4 — insights (30s)

**Plot: "Where did the money go?", answered with evidence.** A question the
reader actually arrives with, whose answer forces a walk through the timeline and
the payload — so the visibility story lands inside the cost story.

| Beat | Runtime | Screen | The insight |
|---|---|---|---|
| 1 | 5s | Sessions table fills: three sessions across `SESSION · TITLE · UPDATED · EVENTS · TOKENS · COST · SAVED~ · CONTEXT(1M)` | Three agents, what each costs, how much window each has left. |
| 2 | 5s | `$` expands the spend band into tiers — the pane's own heading is *"where the money went"* | `cache-read` dominates. Most people do not expect this. |
| 3 | 5s | `u` → usage chart, cost grouped by model | Cost per turn is **rising**, not flat. |
| 4 | 6s | `↵` → events timeline: model call → tool call → result, with `TOKENS`/`COST`/`HOST`/`STATUS` | Every call is visible, including CONNECT tunnels to hosts the reader did not expect. |
| 5 | 6s | `↵` → detail: scroll one model call's JSON | The whole conversation is re-sent from scratch every turn. **That is why** beats 2 and 3 look the way they do. |
| 6 | 3s | `esc` back to sessions, hold | Rest before the loop. |

Note `SAVED~`: the `~` is appended at render time (`sessions_pane.go`) because a
saving is always an estimate. The real renderer gets this right for free; a
hand-drawn screen would not.

## Architecture

Four stages. **The split between capture and emission is the design's insurance
policy**: if animated SVG fails on GitHub, the same captured states feed a GIF
encoder and only stage 4 is replaced.

```
demo.yaml ──► capture ──► []State ──► emit/svg ──► cortex-demo.svg
              (shell │ tui)             (emit/gif — fallback only)
```

### Stage 1 — `demo.yaml`

The storyboard as data, so editing the demo does not mean recompiling.

```yaml
total: 50s
acts:
  - kind: shell                 # acts 1, 2
    steps:
      - cmd: "curl -fsSL .../install.sh | sh -s -- --claude-code"
        out: ["Resolving newest release...", "Downloading v0.9.3 for darwin/arm64...", ...]
        slow: [0, 2500, 200, 200]     # per-line delay overrides, AX's trick
  - kind: windows               # act 3: stacked mini-windows
    windows:
      - {title: "~/work/api",   lines: [...]}
  - kind: tui                   # act 4: real abctl frames
    sessions: [{id: api, title: "fix handler", events: [...]}, ...]
    beats:
      - {keys: ["$"], hold: 5s}
```

### Stage 2 — capture → `[]State`

Two producers behind one interface. Each `State` carries styled rows plus the
reveal metadata the emitter needs (which rows type, which fill, and when).

The **shell** producer is straightforward: authored text plus typing metadata.

The **tui** producer drives the real TUI. `tui/e2e_test.go` already proves the
machinery; this does the same thing from outside the package using only exported
API (all four verified against the source):

| Need | API |
|---|---|
| Session store | `session.New(ttl, maxEvents, maxSessions) *Store` |
| API server | `sessionapi.New(addr, store, opts...) *Server` + `httptest` |
| The model | `tui.New(ctx, *apiclient.Client) tea.Model` |
| Synthetic traffic | `store.Append(sessionID, pipeline.SessionEvent{...})` |

**No PTY and no `teatest` (not a dependency).** `tea.Cmd` is `func() tea.Msg` and
`tea.BatchMsg` is `[]Cmd` — both exported — so a ~60-line synchronous runtime
drives it: call `Init()`, run returned commands, expand `BatchMsg`, feed results
back through `Update()`, and capture `View()` once the command queue drains. Fully
deterministic, no goroutine racing, no terminal.

Fix the grid with `tea.WindowSizeMsg{Width: 100, Height: 32}` and walk the panes
with `tea.KeyMsg`.

### Stage 3 — SGR decode

`View()` returns lipgloss's ANSI, so a tokenizer turns escape sequences into
styled runs. `github.com/charmbracelet/x/ansi` is already a direct dependency of
the abctl module.

### Stage 4 — SVG emitter

Two techniques carry the whole thing:

**Rows, not characters, with forced metrics.** Each row is one `<text>` with
`textLength` and `lengthAdjust="spacingAndGlyphs"`, so the grid stays aligned
regardless of which monospace font the reader happens to have. Without this the
columns shear on any machine whose default monospace differs.

**Typing is a clip, not per-character elements.** A `clipPath` rect whose width
animates stepwise across the row gives the identical visual to AX's per-character
typing at a fraction of the bytes, and the cursor block rides the clip edge.

Sequencing without JS: every state group animates on one absolute 50s timeline
with `infinite`, its visibility keyframes marking its own window. Groups cannot
drift because they share the timeline rather than chaining off each other.

A `prefers-reduced-motion: reduce` media query freezes the asset on act 4's final
state — the same courtesy AX extends, and it means the still image a
motion-sensitive reader sees is the product, not an install log.

### Determinism and safety

- **Pin the color profile.** lipgloss strips color when stdout is not a TTY, so
  the generator must call `lipgloss.SetColorProfile(termenv.TrueColor)` and
  `SetHasDarkBackground(true)` (both exist in v1.1.0, verified). Without this, CI
  and a developer laptop produce different assets and the staleness check below
  fails for the wrong reason.
- **Fixtures only, by construction.** The generator reads `demo.yaml` and nothing
  else — no `~/.claude`, no live proxy, no `~/.cortex`. There is no code path by
  which a real prompt or a real cost can reach a public asset.
- **The fixture must be arithmetically self-consistent.** Per-session `COST` has
  to equal the sum of its tiers, and the tier percentages have to match the token
  counts. Readers do reverse-engineer demo numbers.
- **Module wiring.** The generator's `go.mod` needs `replace` directives for the
  authbridge modules it imports, so it builds under `GOWORK=off` as CI runs
  (repo gotcha 1) rather than only inside the workspace.

## Placement

| Path | Role |
|---|---|
| `authbridge/scripts/readme-demo/` | Generator, its own Go module, added to `go.work` (follows the `scripts/profile-tags` precedent) |
| `authbridge/scripts/readme-demo/demo.yaml` | The storyboard |
| `docs/assets/cortex-demo.svg` | The committed asset — GitHub cannot build it |
| `README.md` | `<img>` with alt text, above the quick start |

## Testing

| Test | Guards |
|---|---|
| Script parsing | Malformed YAML, durations that do not sum to `total`. |
| SGR decode | Golden: a known lipgloss-styled string → expected styled runs. |
| Emitter invariants | Output parses as XML; state windows tile the timeline with no gap or overlap; total duration equals the sum of acts; all text is escaped. |
| Golden asset | A two-act toy script → committed expected SVG. |
| **Staleness** | Regenerate the real asset, compare to the committed copy, fail on difference. |

The staleness test is the important one. A fabricated demo is only trustworthy if
it cannot silently diverge from the UI it claims to show; this converts a TUI
change that would have made the demo a lie into a failing check.

## CI

Extend `ci.yaml` with a job that builds the generator and runs the staleness
test. No new workflow file.

## Risks

| Risk | Mitigation |
|---|---|
| Animated SVG does not animate through camo | **Step 0 spike**, before any generator code: commit a minimal 3-state animated SVG to a throwaway branch, view the rendered README, confirm. GIF fallback preserved by the capture/emit split. |
| Asset too large | Budget 500KB. Text is written once per state; ~15 states of a 100×32 grid is ~45KB of text plus markup. If exceeded, emit only rows that changed between consecutive states. GitHub serves gzipped. |
| Font metrics shear the grid | Per-row `textLength` + `lengthAdjust`. |
| Colors differ between CI and laptop | Pin the color profile explicitly. |
| Demo drifts from the real UI | Staleness check; abctl screens come from the real renderer. |
| Reader arrives mid-loop | Accepted. The 3s hold on act 4's final state plus a 50s cycle means the most likely landing frame is the product. |

## Implementation notes — where the build diverged from this design

Recorded because each of these was discovered by building, and the reasons outlive
the commit that found them.

| Design said | Built as | Why |
|---|---|---|
| Acts 1 and 2 are separate | One `shell` act of 14s | `--claude-code` *runs* `abctl claude-code enable` (`install.sh:1227-1233`) rather than suggesting it, so there is no second command to type. |
| State groups animate `visibility` | They animate `opacity` | `visibility` is inheritable but a descendant may re-declare `visible` and show through a hidden ancestor. Every row reveal does exactly that, which drew all nine states on top of each other. |
| Fixtures set `ToolCount` / `MessageCount` | They carry real `Tools` and `Messages` arrays | `sessionapi.summarizeEvent` recomputes both counts from `len()` and nils the arrays, so counts alone arrive as zero and the `CONTEXT(1M)` fold skips every response. `AgentRole: main` is required for the same reason. |
| Pump abandons overdue commands | It parks them and reads them later | Their goroutines hold the model's timer chain; dropping them permanently stops the 2s sessions refresh. |
| Events may be recorded after the server starts | All of them are recorded before it | The spend band's four spans poll on independent cadences, so late arrivals produced screens where `LAST 1H` exceeded `TODAY`. Per-session caches are warmed by drilling into each session instead. |
| Asset is byte-identical run to run | Clock-dependent strings are canonicalised first | Ages, the TIME column and ISO timestamps move every second, and their *width* shifts the padding of every cell after them. They are rewritten to fixed digits of the same length; the staleness check masks digit runs as a backstop. |
| Verify with `--virtual-time-budget` | Verify with a global negative `animation-delay` | `--virtual-time-budget` leaves an `<img>`-embedded SVG's animation clock at zero. Every screenshot came back identical, which looks exactly like proof that animated SVG does not work — the wrong conclusion from a broken measurement. |
| The staleness check compares the whole asset | It elides the usage-chart state | That pane plots a ten-minute window *ending now* on a continuous axis, so every bar's column is a function of the current second and the whole plot slides between runs. Minute-aligning the fixture does not help — the axis is continuous, not bucket-indexed. Its contract is asserted structurally instead (summary labels render, bars are drawn). |
| Clock canonicalisation preserves each match's length | Timestamps become one fixed string; ages become fixed text plus compensating padding | Length is exactly what varies. Go renders a UTC offset as `Z` and a local one as `-04:00`, which moved a `textLength` by 42px between a laptop and CI; and an age is six or seven characters depending on how long the capture took. |
| Digit runs are masked in the comparison as a backstop | Nothing is masked | Masking made `CONTEXT(1M)` and `CONTEXT(2M)` compare equal — the check could not see a renamed column, a reformatted figure or a changed count, which is most of what it exists to catch. Verified by mutating that label and confirming the check fails. |
| The tool manifest is reached by scrolling | Two page-downs (`f`) | The conversation is ~70 lines; arrow keys could not reach the manifest inside the beat. |

## Out of scope

- Light/dark variants — one dark asset.
- GIF emission — built only if step 0 fails.
- A landing page. The rossoctl web site and per-README-section assets are
  separate decisions.
- tool-prune as a narrative beat — deliberately cut. Act 4 is observability and
  insight, not a feature pitch for one plugin.
- Pause/Play control. AX has one because JS can offer it; declarative SVG cannot
  without scripting, and `prefers-reduced-motion` covers the accessibility need.
