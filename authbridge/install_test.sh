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

# shellcheck disable=SC1007 # `CDPATH= cd` is deliberate, not a typo: it empties
# CDPATH for this one command. The documented invocation is `sh
# authbridge/install_test.sh`, so $0 is RELATIVE — with a CDPATH set, `cd` can
# resolve it against a CDPATH entry and land somewhere else entirely.
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

# --- resolve_version: --ref=X selects binaries too ---
#
# --ref was inconsistent: `--ref=v0.7.0-alpha.4` set script AND binaries, while
# `--ref=main` set only the script, because there was no main release to download
# from. One rule now covers both.

with_resolve_version() { # script_ref fixture-path [channel_requested]
	_ref=$1; _f=$2; _req=${3:-}
	{
		printf 'REPO=rossoctl/cortex\n'
		# CHANNEL_TAG is read out of install.sh, not restated here, so this probe
		# cannot disagree with the script about what the channel is called.
		sed -n '/^CHANNEL_TAG=/p' "${INSTALL_SH}"
		printf 'warn() { printf "warning: %%s\\n" "$*" >&2; }\n'
		printf 'die() { printf "error: %%s\\n" "$*" >&2; exit 1; }\n'
		# info() is stubbed to match install.sh's definition EXACTLY — stdout, not
		# stderr. Stubbing it silent (`info() { :; }`) would leave the
		# "stdout carries no progress text" case below unable to fail, which is the
		# one thing it exists to catch.
		printf 'info() { printf "%%s\\n" "$*"; }\n'
		printf 'curl() { cat "%s"; }\n' "${_f}"
		sed -n '/^newest_release()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^resolve_version()/,/^}/p' "${INSTALL_SH}"
		printf 'CHANNEL_REQUESTED=%s\n' "${_req}"
		printf 'resolve_version "%s"\n' "${_ref}"
	} >"${TMP}/rv.sh"
	sh "${TMP}/rv.sh" 2>/dev/null
}

fixture rv_releases.json <<'EOF'
[{"tag_name":"main"},{"tag_name":"v0.7.0-alpha.7"}]
EOF
# --ref=main resolves to the channel tag, not the literal string "main": a
# release tagged `main` would collide with the branch. See CHANNEL_TAG in
# install.sh. The harness must pick the constant up from the script rather than
# hardcode it, or renaming the channel silently passes a stale test.
CHANNEL_TAG=$(sed -n 's/^CHANNEL_TAG="\(.*\)"$/\1/p' "${INSTALL_SH}")
check "install.sh defines a non-colliding CHANNEL_TAG" "1" "$(printf '%s' "${CHANNEL_TAG}" | grep -c '^main-' || true)"
check "--ref=main installs the channel tag" "${CHANNEL_TAG}" "$(with_resolve_version main "${FIXTURE}" 1)"

# The bootstrap sets SCRIPT_REF=main for TWO other reasons: the release API was
# unreachable, and the wanted release has no install.sh. Both mean "run main's script";
# neither means "install main's binaries". Without this distinction a rate-limited user
# who ran the plain one-liner silently received an unreleased build — and rate limiting
# is reachable, not theoretical. Guarding on the literal "main" alone is what caused it,
# so the test pins the fallback, not just the happy path.
check "fallback to main's script still installs a RELEASE" "v0.7.0-alpha.7" "$(with_resolve_version main "${FIXTURE}")"
check "--ref=v0.7.0-alpha.4 installs that release" "v0.7.0-alpha.4" "$(with_resolve_version v0.7.0-alpha.4 "${FIXTURE}")"
check "no ref resolves the newest v-tag" "v0.7.0-alpha.7" "$(with_resolve_version "" "${FIXTURE}")"

# stdout must be the version and nothing else. info() writes to stdout
# (install.sh:81), so an un-redirected progress line inside resolve_version would be
# captured into $version and corrupt every download URL — a one-line mistake that
# breaks every install and no other test would catch.
# set +e around the capture: under `set -e` a bare assignment from a failing command
# substitution aborts the whole suite, so a regression that broke resolve_version
# would kill the run at this line instead of reporting a FAIL and carrying on.
set +e
_out=$(with_resolve_version "" "${FIXTURE}")
set -e
check "stdout carries no progress text" "1" "$(printf '%s\n' "${_out}" | wc -l | tr -d ' ')"
check "stdout is exactly the version" "v0.7.0-alpha.7" "${_out}"

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

printf '\n%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = "0" ]
