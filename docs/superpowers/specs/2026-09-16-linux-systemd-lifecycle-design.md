# Linux install and systemd service lifecycle — Design

**Date:** 2026-09-16
**Status:** Research and gap analysis complete.
**Issue:** cortex #945 — "feature: verified Linux install and systemd service lifecycle"
**Repos touched:** `cortex/authbridge` only.

## Problem

#945 is a verification and gap-closing issue, not a build-from-scratch feature: the
core service-lifecycle mechanism already shipped in PR #876 ("Keep Cortex running
across crashes and logins (launchd / systemd)"), with follow-on fixes #880, #897,
#931, #911. What follows is what auditing that existing code against #945's
checklist found.

## Conceptual background: systemd vs. launchd, and why the two platforms differ

A service manager's job: start a background process, notice when it dies (including
crashes, not just deliberate stops), and decide whether to relaunch it — without a
human watching a terminal.

**The key asymmetry that shapes the whole design:**

- **systemd** (Linux): a unit file's `Restart=on-failure` is honored by systemd's own
  supervisor loop, reliably, even for a unit just `systemctl --user start`ed while
  already logged in (exactly Cortex's `curl | sh` scenario).
- **launchd** (macOS): the team found — and documented in code comments — that
  launchd's equivalent (`KeepAlive`, tried alongside `StartInterval` and
  `RunAtLoad`) does **not** reliably restart a LaunchAgent added mid-session (as
  opposed to one present at boot when launchd first scans `LaunchAgents`). Since
  Cortex is always installed mid-session, this gap is squarely in the installer's
  path.

**Consequence for the architecture:**

- **macOS** runs **two processes**: launchd starts a small Go-written supervisor
  (`authbridge-proxy --supervise`, `authbridge/cmd/authbridge-proxy/supervise.go`)
  that itself watches and restarts the actual proxy child. launchd supervises the
  supervisor; the supervisor does the real crash-recovery job launchd won't
  reliably do.
- **Linux** runs **one process**: `authbridge-proxy` directly as the systemd unit's
  `ExecStart=`, with `Restart=on-failure` doing all the crash-recovery work
  natively. No supervisor binary involved. Confirmed in code:
  `cmd_service_platform.go:92-93` and `TestSupervisionIsPlatformCorrect`
  (`cmd_service_test.go:312-332`) explicitly assert Linux does **not** use
  `--supervise`.

This makes #945 structurally simpler than #944 on the crash-recovery axis — but it
also means the Linux "`Restart=on-failure` actually works" assumption had never been
put through the same real-world verification (and design correction) that produced
the macOS supervisor. That was the single biggest gap this issue needed to close.

## Where things live

- `authbridge/install.sh` — the `curl | sh` installer (~1300 lines, POSIX `sh`).
- `authbridge/install_test.sh` — installer unit tests (shell functions in isolation,
  no real network/systemctl).
- `authbridge/cmd/abctl/cmd_service.go` — cross-platform `abctl service` command
  logic (install/uninstall/status/start/stop/restart).
- `authbridge/cmd/abctl/cmd_service_platform.go` — the OS-specific half: systemd
  unit rendering, launchd plist rendering, `systemctl`/`launchctl`/`loginctl`
  invocations.
- `authbridge/cmd/authbridge-proxy/supervise.go` — the macOS-only (and
  unsupervised-fallback) Go restart loop.
- `authbridge/cmd/authbridge-proxy/main.go` — the proxy itself; on SIGTERM/SIGINT
  does a graceful shutdown with a **15-second drain** (`main()`'s
  `shutdownCtx, shutdownCancel := context.WithTimeout(..., 15*time.Second)`, right
  after the signal wait — cited by site rather than line number, since `main.go`
  isn't part of this PR's diff and keeps moving independently) — every "does stop
  tolerate the drain" question traces back to this constant.
- `authbridge/docs/laptop-service.md` — user-facing doc for `abctl service *`,
  `~/.cortex/` layout, restricted-environment fallback, manual-removal
  instructions.

## Gap analysis against #945's checklist, as found

### 1. Fresh install via `curl | sh`, amd64 and arm64
Install-time OS/arch detection, checksum verification, and a live preflight
(`supervisor_usable()`, `install.sh:645-653`) all exist and are unit-tested. No CI
job installs and runs the real released binaries against a live Linux box, for
either architecture — arm64 is exercised at build time only.

### 2. Upgrade over an older install: service restarted, config preserved
Idempotency (`installCanSkip`/`serviceIsCurrent`), config migration, and
binary-swap detection all exist and are well tested. Nothing drives an actual
upgrade against a real running `systemd --user` unit — macOS has a dedicated
real-launchd test for the equivalent scenario (`TestWaitBootedOut_RealLaunchd`);
Linux has no analog.

### 3. Uninstall leaves no unit file, no `~/.cortex`, no modified agent settings
`serviceUninstall`/`unloadService` exist, including a thoughtful linger-marker
mechanism so uninstall doesn't clobber a linger setting the user set for unrelated
units. None of this Linux uninstall logic had a test driving faked
`systemctl`/`loginctl` — the existing test only greps source for expected strings.

