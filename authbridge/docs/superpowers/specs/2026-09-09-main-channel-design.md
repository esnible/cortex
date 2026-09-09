# Main Channel — installing unreleased binaries — Design

**Date:** 2026-09-09
**Status:** Converged design — ready for implementation plan.
**Repos touched:** `cortex` only (`.github/workflows/release-binaries.yaml`, `authbridge/install.sh`, `CONTRIBUTING.md`, `README.md`)

> **"Channel" means only where binaries come from** — a tagged release, or the tip of
> `main`. Not a subscription, an update daemon, or a promotion pipeline. There are two
> channels and no mechanism for a third.

## Problem

A fix merged to `main` is not installable until someone cuts a tag. Not theoretical: a
colleague hit a sandbox failure on `v0.7.0-alpha.7`, the fix merged the next day as
#897, and the only ways to get it to him were "wait for a release" or "clone the repo
and build with Go" — neither reasonable for confirming a bug he reported.

`--ref=main` already exists but does not solve it: it swaps **the script only**, and the
fix he needed (`launchdUsable`) is compiled into `abctl`. No workflow publishes binaries
from `main` — `release-binaries.yaml` triggers on `v*` tags, `ci.yaml` uploads no
artifacts, and `build.yaml` produces container images rather than the standalone
binaries the installer downloads.

CI artifacts are not a substitute: `actions/.../artifacts/<id>/zip` returns **HTTP 401
anonymously** (verified), so `curl | sh` cannot fetch them without a token.

## Goals / Non-goals

**Goals**

- A developer installs unreleased `main` binaries with `--ref=main`.
- The default one-liner is unchanged, in command *and* behaviour.
- Which `main` build is installed is identifiable after the fact.
- Switching channels works both directions with no manual cleanup.
- **The selection surface gets smaller, not larger.**

**Non-goals**

- A public bleeding-edge channel. Audience is teammates and contributors; documented in
  `CONTRIBUTING.md`, deliberately **not** in the README quickstart. Putting it beside the
  getting-started command would place new users one flag from untested code, which is
  what the release-bootstrap exists to prevent.
- Container images from `main` — `build.yaml` already publishes those on main pushes.
- Promotion, subscriptions, or automatic updates.
- Reproducibility of a `main` install. The channel is rolling by construction.

## What breaks without care

Publishing a release tagged `main` breaks **the default install for every new user**, and
quietly. Traced through the current script:

1. the bootstrap calls `newest_release()`, which fetches `releases?per_page=1`;
2. the rolling `main` release sorts first;
3. the tag-shape guard requires `v[0-9]*`, so `main` fails it and the function returns 1;
4. `want_ref` is empty, so the script warns *"could not resolve the newest release;
   continuing with the copy from main"* and sets `SCRIPT_REF=main`;
5. version resolution calls `newest_release()` again, fails again, and dies with
   *"could not resolve the newest release"*.

Second break: `gh release create` passes `--prerelease` only for tags matching
`*-rc*|*-alpha*|*-beta*`. `main` matches none, so the rolling release would be created
**without** it and would take GitHub's "Latest" badge, displacing `v0.3.1`.

Both follow from publishing into the same release namespace the installer reads. Neither
guard below is optional.

## Design decisions

| # | Decision | Choice |
| --- | --- | --- |
| D1 | Publishing mechanism | **One rolling pre-release** tagged `main`, assets overwritten each push. Reuses the existing `releases/download/<tag>` URL shape, so the download path is unchanged. |
| D2 | Trigger | **Every push to `main`.** A merged fix is installable minutes later — the motivating case. |
| D3 | Selection | **One selector: `--ref=X` means "install X"** — script *and* binaries — for every X, including `main`. No new flag. |
| D4 | Build identity | Asset names use the tag (`main`); the binary is stamped `main-<short-sha>`. The two deliberately disagree. |
| D5 | Default resolution | `newest_release()` fetches `?per_page=10` and returns the first tag matching `v[0-9]*`, ignoring the channel entirely. |
| D6 | Workflow home | **Extend `release-binaries.yaml`** rather than add a second workflow. |
| D7 | Knob cleanup | Remove `AUTHBRIDGE_REF`, `AUTHBRIDGE_INSTALL_ONLY`, `AUTHBRIDGE_VERSION`. Keep `AUTHBRIDGE_SKIP_DOWNLOAD` (undocumented, maintainer) and `AUTHBRIDGE_SCRIPT_REF` (internal). |
| D8 | First publish | Gated on repo variable `MAIN_CHANNEL_ENABLED`, unset by default — this is what lets everything ship in one PR safely (see Sequencing). |

**On D3.** `--ref` is inconsistent *today*: `--ref=v0.7.0-alpha.4` sets both halves
(`v*) version="${SCRIPT_REF}"`), while `--ref=main` sets only the script because there is
no `main` release to download from. Once one exists, one rule covers both. This design
therefore adds no selector — it makes an existing one consistent.

