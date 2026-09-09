# Main Channel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a developer install unreleased `main` binaries with `--ref=main`, without changing the default one-liner's command or behaviour.

**Architecture:** One rolling GitHub pre-release tagged `main`, assets overwritten on every push to main by the existing `release-binaries.yaml`. `install.sh` gains a filter so the default path ignores that release, and `--ref=X` becomes consistent — script *and* binaries from X, for any X. Three vestigial env vars are removed in the same change, so the selection surface shrinks.

**Tech Stack:** POSIX `sh` (`authbridge/install.sh`), GitHub Actions, `gh` CLI, `curl`, `shellcheck`.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-09-main-channel-design.md`

## Global Constraints

- **POSIX `sh` only** in `install.sh`. `set -eu`, never `pipefail` (a bashism; the documented entry point is `curl … | sh`).
- **No new dependencies.** The script may use only `curl`, `tar`, `tr`, `grep`, `cut`, `awk`, `sed`, and a checksum tool. Not `jq`. Not `gh`.
- **`shellcheck --severity=error` must pass**, and `shellcheck -s sh` must pass, on every commit touching `install.sh`. CI runs the former.
- **Work in the worktree** `.worktrees/main-channel` on branch `feat/main-channel`. Never in the shared top-level checkout (root `CLAUDE.md`, "AI Assistant Instructions").
- **Commit trailer:** `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`. All commits use `git commit -s` (DCO required).
- **`AUTHBRIDGE_SCRIPT_REF` must keep working** — it is the bootstrap's recursion guard. Removing it causes infinite re-exec.
- **`AUTHBRIDGE_SKIP_DOWNLOAD` must keep working** — the install tests and all manual installer testing depend on it.
- **Do not enable the channel.** `MAIN_CHANNEL_ENABLED` is set in repo settings *after* a release containing the guard is cut. No task flips it.

---

### Task 1: First committed test harness for `install.sh`

`install.sh` has no tests today; `security-scans.yaml` runs `shellcheck --severity=error` and `ci.yaml` does not reference it. Every later task in this plan asserts behaviour, so the harness comes first.

**Files:**
- Create: `authbridge/install_test.sh`
- Modify: `.github/workflows/ci.yaml`

**Interfaces:**
- Consumes: nothing.
- Produces: `authbridge/install_test.sh`, runnable as `sh authbridge/install_test.sh` from the repo root. Exits 0 on success, 1 on any failure, and prints one line per case. Later tasks add cases to it and rely on these helpers existing:
  - `fixture <name> <<'EOF' … EOF` — writes a fixture file into the test tempdir, returns its path via `$FIXTURE`
  - `with_newest_release <fixture-path>` — sources `newest_release` from `install.sh` with `curl` stubbed to print that fixture, echoes the resolved tag, returns the function's exit status
  - `check <label> <expected> <actual>` — records pass/fail
  - `check_fails <label> <exit-status>` — records pass when status is non-zero

- [ ] **Step 1: Write the harness with one failing case**

The first case asserts today's behaviour so the harness is proven before it is trusted. Create `authbridge/install_test.sh`:

```sh
#!/bin/sh
# Tests for authbridge/install.sh.
#
# install.sh is a curl|sh entry point, so its failure modes are other people's
# first experience of Cortex. Until this file existed the only automated check was
# `shellcheck --severity=error`, which is why the tag-parser bug (returning the
# OLDEST release from compact JSON) shipped in #902 without a test.
#
# Approach: source a single function out of install.sh with `curl` replaced by one
# that prints a fixture. No network, no GitHub, no downloads. Plain POSIX sh
# because the repo has no bats or shunit2 and a framework is not worth it for a
# handful of functions.
#
# Run: sh authbridge/install_test.sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
INSTALL_SH="${SCRIPT_DIR}/install.sh"
[ -f "${INSTALL_SH}" ] || { printf 'cannot find %s\n' "${INSTALL_SH}" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

PASS=0
FAIL=0

check() { # label expected actual
	if [ "$2" = "$3" ]; then
		PASS=$((PASS + 1))
		printf '  ok   %s\n' "$1"
	else
		FAIL=$((FAIL + 1))
		printf '  FAIL %s\n       expected %s\n       actual   %s\n' "$1" "$2" "$3"
	fi
}

check_fails() { # label status
	if [ "$2" != "0" ]; then
		PASS=$((PASS + 1))
		printf '  ok   %s (exit %s)\n' "$1" "$2"
	else
		FAIL=$((FAIL + 1))
		printf '  FAIL %s: expected non-zero exit, got 0\n' "$1"
	fi
}

fixture() { # name; body on stdin. Sets $FIXTURE to the path.
	FIXTURE="${TMP}/$1"
	cat >"${FIXTURE}"
}

# with_newest_release runs install.sh's newest_release() against a fixture.
#
# The function is extracted by line range rather than sourcing install.sh, because
# sourcing would run the whole installer. `curl` is replaced by a function so the
# extracted code is unmodified — testing what ships, not a copy of it.
with_newest_release() { # fixture-path
	_f=$1
	{
		printf 'REPO=rossoctl/cortex\n'
		printf 'warn() { printf "warning: %%s\\n" "$*" >&2; }\n'
		printf 'curl() { cat "%s"; }\n' "${_f}"
		sed -n '/^newest_release()/,/^}/p' "${INSTALL_SH}"
		printf 'newest_release\n'
	} >"${TMP}/probe.sh"
	sh "${TMP}/probe.sh" 2>/dev/null
}

printf 'install.sh tests\n'

# --- newest_release: the shape it is documented to handle ---

fixture pretty.json <<'EOF'
[
  {
    "tag_name": "v0.7.0-alpha.7",
    "name": "v0.7.0-alpha.7"
  },
  {
    "tag_name": "v0.3.1"
  }
]
EOF
check "pretty JSON resolves the newest tag" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

fixture compact.json <<'EOF'
[{"tag_name":"v0.7.0-alpha.7"},{"tag_name":"v0.7.0-alpha.6"},{"tag_name":"v0.3.1"}]
EOF
check "compact JSON resolves the newest tag, not the oldest" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

printf '\n%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = "0" ]
```

- [ ] **Step 2: Run it and verify both cases pass**

Run: `sh authbridge/install_test.sh`

Expected: two `ok` lines, `2 passed, 0 failed`, exit 0. These assert current behaviour, so they must pass against unmodified `install.sh`. If either fails, the harness is wrong — fix the harness, not `install.sh`.

- [ ] **Step 3: Prove the harness can fail**

Temporarily change the `compact.json` expectation from `v0.7.0-alpha.7` to `v0.3.1`, run again, and confirm you get `FAIL` and exit 1. Then change it back and re-run to confirm green. A harness never observed failing is not evidence.

- [ ] **Step 4: Wire it into CI**

In `.github/workflows/ci.yaml`, add a job alongside the existing ones:

```yaml
  install-script:
    name: install.sh tests
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Run install.sh tests
        run: sh authbridge/install_test.sh
```

Match the checkout action version already pinned elsewhere in that file — read it and copy, do not assume `v4`.

- [ ] **Step 5: Verify shellcheck accepts the new file**

Run: `shellcheck --severity=error authbridge/install_test.sh && shellcheck -s sh authbridge/install_test.sh`

Expected: no output. `security-scans.yaml` shellchecks shell scripts in the repo, so a new one must pass or CI fails on an unrelated job.

- [ ] **Step 6: Commit**

```bash
git add authbridge/install_test.sh .github/workflows/ci.yaml
git commit -s -m "$(printf 'test: First committed tests for install.sh\n\ninstall.sh is a curl|sh entry point, so its failure modes are other people first\nexperience of Cortex — and until now the only automated check was shellcheck\n--severity=error. That is why #902 fixed the tag parser returning the OLDEST\nrelease from compact JSON with no test.\n\nPlain POSIX sh: the repo has no bats or shunit2, and a framework is not worth it\nfor a handful of functions. newest_release() is extracted by line range and run\nwith curl replaced by a fixture-printing stub, so the code under test is the code\nthat ships rather than a copy.\n\nWired into ci.yaml so it cannot rot.\n\nAssisted-By: Claude (Anthropic AI) <noreply@anthropic.com>')"
```

---

### Task 2: Filter `newest_release()` so the channel cannot hijack the default

This is the guard the whole design rests on. Without it, publishing a release tagged `main` makes the default `curl | sh` warn and then die for every new user.

**Files:**
- Modify: `authbridge/install.sh` (the `newest_release()` function)
- Test: `authbridge/install_test.sh`

**Interfaces:**
- Consumes: `with_newest_release`, `fixture`, `check`, `check_fails` from Task 1.
- Produces: `newest_release()` returns the newest `tag_name` matching `v[0-9]*` from the first 10 releases, or non-zero if none matches. Task 3 relies on it still printing a bare tag on stdout and returning non-zero on failure.

- [ ] **Step 1: Write the failing tests**

Append to `authbridge/install_test.sh`, before the final `printf '\n%s passed…'` summary block:

```sh
# --- newest_release: the main channel must not hijack the default ---
#
# The rolling `main` release sorts first until the next tagged release, because the
# API sorts by created_at and created_at is fixed at creation. Unfiltered, the
# v[0-9]* shape check then rejects it and the whole default install dies.

fixture main_first.json <<'EOF'
[
  {
    "tag_name": "main",
    "prerelease": true
  },
  {
    "tag_name": "v0.7.0-alpha.7"
  }
]
EOF
check "a main release sorting first is skipped" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

fixture main_first_compact.json <<'EOF'
[{"tag_name":"main"},{"tag_name":"v0.7.0-alpha.7"},{"tag_name":"v0.3.1"}]
EOF
check "main skipped in compact JSON too" "v0.7.0-alpha.7" "$(with_newest_release "${FIXTURE}")"

fixture only_non_v.json <<'EOF'
[{"tag_name":"main"},{"tag_name":"nightly"}]
EOF
set +e
_out=$(with_newest_release "${FIXTURE}"); _st=$?
set -e
check_fails "a page with no v-tag returns non-zero rather than guessing" "${_st}"
check "and prints nothing on stdout" "" "${_out}"

# --- newest_release: hostile inputs still fail closed ---

fixture ratelimit.json <<'EOF'
{"message":"API rate limit exceeded","documentation_url":"https://x"}
EOF
set +e
_out=$(with_newest_release "${FIXTURE}"); _st=$?
set -e
check_fails "rate-limit body fails" "${_st}"

fixture html.json <<'EOF'
<html><body>502 Bad Gateway</body></html>
EOF
set +e
_st=0; _out=$(with_newest_release "${FIXTURE}") || _st=$?
set -e
check_fails "an HTML error page fails" "${_st}"

fixture empty.json </dev/null
set +e
_st=0; _out=$(with_newest_release "${FIXTURE}") || _st=$?
set -e
check_fails "an empty body fails" "${_st}"
```

- [ ] **Step 2: Run and verify the new cases fail**

Run: `sh authbridge/install_test.sh`

Expected: the two `main` skip cases FAIL. Unfiltered, `newest_release` sees `main` first, the shape check rejects it, and the function returns non-zero — so the assertion of `v0.7.0-alpha.7` fails. The hostile-input cases should already pass; they are regression cover for behaviour Task 2 must not break.

- [ ] **Step 3: Implement the filter**

In `authbridge/install.sh`, replace the `_tag=$(…)` assignment and the `case` block inside `newest_release()` with:

```sh
	# ?per_page=10 and a v-tag filter, not ?per_page=1: the rolling `main` release of
	# the developer channel sorts first until the next tagged release (the API sorts
	# by created_at, which is fixed at creation). Taking the first entry blindly
	# would hand `main` to someone who never asked for it — and then the shape check
	# below would reject it and kill the install outright. Ten is headroom, not a
	# calculation: there is one rolling release, so two would do.
	_tag=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases?per_page=10" 2>/dev/null \
		| tr ',{}' '\n' \
		| grep '^[[:space:]]*"tag_name"[[:space:]]*:' \
		| cut -d'"' -f4 \
		| grep -m1 '^v[0-9]')
	case "${_tag}" in
		v[0-9]*) ;;
		*) return 1 ;;
	esac
	printf '%s\n' "${_tag}"
