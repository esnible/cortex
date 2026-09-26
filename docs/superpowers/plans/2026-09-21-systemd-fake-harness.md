# fakeSystemctl/fakeLoginctl test harness — Implementation Plan

**Goal:** Let `loadService`, `controlService`, `supervisorRunning`, and
`unloadService`'s Linux logic be exercised and tested from any host, and prove
their reaction to systemd's real vocabulary (`activating`/`failed`/`deactivating`,
`daemon-reload`/`enable` failures) rather than only the generated unit text —
closing the unit-test half of checklist bullet 6 of #945. The spec doc's bullet 6
also covers `serviceStatus`/`serviceControl` accuracy more broadly and a
real-systemd integration test analogous to `TestWaitBootedOut_RealLaunchd`;
neither is touched here.

**Architecture:** All four functions took `runtime.GOOS` directly, unlike
`renderUnitFor`, which already took `goos` as an explicit parameter for exactly
this reason — refactor them the same way first. Then a `fakeSystemctl`/
`fakeLoginctl` PATH-decoy harness mirroring the existing `fakeLaunchctl`
(`cmd_service_restricted_test.go`): a shared `installStub` helper writes a stub
script and immediately confirms it's reachable via `exec.LookPath`, closing a
vacuous-pass hole where a broken harness (stub not on `PATH`, or not executable)
reads identically to "the code under test correctly made zero calls."

**Tech Stack:** Go 1.26.5.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-16-linux-systemd-lifecycle-design.md`

**Issue:** cortex #945

## Tasks

- [x] Refactor `loadService`, `controlService`, `supervisorRunning`,
      `unloadService` to take `goos string` explicitly; update all call sites in
      `cmd_service.go` to pass `runtime.GOOS`.
- [x] `cmd_service_systemd_test.go`: `fakeSystemctl`/`fakeLoginctl` via a shared
      `installStub` helper (PATH-decoy binary + immediate `exec.LookPath`
      reachability check).
- [x] `TestSupervisorRunning_Linux`: `is-active` states
      `active`/`activating`/`failed`/`deactivating`/`inactive`, no-`systemctl`,
      silent-failure cases.
- [x] `TestLoadService_Linux`: `daemon-reload`/`enable --now` failure messages, the
      full happy-path invocation order (`daemon-reload` then `enable --now`, with
      the exact unit name), and all three linger branches (already-on, newly
      enabled, `enable-linger` failure).
- [x] `TestUnloadService_Linux`: marker-gated `disable-linger` (never called
      without a marker, called with the right uid when there is one).
- [x] `TestLingerEnabled`: `loginctl show-user` parsed as yes/no/malformed/
      command-failure.
- [x] `TestControlService_Linux`: exact verb-per-action mapping
      (stop→`disable --now`, start→`enable --now`, restart→plain `restart`).
- [x] Fix: `readCallLog` returning `nil` on any read error (not just
      "file doesn't exist") made every negative call-count assertion vacuous — a
      broken harness read identically to "genuinely never called". Reproduced by
      mutating a stub non-executable before fixing, and again after, to confirm
      the fix actually closes it.
- [x] Fix: fake `systemctl`/`loginctl` argument matching was positional
      (`$1`/`$2`) — switched to `case "$*" in *pattern*)`, robust to the real call
      sites ever reordering arguments.
- [x] Fix: unquoted call-log path in the shell fragment — reused the existing
      `shQuote` helper.
- [x] Style: collapsed `controlService(goos string, action string, ...)` to
      `controlService(goos, action string, ...)`.

## Result

27 subtests, all passing, all genuinely executed (not skipped) on any host —
including the one they were written on — because of the `goos` refactor.