An earlier draft added a `--main` flag to preserve "main's script with released
binaries". That capability is maintainer-only (testing an installer change without
moving the binaries under it), and two flags differing by punctuation but not by meaning
is worse than losing it. It remains reachable via `AUTHBRIDGE_SKIP_DOWNLOAD` with a
locally built pair, which is how installer changes were actually tested.

**On D4.** Required, not stylistic. `serviceIsCurrent` compares the `AbctlVersion` stamp
in the installed unit against the running `abctl` to decide whether `service install` is
a no-op. If every `main` build stamped `main`, an upgrade would compare equal and
`service install` would report *"Already current"* while the old binary kept running —
the idempotency work would hide upgrades on the one channel that changes most.

**On D6.** A second workflow means a second copy of a 16-archive build matrix. Two
implementations of one thing have already cost this repo twice: `install.sh`'s
`stop_previous_cortex` duplicating `abctl`'s adoption logic, and the migration's two
parallel lists of pins. Drift in a duplicated matrix would mean the channel builds
differently from releases — the exact failure a developer using it could not diagnose.

**On D7.** All five were audited: **none is referenced anywhere outside `install.sh`** —
no CI, docs, demos, or Makefiles. `AUTHBRIDGE_REF` and `AUTHBRIDGE_INSTALL_ONLY` are pure
aliases for flags that exist, dating from when piping to `sh` made flags awkward, which
`sh -s -- --flag` solves. `AUTHBRIDGE_VERSION` is redundant once D3 lands.
`AUTHBRIDGE_SKIP_DOWNLOAD` does something no flag does and stays, demoted to a header
comment. `AUTHBRIDGE_SCRIPT_REF` is the bootstrap's recursion guard — removing it causes
infinite re-exec — and is already undocumented.

## Architecture

### Workflow (`release-binaries.yaml`)

Add `push: branches: [main]` alongside the existing `tags: ['v*']`, with the main path
gated `if: vars.MAIN_CHANNEL_ENABLED == 'true'` (D8). Split the single `VERSION` variable
in the build step:

```
TAG      = main                 # release target and asset names  (stable URL)
VERSION  = main-<short-sha>     # -ldflags -X main.version        (unique per build)
```

For a tag push both keep today's value (`TAG = VERSION = $GITHUB_REF_NAME`), so the
release path is byte-identical to now.

Publishing needs no new logic — the existing branch already is rolling behaviour:

```sh
if gh release view "${TAG}" >/dev/null 2>&1; then
  gh release upload "${TAG}" dist/*.tar.gz dist/checksums.txt --clobber
else
  ...
fi
```

Two edits to the `create` arm: add `main` to the prerelease case so the badge stays on
`v0.3.1`, and give the rolling release notes saying it is an unreleased build from `main`
and pointing at the newest `v*` tag — for the human who browses releases and sees `main`
on top.

### Installer (`authbridge/install.sh`)

**The guard (D5).** `newest_release()` fetches `?per_page=10` and returns the first
`tag_name` matching `v[0-9]*`. Ten is headroom, not a calculation: there is exactly one
rolling `main` release, so two would do — the extra costs nothing and absorbs any future
non-`v` tag. The existing shape check stays as the last line of defence, and an
all-non-`v` page still returns non-zero rather than guessing.

**One selector (D3).** Version resolution gains one arm so any ref, not just a `v*` tag,
sources both halves:

```sh
case "${SCRIPT_REF}" in
    v*|main) version="${SCRIPT_REF}" ;;
    *)       version=$(newest_release) ;;
esac
```

**Cleanup (D7).** Delete the three env vars from the header comment and `usage()`, and
drop their read sites. Three messages currently advise `AUTHBRIDGE_VERSION` and must be
rewritten to `--ref=vX.Y.Z`, or the script ends up recommending a knob it no longer
honours — worse than the clutter it replaced:

- `install.sh:373` — *"could not resolve the newest release (set AUTHBRIDGE_VERSION=…)"*
- `install.sh:591` — the abctl-lacks-`service` guard
- `install.sh:596` — the same guard's non-tag branch

**Reporting.** With `--ref=main` the installer says it is installing unreleased code, and
reports the build it got by asking the binary (`installed_version abctl` →
`main-a1b2c3d`) rather than echoing the channel name. "I am on main" is not a usable bug
report; "I am on `main-a1b2c3d`" is.

## Consequences accepted

- **Every `--ref=main` run reinstalls and restarts.** The installed binary reports
  `main-<sha>`, never equal to `main`, so the skip-if-already-at-this-version check always
  downloads; the differing stamp then makes `serviceIsCurrent` false. Correct for a
  rolling channel, but each run cuts live Claude Code sessions. The existing
  connection-count warning covers it.