```

Two deliberate changes beyond the page size. `grep -m1` moves from the key match to the *value* filter, so it selects the first `v`-shaped tag rather than the first tag. And the `warn` on an unexpected tag is dropped: with the filter, "no `v` tag in ten releases" is the only way to reach the failure, and warning `main` at someone who never mentioned it is noise. The caller already dies with an actionable message.

- [ ] **Step 4: Run the tests and verify all pass**

Run: `sh authbridge/install_test.sh`

Expected: all cases `ok`, `0 failed`, exit 0. Both `main`-skip cases now resolve `v0.7.0-alpha.7`; the hostile inputs still fail closed.

- [ ] **Step 5: Verify against the live API**

Run:
```bash
curl -fsSL "https://api.github.com/repos/rossoctl/cortex/releases?per_page=10" \
  | tr ',{}' '\n' | grep '^[[:space:]]*"tag_name"[[:space:]]*:' \
  | cut -d'"' -f4 | grep -m1 '^v[0-9]'
```
Expected: the newest `v*` tag (`v0.7.0-alpha.7` or later). Fixtures prove the logic; this proves the shape assumption about the real API still holds.

- [ ] **Step 6: shellcheck and commit**

```bash
shellcheck --severity=error authbridge/install.sh && shellcheck -s sh authbridge/install.sh
git add authbridge/install.sh authbridge/install_test.sh
git commit -s -m "$(printf 'fix: Ignore non-release tags when resolving the newest release\n\nnewest_release() took the first entry of ?per_page=1 and required it to look like\nv<digit>. That is about to break: the developer channel publishes a rolling\npre-release tagged `main`, and it sorts first until the next tagged release,\nbecause the API sorts by created_at and created_at is fixed at creation.\n\nUnfiltered, the default curl|sh would then warn "could not resolve the newest\nrelease; continuing with the copy from main", fall through to main script, fail to\nresolve a version, and die — for every new user, on the documented one-liner.\n\nNow fetches 10 and takes the first tag matching v[0-9]*, so the channel is\ninvisible to the default path. Ten is headroom, not a calculation.\n\nThe unexpected-tag warning is gone: with the filter, the only way to fail is "no\nv-tag in ten releases", and warning `main` at someone who never asked for it is\nnoise. The caller already dies with actionable advice.\n\nLands before anything publishes a main release, and must be in a cut release\nbefore the channel is enabled — the released copy is what every curl|sh\nbootstraps into.\n\nAssisted-By: Claude (Anthropic AI) <noreply@anthropic.com>')"
```

---

### Task 3: Make `--ref=X` select binaries as well as the script

**Files:**
- Modify: `authbridge/install.sh` (the `# --- resolve the release tag ---` block)
- Test: `authbridge/install_test.sh`

