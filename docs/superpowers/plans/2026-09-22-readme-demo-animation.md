# README Demo Animation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Generate a ~50s animated SVG demo of cortex for the README, from a YAML storyboard, with the abctl screens rendered by the real TUI.

**Architecture:** A standalone Go module reads `demo.yaml`, captures a list of `State`s (one full screen each) from two producers — authored shell text, and the real Bubble Tea TUI driven headlessly — then emits one animated SVG whose groups share a single absolute CSS timeline. Capture and emission are separate stages so a GIF fallback would replace only the emitter.

**Tech Stack:** Go 1.26.5, `charmbracelet/bubbletea` v1.3.10, `lipgloss` v1.1.0, `charmbracelet/x/ansi`, `gopkg.in/yaml.v3`, headless Chrome for verification.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-22-readme-demo-animation-design.md`

## Global Constraints

- Grid is **100 columns × 32 rows**. Every state is exactly this size.
- Total runtime **50s**: act 1 = 9s, act 2 = 5s, act 3 = 6s, act 4 = 30s.
- Asset budget **500KB** for `docs/assets/cortex-demo.svg`.
- The generator reads `demo.yaml` and nothing else. **No** access to `~/.claude`, `~/.cortex`, or a live proxy — no real prompt or cost may reach a public asset.
- Pin rendering determinism: `lipgloss.SetColorProfile(termenv.TrueColor)` and `lipgloss.SetHasDarkBackground(true)` before any capture.
- Generator module must build with `GOWORK=off` (as CI runs) — needs `replace` directives for the authbridge modules.
- Commits: `-s` for DCO, and `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.
- All work happens in the `.worktrees/readme-demo` worktree on branch `feat/readme-demo`.

---

### Task 0: Spike — prove animated SVG survives GitHub

The design's one unverified assumption. Do this before writing any generator code; a failure here switches stage 4 to a GIF encoder.

**Files:**
- Create: `docs/assets/spike-animation.svg` (throwaway, deleted in Task 8)

- [ ] **Step 1: Hand-write a minimal 3-state animated SVG**

40 lines, no generator. Three `<g>` groups of monospace `<text>`, each visible for one third of a 3s loop via CSS `@keyframes` on `visibility`, plus one `clipPath` rect animating width to prove the typing reveal renders. Include a `@media (prefers-reduced-motion: reduce)` block that pins the last group visible.

- [ ] **Step 2: Verify locally with headless Chrome at three timestamps**

```bash
cat > /tmp/spike.html <<'EOF'
<body style="margin:0;background:#111"><img src="spike-animation.svg" width="1000"></body>
EOF
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
for t in 500 1500 2500; do
  "$CHROME" --headless --disable-gpu --force-device-scale-factor=2 \
    --virtual-time-budget=$t --window-size=1000,400 \
    --screenshot=/tmp/spike-$t.png file:///tmp/spike.html
done
```

Expected: three visibly different PNGs. `--virtual-time-budget` advances the animation before the screenshot; if all three are identical, the animation is not running and the SVG is wrong (fix before proceeding).

- [ ] **Step 3: Commit and push the branch**

```bash
git add docs/assets/spike-animation.svg && git commit -s -m "chore: Spike animated SVG rendering"
git push -u origin feat/readme-demo
```

- [ ] **Step 4: Confirm it animates in a rendered GitHub README**

Add `<img src="docs/assets/spike-animation.svg">` to the README on this branch, push, and open the branch's README on github.com. **Gate:** if it animates, continue to Task 1. If it renders as a still, stop and re-plan stage 4 as a GIF encoder.

---

### Task 1: Module scaffold and storyboard parsing

**Files:**
- Create: `authbridge/scripts/readme-demo/go.mod`, `script.go`, `script_test.go`, `demo.yaml`
- Modify: `authbridge/go.work`

**Interfaces:**
- Produces:
```go
type Script struct {
    Total time.Duration `yaml:"total"`
    Grid  Grid          `yaml:"grid"`   // Cols, Rows
    Acts  []Act         `yaml:"acts"`
}
type Act struct {
    Kind     string        `yaml:"kind"`      // "shell" | "windows" | "tui"
    Runtime  time.Duration `yaml:"runtime"`
    Steps    []Step        `yaml:"steps"`     // kind: shell
    Windows  []Window      `yaml:"windows"`   // kind: windows
    Sessions []Session     `yaml:"sessions"`  // kind: tui
    Beats    []Beat        `yaml:"beats"`     // kind: tui
}
type Step struct {
    Cmd  string          `yaml:"cmd"`
    Out  []string        `yaml:"out"`
    Slow []time.Duration `yaml:"slow"`   // per-line override, AX's trick
}
type Beat struct {
    Keys []string      `yaml:"keys"`
    Hold time.Duration `yaml:"hold"`
}
func Load(path string) (*Script, error)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestLoad_RuntimesMustSumToTotal(t *testing.T) {
    // total: 50s with acts of 9s+5s+6s+10s must be rejected
    _, err := Load(writeTemp(t, badSumYAML))
    if err == nil || !strings.Contains(err.Error(), "sum") {
        t.Fatalf("want sum error, got %v", err)
    }
}

func TestLoad_RejectsUnknownKind(t *testing.T) {
    _, err := Load(writeTemp(t, `total: 1s