### 4. Re-running install is idempotent
`installCanSkip` and `install.sh`'s already-at-this-version guard exist and are
tested. `lingerEnabled()`'s parsing of `loginctl show-user --property=Linger`
output had zero test coverage.

### 5. Service survives reboot and a crash — the central gap
Unit rendering (`Restart=on-failure`, `RestartSec=10`, `StartLimit*` correctly
placed in `[Unit]`) is well-reasoned and has good string-shape test coverage. No
test — unit, integration, or a documented manual-verification note — proved that a
real `systemd --user` instance actually restarts the unit after `kill -9`, or
survives a real reboot/logout with lingering enabled. macOS has exactly this kind
of verification on record: `KeepAlive` was tested, found not to work end-to-end,
and that finding drove the supervisor redesign. No equivalent verification episode
existed for Linux's `Restart=on-failure` assumption — trusted, not proven.

### 6. `abctl service status | start | stop | restart` accurate in every state
`serviceStatus`/`serviceControl` are platform-agnostic and reasonably designed
(`controlService` maps stop→`disable --now`, start→`enable --now`,
restart→transient `restart`). `loadService`, `controlService`, `supervisorRunning`,
and `unloadService` all took `runtime.GOOS` directly rather than an explicit `goos`
parameter (unlike `renderUnitFor`), so none of their Linux logic could be exercised
from a non-Linux host. Nothing faked or drove real `systemctl`/`loginctl` for
Linux at all — macOS has both a `fakeLaunchctl` unit-test harness and a
real-launchd integration test.

### 7. `stop` tolerates the proxy's ~15s drain
The proxy's own 15s shutdown timeout is the anchor value everything else has to
respect. macOS handles this explicitly in Go (a 30s bootout timeout, plus the
supervisor's own 20s-before-SIGKILL logic). As of this audit (2026-09-16), the
rendered Linux unit set no `TimeoutStopSec` at all — it "worked" only by accident
of systemd's own 90s default (`systemd.system.conf(5)`) exceeding 15s.

**Closed by #1079** — `renderUnitFor("linux", ...)` now sets `TimeoutStopSec=20`
explicitly on `main`, matching the macOS supervisor's 20s headroom over the same
15s drain, with a subtest in `cmd_service_test.go` asserting the line is present.
Cited by symbol, not line, since #1079 landed in a sibling PR and this file's own
history spans branches where it wasn't there yet.

### 8. Works under user systemd, and states what happens where systemd is absent
`loadService` gives a clear, actionable message when `systemctl` isn't found;
`install.sh`'s `supervisor_usable()` does a live preflight (`systemctl` present but
user manager/D-Bus unreachable) and falls back to an unsupervised mode. As of this
audit (2026-09-16), `abctl`'s own preflight had no Linux equivalent of
`launchdUsable()` — the function that lets macOS refuse cleanly *before writing
anything to disk*, with a dedicated tested exit code. On Linux, "no systemd" was
discovered later, inside `loadService`, after the unit file had already been
written (then cleaned up on failure).

**Closed** — `systemdUsable()` mirrors `launchdUsable()`'s live probe
(`systemctl --user show-environment`, the same check `install.sh`'s shell-level
preflight and this file's own `requireRealSystemd` already use), and
`serviceManagerUsable(goos)` dispatches to it from `serviceInstall`'s existing
preflight call site — the same place, same exit code, same message, that used to
call `launchdUsable()` unconditionally and get a silent `true` on Linux.

## Relationship to other issues

- **#944** — macOS mirror of this issue, owned by @huang195. Already has real
  end-to-end verification behind it (the `KeepAlive` finding, bootout-race fix
  #880) that Linux lacked. **A related, separately-discovered gap:**
  `ABCTL_LAUNCHD_TESTS=required` exists in `cmd_service_bootout_test.go` (an
  escape hatch that turns a silent skip into a hard failure) but is never set by
  any workflow, so `TestWaitBootedOut_RealLaunchd` skips in every CI run. No macOS
  runner exists in this repo to close that from the Linux side — worth tracking
  against #944 or #956 (macOS smoke tests) rather than leaving it to be
  rediscovered.
- **#955** — unattended agent workloads / headless capture path, owned by
  @esnible. #956 and #957 both need it for a "non-zero token count" smoke-test
  assertion. Not required for #945 itself.
- **#964** — manual reboot verification (macOS + Linux), explicitly deferred from
  #944/#945 because CI runners don't reboot. Shares the "does
  `Restart=on-failure`/launchd `KeepAlive` really survive a restart" question
  raised above — worth coordinating rather than duplicating.
- **#966** — plugin build-tag convention cleanup (retired `exclude_plugin_*` form →
  `include_plugin_*`; allow-legacy-plugin-tag: this reference is design history, not
  a live usage — the guard matches on the whole file via `strings.Contains`, not
  scoped to this one mention, so a real `exclude_plugin_*` usage added anywhere
  else in this file would also pass silently) and a smaller desktop artifact.
  Touches the same install/upgrade path: upgrading to a build that dropped a
  plugin an existing config still names must be handled gracefully — #966 itself
  closed 2026-09-16 having noted this as something #944/#945/#964 would need to
  cover; tracking it directly against one of those now that #966 is no longer
  open would keep the concern from being lost.