**Interfaces:**
- Consumes: `newest_release()` from Task 2; `fixture`/`check` from Task 1.
- Produces: a `resolve_version` shell function extracted for testability, signature `resolve_version <script_ref>`, echoing the version to install. Task 4 does not depend on it; Task 5 documents it.

- [ ] **Step 1: Write the failing test**

The current code inlines version resolution, which cannot be tested without running the installer. Extract it into a function first, then test the function. Append to `authbridge/install_test.sh` before the summary:

```sh
# --- resolve_version: --ref=X selects binaries too ---
#
# --ref was inconsistent: `--ref=v0.7.0-alpha.4` set script AND binaries, while
# `--ref=main` set only the script, because there was no main release to download
# from. One rule now covers both.

with_resolve_version() { # script_ref fixture-path
	_ref=$1; _f=$2
	{
		printf 'REPO=rossoctl/cortex\n'
		printf 'warn() { printf "warning: %%s\\n" "$*" >&2; }\n'
		printf 'die() { printf "error: %%s\\n" "$*" >&2; exit 1; }\n'
		printf 'info() { :; }\n'
		printf 'curl() { cat "%s"; }\n' "${_f}"
		sed -n '/^newest_release()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^resolve_version()/,/^}/p' "${INSTALL_SH}"
		printf 'resolve_version "%s"\n' "${_ref}"
	} >"${TMP}/rv.sh"
	sh "${TMP}/rv.sh" 2>/dev/null
}

fixture rv_releases.json <<'EOF'
[{"tag_name":"main"},{"tag_name":"v0.7.0-alpha.7"}]
EOF
check "--ref=main installs main binaries" "main" "$(with_resolve_version main "${FIXTURE}")"
check "--ref=v0.7.0-alpha.4 installs that release" "v0.7.0-alpha.4" "$(with_resolve_version v0.7.0-alpha.4 "${FIXTURE}")"
check "no ref resolves the newest v-tag" "v0.7.0-alpha.7" "$(with_resolve_version "" "${FIXTURE}")"

# stdout must be the version and nothing else. info() writes to stdout
# (install.sh:81), so an un-redirected progress line inside resolve_version would be
# captured into $version and corrupt every download URL — a one-line mistake that
# breaks every install and no other test would catch.
_out=$(with_resolve_version "" "${FIXTURE}")
check "stdout carries no progress text" "1" "$(printf '%s\n' "${_out}" | wc -l | tr -d ' ')"
```

