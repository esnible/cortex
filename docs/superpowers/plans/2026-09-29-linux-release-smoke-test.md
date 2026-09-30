# Linux release smoke test — Implementation Plan

**Goal:** A CI job that installs a just-published release exactly as a real
user would (`curl | sh`, downloading from GitHub Releases) and exercises the
systemd install/upgrade/uninstall path for real — closing 5 of #957's 6
checklist bullets.

**Architecture:** Triggers on `workflow_run` of the workflow named
`"Release binaries"` finishing (`types: [completed]`, `conclusion ==
'success'`) — matched by that exact name, not by filename, which is why the
name has to be byte-identical to `release-binaries.yaml`'s own `name:` field.
Triggering on this rather than the tag/main push directly guarantees the
release assets already exist rather than racing the upload. Maps the
triggering run's `head_branch` to the tag under test (`v*` as-is; `main` →
`main-latest`, the same `CHANNEL_TAG` `scripts/install.sh` itself uses).
Reuses the exact `enable-linger` + wait-for-`/run/user/<uid>/bus` recipe
`ci.yaml`'s `abctl` leg already uses for the real-systemd integration test
(#1076) — proven to work on GitHub-hosted `ubuntu-latest` runners.

The actual test logic lives in `scripts/release_smoke_test_linux.sh`, kept
out of the YAML for the same reason `deploy/proxy-init/test-enforce-redirect.sh`
is a separate script rather than inline `run:` steps. It always pipes
`curl | sh` rather than invoking the checked-out `install.sh` as a local
file — `install.sh`'s own re-exec bootstrap only re-fetches the tagged copy
of itself (matching the requested `--ref`) when `$0` isn't a readable file,
which is the exact condition a real `curl | sh` user hits and a local
invocation would skip.

**Spec:** none — #957 is its own checklist, not a design doc.

**Issue:** cortex #957

## Tasks

- [x] `scripts/release_smoke_test_linux.sh`: fresh install of the most recent
      stable release before the tag under test (auto-detected via
      `gh release list`, not hardcoded — a fixed "known good" version would
      drift out of the release list over time), assert `abctl service status`
      reports healthy.
- [x] Upgrade to the tag under test: a config marker only this test writes,
      asserted still present after the upgrade (proves `migrateConfig`'s
      additive-only behavior holds beyond just its own listener pins),
      re-assert healthy.
- [x] No-op re-run of the same tag: assert `abctl service install` reports
      `"Already current"` rather than re-installing.
- [x] `abctl service uninstall`: assert `"Removed"`, the unit file is gone,
      and `~/.cortex/config.yaml` is untouched (per uninstall's own promise).
- [x] `.github/workflows/release-smoke-linux.yaml`: the `workflow_run`
      trigger, tag resolution, systemd session setup, and the script
      invocation.
- [x] `shellcheck --severity=error` and `yamllint` (repo's relaxed config)
      both clean locally.
- [x] Fix (review): `workflows: ["Release Binaries"]` didn't match
      `release-binaries.yaml`'s actual `name: Release binaries` (lowercase
      `b`) — a `workflow_run` name mismatch fails silently, no run is ever
      created. This meant the trigger never fired at all until fixed.
- [x] Fix (review): `github.event.workflow_run.head_branch` and
      `steps.tag.outputs.tag` were interpolated directly into `run:` shell
      bodies rather than passed through `env:` first — a standard GitHub
      Actions hardening issue (flagged independently by `zizmor`) regardless
      of how constrained the practical input space is here.
- [x] Fix (review): the older-release lookup used `|| true`, which cannot
      distinguish "no earlier release exists" from "`gh` failed" — and used
      "the most recent *other* release" rather than "the most recent release
      *before* the tag under test," which breaks if this workflow is
      triggered by re-running an old completed Release Binaries run (a real
      possibility) once newer releases already exist. Fixed by anchoring on
      `createdAt` (not `publishedAt`, which stays frozen at first-creation
      for a rolling tag like `main-latest` whose assets get clobbered on
      every push to main — verified against the live API) and checking each
      `gh` call's own exit status explicitly, since dash has no `pipefail`.
- [x] Fix (review): `curl | sh` piped straight through would, on a failed
      download, leave `sh` reading empty stdin and exit 0 under dash (no
      pipefail) — the failure would only surface later at `assert_healthy`,
      pointing at the wrong thing. Now downloads to a temp file first and
      checks it explicitly, still reading from a file via stdin redirection
      (not as a named file argument) so `$0` stays non-file and install.sh's
      re-exec bootstrap still triggers.
- [x] Fix (review, self-caught while implementing the above): piping
      `install_cortex | tee` would have silently swallowed the function's own
      new `exit 1` calls — each side of a pipe runs in its own subshell under
      dash, so an `exit` inside the left side only kills that subshell, and
      the pipeline's reported exit status comes from `tee` (0) regardless.
      Fixed by redirecting to a file and checking the function's own exit
      status directly instead of piping through `tee`.
- [x] Fix (review): uninstall's config check asserted only that the file
      still exists, not that its contents are unchanged — existence survives
      truncation or a rewrite. Now compares a `cksum` taken immediately
      before uninstall against one taken after.

## Result

5 of #957's 6 bullets closed: fresh install from a published tag, healthy
status, upgrade-with-config-preserved, idempotent re-run, and clean
uninstall — all against a real release, on a real Linux runner, via the same
`curl | sh` path a real user takes. Runs on every `v*` tag and on
`main-latest`, automatically, once `release-binaries.yaml` finishes
publishing either.

Not closed: the third bullet (drive a real request through the proxy and
assert a parsed event with a non-zero token count) needs the unattended/
headless capture path from #955, which doesn't exist yet — tracked there.
arm64 coverage is explicitly out of scope: no real arm64 GitHub-hosted
runner exists in this org, and no arm64-*execution* pattern exists anywhere
in this repo's CI today (only cross-compilation/QEMU image builds, never a
running arm64 binary) — extending that is new infrastructure, not a small
addition to this workflow.
