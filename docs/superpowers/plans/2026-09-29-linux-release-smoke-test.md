# Linux release smoke test — Implementation Plan

**Goal:** A CI job that installs a just-published release exactly as a real
user would (`curl | sh`, downloading from GitHub Releases) and exercises the
systemd install/upgrade/uninstall path for real — closing 5 of #957's 6
checklist bullets.

**Architecture:** Triggers on `workflow_run` of `release-binaries.yaml`
finishing (`types: [completed]`, `conclusion == 'success'`), not on the tag/
main push directly — guarantees the release assets already exist rather than
racing the upload. Maps the triggering run's `head_branch` to the tag under
test (`v*` as-is; `main` → `main-latest`, the same `CHANNEL_TAG`
`scripts/install.sh` itself uses). Reuses the exact `enable-linger` +
wait-for-`/run/user/<uid>/bus` recipe `ci.yaml`'s `abctl` leg already uses for
the real-systemd integration test (#1076) — proven to work on GitHub-hosted
`ubuntu-latest` runners.

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