- [ ] **Step 2: Run and verify it fails**

Run: `sh authbridge/install_test.sh`

Expected: the three `resolve_version` cases FAIL — `sed -n '/^resolve_version()/…'` matches nothing, so the probe script calls an undefined function and produces empty output.

- [ ] **Step 3: Extract and extend the resolution**

In `authbridge/install.sh`, replace the whole `# --- resolve the release tag ---` block with a function definition plus its call. Place the function next to `newest_release()` (above first use, since `sh` executes top to bottom):

```sh
# resolve_version prints the release tag whose binaries should be installed, given
# the ref this script came from.
#
# One rule: --ref=X installs X. That was not true before — `--ref=v0.7.0-alpha.4`
# set script and binaries, while `--ref=main` set only the script, because there
# was no `main` release to download from. Now that the developer channel publishes
# one, the special case disappears rather than growing a second flag.
resolve_version() { # script_ref
	case "$1" in
		v*|main) printf '%s\n' "$1" ;;
		*)
			# >&2 deliberately: this function's stdout IS the resolved version, and
			# info() writes to stdout (install.sh:81). Without the redirect the
			# progress line lands inside `version` and corrupts every download URL.
			info "Resolving newest release..." >&2
			newest_release || return 1
			;;
	esac
}
```

Then at the original location:

```sh
# --- resolve the release tag ---
version=$(resolve_version "${SCRIPT_REF}") \
	|| die "could not resolve the newest release (pass --ref=vX.Y.Z to pin one)"
```

- [ ] **Step 4: Run the tests and verify all pass**

Run: `sh authbridge/install_test.sh`

Expected: all cases `ok`, exit 0.

- [ ] **Step 5: Verify the real paths still work**

```bash
shellcheck --severity=error authbridge/install.sh && shellcheck -s sh authbridge/install.sh
sh -n authbridge/install.sh
# default path, real network, into a throwaway HOME on shifted ports
rm -rf /tmp/t1 && mkdir -p /tmp/t1/.local/bin /tmp/t1/.claude
printf '{}\n' > /tmp/t1/.claude/settings.json
sed -e 's|"${BIN_DIR}/abctl" service install --yes --proxy "${BIN_DIR}/authbridge-proxy"|true|' \
    -e 's/^DEMO_\(FORWARD\|SESSION\|STATS\|HEALTH\)_PORT=4760/DEMO_\1_PORT=4779/' \
    authbridge/install.sh > /tmp/t1/inst.sh
HOME=/tmp/t1 sh /tmp/t1/inst.sh --ref=main --install-only --yes </dev/null 2>&1 | tail -4
```

Expected: it resolves and reports `v0.7.0-alpha.7` (no `main` release exists yet, so `--ref=main` will fail the *download*, not the resolution). Confirm the failure is `download failed: abctl_main_darwin_arm64.tar.gz` — that is the correct error before the channel exists, and it proves resolution now yields `main`.

- [ ] **Step 6: Commit**

```bash
git add authbridge/install.sh authbridge/install_test.sh
git commit -s -m "$(printf 'fix: Make --ref=X select binaries as well as the script\n\n--ref was inconsistent: --ref=v0.7.0-alpha.4 set script AND binaries, while\n--ref=main set only the script, because there was no main release to download\nfrom. Whether one flag set one half or both depended on whether the value looked\nlike a tag.\n\nNow that the developer channel publishes a rolling `main` release, one rule covers\nboth: --ref=X installs X. The special case disappears rather than growing a second\nflag — an earlier draft added --main, and two flags differing by punctuation but\nnot by meaning is worse than the inconsistency it papered over.\n\nResolution moves into resolve_version() so it can be tested without running the\ninstaller; it was inline before and effectively untestable.\n\nAssisted-By: Claude (Anthropic AI) <noreply@anthropic.com>')"
```

---

### Task 4: Remove three vestigial env vars

