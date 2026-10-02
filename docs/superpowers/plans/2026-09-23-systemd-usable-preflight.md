# Linux equivalent of launchdUsable() — Implementation Plan

**Goal:** Give `serviceInstall` a Linux preflight that refuses cleanly before
writing anything to disk when systemd is unusable, matching what `launchdUsable()`
already does for macOS — closing checklist bullet 8 of #945, the one item the
spec doc's gap analysis explicitly left as "an open decision, not yet resolved
either way."

**Architecture:** `systemdUsable()` mirrors `launchdUsable()`'s shape exactly
(PATH check, then a live probe command, then first-line-of-output-or-err), but
with no internal `runtime.GOOS` guard — the caller picks which of the two to run,
via a new `serviceManagerUsable(goos string)` dispatcher, so `systemdUsable()` is
directly testable from any host the same way `renderUnitFor`/`loadService`/etc.
already are. The probe itself (`systemctl --user show-environment`) is the same
one `install.sh`'s shell-level `supervisor_usable()` and this repo's own
`requireRealSystemd` already use — nothing new invented, just brought into the Go
preflight that was missing it.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-16-linux-systemd-lifecycle-design.md`

**Issue:** cortex #945

## Tasks

- [x] `systemdUsable()` (`cmd_service_platform.go`): PATH check, live probe,
      first-line-of-output-or-err — same shape as `launchdUsable()`, no GOOS guard.
- [x] `serviceManagerUsable(goos string) (bool, string)`: dispatches to
      `launchdUsable()`/`systemdUsable()`. `serviceInstall`'s existing preflight
      call site now calls `serviceManagerUsable(runtime.GOOS)` instead of
      `launchdUsable()` unconditionally.
- [x] `TestSystemdUsable`: no-systemctl, unreachable-session, and working-session
      cases, mirroring `TestLaunchdUsable`.
- [x] `TestServiceManagerUsable_Linux`: confirms the dispatch itself — that
      `"linux"` reads through `systemdUsable`'s fake, not `launchdUsable`'s.
- [x] Confirmed `TestLaunchdUsable` and `TestServiceInstall_RestrictedEnvironment`
      (darwin) still pass unchanged — `launchdUsable()` itself untouched.

## Result

`abctl service install` now refuses cleanly on Linux with no usable `systemd --user`
session, before the unit file is written — the same protection macOS already had.
`serviceInstall` itself still calls the dispatcher with `runtime.GOOS` rather than
an explicit parameter (consistent with `serviceIsCurrent`/`reportInstallSuccess`,
tracked as a further increment in #1106); `systemdUsable`/`serviceManagerUsable`
are the two pieces that needed to be host-testable on their own, and now are.
