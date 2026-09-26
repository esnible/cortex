# Real-systemd integration test for crash recovery — Implementation Plan

**Goal:** Prove, against a live `systemd --user` session rather than by reading the
unit file, that `Restart=on-failure` actually restarts a crashed unit and that a
deliberate stop does not — closing checklist bullet 5 (crash half) of #945 for real.

**Architecture:** A throwaway `systemd-run --user` transient unit running a
synthetic slow-to-exit script (not the real proxy, for isolation). Covers the same
ground as macOS's `TestWaitBootedOut_RealLaunchd` deliberately in a different
shape — three skip-guard categories here versus that test's four, since systemd's
native restart means there's no supervisor layer or bootout race to reproduce, just
the bare `Restart=on-failure` claim itself. Wired into the `abctl` leg of
`go-ci-authbridge-cmd` via a new `enable-linger` + `XDG_RUNTIME_DIR` setup step.

**Tech Stack:** Go 1.26.5, `systemd-run`/`systemctl --user`, GitHub Actions.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-16-linux-systemd-lifecycle-design.md`

**Issue:** cortex #945

## Tasks

- [x] `cmd_service_systemd_integration_test.go`: `requireRealSystemd` skip-guard
      helper (wrong GOOS, missing binaries, no reachable `systemctl --user`
      session), with `ABCTL_SYSTEMD_TESTS=required` escape hatch mirroring
      `ABCTL_LAUNCHD_TESTS`.
- [x] `TestSupervisorRestartsAfterCrash_RealSystemd`: `kill -9` the unit's main PID,
      confirm it comes back with a genuinely new PID (`waitForNewMainPID`, not just
      `is-active`, to close a race window right after the kill).
- [x] `TestSupervisorStaysStoppedAfterDeliberateStop_RealSystemd`: a deliberate
      `systemctl stop` must not trigger a restart.
- [x] CI: `enable-linger` + wait-for-bus-socket + `XDG_RUNTIME_DIR` setup step on the
      `abctl` leg of `go-ci-authbridge-cmd`, `ABCTL_SYSTEMD_TESTS=required` set for
      that job only.
- [x] Fix: `systemd-run` invocation had a stray literal `"run"` argument (unlike
      `systemctl`, `systemd-run` has no separate verb) — caught by the first real
      CI run, both tests failing identically.
- [x] Fix: cleanup's `t.Logf` on `stop`/`reset-failed` failure printed on every
      normal passing run — `systemd-run` transient units are garbage-collected once
      inactive, so both calls routinely exit nonzero by teardown time. `stop`'s
      exit 5 is a narrow, confirmed-benign code, filtered by exit code.
      `reset-failed`'s exit 1 is systemd's *generic* failure code, so filtering it
      by code would suppress nearly everything that call can produce — filtered by
      output text (`"not loaded"`/`"not found"`) instead, catching only the
      confirmed "unit doesn't exist" case.
- [x] Confirmed against real CI (not just local skip behavior): both tests pass with
      realistic timing (~3s crash-restart, ~7s stay-stopped), and the exit-code
      version of the cleanup fix verified to produce zero spurious log lines on a
      clean run. The later switch to message-matching for `reset-failed` has not
      yet had its own real-CI confirmation.

## Result

Confirmed for real, against a live systemd: `Restart=on-failure` restarts a crashed
unit, and a deliberate stop does not. This is the real-world verification the
darwin `KeepAlive` claim already had (tested, found false, drove the supervisor
redesign) that the Linux assumption never did.