All five `AUTHBRIDGE_*` vars were audited in the spec: none is referenced anywhere outside `install.sh` — no CI, docs, demos, or Makefiles. Three go.

**Files:**
- Modify: `authbridge/install.sh` (header comment, `usage()`, arg parsing, read sites, three messages)
- Test: `authbridge/install_test.sh`

**Interfaces:**
- Consumes: nothing from earlier tasks beyond the harness helpers.
- Produces: no new interfaces. `AUTHBRIDGE_SKIP_DOWNLOAD` and `AUTHBRIDGE_SCRIPT_REF` continue to work.

- [ ] **Step 1: Write the failing test**

The likely regression is not the removal — it is leaving a message that still recommends a removed var. Assert against that. Append before the summary:

```sh
# --- removed knobs leave no stale advice ---
#
# The failure mode is not "the var still works" — it is a message telling someone
# to set a var the script no longer reads. Three sites advised AUTHBRIDGE_VERSION.

for _v in AUTHBRIDGE_VERSION AUTHBRIDGE_REF AUTHBRIDGE_INSTALL_ONLY; do
	_hits=$(grep -c "${_v}" "${INSTALL_SH}" || true)
	check "${_v} is gone from install.sh entirely" "0" "${_hits}"
done

for _v in AUTHBRIDGE_SKIP_DOWNLOAD AUTHBRIDGE_SCRIPT_REF; do
	_hits=$(grep -c "${_v}" "${INSTALL_SH}" || true)
	check_fails "${_v} is still present" "${_hits}"
done
```

`check_fails` is reused here for "non-zero count" — the same predicate, and clearer than adding a near-identical helper.

- [ ] **Step 2: Run and verify it fails**

Run: `sh authbridge/install_test.sh`

Expected: the three "is gone" cases FAIL with non-zero counts (`AUTHBRIDGE_VERSION` around 5, `AUTHBRIDGE_REF` 3, `AUTHBRIDGE_INSTALL_ONLY` 3). The two "still present" cases pass.

- [ ] **Step 3: Remove them**

Four edits in `authbridge/install.sh`:

1. **Header comment** — delete these four lines from the `# Environment:` block, keeping the `AUTHBRIDGE_SKIP_DOWNLOAD` pair, and retitle the block to mark it internal:

```
#   AUTHBRIDGE_REF=REF          same as --ref
#   AUTHBRIDGE_VERSION=vX.Y.Z   install binaries from a specific release
#                               (default: the release this script came from)
#   AUTHBRIDGE_INSTALL_ONLY=1   same as --install-only
```

Replace the block header `# Environment:` with:

```
# Environment (maintainer testing only — not part of the documented interface):
```

2. **`usage()`** — delete the whole `Environment:` section, all six lines:

```
Environment:
  AUTHBRIDGE_VERSION=vX.Y.Z   install a specific release tag (default: newest)
  AUTHBRIDGE_INSTALL_ONLY=1   same as --install-only
  AUTHBRIDGE_SKIP_DOWNLOAD=1  use the binaries already in ~/.local/bin instead of
                              downloading (re-run setup offline)
```

`AUTHBRIDGE_SKIP_DOWNLOAD` keeps working but stops being advertised: it is a testing hook, not a way to install.

3. **Arg parsing** — `--ref=*` currently assigns to the env var. Make it a plain local:

```sh
		--ref=*) WANT_REF="${arg#*=}" ;;
```

Initialise `WANT_REF=""` beside the other defaults (`MODE=local`, `WIRE_CLAUDE_CODE=""`, `ASSUME_YES=""`), and change the bootstrap's read from `want_ref="${AUTHBRIDGE_REF:-}"` to `want_ref="${WANT_REF:-}"`.

Delete the `AUTHBRIDGE_INSTALL_ONLY` fallback block entirely:

```sh
# Env form kept working; the flag wins if both are given.
if [ "${AUTHBRIDGE_INSTALL_ONLY:-}" = "1" ] && [ "$MODE" = "local" ]; then
	MODE=install-only
fi
```

4. **Three messages** — rewrite each to name `--ref` instead:

- The `resolve_version` caller from Task 3 already says `pass --ref=vX.Y.Z to pin one`. Confirm no `AUTHBRIDGE_VERSION` remains there.
- In the abctl-lacks-`service` guard, `AUTHBRIDGE_VERSION=<newer tag>` becomes `--ref=<newer tag>`.
- In that guard's non-tag branch, `or point AUTHBRIDGE_VERSION at a release that has it.` becomes `or pass --ref=<a release that has it>.`

- [ ] **Step 4: Run the tests and verify all pass**

Run: `sh authbridge/install_test.sh`

Expected: all `ok`, exit 0.

- [ ] **Step 5: Verify the surviving paths and the help text**

```bash
shellcheck --severity=error authbridge/install.sh && shellcheck -s sh authbridge/install.sh
sh authbridge/install.sh --help | sed -n '1,40p'   # no Environment section
# SKIP_DOWNLOAD must still work — every manual installer test uses it
rm -rf /tmp/t2 && mkdir -p /tmp/t2/.local/bin /tmp/t2/.claude
printf '{}\n' > /tmp/t2/.claude/settings.json
cp ~/.local/bin/abctl ~/.local/bin/authbridge-proxy /tmp/t2/.local/bin/ 2>/dev/null || true
sed -e 's|"${BIN_DIR}/abctl" service install --yes --proxy "${BIN_DIR}/authbridge-proxy"|true|' \
    -e 's/^DEMO_\(FORWARD\|SESSION\|STATS\|HEALTH\)_PORT=4760/DEMO_\1_PORT=4778/' \
    authbridge/install.sh > /tmp/t2/inst.sh
HOME=/tmp/t2 AUTHBRIDGE_SKIP_DOWNLOAD=1 sh /tmp/t2/inst.sh --ref=main --install-only --yes </dev/null 2>&1 | tail -3
```

