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

with_resolve_version() { # version_ref fixture-path
	_ref=$1; _f=$2
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
check "--ref=main installs the channel tag" "${CHANNEL_TAG}" "$(with_resolve_version main "${FIXTURE}")"

# Both channel spellings resolve identically — CHANNEL_TAG is the title on the Releases
# page, so it is what someone types after seeing it there.
check "--ref=CHANNEL_TAG resolves the same" "${CHANNEL_TAG}" "$(with_resolve_version "${CHANNEL_TAG}" "${FIXTURE}")"

# An empty VERSION_REF means "nobody named anything installable" and must resolve the
# newest RELEASE. It must never fall through to the channel: that is what the bootstrap
# passes when the API was unreachable, and the whole point is that a rate-limited plain
# one-liner does not silently receive an unreleased build. Asserted here at the function
# level and again against the real bootstrap block further down.
check "empty version ref resolves a RELEASE, never the channel" "v0.7.0-alpha.7" "$(with_resolve_version "" "${FIXTURE}")"
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


# --- bootstrap: which script runs vs which binaries get installed ---
#
# These two questions have different answers on every fallback, and conflating them let
# the DEFAULT one-liner install channel binaries. The resolve_version cases above cannot
# catch that: in isolation main -> CHANNEL_TAG is correct. The bug was in the bootstrap
# deciding to pass "main" at all. So extract the bootstrap block itself and assert the
# pair it produces.
with_bootstrap() { # want_ref http_code_for_script_fetch newest_release_output
	_want=$1; _http=$2; _newest=$3
	_start=$(awk '/^SCRIPT_REF="\$\{AUTHBRIDGE_SCRIPT_REF:-\}"/{print NR; exit}' "${INSTALL_SH}")
	_end=$(awk -v s="${_start}" 'NR>=s && /^fi$/{print NR; exit}' "${INSTALL_SH}")
	{
		printf 'REPO=rossoctl/cortex\n'
		sed -n '/^CHANNEL_TAG=/p' "${INSTALL_SH}"
		printf 'info() { :; }\nwarn() { :; }\ndie() { printf "DIED\\n"; exit 1; }\n'
		# newest_release: empty output + non-zero mimics an unreachable/rate-limited API.
		printf 'newest_release() { [ -n "%s" ] || return 1; printf "%%s\\n" "%s"; }\n' "${_newest}" "${_newest}"
		# curl here is only the raw.githubusercontent fetch of the released script; -w
		# makes the real one print the status code, so the stub prints the scenario's.
		printf 'curl() { printf "%%s" "%s"; }\n' "${_http}"
		# shellcheck disable=SC2016 # literal on purpose: these expand in the
		# generated probe, not here. Same reason install.sh disables SC2016.
		printf 'mktemp() { printf "%%s\\n" "${TMPDIR:-/tmp}/boot.$$"; }\n'
		printf 'AUTHBRIDGE_SCRIPT_REF=""\nWANT_REF="%s"\n' "${_want}"
		sed -n "${_start},${_end}p" "${INSTALL_SH}"
		# shellcheck disable=SC2016 # same: the probe prints its own variables.
		printf 'printf "script=%%s version=%%s\\n" "${SCRIPT_REF}" "${VERSION_REF}"\n'
	} >"${TMP}/bs.sh"
	sh "${TMP}/bs.sh" 2>/dev/null
}

# The plain one-liner while the release API is unreachable. VERSION_REF must be empty so
# resolution still hunts for a release and fails loudly — NOT the channel. This is the
# regression: before the channel existed this path died with "could not resolve the
# newest release"; keying resolution on SCRIPT_REF turned it into a silent unreleased
# install. Unauthenticated api.github.com is 60 req/hr per IP, so it is routine from
# behind NAT, not a corner case.
check "API unreachable, no --ref: script=main, nothing to install" \
	"script=main version=" "$(with_bootstrap "" 000 "")"

# --ref=main, the documented spelling.
check "--ref=main: script=main, install main" \
	"script=main version=main" "$(with_bootstrap main 000 v0.7.0-alpha.7)"

# --ref=<CHANNEL_TAG>, which is the title shown on the Releases page, so someone will
# type it after seeing it there. It must mean the same thing as --ref=main rather than
# bootstrapping that tag's frozen script and then installing newest-release binaries.
check "--ref=CHANNEL_TAG behaves as --ref=main" \
	"script=main version=main" "$(with_bootstrap "${CHANNEL_TAG}" 000 v0.7.0-alpha.7)"

# A pinned release whose tag predates authbridge/install.sh: fall back to main's SCRIPT,
# but keep the pin for the BINARIES. Losing it here would break "--ref=X installs X" on
# the one path where the user was most explicit about X.
check "--ref=v0.5.0 with a 404 script: script=main, install v0.5.0" \
	"script=main version=v0.5.0" "$(with_bootstrap v0.5.0 404 v0.7.0-alpha.7)"

printf '\n%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = "0" ]