acts: [{kind: bogus, runtime: 1s}]`))
    if err == nil { t.Fatal("want error for unknown act kind") }
}

func TestLoad_SlowLongerThanOutIsRejected(t *testing.T) {
    // A slow array longer than out silently does nothing; catch it.
}
```

- [ ] **Step 2: Run to verify they fail** — `go test ./... -run TestLoad` → FAIL (no `Load`)
- [ ] **Step 3: Write `go.mod` with replace directives**

```
module github.com/rossoctl/cortex/authbridge/scripts/readme-demo
go 1.26.5
require (
	github.com/rossoctl/cortex/authbridge/authlib v0.0.0
	github.com/rossoctl/cortex/authbridge/cmd/abctl v0.0.0
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/lipgloss v1.1.0
	gopkg.in/yaml.v3 v3.0.1
)
replace github.com/rossoctl/cortex/authbridge/authlib => ../../authlib
replace github.com/rossoctl/cortex/authbridge/cmd/abctl => ../../cmd/abctl
```

Add `./scripts/readme-demo` to `authbridge/go.work`.

- [ ] **Step 4: Implement `Load` with validation** — parse, then validate: known kinds, runtimes sum to total, `len(Slow) <= len(Out)`, grid non-zero.
- [ ] **Step 5: Run tests** → PASS. Also `GOWORK=off go build ./...` must succeed.
- [ ] **Step 6: Commit**

---

### Task 2: SGR decode

`View()` returns lipgloss ANSI; the emitter needs styled runs.

**Files:**
- Create: `authbridge/scripts/readme-demo/sgr.go`, `sgr_test.go`

**Interfaces:**
- Produces:
```go
type Run struct {
    Text string
    FG   string // "#rrggbb" or "" for default
    BG   string
    Bold bool
    Dim  bool
}
func DecodeLine(s string) []Run   // splits one line into styled runs, strips escapes
```

- [ ] **Step 1: Write the failing tests** — build inputs with lipgloss itself so the test tracks the real encoder, not a guess:

```go
func TestDecodeLine_LipglossForeground(t *testing.T) {
    lipgloss.SetColorProfile(termenv.TrueColor)
    in := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff8800")).Render("SESSION")
    runs := DecodeLine(in)
    if len(runs) != 1 || runs[0].Text != "SESSION" || runs[0].FG != "#ff8800" {
        t.Fatalf("got %+v", runs)
    }
}

func TestDecodeLine_PlainTextIsOneRun(t *testing.T)      { /* no escapes → single default run */ }
func TestDecodeLine_ResetEndsStyle(t *testing.T)         { /* styled + plain → two runs */ }
func TestDecodeLine_TotalTextEqualsVisibleWidth(t *testing.T) {
    // concatenated run text must equal ansi.Strip(in) — the invariant that
    // guarantees the emitted grid keeps its column alignment.
}
```

- [ ] **Step 2: Run to verify they fail**
- [ ] **Step 3: Implement using `charmbracelet/x/ansi`** to tokenize; handle SGR 0/1/2/38;2;r;g;b/48;2;r;g;b/39/49 and the 16 basic colors.
- [ ] **Step 4: Run tests** → PASS
- [ ] **Step 5: Commit**

---

### Task 3: State model and the shell producer

**Files:**
- Create: `authbridge/scripts/readme-demo/state.go`, `shell.go`, `shell_test.go`

**Interfaces:**
- Consumes: `Script`, `Step` (Task 1); `Run`, `DecodeLine` (Task 2)
- Produces:
```go
type Row struct {
    Runs   []Run
    Reveal Reveal // how this row appears
}
type Reveal struct {
    Kind  string        // "instant" | "type" | "fill"
    At    time.Duration // offset from the state's start
    Dur   time.Duration // 0 for instant
}
type State struct {
    Rows  []Row
    At    time.Duration // absolute offset on the 50s timeline
    Dur   time.Duration
    Chrome *WindowChrome // nil for full-screen TUI states
}
func ShellStates(a Act, start time.Duration, g Grid) ([]State, error)
func WindowStates(a Act, start time.Duration, g Grid) ([]State, error)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestShellStates_TypingRowCarriesTypeReveal(t *testing.T) {
    // A Step with Cmd "abctl" must produce a row whose Reveal.Kind == "type"
    // and whose Dur is proportional to len(Cmd).
}
func TestShellStates_SlowOverrideDelaysThatLineOnly(t *testing.T) {
    // slow: [0, 2500ms] must push row 2's Reveal.At out by 2500ms and
    // leave row 1 alone. This is the AX "Downloading" hang.
}
func TestShellStates_TotalNeverExceedsActRuntime(t *testing.T) {
    // The last row's At+Dur must fit inside Act.Runtime.
}
func TestWindowStates_ThreeWindowsStackWithoutOverlap(t *testing.T) {
    // Three windows in a 32-row grid must occupy disjoint row ranges.
}
```