Expected: `--help` shows no `Environment:` section; the `SKIP_DOWNLOAD` run reports using the existing binaries rather than downloading.

- [ ] **Step 6: Commit**

```bash
git add authbridge/install.sh authbridge/install_test.sh
git commit -s -m "$(printf 'refactor: Remove three vestigial installer env vars\n\nAll five AUTHBRIDGE_* vars were audited: none is referenced anywhere outside\ninstall.sh — no CI, docs, demos or Makefiles.\n\nAUTHBRIDGE_REF and AUTHBRIDGE_INSTALL_ONLY are pure aliases for flags that exist,\nleft over from when piping to sh made flags awkward to pass, which `sh -s --\n--flag` solves. AUTHBRIDGE_VERSION is redundant now that --ref=X selects binaries\ntoo.\n\nAUTHBRIDGE_SKIP_DOWNLOAD stays because it does something no flag does and the\ninstall tests depend on it, but it drops out of usage(): it is a testing hook, not\na way to install. AUTHBRIDGE_SCRIPT_REF stays untouched — it is the bootstrap\nrecursion guard, and removing it causes infinite re-exec.\n\nThree messages advised AUTHBRIDGE_VERSION and now name --ref. A script\nrecommending a knob it no longer honours would be worse than the clutter removed,\nso the tests assert the strings are gone rather than merely that the vars are\nunread.\n\nBREAKING for anyone scripting the three removed vars. Zero in-repo references,\nalpha-stage, teammate-sized audience.\n\nAssisted-By: Claude (Anthropic AI) <noreply@anthropic.com>')"
```

---

### Task 5: Publish the rolling `main` release from CI

**Files:**
- Modify: `.github/workflows/release-binaries.yaml`

**Interfaces:**
- Consumes: nothing from earlier tasks at runtime, but the guard from Task 2 **must be in a cut release** before `MAIN_CHANNEL_ENABLED` is set (see Task 6).
- Produces: for a push to `main` with the variable set, a pre-release tagged `main` whose assets are named `abctl_main_<os>_<arch>.tar.gz` / `authbridge-proxy[-variant]_main_<os>_<arch>.tar.gz`, each binary stamped `main-<short-sha>`.

- [ ] **Step 1: Add the trigger, gated**

In `.github/workflows/release-binaries.yaml`, add the branch trigger beside the existing tag trigger:

```yaml
on:
  push:
    tags:
      - 'v*'
    branches:
      - main
  workflow_dispatch:
```

Then gate the job so an unset variable is a no-op. On the existing job, add:

```yaml
    # The developer channel is off until a release containing the newest_release()
    # v-tag filter exists. Publishing a `main` release before that would make the
    # DEFAULT curl|sh die, because the released copy of install.sh is what every
    # one-liner bootstraps into. Set MAIN_CHANNEL_ENABLED=true in repo variables
    # after cutting that release.
    if: github.ref_type == 'tag' || vars.MAIN_CHANNEL_ENABLED == 'true'
```

- [ ] **Step 2: Split TAG from VERSION**

Replace the version-derivation block (currently `if [ "${GITHUB_REF_TYPE}" = "tag" ]; then VERSION="${GITHUB_REF_NAME}" … fi`) with:

```sh
            if [ "${GITHUB_REF_TYPE}" = "tag" ]; then
              TAG="${GITHUB_REF_NAME}"
              VERSION="${GITHUB_REF_NAME}"
            elif [ "${GITHUB_REF_NAME}" = "main" ]; then
              # TAG names the rolling release and the assets, so the download URL is
              # predictable. VERSION identifies the build, and the two must differ:
              # abctl's serviceIsCurrent compares the stamp recorded in the installed
              # launchd unit against the running binary to decide whether `service
              # install` is a no-op. A constant stamp would report "Already current"
              # while the old binary kept running — hiding upgrades on the one channel
              # that changes every merge.
              TAG="main"
              VERSION="main-$(printf '%s' "${GITHUB_SHA}" | cut -c1-7)"
            else
              TAG="${INPUT_TAG:-dev}"
              VERSION="${INPUT_TAG:-dev}"
            fi
            echo "Building ${VERSION} for release ${TAG}"
```

Add `GITHUB_SHA: ${{ github.sha }}` to that step's `env:` block if it is not already exposed. Then replace every `${VERSION}` used in an **asset filename** with `${TAG}`, and leave `${VERSION}` in the two `-ldflags` invocations. The filename sites are `dist/authbridge-proxy${suffix}_${VERSION}_${os}_${arch}.tar.gz` and `dist/abctl_${VERSION}_${os}_${arch}.tar.gz`; every later reference to those archives must use the same variable.

- [ ] **Step 3: Mark it a prerelease and give it notes**

In the `gh release create` arm, add `main` to the prerelease case and give the rolling release its own notes. The existing case is `case "${TAG}" in *-rc*|*-alpha*|*-beta*) prerelease="--prerelease" ;; esac` — change it to:

```sh
              case "${TAG}" in
                main | *-rc* | *-alpha* | *-beta*) prerelease="--prerelease" ;;
              esac