- **Removing three env vars is a breaking change** for anyone scripting them. Zero
  references in-repo, alpha-stage, teammate-sized audience — acceptable, and recorded
  rather than hidden.
- **`checksums.txt` rolls with the assets.** `install.sh` fetches assets and checksums in
  the same run, so verification stays sound. A human downloading them hours apart gets a
  mismatch. Documented, not fixed.
- **The releases page gains a rolling `main` entry.** It sorts first only until the next
  tagged release, since `created_at` is fixed at creation. Badge unaffected via the
  prerelease flag.
- **CI cost roughly doubles per merge to `main`** — a 16-archive cross-compile alongside
  the container images `build.yaml` already builds. Accepted for the freshness.

## Sequencing

The constraint is about **releases, not PRs**: the guard must be in the *released* script
before a `main` release exists, because the released copy is what every `curl | sh`
bootstraps into. Merging the workflow trigger and the guard together would otherwise fire
the first main build while the released script (`v0.7.0-alpha.7`) still lacks the guard.

`MAIN_CHANNEL_ENABLED` (D8) resolves this without splitting the PR:

1. Merge the PR — guard, selector, cleanup, and workflow all land. The main job is
   skipped because the variable is unset, so **no `main` release is created**.
2. Cut a release from that merge. The released script now contains the guard.
3. Set `MAIN_CHANNEL_ENABLED=true` in repo settings. The next push to `main` publishes the
   channel.

There is no window in which a `main` release exists and the released installer cannot
cope. Step 3 is a settings toggle, not a code change, so nothing waits on a second PR.

## Testing & success criteria

Fixture-driven. `newest_release()` is exercised by sourcing the function and replacing
`curl` with one that prints a fixture — the technique used ad hoc while diagnosing the
tag-parser bug.

**This harness does not exist yet, and that is scope.** `install.sh` has **no committed
tests**: `security-scans.yaml` runs `shellcheck --severity=error` over it and `ci.yaml`
does not reference it at all. The tag-parser fix (#902) shipped untested for exactly that
reason. So this design adds the first one — a plain POSIX `sh` script, since the repo has
no `bats` or `shunit2` and a framework is not worth it for a handful of functions — wired
into CI so it cannot rot.

Cases:

- **Guard:** a fixture where a `main` release sorts first still resolves
  `v0.7.0-alpha.7`; likewise with `main` first and several `v*` following.
- **Guard, hostile:** rate-limit JSON, an HTML error page, empty, and an all-non-`v` page
  each return non-zero rather than a wrong tag.
- **Default install unchanged:** no flags installs the newest `v*` release.
- **Selector:** `--ref=v0.7.0-alpha.7` sources both halves from it; `--ref=main` sources
  both from main and installs a binary whose `--version` is `main-<sha>`.
- **Removed vars:** setting `AUTHBRIDGE_VERSION`, `AUTHBRIDGE_REF`, or
  `AUTHBRIDGE_INSTALL_ONLY` has no effect — and no message anywhere still recommends them
  (grep assertion, since a stale suggestion is the likely regression).
- **`AUTHBRIDGE_SKIP_DOWNLOAD` still works**, since installer testing depends on it.
- **Channel switching, both directions:** release → main downloads main; main → release
  downloads the release *and* reinstalls the service (the unit's stamp differs, so
  `serviceIsCurrent` must be false). This is what makes the channel safe to hand to
  someone: the plain one-liner always returns them to releases.
- **Workflow:** a tag push produces assets named `_<tag>_` stamped with the tag; a main
  push produces `_main_` stamped `main-<sha>`, flagged prerelease; with
  `MAIN_CHANNEL_ENABLED` unset the main job is skipped.

Success: a developer runs the one-liner with `--ref=main`, gets a merged-but-unreleased
fix, and the default one-liner is provably unaffected — with a smaller documented surface
than before.

## Implementation surface (summary)

| File | Change |
| --- | --- |
| `authbridge/install.sh` | `newest_release()` `?per_page=10` + `v[0-9]*` filter; `main` added to the version-resolution arm; remove three env vars from docs, `usage()` and read sites; rewrite three messages that advise `AUTHBRIDGE_VERSION`; demote `AUTHBRIDGE_SKIP_DOWNLOAD` to a comment; install-time build reporting |
| `.github/workflows/release-binaries.yaml` | `push: branches: [main]` gated on `vars.MAIN_CHANNEL_ENABLED`; `TAG`/`VERSION` split; `main` added to the prerelease case; rolling-release notes |
| `authbridge/install_test.sh` *(new)* | first committed tests for `install.sh` — fixture-driven `newest_release()` and selector cases |
| `.github/workflows/ci.yaml` | run the new harness so it cannot rot |
| `CONTRIBUTING.md` | document the channel, the rolling-checksum caveat, and that the plain one-liner returns you to releases |
| `README.md` | one sentence: `--ref` now selects script *and* binaries |
