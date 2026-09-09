# Main Channel — installing unreleased binaries — Design

**Date:** 2026-09-09
**Status:** Converged design — ready for implementation plan.
**Repos touched:** `cortex` only (`.github/workflows/release-binaries.yaml`, `authbridge/install.sh`, `CONTRIBUTING.md`)

> **Naming:** "channel" here means only *where binaries come from* — a tagged
> release, or the tip of `main`. It is not a subscription, an update daemon, or a
> promotion pipeline. There are exactly two channels and no mechanism for adding a
> third.

## Problem

A fix merged to `main` is not installable until someone cuts a tag. That gap is not
theoretical: a colleague hit a sandbox failure on `v0.7.0-alpha.7`, the fix merged the
next day as #897, and the only ways to get it to him were "wait for a release" or
"clone the repo and build with Go". Neither is reasonable for confirming a fix he
reported.

`--ref=main` already exists but does not solve it: it swaps **the script only**, and
the fix he needed (`launchdUsable`) is compiled into `abctl`. No workflow publishes
binaries from `main` — `release-binaries.yaml` triggers on `v*` tags, `ci.yaml` uploads
no artifacts, and `build.yaml` produces container images rather than the standalone
binaries the installer downloads.

CI artifacts are not a substitute. `actions/.../artifacts/<id>/zip` returns **HTTP 401
anonymously** (verified), so `curl | sh` cannot fetch them without a token.

## Goals / Non-goals

**Goals**

- A developer can install unreleased `main` binaries with one extra flag.
- The default one-liner is unchanged, in command *and* behaviour.
- Which `main` build is installed is identifiable after the fact.
- Switching between channels works in both directions with no manual cleanup.

**Non-goals**

- A public bleeding-edge channel. Audience is teammates and contributors; this is
  documented in `CONTRIBUTING.md`, deliberately **not** in the README quickstart.
  Putting it beside the getting-started command would place new users one flag from
  untested code, which is what the release-bootstrap exists to prevent.
- Container images from `main`. `build.yaml` already publishes those on main pushes;
  this design covers standalone binaries only.
- Promotion, channel subscriptions, or automatic updates.
- Reproducibility of a `main` install. The channel is rolling by construction.

## What breaks without care

Publishing a release tagged `main` breaks **the default install for every new user**,
and quietly. Traced through the current script:

1. the bootstrap calls `newest_release()`, which fetches `releases?per_page=1`;
2. the rolling `main` release sorts first;
3. the tag-shape guard requires `v[0-9]*`, so `main` fails it and the function returns 1;
4. `want_ref` is empty, so the script warns *"could not resolve the newest release;
   continuing with the copy from main"* and sets `SCRIPT_REF=main`;
5. version resolution then calls `newest_release()` again, fails again, and dies with
   *"could not resolve the newest release"*.

A second break: `gh release create` only passes `--prerelease` for tags matching
`*-rc*|*-alpha*|*-beta*`. `main` matches none, so the rolling release would be created
**without** the prerelease flag and would take GitHub's "Latest" badge, displacing
`v0.3.1`.

Both are consequences of publishing to the same release namespace the installer reads.
Neither guard below is optional.

## Design decisions

| # | Decision | Choice |
| --- | --- | --- |
| D1 | Publishing mechanism | **One rolling pre-release** tagged `main`, assets overwritten on each push. Reuses the existing `releases/download/<tag>` URL shape, so the installer's download path is unchanged. |
| D2 | Trigger | **Every push to `main`.** A merged fix is installable minutes later, which is the case that motivated this. |
| D3 | Opt-in surface | **A dedicated `--main` flag**, not an overload of `--ref`. |
| D4 | Build identity | Asset names use the tag (`main`); the binary is stamped `main-<short-sha>`. The two deliberately disagree. |
| D5 | Default resolution | `newest_release()` filters to `v[0-9]*`, ignoring the channel entirely. |
| D6 | Workflow home | **Extend `release-binaries.yaml`** rather than add a second workflow. |

**On D3.** `--ref=main` today means *script from main, binaries from the newest
release* — the way to test an `install.sh` change without also changing the binaries
underneath it. Overloading it to switch both would make that combination unreachable.
A dedicated flag also appears in `usage()`, where a magic `--ref` value would not.

**On D4.** This asymmetry is required, not stylistic. `serviceIsCurrent` compares the
`AbctlVersion` stamp recorded in the installed unit against the running `abctl`'s
version to decide whether `service install` is a no-op. If every `main` build stamped
`main`, an upgrade would compare equal and `service install` would report *"Already
current"* while the old binary kept running — the idempotency work would actively hide
upgrades on precisely the channel that changes most.

**On D6.** A second workflow means a second copy of a 16-archive build matrix. Two
implementations of one thing have already cost this repo twice: `install.sh`'s
`stop_previous_cortex` duplicating `abctl`'s adoption logic, and the migration's two
parallel lists of pins. Drift in a duplicated matrix would mean the channel builds
differently from releases — the exact failure a developer using it could not diagnose.

## Architecture

### Workflow (`release-binaries.yaml`)

Add `push: branches: [main]` alongside the existing `tags: ['v*']`. Split the single
`VERSION` variable in the build step into two:

```
TAG      = main                 # release target and asset names  (stable URL)
VERSION  = main-<short-sha>     # -ldflags -X main.version        (unique per build)
```

For a tag push both keep today's value (`TAG = VERSION = $GITHUB_REF_NAME`), so the
release path is byte-identical to now.

Publishing needs no new logic. The existing branch already implements rolling
behaviour:

```sh
if gh release view "${TAG}" >/dev/null 2>&1; then
  gh release upload "${TAG}" dist/*.tar.gz dist/checksums.txt --clobber
else
  ...
fi
```