```

Without `main` in that list the rolling release would be created without `--prerelease` and would take GitHub's "Latest" badge, displacing `v0.3.1`.

For the notes, when `TAG` is `main`, replace the release-notes body with:

```
Unreleased build from the tip of `main`, rebuilt on every merge. Not a release.

Install with:
    curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh | sh -s -- --ref=main

Each binary reports its own build (`abctl --version` → `main-<sha>`). For a
supported install, use the newest `v*` release instead.
```

- [ ] **Step 4: Validate the workflow file**

```bash
python3 -c "import yaml,io; yaml.safe_load(io.open('.github/workflows/release-binaries.yaml')); print('YAML parses')"
```

Expected: `YAML parses`. Then re-read the diff and confirm by eye: every asset filename uses `${TAG}`, both `-ldflags` use `${VERSION}`, and no `${VERSION}` remains in a `tar`, `gh release`, or `checksums` path. A mismatch here produces assets the installer cannot find — a 404 for every user of the channel.

- [ ] **Step 5: Dry-run the build without publishing**

Use the existing `workflow_dispatch` path, which builds and uploads artifacts without creating a release:

```bash
gh workflow run release-binaries.yaml -f tag=dev-mainchannel-check
gh run watch "$(gh run list --workflow=release-binaries.yaml --limit 1 --json databaseId -q '.[0].databaseId')"
```

Expected: success, and the run's artifacts contain 16 archives named `_dev-mainchannel-check_`. This exercises the split code path without touching the release namespace. Confirm no release was created: `gh release view dev-mainchannel-check` must fail with "release not found".

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/release-binaries.yaml
git commit -s -m "$(printf 'feat: Publish a rolling main release for the developer channel\n\nPushes to main now build the same 16 archives a tag does and overwrite the assets\non a single pre-release tagged `main`, so a merged fix is installable with\n--ref=main minutes later instead of waiting for someone to cut a tag.\n\nNo new publishing logic: the existing `gh release view || create` branch already\nis rolling behaviour. The trigger is extended rather than a second workflow added,\nbecause a duplicated 16-archive matrix would drift — this repo has already paid\nfor two implementations of one thing twice (install.sh stop_previous_cortex vs\nabctl adoption, and the migration two parallel pin lists).\n\nTAG and VERSION now differ, deliberately. TAG names the release and the assets so\nthe download URL is predictable; VERSION stamps main-<sha> into the binary. abctl\nserviceIsCurrent compares the stamp in the installed unit against the running\nbinary, so a constant stamp would report "Already current" while the old binary\nkept running — hiding upgrades on the one channel that changes every merge.\n\n`main` joins the prerelease case, or the rolling release would take GitHub Latest\nbadge from v0.3.1.\n\nGated on MAIN_CHANNEL_ENABLED, unset. The guard in newest_release() must ship in\na cut release first: the released copy of install.sh is what every curl|sh\nbootstraps into, so a main release existing before then would kill the default\ninstall.\n\nAssisted-By: Claude (Anthropic AI) <noreply@anthropic.com>')"
```

---

### Task 6: Document the channel and the enable procedure

**Files:**
- Modify: `CONTRIBUTING.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: the behaviour built in Tasks 2, 3, 5.
- Produces: no code.

- [ ] **Step 1: Document the channel in CONTRIBUTING.md**

Add a section. Read the file first and match its heading depth and tone; place it near other developer-workflow content, not at the end by default.

```markdown
## Installing an unreleased build

A fix merged to `main` is installable immediately, without waiting for a release:

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh \
  | sh -s -- --claude-code --ref=main
```

`--ref=X` means "install X" — both the installer script and the binaries. Every push
to `main` rebuilds a rolling pre-release tagged `main`, so this tracks the tip.

Each binary reports its own build: `abctl --version` → `main-a1b2c3d`. Quote that,
not "main", in a bug report — the channel moves under you.

Three things to know:

- **Every `--ref=main` run replaces and restarts the service.** The installed build
  never matches the requested `main`, so the installer always re-downloads and
  `service install` always reinstalls. That cuts any running Claude Code session,
  because `HTTPS_PROXY` is fixed in each session's environment at startup.
- **`checksums.txt` rolls with the assets.** The installer fetches assets and
  checksums in the same run, so verification is sound. Downloading them hours apart
  will mismatch.
- **The plain one-liner takes you back.** Running it without `--ref` reinstalls the
  newest release and reinstalls the service, so there is no stuck state to clean up.

### Enabling the channel (one-time, maintainers)

The rolling release must not exist before a cut release contains the
`newest_release()` v-tag filter — the released copy of `install.sh` is what every
`curl | sh` bootstraps into, and an unfiltered copy dies when it sees a `main` tag.
So: merge, cut a release, then set the repo variable `MAIN_CHANNEL_ENABLED=true`
(Settings → Secrets and variables → Actions → Variables). The next push to `main`
publishes it.
```

- [ ] **Step 2: Fix the README's `--ref` sentence**