- [ ] **Step 2: Run to verify they fail**
- [ ] **Step 3: Implement** — typing at 45ms/char (AX uses 35–80ms jitter; a fixed rate keeps the asset deterministic for the staleness check), 350ms beat after a command, 120ms default per output line, `Slow[i]` overriding line `i`.
- [ ] **Step 4: Run tests** → PASS
- [ ] **Step 5: Commit**

---

### Task 4: TUI producer — capture the real abctl

**Files:**
- Create: `authbridge/scripts/readme-demo/tuicapture.go`, `tuicapture_test.go`

**Interfaces:**
- Consumes: `Act`, `Session`, `Beat` (Task 1); `DecodeLine` (Task 2); `State` (Task 3)
- Produces: `func TUIStates(a Act, start time.Duration, g Grid) ([]State, error)`

- [ ] **Step 1: Write the failing test** — this is the task's whole point, so assert on real rendered content:

```go
func TestTUIStates_FirstStateIsTheSessionsTable(t *testing.T) {
    states, err := TUIStates(actFromFixture(t), 0, Grid{100, 32})
    if err != nil { t.Fatal(err) }
    text := plain(states[0]) // concatenate run text per row
    for _, want := range []string{"SESSION", "TOKENS", "COST", "SAVED~", "CONTEXT(1M)"} {
        if !strings.Contains(text, want) {
            t.Errorf("sessions table missing %q\n%s", want, text)
        }
    }
}

func TestTUIStates_BeatKeysAdvanceThePane(t *testing.T) {
    // A beat with keys ["enter"] must produce a later state whose text
    // contains the events-pane headers (TIME, ACTION, HOST) and not SAVED~.
}

func TestTUIStates_NoRealFilesRead(t *testing.T) {
    // Set HOME to a temp dir with nothing in it; capture must still succeed.
    // Guards the spec's "fixtures only, by construction" constraint.
}
```

- [ ] **Step 2: Run to verify they fail**
- [ ] **Step 3: Implement the headless driver**

```go
// 1. store := session.New(5*time.Minute, 0, 100)
// 2. srv := sessionapi.New(":0", store, ...); ts := httptest.NewServer(srv.Server().Handler)
// 3. for each fixture session: store.Append(id, pipeline.SessionEvent{...})
// 4. m := tui.New(ctx, apiclient.New(ts.URL))
// 5. pump(): run m.Init()'s Cmd, expand tea.BatchMsg, feed each msg to m.Update,
//    repeat until the queue drains (cap iterations; fail loudly on overrun)
// 6. m.Update(tea.WindowSizeMsg{Width: g.Cols, Height: g.Rows}); pump()
// 7. capture m.View() → DecodeLine per row → State
// 8. for each beat: m.Update(tea.KeyMsg{...}) for each key; pump(); capture
```

Pin the color profile first (Global Constraints) or CI and laptop diverge.

- [ ] **Step 4: Run tests** → PASS
- [ ] **Step 5: Commit**

---

### Task 5: SVG emitter

**Files:**
- Create: `authbridge/scripts/readme-demo/svg.go`, `svg_test.go`

**Interfaces:**
- Consumes: `[]State` (Tasks 3, 4)
- Produces: `func EmitSVG(w io.Writer, states []State, total time.Duration, g Grid) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestEmitSVG_IsWellFormedXML(t *testing.T) {
    var buf bytes.Buffer
    if err := EmitSVG(&buf, fixtureStates(), 50*time.Second, Grid{100,32}); err != nil { t.Fatal(err) }
    d := xml.NewDecoder(&buf)
    for { _, err := d.Token(); if err == io.EOF { break }; if err != nil { t.Fatal(err) } }
}

func TestEmitSVG_StateWindowsTileTheTimeline(t *testing.T) {
    // Parse the emitted keyframe percentages; each state's visible window must
    // start where the previous ended — no gap (blank frame) and no overlap
    // (two states drawn at once).
}

func TestEmitSVG_EscapesMarkupInTerminalText(t *testing.T) {
    // A row containing `<script>` and `&` must appear escaped, never raw.
}

func TestEmitSVG_EveryRowCarriesTextLength(t *testing.T) {
    // Guards grid shear on readers whose default monospace differs.
}

func TestEmitSVG_HasReducedMotionBlock(t *testing.T) {
    // prefers-reduced-motion must pin the final state visible.
}
```