Two edits to the `create` arm: add `main` to the prerelease case so the badge stays on
`v0.3.1`, and give the rolling release notes that say it is an unreleased build from
`main` and point at the newest `v*` tag — for the human who browses the releases page
and sees `main` at the top.

### Installer (`authbridge/install.sh`)

**The guard (D5).** `newest_release()` fetches `?per_page=10` rather than
`?per_page=1` and returns the first `tag_name` matching `v[0-9]*`. Ten is headroom, not
a calculation: there is exactly one rolling `main` release, so two would do — the extra
costs nothing and absorbs any future non-`v` tag. The existing shape check stays as the
last line of defence, and an all-non-`v` page still returns non-zero rather than
guessing.

**The flag (D3).** `--main` sets the default for both halves; the more specific knob
overrides its own half:

| Invocation | script | binaries |
| --- | --- | --- |
| *(no flags)* | newest `v*` release | same release |
| `--main` | main | main |
| `--ref=main` | main | newest `v*` release (unchanged) |
| `--main --ref=v0.7.0-alpha.7` | that release | main |
| `--main` + `AUTHBRIDGE_VERSION=v0.7.0-alpha.7` | main | that release |

No precedence surprises and no error paths: each knob keeps one meaning.

**Reporting.** With `--main`, the installer prints that it is installing unreleased
code, and reports the build it actually got by asking the binary (`installed_version
abctl` → `main-a1b2c3d`) rather than echoing the channel name. "I am on main" is not a
usable bug report; "I am on `main-a1b2c3d`" is.

## Consequences accepted

- **Every `--main` run reinstalls and restarts.** The installed binary reports
  `main-<sha>`, which never equals `main`, so the skip-if-already-at-this-version check
  always downloads; the differing stamp then makes `serviceIsCurrent` false. Correct for
  a rolling channel, but each run cuts live Claude Code sessions. The existing
  connection-count warning covers it.
- **`checksums.txt` rolls with the assets.** `install.sh` fetches assets and checksums
  in the same run, so verification stays sound. A human who downloads them hours apart
  gets a mismatch. Documented, not fixed.
- **The releases page gains a rolling `main` entry** that sorts first. Mitigated by the
  prerelease flag (badge unaffected) and the release notes.
- **CI cost roughly doubles per merge to `main`**: a 16-archive cross-compile alongside
  the container images `build.yaml` already builds. Accepted for the freshness.

## Sequencing (this is what makes "nothing breaks" true)

The order is forced, because the guard must be in the **released** script before a
`main` release exists — the released copy is what every `curl | sh` bootstraps into.

1. Land the `newest_release()` filter alone.
2. Cut a release containing it.
3. Only then add the workflow trigger and the `--main` flag.

Steps 1–2 are independently valuable: the filter also hardens against any future
non-`v` tag. There is no window in which a `main` release exists and the released
installer cannot cope.

## Testing & success criteria

Fixture-driven. `newest_release()` is exercised by sourcing the function and replacing
`curl` with one that prints a fixture — the technique used ad hoc while diagnosing the
tag-parser bug.

**This harness does not exist yet, and that is scope.** `install.sh` currently has
**no committed tests**: `security-scans.yaml` runs `shellcheck --severity=error` over it
and `ci.yaml` does not reference it at all. The tag-parser fix (#902) shipped with no
test for exactly this reason. So this design adds the first one — a plain POSIX `sh`
script, since the repo has no `bats` or `shunit2` and adding a framework is not worth it
for a handful of functions. It must be runnable locally and wired into CI, or it will
rot.

Committing that harness is independently valuable and is a prerequisite for the guard
being verifiable rather than merely written.

- **Guard:** a fixture where a `main` release sorts first still resolves
  `v0.7.0-alpha.7`. Also with `main` first *and* several `v*` entries following.
- **Guard, hostile:** rate-limit JSON, an HTML error page, empty, and a non-`v` tag all
  still fail non-zero rather than returning a wrong tag.
- **Default install unchanged:** no flags installs the newest `v*` release.
- **Pinning unchanged:** `--ref=v0.7.0-alpha.7` and `AUTHBRIDGE_VERSION=...` behave as
  today.
- **`--ref=main` unchanged:** main's script, newest release's binaries.
- **`--main`:** installs a binary whose `--version` matches `main-<sha>`, and the
  install output names that build.
- **Channel switching, both directions:** release → main downloads main; main → release
  downloads the release *and* reinstalls the service (the unit's stamp differs, so
  `serviceIsCurrent` must be false). This is the property that makes the channel safe to
  hand to someone: the plain one-liner always returns them to releases.
- **Workflow:** a tag push still produces assets named `_<tag>_` stamped with the tag; a
  main push produces assets named `_main_` stamped `main-<sha>`; the rolling release is
  flagged prerelease.

Success: a developer runs the one-liner with `--main`, gets a merged-but-unreleased fix,
and the default one-liner is provably unaffected.

## Implementation surface (summary)

| File | Change |
| --- | --- |
| `authbridge/install.sh` | `newest_release()` filter; `--main` flag + `usage()` entry; version resolution arm; install-time reporting |
| `.github/workflows/release-binaries.yaml` | `push: branches: [main]`; `TAG`/`VERSION` split; `main` added to the prerelease case; rolling-release notes |
| `CONTRIBUTING.md` | document the channel, the rolling-checksum caveat, and that the plain one-liner returns you to releases |
| `authbridge/install_test.sh` *(new)* | first committed tests for `install.sh` — fixture-driven `newest_release()` cases |
| `.github/workflows/ci.yaml` | run the new harness, so it cannot rot |