The quickstart currently ends with a sentence saying the script re-runs the copy from the newest release and that "`--ref` overrides". That now understates `--ref`. Locate it and change the final clause to name both halves — for example `--ref=vX.Y.Z pins a release, and --ref=main installs the unreleased tip (see CONTRIBUTING.md)`.

Do **not** add the `--ref=main` command itself to the quickstart. The audience there is new users; the channel belongs in CONTRIBUTING.md.

- [ ] **Step 3: Verify links and rendering**

```bash
# every relative link in the touched files resolves
for f in CONTRIBUTING.md README.md; do
  grep -o '](\.\{0,2\}/\?[^)]*\.md[^)]*)' "$f" | sed 's/^](//;s/)$//' | while read -r l; do
    t="${l%%#*}"; [ -e "$t" ] && echo "ok: $f -> $l" || echo "BROKEN: $f -> $l"
  done
done
pre-commit run --files CONTRIBUTING.md README.md
```

Expected: no `BROKEN` lines; pre-commit passes.

- [ ] **Step 4: Commit**

```bash
git add CONTRIBUTING.md README.md
git commit -s -m "$(printf 'docs: Document the developer channel and how to enable it\n\nA fix merged to main is now installable with --ref=main. Documented in\nCONTRIBUTING.md rather than the README quickstart on purpose: the audience there\nis new users, and putting an unreleased channel beside the getting-started command\nwould place them one flag from untested code — which is what the release-bootstrap\nexists to prevent.\n\nRecords the three things that surprise people: every --ref=main run restarts the\nservice and cuts live Claude Code sessions; checksums roll with the assets, so\ndownloading them hours apart mismatches; and the plain one-liner takes you back to\nreleases with no stuck state.\n\nAlso records the enable procedure, because the ordering is load-bearing and easy\nto get wrong: the v-tag filter must be in a CUT RELEASE before the rolling `main`\nrelease exists, since the released copy of install.sh is what every curl|sh\nbootstraps into.\n\nREADME `--ref` sentence corrected — it said "--ref overrides", which understated a\nflag that now selects binaries too.\n\nAssisted-By: Claude (Anthropic AI) <noreply@anthropic.com>')"
```

---

### Task 7: End-to-end verification and PR

**Files:** none modified — this task verifies and opens the PR.

**Interfaces:**
- Consumes: everything from Tasks 1-6.
- Produces: a PR against `main`.

- [ ] **Step 1: Full local check**

```bash
sh authbridge/install_test.sh
shellcheck --severity=error authbridge/install.sh authbridge/install_test.sh
shellcheck -s sh authbridge/install.sh authbridge/install_test.sh
sh -n authbridge/install.sh
python3 -c "import yaml,io; yaml.safe_load(io.open('.github/workflows/release-binaries.yaml')); yaml.safe_load(io.open('.github/workflows/ci.yaml')); print('workflows parse')"
pre-commit run --files authbridge/install.sh authbridge/install_test.sh CONTRIBUTING.md README.md .github/workflows/release-binaries.yaml .github/workflows/ci.yaml
```

Expected: tests pass, shellcheck silent, workflows parse, pre-commit clean.

- [ ] **Step 2: Prove the default install is unaffected, on real network**

```bash
rm -rf /tmp/e2e-default && mkdir -p /tmp/e2e-default/.local/bin /tmp/e2e-default/.claude
printf '{}\n' > /tmp/e2e-default/.claude/settings.json
sed -e 's|"${BIN_DIR}/abctl" service install --yes --proxy "${BIN_DIR}/authbridge-proxy"|true|' \
    -e 's/^DEMO_\(FORWARD\|SESSION\|STATS\|HEALTH\)_PORT=4760/DEMO_\1_PORT=4777/' \
    authbridge/install.sh > /tmp/e2e-default/inst.sh
HOME=/tmp/e2e-default sh /tmp/e2e-default/inst.sh --ref=main --install-only --yes </dev/null 2>&1 | tail -3
HOME=/tmp/e2e-default sh /tmp/e2e-default/inst.sh --install-only --yes </dev/null 2>&1 | tail -3
/tmp/e2e-default/.local/bin/abctl --version
```

Expected: the second run downloads and installs the newest `v*` release, and `abctl --version` reports that tag. The first run fails at download (`abctl_main_…tar.gz`) because no `main` release exists yet — that is the correct pre-enable state and confirms resolution reaches `main`.

- [ ] **Step 3: Do not enable the channel**

Confirm `MAIN_CHANNEL_ENABLED` is unset:

```bash
gh variable list 2>/dev/null | grep -c MAIN_CHANNEL_ENABLED
```

Expected: `0`. If it is set, unset it — enabling belongs after a release is cut (Task 6, Step 1).

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin feat/main-channel
```

Then open a PR titled `Feat: Developer channel — install unreleased main binaries`, with a body covering: the problem (a merged fix was not installable, with the sandbox report as the concrete case); the three verified findings (artifacts 401 anonymously, an unfiltered `main` release kills the default install, the TAG/VERSION split is required by `serviceIsCurrent`); the surface *shrinking* by three env vars; the enable sequencing; and that `install.sh` gained its first tests. End with the `Assisted-By` trailer.

- [ ] **Step 5: Confirm CI is green**

```bash
gh pr checks --watch
```

Expected: all checks pass, including the new `install.sh tests` job. The `Release binaries` workflow must **not** run for this PR — it triggers on tag pushes and on `main` pushes, not on pull requests. If it appears, the trigger is wrong.