- [ ] **Step 2: Run to verify they fail**
- [ ] **Step 3: Implement** — one `<style>` with per-state `@keyframes`; each state a `<g>` with `animation: <name> 50s infinite`; each row one `<text x=0 y=n*lineHeight textLength=... lengthAdjust="spacingAndGlyphs" xml:space="preserve">` with a `<tspan>` per run; typing rows wrapped in a `clipPath` whose rect width animates stepwise; a blinking cursor `<rect>`.
- [ ] **Step 4: Run tests** → PASS
- [ ] **Step 5: Commit**

---

### Task 6: Wire main, author demo.yaml, generate the asset

**Files:**
- Create: `authbridge/scripts/readme-demo/main.go`
- Modify: `authbridge/scripts/readme-demo/demo.yaml`
- Create: `docs/assets/cortex-demo.svg`

- [ ] **Step 1: Implement `main.go`** — `-script` (default `demo.yaml`), `-out` (default `../../../docs/assets/cortex-demo.svg`); load, dispatch each act to its producer, concatenate states, emit.
- [ ] **Step 2: Author the full `demo.yaml`** — all four acts per the spec's storyboard. Exact strings from `install.sh:376,892,1048,1157` and `cmd_claudecode.go:462,464,724,495`. Act 4 fixture numbers must be arithmetically self-consistent: each session's `COST` equals the sum of its tiers, and tier percentages match the token counts.
- [ ] **Step 3: Generate** — `go run . && ls -la ../../../docs/assets/cortex-demo.svg`. Expected: under 500KB.
- [ ] **Step 4: Verify the animation at six timestamps with headless Chrome**

```bash
for t in 3000 11000 15000 22000 30000 42000; do
  "$CHROME" --headless --disable-gpu --force-device-scale-factor=2 \
    --virtual-time-budget=$t --window-size=1100,760 \
    --screenshot=/tmp/demo-$t.png file:///tmp/demo.html
done
```

Read each PNG. Expected: t=3s mid-install, t=11s the consent prompt, t=15s three windows, t=22s the sessions table, t=30s the spend tiers, t=42s the payload. A timestamp landing on the wrong act means the timeline arithmetic is off.

- [ ] **Step 5: Commit**

---

### Task 7: Staleness check and CI

**Files:**
- Create: `authbridge/scripts/readme-demo/staleness_test.go`
- Modify: `.github/workflows/ci.yaml`

- [ ] **Step 1: Write the test**

```go
func TestCommittedAssetIsCurrent(t *testing.T) {
    // Regenerate from demo.yaml into a buffer; compare byte-for-byte with
    // docs/assets/cortex-demo.svg. On mismatch, fail with the exact
    // regeneration command so the fix is obvious:
    //   "docs/assets/cortex-demo.svg is stale — run: go -C authbridge/scripts/readme-demo run ."
}
```

- [ ] **Step 2: Run it** → PASS against the asset from Task 6. Then hand-edit one character of the committed SVG, re-run, confirm FAIL, and restore. A staleness test that cannot fail is worthless.
- [ ] **Step 3: Add the CI job** to `ci.yaml`, running with `GOWORK=off` like its siblings.
- [ ] **Step 4: Commit**

---

### Task 8: README wiring and spike cleanup

**Files:**
- Modify: `README.md`
- Delete: `docs/assets/spike-animation.svg`

- [ ] **Step 1: Add the image above the quick start** with alt text describing what the animation shows (screen-reader users get no frames).
- [ ] **Step 2: Delete the spike asset and its README reference**
- [ ] **Step 3: Push and confirm the real asset animates on the branch's rendered README**
- [ ] **Step 4: Commit**

---

## Self-Review

**Spec coverage:** surface/format → Task 0; storyboard acts 1–3 → Tasks 3, 6; act 4 → Tasks 4, 6; four-stage architecture → Tasks 1–5; determinism → Global Constraints + Task 4; safety (fixtures only) → `TestTUIStates_NoRealFilesRead`; placement → Tasks 1, 6, 8; testing table → Tasks 1–5, 7; CI → Task 7; size budget → Task 6 Step 3; GIF fallback → Task 0 gate.

**Type consistency:** `Run`/`DecodeLine` (Task 2) are consumed by name in Tasks 3–4; `State`/`Row`/`Reveal`/`Grid` (Tasks 1, 3) by Tasks 4–5; `EmitSVG` (Task 5) by Task 6. Names match across tasks.

**Known gap, deliberate:** the emitter's row-diffing optimization is not a task. It is the Task 6 Step 3 contingency if the asset exceeds 500KB, and building it unconditionally would be speculative.
