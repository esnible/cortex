#!/bin/sh
# Tests for install.sh.
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
# Run: sh scripts/install_test.sh
set -eu

# shellcheck disable=SC1007 # `CDPATH= cd` is deliberate, not a typo: it empties
# CDPATH for this one command. The documented invocation is `sh
# install_test.sh`, so $0 is RELATIVE — with a CDPATH set, `cd` can
# resolve it against a CDPATH entry and land somewhere else entirely.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
INSTALL_SH="${SCRIPT_DIR}/install.sh"
# The suite also reads repo-level files. SCRIPT_DIR was the repository root until
# this script moved under scripts/, so resolve those from an explicit root instead.
REPO_ROOT=$(CDPATH= cd -- "${SCRIPT_DIR}/.." && pwd)
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

# emit_curl_stub prints a `curl` replacement that dispatches on the URL.
#
# One definition, used by every probe. newest_release() has two sources now, and a stub
# serving one body to both could not tell them apart: the feed fallback would look
# exercised while never running. Writing this twice is the drift that has already cost
# this file two vacuous passes.
# shellcheck disable=SC2016 # every expression below is literal on purpose: it is
# expanded by the generated probe, not here. One directive for the whole function beats
# five identical ones inline.
emit_curl_stub() { # api-fixture feed-fixture api-http-code
	printf 'curl() {\n'
	printf '  _u=""; for _a in "$@"; do case "$_a" in http*) _u="$_a" ;; esac; done\n'
	printf '  _o=""; _p=""; for _a in "$@"; do [ "${_p}" = "-o" ] && _o="$_a"; _p="$_a"; done\n'
	printf '  case "${_u}" in\n'
	printf '    *api.github.com*)\n'
	printf '      [ -z "${_o}" ] || cat "%s" > "${_o}"\n' "$1"
	printf '      [ -n "${_o}" ] || cat "%s"\n' "$1"
	printf '      printf "%%s" "%s"\n' "$3"
	printf '      [ "%s" = "200" ] || return 22\n' "$3"
	printf '      ;;\n'
	printf '    *releases.atom*) cat "%s" ;;\n' "$2"
	printf '  esac\n'
	printf '}\n'
}

# with_newest_release runs install.sh's newest_release() against a fixture.
#
# The function is extracted by line range rather than sourcing install.sh, because
# sourcing would run the whole installer. `curl` is replaced by a function so the
# extracted code is unmodified — testing what ships, not a copy of it.
with_newest_release() { # api-fixture-path [feed-fixture-path] [api-http-code]
	_f=$1
	_feed=${2:-/dev/null}
	_code=${3:-200}
	{
		printf 'REPO=rossoctl/cortex\n'
		printf 'warn() { printf "warning: %%s\\n" "$*" >&2; }\n'
		emit_curl_stub "${_f}" "${_feed}" "${_code}"
		sed -n '/^newest_release()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_api()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_feed()/,/^}/p' "${INSTALL_SH}"
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
		emit_curl_stub "${_f}" /dev/null 200
		sed -n '/^newest_release()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_api()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^release_tag_from_feed()/,/^}/p' "${INSTALL_SH}"
		sed -n '/^resolve_version()/,/^}/p' "${INSTALL_SH}"
		printf 'resolve_version "%s"\n' "${_ref}"
	} >"${TMP}/rv.sh"
	# stderr goes to a file rather than being discarded or leaked: two cases below assert
	# on the warnings it carries, and letting it reach the terminal would scatter
	# "Resolving newest release..." through the suite's own output.
	sh "${TMP}/rv.sh" 2>"${TMP}/rv.err"
}

# with_resolve_version_stderr is the same probe, returning stderr instead of stdout —
# the warnings are the behaviour under test here, and they must not reach stdout because
# stdout IS the resolved version.
with_resolve_version_stderr() { # version_ref fixture-path
	with_resolve_version "$1" "$2" >/dev/null
	cat "${TMP}/rv.err"
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

check "--ref=v0.7.0-alpha.4 installs that release" "v0.7.0-alpha.4" "$(with_resolve_version v0.7.0-alpha.4 "${FIXTURE}")"

# An empty VERSION_REF means "nobody named anything installable" and must resolve the
# newest RELEASE. It must never fall through to the channel: that is what the bootstrap
# passes when the API was unreachable, and the point is that a rate-limited plain
# one-liner does not silently receive an unreleased build. Asserted here at the function
# level and again against the real bootstrap block further down.
check "empty version ref resolves a RELEASE, never the channel" "v0.7.0-alpha.7" "$(with_resolve_version "" "${FIXTURE}")"

# A branch or a SHA has no published binaries, so the newest release is the only option
# — but it must SAY so, or it is indistinguishable from the plain one-liner and someone
# testing a branch gets a mixed set with nothing to attribute it to.
_st=0; _err=$(with_resolve_version_stderr feat/my-branch "${FIXTURE}") || _st=$?
check "a branch ref warns that binaries come from a release" "1" "$(printf '%s\n' "${_err}" | grep -c 'no binaries are published for feat/my-branch')"
check "an empty ref warns nothing" "0" "$(printf '%s\n' "$(with_resolve_version_stderr "" "${FIXTURE}")" | grep -c 'no binaries are published')"

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

# Each removed var is now mentioned on exactly ONE line, and that line rejects it. The
# earlier "zero occurrences" assertion was the wrong shape: ignoring a var someone still
# has set is a silent wrong answer, which is what this script's style exists to avoid, so
# a guard that dies is correct and has to be allowed to mention the name.
for _v in AUTHBRIDGE_VERSION AUTHBRIDGE_REF AUTHBRIDGE_INSTALL_ONLY; do
	_guard=$(grep -c "^\[ -z \"\${${_v}:-}\" \] || die" "${INSTALL_SH}" || true)
	check "${_v} has a guard that dies" "1" "${_guard}"
	# Expanded ONLY on the guard line. A line count would fail on prose that merely
	# names the var, which comments legitimately do; what matters is that nothing else
	# reads it to decide anything.
	_elsewhere=$(grep -v "^\[ -z \"\${${_v}:-}\" \] || die" "${INSTALL_SH}" | grep -c "\${${_v}" || true)
	check "${_v} is not expanded anywhere else" "0" "${_elsewhere}"
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
with_bootstrap() { # want_ref http_scripts http_root http_legacy newest_release [run_as=pipe|file]
	_want=$1; _httpnew=$2; _httproot=${3:-$2}; _httplegacy=${4:-$3}; _newest=$5; _runas=${6:-pipe}
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
		#
		# It must also WRITE the file, because the real curl does (-o "${boot}") and the
		# branch under test is `[ "${http}" = "200" ] && [ -s "${boot}" ]`. A stub that
		# only echoed the code left ${boot} empty, so the 200 arm was unreachable and
		# every scenario silently fell through to the failure handling below — including
		# the one asserting that a transport failure refuses to run main.
		printf 'curl() {\n'
		printf '  _out=""; _prev=""; _u=""\n'
		# shellcheck disable=SC2016 # literal on purpose: expanded by the probe, not here.
		printf '  for _a in "$@"; do [ "${_prev}" = "-o" ] && _out="${_a}"; case "$_a" in http*) _u="$_a" ;; esac; _prev="${_a}"; done\n'
		# shellcheck disable=SC2016 # same.
		# Pin the whole URL, not just its suffix. A suffix-only match sent a
		# mis-interpolated ref, a doubled slash or a truncated path into the `*)`
		# arm and returned the new-layout status, so a regression in root-URL
		# construction still passed. BADURL is neither 200 nor 404, so it reaches
		# install.sh's die and the scenario fails instead of quietly succeeding.
		# shellcheck disable=SC2016 # literal on purpose: expanded by the probe, not here.
		printf '  case "${_u}" in\n'
		# ARM ORDER IS LOAD-BEARING: shell `*` matches `/`, so the bare
		# .../cortex/*/install.sh arm also matches .../main/scripts/install.sh. The
		# scripts/ and authbridge/ arms must precede it. Mutant M37 reorders them and
		# reds two scenarios, so this constraint is tested, not merely asserted.
		printf '    *//install.sh|*/cortex//*|*/cortex/install.sh|*/cortex/scripts/install.sh) _c="BADURL" ;;\n'
		printf '    https://raw.githubusercontent.com/rossoctl/cortex/*/scripts/install.sh) _c="%s" ;;\n' "${_httpnew}"
		printf '    https://raw.githubusercontent.com/rossoctl/cortex/*/authbridge/install.sh) _c="%s" ;;\n' "${_httplegacy}"
		printf '    https://raw.githubusercontent.com/rossoctl/cortex/*/install.sh) _c="%s" ;;\n' "${_httproot}"
		printf '    *) _c="BADURL" ;;\n'
		printf '  esac\n'
		# shellcheck disable=SC2016 # same.
		printf '  [ "${_c}" = "200" ] || [ -z "${_out}" ] || : > "${_out}"\n'
		# The released script it serves says REEXECED, or is BS_CHILD when that is set.
		if [ -n "${BS_CHILD:-}" ]; then
			# shellcheck disable=SC2016 # same.
			printf '  [ "${_c}" != "200" ] || [ -z "${_out}" ] || cat "%s" > "${_out}"\n' "${BS_CHILD}"
		else
			# shellcheck disable=SC2016 # same.
			printf '  [ "${_c}" != "200" ] || [ -z "${_out}" ] || printf "#!/bin/sh\\nprintf \\"REEXECED\\\\n\\"\\nexit 0\\n" > "${_out}"\n'
		fi
		# shellcheck disable=SC2016 # same.
		printf '  printf "%%s" "${_c}"\n'
		printf '}\n'
		# shellcheck disable=SC2016 # literal on purpose: these expand in the
		# generated probe, not here. Same reason install.sh disables SC2016.
		printf 'mktemp() { printf "%%s\\n" "${TMPDIR:-/tmp}/boot.$$"; }\n'
		printf 'AUTHBRIDGE_SCRIPT_REF=""\nWANT_REF="%s"\n' "${_want}"
		sed -n "${_start},${_end}p" "${INSTALL_SH}"
		# shellcheck disable=SC2016 # same: the probe prints its own variables.
		printf 'printf "script=%%s version=%%s\\n" "${SCRIPT_REF}" "${VERSION_REF}"\n'
	} >"${TMP}/bs.sh"
	if [ "${_runas}" = file ]; then
		# $0 is a readable file -> the re-exec must be skipped (local clone / edit).
		sh "${TMP}/bs.sh" 2>/dev/null
	else
		# Simulate the documented `curl … | sh` pipe: the script arrives on stdin, so $0
		# is "sh" with no file at that path — the case the re-exec bootstrap targets. cd
		# to TMP so no stray "sh" file in the caller's cwd can spoof the $0-is-a-file test.
		( cd "${TMP}" && sh ) < "${TMP}/bs.sh" 2>/dev/null
	fi
}

# The plain one-liner while the release API is unreachable. VERSION_REF must be empty so
# resolution still hunts for a release and fails loudly — NOT the channel. This is the
# regression: before the channel existed this path died with "could not resolve the
# newest release"; keying resolution on SCRIPT_REF turned it into a silent unreleased
# install. Unauthenticated api.github.com is 60 req/hr per IP, so it is routine from
# behind NAT, not a corner case.
check "API unreachable, no --ref: script=main, nothing to install" \
	"script=main version=" "$(with_bootstrap "" 000 000 000 "")"

# --ref=main, the documented spelling.
check "--ref=main: script=main, install main" \
	"script=main version=main" "$(with_bootstrap main 000 000 000 v0.7.0-alpha.7)"

# --ref=<CHANNEL_TAG>, which is the title shown on the Releases page, so someone will
# type it after seeing it there. It must mean the same thing as --ref=main rather than
# bootstrapping that tag's frozen script and then installing newest-release binaries.
check "--ref=CHANNEL_TAG behaves as --ref=main" \
	"script=main version=main" "$(with_bootstrap "${CHANNEL_TAG}" 000 000 000 v0.7.0-alpha.7)"

# A pinned release older than every layout this script has had: all three paths
# 404, so fall back to main's SCRIPT, but keep the pin for the BINARIES. Losing it here
# would break "--ref=X installs X" on the one path where the user was most explicit
# about X.
check "--ref=v0.5.0 with a 404 script: script=main, install v0.5.0" \
	"script=main version=v0.5.0" "$(with_bootstrap v0.5.0 404 404 404 v0.7.0-alpha.7)"

# A ref from AFTER the move under scripts/: that path exists, so neither legacy path
# is consulted and re-exec fires straight off the first fetch. Asserted on the child's
# OWN output (REEXECED), same convention as the 200 case below: had the code
# wrongly gone to the legacy path first, that 404 would fall through to main
# instead ("script=main version=v9.9.9"), so this still catches the regression.
check "--ref=v9.9.9 with scripts/install.sh present: re-execs without consulting a legacy path" \
	"REEXECED" "$(with_bootstrap v9.9.9 200 404 404 v0.7.0-alpha.7)"

# A ref from BEFORE the move but after the flatten: scripts/install.sh 404s, the
# root /install.sh answers, and re-exec fires off that second fetch. Without
# the fallback this would 404 straight through to main ("script=main
# version=v0.8.0") instead of reaching REEXECED. (This one happens to reach the same
# REEXECED even against the single-fetch predecessor of this code, which always dialled
# the legacy URL and would have hit its 200 directly — the case below is what actually
# tells the two apart.)
check "--ref=v0.8.0 falls back to the root path and re-execs" \
	"REEXECED" "$(with_bootstrap v0.8.0 404 200 404 v0.7.0-alpha.7)"

# NEW: a ref from before the flatten, reachable only on the THIRD hop. Without this
# scenario the authbridge/ arm is never exercised by any test, so deleting it would
# leave the suite green while silently dropping support for every pre-#1134 tag.
check "--ref=v0.6.0 falls back past root to the pre-flatten path and re-execs" \
	"REEXECED" "$(with_bootstrap v0.6.0 404 404 200 v0.7.0-alpha.7)"

# The fallback triggers on a clean 404 ONLY. A transport error on the new path (down,
# rate-limited, proxied) must die rather than quietly trying the legacy path — even
# though that legacy fetch would have succeeded here. Getting this wrong (falling back
# on ANY non-200) would report REEXECED instead of DIED, and is also what distinguishes
# the two-path code from the single-fetch predecessor for this exact input: the old code
# never saw "000" at all, since it only ever dialled the legacy URL.
check "--ref=v0.8.0 with a transport failure on the new path never falls back to a working legacy path" \
	"DIED" "$(with_bootstrap v0.8.0 000 200 200 v0.7.0-alpha.7)"

# NEW: the same rule on the MIDDLE hop. A clean 404 on scripts/ legitimately advances
# to root, but a transport error THERE must stop rather than reach the pre-flatten
# path — otherwise "fall back on a clean 404 only" holds on hop 1 and not on hop 2.
check "--ref=v0.8.0 with a transport failure on the root path never reaches the pre-flatten path" \
	"DIED" "$(with_bootstrap v0.8.0 404 000 200 v0.7.0-alpha.7)"

# A transport failure is NOT a 404. We cannot tell whether a released installer exists,
# so running main instead would break the exact guarantee the bootstrap provides — the
# script has to refuse. This is the security-relevant arm and it was unreachable through
# the harness until the curl stub started writing the file the real one writes.
check "--ref=v0.5.0 with a transport failure refuses to run main" \
	"DIED" "$(with_bootstrap v0.5.0 000 000 000 v0.7.0-alpha.7)"

# HTTP 200 re-execs the released copy and exits with its status rather than continuing in
# this process. Asserted on the child's OWN output, not on the absence of the probe's:
# an empty result would also be produced by the probe dying early, which is exactly the
# kind of vacuous pass that hid the unreachable arm above.
check "--ref=v0.5.0 with a 200 script re-execs into it" \
	"REEXECED" "$(with_bootstrap v0.5.0 200 200 200 v0.7.0-alpha.7)"

# --- the bootstrap outlives a signal its child catches ---
#
# A ^C at setup's prompt reaches the whole process group: the re-exec'd script, which
# catches it and exits 3, and the bootstrap waiting for it. dash dies of an INT it does
# not catch even then, so the user's shell got 130 and the script was left in TMPDIR;
# bash waits to see what its child did, which is why macOS never showed it. A TERM to
# the group, or a closed terminal's HUP, kills bash too.
#
# The fake child is the re-exec'd script. It sends the signal to the bootstrap and to
# itself, as the terminal does, and exits 3 from its trap a moment later, as setup
# finishes its own output first. A suite started with the signal ignored (in the
# background, or under nohup) passes that on, and no shell can undo it, so that is
# probed first and fails as itself, as the ^C mid-download check below does.
with_bootstrap_signal() { # INT|TERM|HUP
	if [ -n "$(sh -c "kill -$1 \$\$; echo ignored" 2>/dev/null || :)" ]; then
		printf 'cannot run: this suite was started with SIG%s ignored\n' "$1"
		return 0
	fi
	# One file per signal: a child the bootstrap did not wait for may still be running.
	# Should its trap never run, it gives up after 5s, with 4.
	# shellcheck disable=SC2016 # literal on purpose: expanded by the child, not here.
	{
		printf "trap 'sleep 0.3; exit 3' %s\n" "$1"
		printf 'kill -%s "${PPID}" "$$"\n' "$1"
		printf '_i=0\nwhile [ "${_i}" -lt 50 ]; do sleep 0.1; _i=$((_i + 1)); done\nexit 4\n'
	} >"${TMP}/sig-child-$1.sh"
	rm -rf "${TMP}/sig-tmp"
	mkdir -p "${TMP}/sig-tmp"
	_bs_st=0
	# A subshell, so BS_CHILD and the TMPDIR the stub's mktemp names reach only this run.
	(
		BS_CHILD="${TMP}/sig-child-$1.sh"
		TMPDIR="${TMP}/sig-tmp"
		export TMPDIR
		with_bootstrap v9.9.9 200 404 404 v0.7.0-alpha.7 >/dev/null
	) || _bs_st=$?
	printf 'exit %s, %s left in TMPDIR\n' "${_bs_st}" "$(ls -A "${TMP}/sig-tmp" | wc -l | tr -d ' ')"
}
check "a ^C the re-exec'd script catches: the bootstrap exits with its status, and removes the script" \
	"exit 3, 0 left in TMPDIR" "$(with_bootstrap_signal INT)"
check "  and a TERM to them both" "exit 3, 0 left in TMPDIR" "$(with_bootstrap_signal TERM)"
check "  and a closed terminal's HUP" "exit 3, 0 left in TMPDIR" "$(with_bootstrap_signal HUP)"

# --- run as a LOCAL FILE: never re-exec the released copy (the reported bug) ---
#
# Running ./install.sh from a clone (or an edit under test) must run THAT file, not
# silently re-fetch the newest release and run it — which is what produced "Using the
# installer from vX", ran the OLD released code, and died. $0 is a readable file here,
# so the re-exec is skipped: no REEXECED, no DIED, SCRIPT_REF stays empty. Binaries
# still follow --ref (or resolve newest when unset), so `--ref=X` pins X's binaries even
# though this local script — not X's — is what runs.
check "local file, --ref=v0.5.0: run THIS script, pin v0.5.0 binaries, no re-exec" \
	"script= version=v0.5.0" "$(with_bootstrap v0.5.0 200 200 200 v0.7.0-alpha.7 file)"
check "local file, no --ref: run THIS script, resolve binaries later, no re-exec" \
	"script= version=" "$(with_bootstrap "" 200 200 200 v0.7.0-alpha.7 file)"

# --- the one-liner survives an exhausted API quota ---
#
# This is the reason the feed source exists. 60 requests/hour per IP, unauthenticated,
# shared behind NAT, two spent per install: exhausting it used to kill the documented
# one-liner and tell the person to look up a version and pass --ref, which is the
# opposite of a one-line install. The API failing must be invisible, not fatal.

fixture feed.xml <<'EOF'
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Release notes from cortex</title>
  <entry>
    <title>v0.7.0-alpha.8</title>
    <content type="html">&lt;p&gt;Prebuilt binaries&lt;/p&gt;</content>
  </entry>
  <entry>
    <title>v0.7.0-alpha.7</title>
  </entry>
</feed>
EOF
FEED="${FIXTURE}"

fixture ratelimit403.json <<'EOF'
{"message":"API rate limit exceeded for 203.0.113.7.","documentation_url":"https://x"}
EOF
check "API 403 falls back to the feed" "v0.7.0-alpha.8" \
	"$(with_newest_release "${FIXTURE}" "${FEED}" 403)"

# The feed's own <title> is the repo's, not a release, and must not be mistaken for one.
check "the feed's own title is not mistaken for a release" "v0.7.0-alpha.8" \
	"$(with_newest_release "${FIXTURE}" "${FEED}" 403)"

# A channel release appears in the feed too, and must be skipped there exactly as it is
# in the API response — otherwise the fallback would reintroduce the bug the v-tag filter
# exists to prevent.
fixture feed_channel.xml <<'EOF'
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Release notes from cortex</title>
  <entry><title>main-latest</title></entry>
  <entry><title>v0.7.0-alpha.8</title></entry>
</feed>
EOF
check "the feed skips the channel release" "v0.7.0-alpha.8" \
	"$(with_newest_release "${TMP}/ratelimit403.json" "${FIXTURE}" 403)"

# Release notes are ours to author and live in the same document, so a version-shaped
# line inside them must not win. The parse is anchored to the <title> element for this
# reason; notes arrive HTML-escaped and cannot forge one.
fixture feed_poisoned.xml <<'EOF'
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Release notes from cortex</title>
  <entry>
    <title>v0.7.0-alpha.8</title>
    <content type="html">v9.9.9-evil is not a release
&lt;p&gt;notes&lt;/p&gt;</content>
  </entry>
</feed>
EOF
check "notes text cannot pose as a release title" "v0.7.0-alpha.8" \
	"$(with_newest_release "${TMP}/ratelimit403.json" "${FIXTURE}" 403)"

# Both sources down is the only remaining failure, and it must stay a failure rather
# than guess.
fixture empty_feed.xml </dev/null
set +e
_st=0; _out=$(with_newest_release "${TMP}/ratelimit403.json" "${FIXTURE}" 403) || _st=$?
set -e
check_fails "both sources failing still fails" "${_st}"
check "and prints nothing" "" "${_out}"

# A healthy API must still be used — the fallback is a fallback, not a replacement.
fixture api_ok.json <<'EOF'
[{"tag_name":"v0.7.0-alpha.8"},{"tag_name":"v0.7.0-alpha.7"}]
EOF
check "a healthy API is used without touching the feed" "v0.7.0-alpha.8" \
	"$(with_newest_release "${FIXTURE}" /dev/null 200)"

# --- the channel tag is one string, in two files ---
#
# install.sh owns CHANNEL_TAG and every test above reads it out of the script rather than
# restating it, so a rename cannot leave a stale expectation passing. That discipline
# stopped at the workflow boundary: CI had its own bare literal with a comment asking a
# human to keep them equal. Rename CHANNEL_TAG and every test here would still pass while
# the channel broke in the only way users see — the installer requesting main-next_*
# assets from a release publishing main-latest_*, i.e. a 404 on download.
#
# The workflow is also where the one destructive operation lives: the tag move is guarded
# by that same literal, so a drift makes a v* release movable.
WORKFLOW="${REPO_ROOT}/.github/workflows/release-binaries.yaml"
if [ -f "${WORKFLOW}" ]; then
	check "CI publishes the tag install.sh asks for" "1" \
		"$(grep -c "TAG=\"${CHANNEL_TAG}\"" "${WORKFLOW}" || true)"
	# Every `[ "${TAG}" = ... ]` in the workflow must name CHANNEL_TAG. Counting matches
	# would be brittle — there are legitimately two today, the release-notes switch and
	# the tag-move guard — so assert the absence of any comparison against a DIFFERENT
	# literal instead. That is the invariant: no branch keyed on a stale channel name.
	check "no TAG comparison names a different tag" "0" \
		"$(grep -oE '\[ "\$\{TAG\}" = "[^"]*" \]' "${WORKFLOW}" | grep -cv "\"${CHANNEL_TAG}\"" || true)"
	check "at least one TAG comparison names CHANNEL_TAG" "1" \
		"$(grep -oE '\[ "\$\{TAG\}" = "[^"]*" \]' "${WORKFLOW}" | grep -c "\"${CHANNEL_TAG}\"" | awk '$1>0{print 1; exit} {print 0}')"
else
	check "release-binaries.yaml is where expected" "found" "missing at ${WORKFLOW}"
fi

# --- port_in_use: the ss branch counts only loopback / wildcard binds ---
#
# ss reports the Local Address:Port in column 4. A listener on an EXTERNAL interface
# does not make the loopback port unavailable, so port_in_use must not count it —
# matching it would make start_unsupervised's health poll return early and stop_cortex
# warn about a proxy that is not there. IPv6 loopback ([::1]) is the case a naive IPv4
# pattern misses: the proxy can bind ::1 only, and reading that as "free" makes the
# health poll spin its full 10s. `command` is stubbed so lsof is "absent" and ss is
# "present", forcing the ss path; ss is stubbed to emit the fixture (the real ss
# filters by sport, but the awk address-column check is the code under test).
with_port_in_use_ss() { # port  ss-listing-fixture
	{
		printf 'command() { case "$2" in ss) return 0 ;; *) return 1 ;; esac; }\n'
		printf 'ss() { cat "%s"; }\n' "$2"
		sed -n '/^port_in_use()/,/^}/p' "${INSTALL_SH}"
		printf 'if port_in_use "%s"; then echo busy; else echo free; fi\n' "$1"
	} >"${TMP}/pin.sh"
	sh "${TMP}/pin.sh" 2>/dev/null
}

fixture ss_v4loop.txt <<'EOF'
LISTEN 0 4096 127.0.0.1:47600 0.0.0.0:*
EOF
check "ss: IPv4 loopback bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_v6loop.txt <<'EOF'
LISTEN 0 4096 [::1]:47600 [::]:*
EOF
check "ss: IPv6 loopback [::1] bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_wild4.txt <<'EOF'
LISTEN 0 4096 0.0.0.0:47600 0.0.0.0:*
EOF
check "ss: IPv4 wildcard bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_wild6.txt <<'EOF'
LISTEN 0 4096 [::]:47600 [::]:*
EOF
check "ss: IPv6 wildcard bind reads busy" "busy" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_external.txt <<'EOF'
LISTEN 0 4096 192.168.1.5:47600 0.0.0.0:*
EOF
check "ss: external-only bind reads free (loopback port still available)" "free" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

fixture ss_none.txt </dev/null
check "ss: nothing listening reads free" "free" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

# The awk anchors the port to end-of-field, so a loopback bind on a DIFFERENT port
# (or a port that is a substring, e.g. :47600 inside :476000) must not count.
fixture ss_otherport.txt <<'EOF'
LISTEN 0 4096 127.0.0.1:9999 0.0.0.0:*
EOF
check "ss: a loopback bind on another port reads free" "free" "$(with_port_in_use_ss 47600 "${FIXTURE}")"

# --- proxy_running: five branches, and it gates a kill ---
#
# stop_cortex signals the pid this returns, so every branch matters: a non-numeric,
# empty, missing, or dead pid must read stopped (never SIGTERM a recycled pid), and
# where the sandbox blinds `ps` we keep the kill -0 result rather than refuse. kill and
# ps are mocked; the pidfile is a real file so the cat + `case` parsing is shipped code.
with_proxy_running() { # pidfile-content(__MISSING__ for none)  kill_exit  ps_mode(fail|empty|COMM)
	_pf="${TMP}/pr_pidfile"
	if [ "$1" = "__MISSING__" ]; then rm -f "${_pf}"; else printf '%s\n' "$1" >"${_pf}"; fi
	{
		printf 'PROXY_PIDFILE=%s\n' "${_pf}"
		printf 'kill() { return %s; }\n' "$2"
		case "$3" in
			fail)  printf 'ps() { return 1; }\n' ;;
			empty) printf 'ps() { return 0; }\n' ;;
			*)     printf 'ps() { printf "%%s\\n" "%s"; }\n' "$3" ;;
		esac
		sed -n '/^proxy_running()/,/^}/p' "${INSTALL_SH}"
		printf 'if proxy_running; then echo running; else echo stopped; fi\n'
	} >"${TMP}/prun.sh"
	sh "${TMP}/prun.sh" 2>/dev/null
}
check "proxy_running: missing pidfile -> stopped" "stopped" "$(with_proxy_running __MISSING__ 0 cortex)"
check "proxy_running: non-numeric pid -> stopped" "stopped" "$(with_proxy_running abc 0 cortex)"
check "proxy_running: empty pid -> stopped" "stopped" "$(with_proxy_running '' 0 cortex)"
check "proxy_running: dead pid (kill -0 fails) -> stopped" "stopped" "$(with_proxy_running 12345 1 cortex)"
check "proxy_running: alive, ps blind (sandbox) -> running" "running" "$(with_proxy_running 12345 0 fail)"
check "proxy_running: alive, ps empty comm -> running" "running" "$(with_proxy_running 12345 0 empty)"
check "proxy_running: alive, ps names cortex -> running" "running" "$(with_proxy_running 12345 0 cortex)"
check "proxy_running: alive, ps names another process -> stopped" "stopped" "$(with_proxy_running 12345 0 sshd)"
# The name is an exact basename, never a pattern: "cortex" is also in the repo's name
# and ~/.cortex, and macOS prints a path, so a pattern would claim strangers.
check "proxy_running: alive, macOS full path to cortex -> running" "running" \
	"$(with_proxy_running 12345 0 /Users/u/.local/bin/cortex)"
check "proxy_running: alive, a process under a cortex checkout -> stopped" "stopped" \
	"$(with_proxy_running 12345 0 /Users/u/src/cortex/bin/agentop)"
check "proxy_running: alive, a process under ~/.cortex -> stopped" "stopped" \
	"$(with_proxy_running 12345 0 /Users/u/.cortex/bin/helper)"
check "proxy_running: alive, cortex-envoy -> stopped" "stopped" "$(with_proxy_running 12345 0 cortex-envoy)"
check "proxy_running: alive, the pre-rename authbridge-proxy -> stopped (clean break)" "stopped" \
	"$(with_proxy_running 12345 0 /Users/u/.local/bin/authbridge-proxy)"

# --- a v0.7.0 install's background proxy, recorded in the pidfile ---
#
# v0.7.0 ran "${BIN_DIR}/authbridge-proxy --local --supervise" wherever no supervisor
# was usable and wrote its pid to the same pidfile. stop_cortex runs against the real
# pidfile; kill, ps and pid_exe_path are mocked. Pid 4242 is the recorded process and
# reads as dead once it has been sent SIGTERM. (Starting is agentop setup's now, so
# only --stop is left to test here.)
with_pre_rename() { # exe-of-4242(__NONE__)  bin_dir  ps-comm(fail)
	_pf="${TMP}/prr_pidfile"
	_log="${TMP}/prr_log"
	: >"${_log}"
	rm -f "${TMP}/prr_termed"
	printf '4242\n' >"${_pf}"
	{
		printf 'PROXY_PIDFILE=%s\nBIN_DIR=%s\nCORTEX_DIR=%s\nSUPERVISOR_NAME=x\n' "${_pf}" "$2" "${TMP}"
		printf 'DEMO_FORWARD_PORT=47600\nDEMO_SESSION_PORT=47601\nDEMO_STATS_PORT=47603\nDEMO_HEALTH_PORT=47604\n'
		printf 'info() { :; }\nwarn() { :; }\nsleep() { :; }\n'
		printf 'port_in_use() { return 1; }\n'
		printf 'kill() {\n'
		printf '\tcase "$1" in\n'
		printf '\t\t-0) [ "$2" = 4242 ] && [ -f "%s" ] && return 1; return 0 ;;\n' "${TMP}/prr_termed"
		printf '\t\t-9) printf "KILL %%s\\n" "$2" >>"%s" ;;\n' "${_log}"
		printf '\t\t*) printf "TERM %%s\\n" "$1" >>"%s"; [ "$1" = 4242 ] && : >"%s" ;;\n' "${_log}" "${TMP}/prr_termed"
		printf '\tesac\n\treturn 0\n}\n'
		if [ "$3" = fail ]; then printf 'ps() { return 1; }\n'
		else printf 'ps() { printf "%%s\\n" "%s"; }\n' "$3"; fi
		if [ "$1" = __NONE__ ]; then printf 'pid_exe_path() { return 1; }\n'
		else printf 'pid_exe_path() { [ "$1" = 4242 ] && printf "%%s\\n" "%s"; }\n' "$1"; fi
		for _fn in proxy_running pre_rename_proxy_running stop_pidfile_proxy stop_cortex; do
			sed -n "/^${_fn}()/,/^}/p" "${INSTALL_SH}"
		done
		printf 'stop_cortex\n'
	} >"${TMP}/prr.sh"
	sh "${TMP}/prr.sh" >/dev/null 2>&1
	if grep -qx 'TERM 4242' "${_log}"; then _old=stopped; else _old=left; fi
	_now=$(cat "${_pf}" 2>/dev/null || true)
	case "${_now}" in
		"") _now=none ;;
		4242) _now=old ;;
		*) _now=new ;;
	esac
	printf 'old:%s pidfile:%s\n' "${_old}" "${_now}"
}
check "pre-rename: --stop stops it rather than discarding the pidfile" \
	"old:stopped pidfile:none" \
	"$(with_pre_rename /home/u/.local/bin/authbridge-proxy /home/u/.local/bin authbridge-prox)"
check "pre-rename: ...but not an authbridge-proxy from another directory" \
	"old:left pidfile:none" \
	"$(with_pre_rename /home/u/src/cortex/authbridge-proxy /home/u/.local/bin authbridge-prox)"
# A live pid that nothing can name may be anyone's, recycled since: not ours to signal.
check "pre-rename: --stop does not signal a pid whose executable cannot be named" \
	"old:left pidfile:none" \
	"$(with_pre_rename __NONE__ /home/u/.local/bin authbridge-prox)"
# /proc/<pid>/exe resolves symlinks, so a BIN_DIR reached through one must still match.
mkdir -p "${TMP}/prr_real"
: >"${TMP}/prr_real/authbridge-proxy"
ln -s "${TMP}/prr_real" "${TMP}/prr_link"
check "pre-rename: --stop matches a BIN_DIR reached through a symlink" \
	"old:stopped pidfile:none" \
	"$(with_pre_rename "$(cd "${TMP}/prr_real" && pwd -P)/authbridge-proxy" "${TMP}/prr_link" authbridge-prox)"

# --- ensure_tmpdir: exports TMPDIR on BOTH paths (regression: unset-TMPDIR abort) ---
#
# With `set -u`, a later "${TMPDIR}" reference (the svc_err mktemp on the supervised
# path) aborts the script if ensure_tmpdir returned without exporting TMPDIR. macOS
# hides this because launchd sets TMPDIR per user; Linux routinely has it unset — so
# the writable-base path must export TMPDIR too, not just the fallback. mkdir/rmdir are
# mocked so the result does not depend on whether /tmp is writable where the test runs;
# TMPDIR is unset for the run so the base defaults to /tmp.
with_ensure_tmpdir() { # base_writable(ok|deny)  [preset-TMPDIR]
	if [ "$1" = ok ]; then _mk='mkdir() { return 0; }'
	else _mk='mkdir() { for _a in "$@"; do [ "$_a" = "-p" ] && return 0; done; return 1; }'
	fi
	{
		printf 'CORTEX_DIR=%s\n' "${TMP}/cortexhome"
		printf 'info() { :; }\ndie() { printf "DIED\\n"; exit 1; }\nrmdir() { :; }\n'
		printf '%s\n' "${_mk}"
		sed -n '/^ensure_tmpdir()/,/^}/p' "${INSTALL_SH}"
		printf 'ensure_tmpdir\n'
		printf 'printf "%%s\\n" "${TMPDIR-__UNSET__}"\n'
	} >"${TMP}/et.sh"
	if [ -n "${2:-}" ]; then TMPDIR="$2" sh "${TMP}/et.sh" 2>/dev/null
	else env -u TMPDIR sh "${TMP}/et.sh" 2>/dev/null; fi
}
check "ensure_tmpdir: unset TMPDIR, writable /tmp -> adopt & export /tmp (else set -u aborts)" "/tmp" "$(with_ensure_tmpdir ok)"
check "ensure_tmpdir: unwritable base falls back under CORTEX_DIR and exports it" "${TMP}/cortexhome/tmp" "$(with_ensure_tmpdir deny)"
# An already-set writable TMPDIR is used AS-IS — never overwritten with a normalised
# copy. The trailing-slash case is the tell: the old code round-tripped through _base
# and stripped it; TMPDIR must come back byte-for-byte.
check "ensure_tmpdir: an already-set writable TMPDIR is kept verbatim" "/custom/t" "$(with_ensure_tmpdir ok /custom/t)"
check "ensure_tmpdir: an already-set TMPDIR keeps its trailing slash (not clobbered)" "/custom/t/" "$(with_ensure_tmpdir ok /custom/t/)"
# A set-but-unwritable TMPDIR still falls back rather than failing.
check "ensure_tmpdir: set-but-unwritable TMPDIR falls back under CORTEX_DIR" "${TMP}/cortexhome/tmp" "$(with_ensure_tmpdir deny /nope)"

# --- pid_exe_path: the full path, because `comm` cannot carry one on Linux ---
#
# The bug this replaced: the path check used `ps -o comm=`. On Linux `comm` is the
# kernel's comm field — argv[0]'s basename capped at 15 chars (TASK_COMM_LEN-1) —
# so the binary's name of the time, 16 characters, printed as a 15-character stub
# and NEVER as a path.
# Compared for equality against an install path that made every busy-port upgrade
# race on Linux classify as foreign-proxy, and die telling the user to kill their
# own proxy. macOS hid it completely: there `comm` does print a path.
#
# Three sources in order: /proc/<pid>/exe (Linux kernel truth), lsof -d txt (the
# macOS equivalent), then `ps -o args=` as a last resort. A fake /proc tree stands
# in for the real one so the readlink branch is exercised on macOS too.
with_pid_exe_path() { # proc_exe_target(empty for no /proc)  lsof_txt  ps_args
	_root="${TMP}/pep"; rm -rf "${_root}"; mkdir -p "${_root}/proc/4242"
	if [ -n "$1" ]; then ln -s "$1" "${_root}/proc/4242/exe"; fi
	{
		printf 'PROCROOT=%s\n' "${_root}"
		if [ -n "$2" ]; then
			printf 'command() { return 0; }\n'
			# Modelled on real lsof rather than echoing a fixed answer: list-selection
			# options are ORed unless -a is given, so without -a `-p <pid> -d txt`
			# lists every process's executable and head -1 takes whatever came first.
			# The stub emits an unrelated record FIRST in that case, so dropping -a
			# fails the test instead of passing silently.
			printf 'lsof() {\n'
			printf '\t_a=no; for _w in "$@"; do [ "${_w}" = "-a" ] && _a=yes; done\n'
			printf '\t[ "${_a}" = no ] && printf "n/usr/bin/some-other-process\\n"\n'
			printf '\tprintf "n%%s\\n" "%s"\n' "$2"
			printf '}\n'
		else
			printf 'command() { case "$2" in lsof) return 1 ;; *) return 0 ;; esac; }\n'
		fi
		if [ -n "$3" ]; then printf 'ps() { printf "%%s\\n" "%s"; }\n' "$3"
		else printf 'ps() { return 1; }\n'; fi
		# Rewrite the two /proc references onto the fixture tree. The logic under
		# test — the readlink, the (deleted) trim, the source ordering — is shipped
		# code; only the root moves.
		sed -n '/^pid_exe_path()/,/^}/p' "${INSTALL_SH}" \
			| sed 's#"/proc/$1/exe"#"${PROCROOT}/proc/$1/exe"#g'
		printf 'pid_exe_path 4242 || echo __NONE__\n'
	} >"${TMP}/pep.sh"
	sh "${TMP}/pep.sh" 2>/dev/null
}
check "pid_exe_path: /proc/<pid>/exe is preferred (Linux truth)" \
	"/home/u/.local/bin/cortex" \
	"$(with_pid_exe_path /home/u/.local/bin/cortex /lsof/path /ps/path)"
# A binary replaced under a running process reads "<path> (deleted)" — an upgrade
# in progress is exactly when this code runs, so the suffix must be trimmed off
# rather than travelling into a path comparison that would then call it foreign.
check "pid_exe_path: a deleted/replaced binary keeps its path, drops ' (deleted)'" \
	"/home/u/.local/bin/cortex" \
	"$(with_pid_exe_path '/home/u/.local/bin/cortex (deleted)' '' '')"
check "pid_exe_path: no /proc -> lsof txt descriptor (the macOS path)" \
	"/Users/u/.local/bin/cortex" \
	"$(with_pid_exe_path '' /Users/u/.local/bin/cortex /ps/path)"
# Same case, stated as the bug it guards: lsof ORs -p and -d unless -a is passed,
# so without it the query means "this pid OR any txt descriptor on the system" and
# head -1 can take another process's executable. On macOS this is the source
# foreign_proxy_holder judges, so the wrong path there classifies our OWN managed
# proxy as foreign and dies. The stub above emits a foreign record first when -a is
# missing, so this check is what fails if the flag is ever dropped.
check "pid_exe_path: the lsof query is ANDed with -a (not 'this pid OR any txt fd')" \
	"/Users/u/.local/bin/cortex" \
	"$(with_pid_exe_path '' /Users/u/.local/bin/cortex '')"
# Belt and braces: pin the flag in the source too, so a refactor that rewrites the
# invocation cannot quietly lose the conjunction while still passing the stub test.
check "the lsof executable query passes -a" "1" \
	"$(grep -c 'lsof -p "\$1" -a -d txt' "${INSTALL_SH}")"
# argv[0] is the weakest source (caller-chosen, possibly relative) so it is last,
# but it beats reporting nothing.
check "pid_exe_path: no /proc, no lsof -> first field of ps args" \
	"/Users/u/.local/bin/cortex" \
	"$(with_pid_exe_path '' '' '/Users/u/.local/bin/cortex --local --supervise')"
# Nothing can name it: must FAIL, never print a placeholder. foreign_proxy_holder
# treats any non-match as foreign, so "unknown" as a value would accuse a process
# nobody can see — the thing the previous `${_ph_cmd:-unknown}` fallback did.
check "pid_exe_path: nothing can name the pid -> fails, prints no placeholder" \
	"__NONE__" "$(with_pid_exe_path '' '' '')"

# The regression guard proper: `ps -o comm=` may appear only in the places that want a
# NAME rather than a path — today only proxy_running, which reads the truncated comm
# deliberately and compares no path.
#
# Counting is the whole point, and the previous form of this check is why. It grepped
# for `comm=` and the binary name on ONE line and asserted zero — but the defect
# spanned two lines (the `comm=` capture in port_holder and the path comparison in
# foreign_proxy_holder), so that pattern returned 0 on the buggy commit too: the exact
# value it asserted as clean. It could not fail on the bug its own comment named, which
# is worse than no check, because the comment invited readers to trust it.
#
# Verified against the history: this count is 2 on d5f8abd3 (the commit that shipped the
# defect — port_holder's capture plus proxy_running's) and was 1 when that was fixed.
# It was 2 again for a while for an unrelated reason — pidfile_process_unnamed, for the
# pre-rename pidfile handling, read comm for the same legitimate "name, not path"
# purpose — and is 1 again now that the start path, and that helper with it, moved to
# agentop setup. That is why the assertion is a COUNT rather than a ceiling: a new
# occurrence has to be argued for and the number moved by hand, rather than appearing by
# accident. If this fails after you touched install.sh, check whether your new `comm=`
# feeds a path comparison before you edit the expected number.
#
# Comment lines are stripped first, and that is not incidental: pid_exe_path's header
# explains at length why `ps -o comm=` cannot carry a path, quoting it verbatim. Counting
# prose would make this fire on someone DOCUMENTING the hazard — the opposite of the
# intent — so only real invocations count.
check "ps -o comm= is invoked only where a name, not a path, is wanted" "1" \
	"$(grep -v '^[[:space:]]*#' "${INSTALL_SH}" | grep -c 'ps .*-o comm=\|ps -o comm=')"

# --- the download's progress bar: what it draws, without a terminal ---
#
# The bar is drawn only when stderr is a terminal, so what it draws is checked here one
# function at a time, as text: size_text (setup's units), progress_line (the arithmetic),
# bar_cells (how much bar fits), and bar_draw and bar_end (the escapes around the line).
# The whole-script runs below cover the rest, off a terminal and on a pseudo-terminal.
#
# with_bar CODE runs CODE with those functions defined, and shows ESC as E and CR as R.
with_bar() {
	{
		for _wb_f in size_text progress_line bar_cells bar_draw bar_end; do
			sed -n "/^${_wb_f}()/,/^}/p" "${INSTALL_SH}"
		done
		printf '%s\n' "$1"
	} >"${TMP}/bar.sh"
	sh "${TMP}/bar.sh" 2>&1 | tr '\033\r' 'ER'
}
check "size_text: bytes under 1000" "999 B" "$(with_bar 'size_text 999')"
check "size_text: kB from 1000" "1.0 kB" "$(with_bar 'size_text 1000')"
check "size_text: kB, to the nearest tenth (not truncated)" "1.1 kB" "$(with_bar 'size_text 1060')"
check "size_text: kB up to 999949" "999.9 kB" "$(with_bar 'size_text 999949')"
check "size_text: MB from 999950, so never 1000.0 kB" "1.0 MB" "$(with_bar 'size_text 999950')"
check "size_text: MB, to the nearest tenth (not truncated)" "27.4 MB" "$(with_bar 'size_text 27360000')"
check "progress_line: 0%" "[----------]   0%  0 B / 2.0 MB" "$(with_bar 'progress_line 0 2000000 10')"
check "progress_line: 50%" "[#####-----]  50%  1.0 MB / 2.0 MB" "$(with_bar 'progress_line 1000000 2000000 10')"
check "progress_line: 100%" "[##########] 100%  2.0 MB / 2.0 MB" "$(with_bar 'progress_line 2000000 2000000 10')"
check "progress_line: one byte short is 99%, not 100%" "[#########-]  99%  2.0 MB / 2.0 MB" \
	"$(with_bar 'progress_line 1999999 2000000 10')"
check "progress_line: total unknown shows the bytes so far, no percentage" "1.5 MB" \
	"$(with_bar 'progress_line 1500000 "" 10')"
check "progress_line: done past total caps at 100%" "[##########] 100%  3.0 MB / 2.0 MB" \
	"$(with_bar 'progress_line 3000000 2000000 10')"
check "progress_line: total 0 is unknown, not a division by zero" "1.5 kB" \
	"$(with_bar 'progress_line 1500 0 10')"
check "progress_line: width 0 is the figures without a bar" " 50%  1.0 MB / 2.0 MB" \
	"$(with_bar 'progress_line 1000000 2000000 0')"
check "bar_cells: a wide terminal gets the 30-cell maximum" "30" "$(with_bar 'bar_cells 200 19')"
check "bar_cells: a narrower one gets what fits beside the prefix and figures" "12" \
	"$(with_bar 'bar_cells 60 19')"
check "bar_cells: under 10 cells is no bar, figures only" "0" "$(with_bar 'bar_cells 50 19')"
check "bar_cells: no room for the figures fails, so nothing wraps" "fails" \
	"$(with_bar 'bar_cells 40 19 || echo fails')"
check "bar_draw: hides the cursor once, redraws the line in place, bar_end erases it and shows the cursor" \
	"E[?25lRDownloading v1 [#####-----]  50%  1.0 MB / 2.0 MBE[KRDownloading v1 [##########] 100%  2.0 MB / 2.0 MBE[KRE[KE[?25h" \
	"$(with_bar 'bar_prefix="Downloading v1 " bar_total=2000000 bar_w=10 bar_shown=""
bar_draw 1000000; bar_draw 2000000; bar_end; bar_end')"
check "bar_end: with no bar drawn, prints nothing" "" "$(with_bar 'bar_shown=""; bar_end')"

# content_length URL asks with `curl -sIL` and reads the redirect chain's last response.
# The fixtures are what a HEAD through a proxy really returns from GitHub: the proxy's
# CONNECT reply, the 302 to the asset host with a length of 0, then the asset.
with_content_length() { # header-fixture [curl-exit]
	{
		printf 'curl() { printf "%%s\\n" "$*" >"%s"; cat "%s"; return %s; }\n' \
			"${TMP}/cl.args" "$1" "${2:-0}"
		sed -n '/^content_length()/,/^}/p' "${INSTALL_SH}"
		printf 'content_length https://example.invalid/a.tar.gz\n'
	} >"${TMP}/cl.sh"
	sh "${TMP}/cl.sh" 2>/dev/null
}
printf 'HTTP/1.1 200 Connection Established\r\n\r\nHTTP/2 302 \r\nlocation: https://release-assets.example/a\r\ncontent-length: 0\r\n\r\nHTTP/1.1 200 Connection Established\r\n\r\nHTTP/2 200 \r\ncontent-length: 3774359\r\n\r\n' \
	>"${TMP}/cl-ok"
check "content_length: the last response's length, past the redirect" "3774359" \
	"$(with_content_length "${TMP}/cl-ok")"
check "  asked with curl -sIL" "1" "$(tr ' ' '\n' <"${TMP}/cl.args" | grep -cx -- '-sIL' || true)"
printf 'HTTP/2 302 \r\ncontent-length: 0\r\n\r\nHTTP/2 200 \r\ntransfer-encoding: chunked\r\n\r\n' >"${TMP}/cl-none"
check "content_length: a last response with no length is unknown, not the 302's 0" "" \
	"$(with_content_length "${TMP}/cl-none")"
printf 'HTTP/2 302 \r\ncontent-length: 0\r\n\r\nHTTP/2 404 \r\ncontent-length: 9\r\n\r\n' >"${TMP}/cl-404"
check "content_length: a 404's length is not the archive's" "" "$(with_content_length "${TMP}/cl-404")"
: >"${TMP}/cl-empty"
check "content_length: a curl that fails is unknown" "" "$(with_content_length "${TMP}/cl-empty" 6)"

# --- the thin installer: stage the release, probe it, hand off to agentop setup ---
#
# install.sh's own job ends at a staging dir: download, verify, extract, then exec
# `agentop setup --from <stage>`. These run the whole script, `sh install.sh`, in a
# sandbox with every way out of it closed:
#   - HOME and TMPDIR are fresh dirs under ${TMP}, so BIN_DIR and CORTEX_DIR are too;
#   - curl is a stub on PATH that serves a fake release from a directory;
#   - the only agentop that can run is the fake below, which records how it ran.
# Nothing reaches the network, the real ~/.local/bin, or launchd/systemd. The release
# is real tarballs and a real checksums.txt, so the checksum filter, sha_check and tar
# are the shipped code rather than stubs.
#
# The platform as install.sh names it, so the stub serves the archives it asks for.
case "$(uname -s)" in Darwin) T_OS=darwin ;; *) T_OS=linux ;; esac
case "$(uname -m)" in x86_64 | amd64) T_ARCH=amd64 ;; *) T_ARCH=arm64 ;; esac

# The fake agentop. T_PROBE decides how it answers `setup --help`, as each kind of
# agentop would:
#   ok          it has setup: exit 0
#   old         a release from before setup: unknown subcommand, exit 2
#   noexec      cannot be run (exit 126) unless it runs from under ~/.cortex/tmp, as
#               from a TMPDIR mounted noexec
#   noexec-all  cannot be run anywhere
# Any other call is the handoff. It records its argv, what was staged beside it, and
# what its stdin holds, to T_LOG.
cat >"${TMP}/fake-agentop" <<'EOF'
#!/bin/sh
if [ "${1:-}" = --version ]; then echo "agentop v9.9.9"; exit 0; fi
if [ "${1:-}" = setup ] && [ "${2:-}" = --help ]; then
	printf 'probe %s\n' "$0" >>"${T_LOG}"
	case "${T_PROBE}" in
		ok) exit 0 ;;
		old)
			printf 'agentop: unknown subcommand "setup" (known: service, configure)\n' >&2
			exit 2
			;;
		noexec)
			case "$0" in "${HOME}"/.cortex/tmp/*) exit 0 ;; esac
			exit 126
			;;
		*) exit 126 ;;
	esac
fi
[ ! -t 1 ] || printf 'setup:start\n' # on a terminal, where setup's output begins
{ printf 'argv'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >>"${T_LOG}"
printf 'staged %s\n' "$(ls "${0%/*}" | tr '\n' ' ')" >>"${T_LOG}"
# Read only when it cannot block: a terminal is a fine stdin, the piped script is not.
if [ -t 0 ]; then echo 'stdin tty' >>"${T_LOG}"; else printf 'stdin [%s]\n' "$(cat)" >>"${T_LOG}"; fi
EOF
printf '#!/bin/sh\necho "cortex v9.9.9"\n' >"${TMP}/fake-cortex"
chmod +x "${TMP}/fake-agentop" "${TMP}/fake-cortex"

# curl serves ${T_REL}/<the URL's last segment>, and fails as `curl -f` does on a 404
# when there is no such file. Every URL it is asked for goes to T_URLS, a HEAD (-I) as
# "HEAD <url>", answered with a GitHub-shaped redirect chain and the file's size.
#
# T_CURL changes how an archive (*.tar.gz) download behaves, recording in T_MEET:
#   meet  mark that it started, then wait up to 3s for the other archive's mark, and
#         log how many it saw. Downloading at once, each sees 2; one after the other,
#         the first sees only itself.
#   slow  take 1s first, so the download outlasts a few polls
#   hold  mark that it started, then hold until killed, and log the kill. It takes 0.3s
#         to stop, so a script that does not wait for it exits before the log says so
# Its own waits use the real sleep, by path, so a stub sleep on PATH never sees them.
T_REAL_SLEEP=$(command -v sleep)
mkdir -p "${TMP}/tbin"
cat >"${TMP}/tbin/curl" <<'EOF'
#!/bin/sh
_u=""; _o=""; _p=""; _head=""
for _a in "$@"; do
	case "${_a}" in http*) _u="${_a}" ;; -*I*) _head=1 ;; esac
	[ "${_p}" != -o ] || _o="${_a}"
	_p="${_a}"
done
_n="${_u##*/}"
_f="${T_REL}/${_n}"
if [ -n "${_head}" ]; then
	printf 'HEAD %s\n' "${_u}" >>"${T_URLS}"
	if [ -f "${_f}" ]; then
		printf 'HTTP/2 302 \r\ncontent-length: 0\r\n\r\nHTTP/2 200 \r\ncontent-length: %s\r\n\r\n' \
			"$(wc -c <"${_f}" | tr -d ' ')"
	else
		printf 'HTTP/2 404 \r\ncontent-length: 9\r\n\r\n'
	fi
	exit 0
fi
printf '%s\n' "${_u}" >>"${T_URLS}"
t_marks() { set -- "${T_MEET}"/*.tar.gz; if [ -e "$1" ]; then echo "$#"; else echo 0; fi; }
case "${T_CURL:-}:${_n}" in
	meet:*.tar.gz)
		: >"${T_MEET}/${_n}"
		_i=0
		while [ "$(t_marks)" -lt 2 ] && [ "${_i}" -lt 30 ]; do
			"${T_REAL_SLEEP}" 0.1
			_i=$((_i + 1))
		done
		printf 'met %s %s\n' "${_n}" "$(t_marks)" >>"${T_MEET}/log"
		;;
	slow:*.tar.gz) "${T_REAL_SLEEP}" 1 ;;
	hold:*.tar.gz)
		"${T_REAL_SLEEP}" 30 &
		_sp=$!
		trap 'kill "${_sp}" 2>/dev/null; "${T_REAL_SLEEP}" 0.3; printf "killed %s\n" "${_n}" >>"${T_MEET}/log"; exit 143' TERM
		: >"${T_MEET}/${_n}"
		wait "${_sp}"
		printf 'finished %s\n' "${_n}" >>"${T_MEET}/log"
		;;
esac
[ -f "${_f}" ] || exit 22
if [ -n "${_o}" ]; then cat "${_f}" >"${_o}"; else cat "${_f}"; fi
EOF
chmod +x "${TMP}/tbin/curl"
# lsof, which only --stop runs, to check the ports: nothing is listening. A real Cortex
# may well hold this machine's ports, and they must not decide a run.
printf '#!/bin/sh\nexit 1\n' >"${TMP}/tbin/lsof"
chmod +x "${TMP}/tbin/lsof"
# sleep, for the runs that put it on PATH: it logs each call to T_SLEEPS, then sleeps
# 0.2s whatever it was asked, so a run stays short. With T_SLEEP_NOFRAC=1 it rejects a
# fraction, as a sleep that takes whole seconds only does.
mkdir -p "${TMP}/sleepbin"
cat >"${TMP}/sleepbin/sleep" <<'EOF'
#!/bin/sh
printf '%s\n' "$1" >>"${T_SLEEPS}"
case "${T_SLEEP_NOFRAC:-}:$1" in
	1:*[!0-9]*)
		printf 'sleep: invalid time interval: %s\n' "$1" >&2
		exit 1
		;;
esac
exec "${T_REAL_SLEEP}" 0.2
EOF
chmod +x "${TMP}/sleepbin/sleep"
# What install.sh's stdin holds when piped: the rest of itself. setup must not get it.
printf 'PIPED-SCRIPT-REMAINDER\n' >"${TMP}/piped-script"

t_sha256() { # file
	if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
	else sha256sum "$1" | cut -d' ' -f1; fi
}

# make_release VERSION [SUMS] builds the release run_install serves, in T_REL, and sets
# T_BYTES to what --handoff-bytes should say. SUMS shapes checksums.txt:
#   two    one line per archive, as release-binaries.yaml writes them (the default)
#   one    the cortex line missing
#   three  agentop listed twice, as ./name and as a bare name
#   bad    agentop's line carries the wrong hash
make_release() {
	T_REL="${TMP}/rel-$1"
	rm -rf "${T_REL}"
	mkdir -p "${T_REL}/src"
	cp "${TMP}/fake-agentop" "${T_REL}/src/agentop"
	cp "${TMP}/fake-cortex" "${T_REL}/src/cortex"
	_mr_a="agentop_$1_${T_OS}_${T_ARCH}.tar.gz"
	_mr_c="cortex_$1_${T_OS}_${T_ARCH}.tar.gz"
	# COPYFILE_DISABLE: macOS tar would otherwise add AppleDouble ._ entries.
	COPYFILE_DISABLE=1 tar -C "${T_REL}/src" -czf "${T_REL}/${_mr_a}" agentop
	COPYFILE_DISABLE=1 tar -C "${T_REL}/src" -czf "${T_REL}/${_mr_c}" cortex
	_mr_ha=$(t_sha256 "${T_REL}/${_mr_a}")
	_mr_hc=$(t_sha256 "${T_REL}/${_mr_c}")
	# Every hex digit moved by one, so the bad hash differs in every position.
	[ "${2:-two}" != bad ] || _mr_ha=$(printf '%s' "${_mr_ha}" | tr '0-9a-f' '1-9a-f0')
	{
		printf '%s  ./%s\n' "${_mr_ha}" "${_mr_a}"
		[ "${2:-two}" = one ] || printf '%s  ./%s\n' "${_mr_hc}" "${_mr_c}"
		[ "${2:-two}" != three ] || printf '%s  %s\n' "${_mr_ha}" "${_mr_a}"
	} >"${T_REL}/checksums.txt"
	printf '#!/usr/bin/env python3\nprint("dump")\n' >"${T_REL}/cortex-session-dump.py"
	T_BYTES=$(($(wc -c <"${T_REL}/${_mr_a}") + $(wc -c <"${T_REL}/${_mr_c}")))
}

# run_install PROBE WHERE ARGS... runs install.sh ARGS in a fresh sandbox. WHERE:
#   release  download T_REL's release (pass --ref=<its version>)
#   repair   AUTHBRIDGE_SKIP_DOWNLOAD=1, with the fakes installed in ~/.local/bin
#   bare     AUTHBRIDGE_SKIP_DOWNLOAD=1, with nothing installed
# It sets T_RUN (the sandbox) and T_ST (the exit status), and leaves in T_RUN: log
# (the fake agentop's record), urls (curl's), meet (curl's marks, for T_CURL), sleeps
# (the stub sleep's calls), and out and err (install.sh's).
#
# Five globals change a run, and each is reset after it:
#   T_CURL   the stub curl's mode, above
#   T_SLEEP  put the stub sleep on PATH: frac (it takes fractions), or nofrac
#   T_BG     run install.sh in the background, and set T_PID instead of T_ST
#   T_PTY    run it on a pseudo-terminal, 100 columns wide, with TERM=xterm: yes; int,
#            to type a ^C once both archive downloads have marked their start and the
#            bar is on screen; hup, to close the terminal at that point instead; dumb,
#            with TERM=dumb; or narrow, 40 columns wide. out is then the terminal's bytes,
#            stdout and stderr together, and pty.pid install.sh's pid.
#   T_PIPE   run it as `curl … | sh -s --` does: the script on stdin, so $0 is "sh" and
#            the bootstrap may re-exec, from the sandbox, so no file named sh passes for $0
T_N=0
T_CURL=""
T_SLEEP=""
T_BG=""
T_PTY=""
T_PIPE=""
# t_pty SCRIPT runs `sh SCRIPT` on a pseudo-terminal, with util-linux's script (CI's)
# or the BSD one (macOS's). With neither, the run fails, and so do its checks.
# t_pty_bg is the same in the background, with script's own pid in T_SPID.
if script --version 2>/dev/null | grep -q util-linux; then
	t_pty() { script -qec "sh '$1'" /dev/null; }
	t_pty_bg() { script -qec "sh '$1'" /dev/null & T_SPID=$!; }
else
	t_pty() { script -q /dev/null sh "$1"; }
	t_pty_bg() { script -q /dev/null sh "$1" & T_SPID=$!; }
fi
t_q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; } # quoted for sh
run_install() {
	_ri_probe=$1
	_ri_where=$2
	shift 2
	T_N=$((T_N + 1))
	T_RUN="${TMP}/run${T_N}"
	# The sandbox HOME keeps every path install.sh can exec under ${TMP}: BIN_DIR and
	# CORTEX_DIR both hang off it, and the stage off TMPDIR. Refuse to run otherwise.
	case "${T_RUN}/home" in
		"${HOME}") printf 'refusing: %s/home is the real HOME\n' "${T_RUN}" >&2; exit 1 ;;
		"${TMP}"/run*/home) ;;
		*) printf 'refusing to run install.sh outside %s\n' "${TMP}" >&2; exit 1 ;;
	esac
	mkdir -p "${T_RUN}/home" "${T_RUN}/tmp" "${T_RUN}/meet"
	: >"${T_RUN}/log"
	: >"${T_RUN}/urls"
	: >"${T_RUN}/sleeps"
	_ri_path="${TMP}/tbin:${PATH}"
	[ -z "${T_SLEEP}" ] || _ri_path="${TMP}/sleepbin:${_ri_path}"
	_ri_skip=""
	case "${_ri_where}" in
		repair)
			mkdir -p "${T_RUN}/home/.local/bin"
			cp "${TMP}/fake-agentop" "${T_RUN}/home/.local/bin/agentop"
			cp "${TMP}/fake-cortex" "${T_RUN}/home/.local/bin/cortex"
			_ri_skip=1
			;;
		bare) _ri_skip=1 ;;
	esac
	if [ -n "${T_PIPE}" ]; then set -- -s -- "$@"; else set -- "${INSTALL_SH}" "$@"; fi
	set -- env -u AUTHBRIDGE_SCRIPT_REF -u AUTHBRIDGE_INSTALL_ONLY -u AUTHBRIDGE_VERSION -u AUTHBRIDGE_REF \
		HOME="${T_RUN}/home" TMPDIR="${T_RUN}/tmp" PATH="${_ri_path}" \
		AUTHBRIDGE_SKIP_DOWNLOAD="${_ri_skip}" \
		T_REL="${T_REL:-}" T_URLS="${T_RUN}/urls" T_LOG="${T_RUN}/log" T_PROBE="${_ri_probe}" \
		T_CURL="${T_CURL}" T_MEET="${T_RUN}/meet" T_REAL_SLEEP="${T_REAL_SLEEP}" \
		T_SLEEPS="${T_RUN}/sleeps" T_SLEEP_NOFRAC="$([ "${T_SLEEP}" != nofrac ] || echo 1)" \
		sh "$@"
	T_ST=0
	if [ -n "${T_PTY}" ]; then
		# A pty starts with no size, so give it one before install.sh asks.
		_ri_cols=100
		[ "${T_PTY}" != narrow ] || _ri_cols=40
		_ri_term=xterm
		[ "${T_PTY}" != dumb ] || _ri_term=dumb
		{
			printf 'echo "$$" >%s\n' "$(t_q "${T_RUN}/pty.pid")" # exec keeps it: install.sh's
			printf 'stty cols %s rows 30 2>/dev/null\nTERM=%s\nexport TERM\nexec' "${_ri_cols}" "${_ri_term}"
			for _ri_a in "$@"; do printf ' %s' "$(t_q "${_ri_a}")"; done
			printf '\n'
		} >"${T_RUN}/pty.sh"
		: >"${T_RUN}/err"
		if [ "${T_PTY}" = hup ]; then
			# script(1) holds the pty's other end, so killing it hangs the pty up, as closing
			# a terminal window does, and install.sh gets a HUP with no terminal left to
			# write to. It is waited out by its pid, since it is no child of this shell.
			: >"${T_RUN}/out"
			t_pty_bg "${T_RUN}/pty.sh" </dev/null >"${T_RUN}/out" 2>&1
			t_until 2 t_marks
			t_until yes t_drawn
			kill -KILL "${T_SPID}" 2>/dev/null || :
			wait "${T_SPID}" 2>/dev/null || :
			t_until gone t_gone
		elif [ "${T_PTY}" = int ]; then
			: >"${T_RUN}/out"
			{ t_until 2 t_marks; t_until yes t_drawn; printf '\003'; "${T_REAL_SLEEP}" 1; } \
				| t_pty "${T_RUN}/pty.sh" >"${T_RUN}/out" 2>&1 || T_ST=$?
		else
			t_pty "${T_RUN}/pty.sh" </dev/null >"${T_RUN}/out" 2>&1 || T_ST=$?
		fi
	elif [ -n "${T_BG}" ]; then
		"$@" <"${TMP}/piped-script" >"${T_RUN}/out" 2>"${T_RUN}/err" &
		T_PID=$!
	elif [ -n "${T_PIPE}" ]; then
		(cd "${T_RUN}" && exec "$@") <"${INSTALL_SH}" >"${T_RUN}/out" 2>"${T_RUN}/err" || T_ST=$?
	else
		"$@" <"${TMP}/piped-script" >"${T_RUN}/out" 2>"${T_RUN}/err" || T_ST=$?
	fi
	T_CURL=""
	T_SLEEP=""
	T_BG=""
	T_PTY=""
	T_PIPE=""
}
# t_drawn says yes once the terminal has been sent a bar: the cursor hidden, then a line.
t_drawn() { if tr '\033\r' 'ER' <"${T_RUN}/out" | grep -q 'E\[?25lRDownloading'; then echo yes; fi; }
# t_gone says gone once the pty run's install.sh has exited, and alive until then. A
# zombie has exited: in a container whose pid 1 is no init, an orphan is never reaped,
# and kill -0 still finds it.
t_gone() {
	_tg_pid=$(cat "${T_RUN}/pty.pid" 2>/dev/null)
	if kill -0 "${_tg_pid}" 2>/dev/null; then
		case "$(ps -o stat= -p "${_tg_pid}" 2>/dev/null | tr -d ' ')" in
			Z*) echo gone ;;
			*) echo alive ;;
		esac
	else
		echo gone
	fi
}
# t_until N CMD... polls, every 0.1s for up to 5s, until CMD prints N.
t_until() {
	_tu_n=$1
	shift
	_tu_i=0
	while [ "$("$@")" != "${_tu_n}" ] && [ "${_tu_i}" -lt 50 ]; do
		"${T_REAL_SLEEP}" 0.1
		_tu_i=$((_tu_i + 1))
	done
}

t_log() { sed -n "s/^$1 //p" "${T_RUN}/log"; } # one kind of record, without its tag
t_nlog() { t_log "$1" | wc -l | tr -d '[:space:]'; }
t_stage() { t_log argv | sed -n 's/^\[setup\] \[--from\] \[\([^]]*\)\].*/\1/p'; }
# t_argv is the handoff's argv with the stage shown as <stage>, a byte count above 0 as
# <N>, and a whole number of seconds as <S>. Anything else, a 0 or a fraction, shows as
# itself and fails the comparison.
t_argv() {
	_ta_s=$(t_stage)
	t_log argv | sed -e "s#\[${_ta_s:-//}\]#[<stage>]#" \
		-e 's/\[--handoff-bytes=[1-9][0-9]*\]/[--handoff-bytes=<N>]/' \
		-e 's/\[--handoff-seconds=[0-9][0-9]*\]/[--handoff-seconds=<S>]/'
}
t_execs() { # dir -> the setupBinaryNames in it that are executable
	for _te_b in agentop cortex cortex-session-dump; do
		[ ! -x "$1/${_te_b}" ] || printf '%s ' "${_te_b}"
	done | sed 's/ $//'
}
t_in() { if grep -qF -- "$1" "${T_RUN}/$2"; then echo yes; else echo no; fi; } # needle out|err
# t_under DIR PATH says yes when PATH is strictly inside DIR, and names PATH if not.
# (A function, not an inline `case`: bash 3.2, macOS's sh, misparses a case pattern's
# `)` inside "$(...)".)
t_under() { case "$2" in "$1"/?*) echo yes ;; *) echo "no: $2" ;; esac; }
t_dumps() { # dir -> how many cortex-session-dump* files, whole or partial, it holds
	_td_n=0
	for _td_f in "$1"/cortex-session-dump*; do [ ! -e "${_td_f}" ] || _td_n=$((_td_n + 1)); done
	printf '%s\n' "${_td_n}"
}
# t_stdin says ok when setup's stdin was the terminal or empty, and shows it if not.
t_stdin() { case "$(t_log stdin)" in tty | '[]') echo ok ;; *) t_log stdin ;; esac; }
t_same() { # stage agentop|cortex -> same when the stage holds that archive as released
	_ts_n="$2_v9.9.9_${T_OS}_${T_ARCH}.tar.gz"
	if cmp -s "$1/${_ts_n}" "${T_REL}/${_ts_n}"; then echo same; else echo "differs: $1/${_ts_n}"; fi
}
t_meet() { grep -c "^$1 " "${T_RUN}/meet/log" 2>/dev/null || true; } # met|killed|finished
t_marks() { # how many archive downloads have marked their start
	set -- "${T_RUN}/meet"/*.tar.gz
	if [ -e "$1" ]; then echo "$#"; else echo 0; fi
}
t_tmp_left() { ls -A "${T_RUN}/tmp" | wc -l | tr -d ' '; } # what is left in TMPDIR
T_NOFLAGS="[setup] [--from] [<stage>] [--handoff-bytes=<N>] [--handoff-seconds=<S>]"

# A release whose checksums.txt has the two lines release-binaries.yaml writes: it is
# downloaded, verified, staged, and handed to setup.
make_release v9.9.9
run_install ok release --ref=v9.9.9
check "stage: two checksum entries reach the handoff" "0 1" "${T_ST} $(t_nlog argv)"
# Sorted: the two archives download at once, so either can ask first. Off a terminal
# there is no bar, so no HEAD for the archives' sizes either.
check "stage: it fetches the two archives, checksums.txt and the dump helper, nothing else" \
	"https://github.com/rossoctl/cortex/releases/download/v9.9.9/agentop_v9.9.9_${T_OS}_${T_ARCH}.tar.gz
https://github.com/rossoctl/cortex/releases/download/v9.9.9/checksums.txt
https://github.com/rossoctl/cortex/releases/download/v9.9.9/cortex_v9.9.9_${T_OS}_${T_ARCH}.tar.gz
https://raw.githubusercontent.com/rossoctl/cortex/v9.9.9/scripts/dev/cortex-session-dump.py" \
	"$(LC_ALL=C sort "${T_RUN}/urls")"
check "download: off a terminal, one plain line says what it downloads" "1" \
	"$(cat "${T_RUN}/out" "${T_RUN}/err" | grep -cxF "Downloading v9.9.9 for ${T_OS}/${T_ARCH}..." || true)"
check "  and nothing is drawn: no ESC and no CR, on stdout or stderr" "0" \
	"$(cat "${T_RUN}/out" "${T_RUN}/err" | tr -cd '\033\r' | wc -c | tr -d ' ')"
check "download: both archives land in the stage, whole" "same same" \
	"$(t_same "$(t_stage)" agentop) $(t_same "$(t_stage)" cortex)"
_stage=$(t_stage)
check "stage: a fresh dir under TMPDIR" "yes" \
	"$(t_under "${T_RUN}/tmp" "${_stage}")"
check "stage: the archives' binaries and the dump helper are extracted into it, executable" \
	"agentop cortex cortex-session-dump" "$(t_execs "${_stage}")"
check "stage: setup runs from the stage, with them beside it" "1" \
	"$(t_log staged | grep -c 'agentop .*cortex .*cortex-session-dump ' || true)"
check "stage: nothing is put in ~/.local/bin (setup does that)" "absent" \
	"$([ -e "${T_RUN}/home/.local/bin" ] && echo present || echo absent)"
check "handoff: setup --from the stage, with the download's counts" "${T_NOFLAGS}" "$(t_argv)"
check "handoff: --handoff-bytes is the size of the two archives" "${T_BYTES}" \
	"$(t_log argv | sed -n 's/.*\[--handoff-bytes=\([0-9]*\)\].*/\1/p')"
check "handoff: setup's stdin is the terminal or /dev/null, never the piped script" "ok" \
	"$(t_stdin)"

# Each flag install.sh shares with setup is passed on, in one fixed order; --local,
# setup's default, is not.
run_install ok release --ref=v9.9.9 --claude-code --yes --no-service --no-modify-path
check "handoff: --claude-code --yes --no-service --no-modify-path are passed on" \
	"[setup] [--from] [<stage>] [--claude-code] [--yes] [--no-service] [--no-modify-path] [--handoff-bytes=<N>] [--handoff-seconds=<S>]" \
	"$(t_argv)"
run_install ok release --ref=v9.9.9 --install-only -y
check "handoff: --install-only, and -y as --yes" \
	"[setup] [--from] [<stage>] [--yes] [--install-only] [--handoff-bytes=<N>] [--handoff-seconds=<S>]" \
	"$(t_argv)"
run_install ok release --ref=v9.9.9 --local
check "handoff: --local is not passed on" "${T_NOFLAGS}" "$(t_argv)"

# --- the download: both archives at once, and every way it can end ---
T_CURL=meet
run_install ok release --ref=v9.9.9
check "download: the two archives download at once (each sees the other start)" "2 2" \
	"$(sed -n 's/^met [^ ]* //p' "${T_RUN}/meet/log" | tr '\n' ' ' | sed 's/ $//')"
check "  and still reach the handoff" "0 1" "${T_ST} $(t_nlog argv)"

# One archive failing dies with today's message for it, whichever of the two it is.
rm -f "${T_REL}/cortex_v9.9.9_${T_OS}_${T_ARCH}.tar.gz"
run_install ok release --ref=v9.9.9
check "download: the second archive failing dies, naming it" "1 1" \
	"${T_ST} $(grep -cxF "error: download failed: cortex_v9.9.9_${T_OS}_${T_ARCH}.tar.gz" "${T_RUN}/err" || true)"
check "  and nothing runs after it: no probe, no handoff, no stage left" "0 0 0" \
	"$(t_nlog probe) $(t_nlog argv) $(t_tmp_left)"
make_release v9.9.9
rm -f "${T_REL}/agentop_v9.9.9_${T_OS}_${T_ARCH}.tar.gz"
run_install ok release --ref=v9.9.9
check "download: the first archive failing dies, naming it" "1 1" \
	"${T_ST} $(grep -cxF "error: download failed: agentop_v9.9.9_${T_OS}_${T_ARCH}.tar.gz" "${T_RUN}/err" || true)"
check "  and nothing runs after it: no probe, no handoff, no stage left" "0 0 0" \
	"$(t_nlog probe) $(t_nlog argv) $(t_tmp_left)"

# The poll: every 0.2s, and every 1s where sleep rejects a fraction, from the first
# rejection on. The archives take 1s, so the stub sleep (0.2s a call) is polled ~5 times.
# Shown as the first call, then the distinct calls after it.
t_sleeps() {
	printf '%s | %s' "$(sed -n 1p "${T_RUN}/sleeps")" "$(sed 1d "${T_RUN}/sleeps" | sort -u | tr '\n' ' ')"
}
make_release v9.9.9
T_CURL=slow
T_SLEEP=frac
run_install ok release --ref=v9.9.9
check "download: polls every 0.2s" "0.2 | 0.2 " "$(t_sleeps)"
check "  more than once while the archives download" "yes" \
	"$(if [ "$(wc -l <"${T_RUN}/sleeps")" -ge 3 ]; then echo yes; else cat "${T_RUN}/sleeps"; fi)"
# The first run's downloads end before the first poll, so it could not draw a bar if it
# tried. This one polls several times, so off a terminal it must draw nothing each time.
check "  and, off a terminal, draws nothing at any poll: no ESC, no CR, no HEAD" "0 0" \
	"$(cat "${T_RUN}/out" "${T_RUN}/err" | tr -cd '\033\r' | wc -c | tr -d ' ') $(grep -c '^HEAD ' "${T_RUN}/urls" || true)"
T_CURL=slow
T_SLEEP=nofrac
run_install ok release --ref=v9.9.9
check "download: where sleep 0.2 fails, polls with sleep 1 from then on" "0.2 | 1 " "$(t_sleeps)"
check "  and still reaches the handoff" "0 1" "${T_ST} $(t_nlog argv)"

# TERM mid-download: the shell traps it, so the EXIT trap runs (dash skips that trap on
# a signal it has no trap for), the curls are stopped rather than left writing into a
# stage being deleted, and setup never runs. Background jobs in a script ignore INT, so
# INT cannot be sent this way; it is typed as a ^C on a pseudo-terminal below.
T_CURL=hold
T_BG=1
run_install ok release --ref=v9.9.9
t_until 2 t_marks
kill -TERM "${T_PID}"
T_ST=0
wait "${T_PID}" || T_ST=$?
t_until 2 t_meet killed
check "TERM mid-download: exits 143" "143" "${T_ST}"
check "  stopping both downloads" "2 0" "$(t_meet killed) $(t_meet finished)"
check "  leaving no stage, and never handing off" "0 0 0" \
	"$(t_tmp_left) $(t_nlog probe) $(t_nlog argv)"

# HUP mid-download: trapped the same way. Untrapped, dash dies of it as it would of an
# untrapped TERM, and the stage stays behind. The downloads are counted the moment the
# script has exited, before any wait here: it waits them out itself, so the rm -rf after
# the kill cannot race a curl still writing into the stage.
T_CURL=hold
T_BG=1
run_install ok release --ref=v9.9.9
t_until 2 t_marks
kill -HUP "${T_PID}"
T_ST=0
wait "${T_PID}" || T_ST=$?
_hup_ended=$(t_meet killed)
t_until 2 t_meet killed
check "HUP mid-download: exits 129" "129" "${T_ST}"
check "  with both downloads stopped before it exits" "2 2 0" \
	"${_hup_ended} $(t_meet killed) $(t_meet finished)"
check "  leaving no stage, and never handing off" "0 0 0" \
	"$(t_tmp_left) $(t_nlog probe) $(t_nlog argv)"

# --- on a terminal: the bar, and what is left of it when the download ends ---
# out is the terminal's bytes with ESC as E and CR as R. The archives take 1s, so the
# bar is drawn at least once; "setup:start" is the fake setup's first output.
t_tty() { tr '\033\r' 'ER' <"${T_RUN}/out"; }
t_ntty() { t_tty | grep -o -- "$1" | wc -l | tr -d ' '; } # how often a string is on screen
T_CURL=slow
T_PTY=yes
run_install ok release --ref=v9.9.9
check "terminal: the bar is drawn, with the archives' sizes asked for by HEAD" "0 2 1" \
	"${T_ST} $(grep -c '^HEAD .*\.tar\.gz$' "${T_RUN}/urls" || true) $(t_ntty 'E\[?25l')"
check "  and the percentage is of their sizes together" "yes" \
	"$(if t_tty | grep -qF -- "/ $(with_bar "size_text ${T_BYTES}")E[K"; then echo yes; else t_tty; fi)"
check "  instead of the plain line" "0" "$(t_ntty "for ${T_OS}/${T_ARCH}\.\.\.")"
# What follows the erase can include a warning of the steps between (no python3 for the
# dump helper, say), but nothing of the bar, and setup comes after it.
t_after_erase() { # does what follows the erase hold $1? yes or no
	if t_tty | tr '\n' 'N' | sed -n 's/.*RE\[KE\[?25h//p' | grep -q -- "$1"; then echo yes; else echo no; fi
}
check "  erased, with the cursor shown again, before setup starts" "1 yes no no" \
	"$(t_ntty 'E\[?25h') $(t_after_erase setup:start) $(t_after_erase Downloading) $(t_after_erase 'E\[')"
make_release v9.9.9
rm -f "${T_REL}/cortex_v9.9.9_${T_OS}_${T_ARCH}.tar.gz"
T_CURL=slow
T_PTY=yes
run_install ok release --ref=v9.9.9
check "terminal: a failed download erases the bar and shows the cursor before the error" "1 1 1" \
	"${T_ST} $(t_ntty "RE\[KE\[?25herror: download failed: cortex_v9.9.9_${T_OS}_${T_ARCH}.tar.gz") $(t_ntty 'E\[?25h')"
# A real ^C: typed into the terminal, so the kernel sends SIGINT to everything in the
# foreground, as it does for a person. The curls ignore it, so the trap has to stop them.
#
# Only where SIGINT reaches this suite, though. A shell started with SIGINT ignored (the
# suite run as a background job, or under nohup) passes that on to everything it starts,
# install.sh included, and no shell can undo it, so a ^C would do nothing. That is
# probed first, and fails as itself rather than as a bug in install.sh.
if [ -n "$(sh -c 'kill -INT $$; echo ignored' 2>/dev/null || :)" ]; then
	check "terminal: ^C mid-download (cannot run: this suite was started with SIGINT ignored)" \
		"SIGINT default" "SIGINT ignored"
else
	make_release v9.9.9
	T_CURL=hold
	T_PTY=int
	run_install ok release --ref=v9.9.9
	t_until 2 t_meet killed
	check "terminal: ^C mid-download exits 130" "130" "${T_ST}"
	check "  erasing the bar and showing the cursor" "1 1" \
		"$(t_ntty 'E\[?25l') $(t_tty | grep -c 'RE\[KE\[?25h$' || true)"
	check "  stopping both downloads, leaving no stage, never handing off" "2 0 0 0" \
		"$(t_meet killed) $(t_tmp_left) $(t_nlog probe) $(t_nlog argv)"
fi
# The terminal closed mid-download, with the bar on screen. The HUP runs the EXIT trap,
# and the bar's erase then fails, as there is no terminal left to write to: under set -e
# that ended the trap before it removed the stage. Cleanup must not depend on the erase.
make_release v9.9.9
T_CURL=hold
T_PTY=hup
run_install ok release --ref=v9.9.9
check "terminal closed mid-download: the bar was drawn, and install.sh has exited" "yes gone" \
	"$(t_drawn) $(if [ -s "${T_RUN}/pty.pid" ]; then t_gone; else echo 'no pid'; fi)"
check "  leaving no stage, and never handing off" "0 0 0" \
	"$(t_tmp_left) $(t_nlog probe) $(t_nlog argv)"
# Terminals the bar cannot use get the plain line, and no HEADs: TERM=dumb cannot erase,
# and a line wider than the terminal would wrap, so each redraw would add a line.
make_release v9.9.9
for _tp in dumb narrow; do
	T_CURL=slow
	T_PTY=${_tp}
	run_install ok release --ref=v9.9.9
	check "terminal, ${_tp}: the plain line instead of the bar" "0 1 0 0" \
		"${T_ST} $(t_ntty "Downloading v9.9.9 for ${T_OS}/${T_ARCH}\.\.\.") $(t_ntty 'E\[') $(grep -c '^HEAD ' "${T_RUN}/urls" || true)"
done

# --- the checksum rule: exactly two verified entries, or nothing runs ---
make_release v9.9.9 one
run_install ok release --ref=v9.9.9
check "checksums: one entry for two archives dies" "1 yes" \
	"${T_ST} $(t_in "checksums.txt has no usable entry for cortex_v9.9.9_${T_OS}_${T_ARCH}.tar.gz" err)"
check "  and runs no agentop" "0 0" "$(t_nlog probe) $(t_nlog argv)"
make_release v9.9.9 three
run_install ok release --ref=v9.9.9
check "checksums: three matching entries die (exactly 2, not at least 2)" "1 yes" \
	"${T_ST} $(t_in 'expected 2 checksum entries, got 3' err)"
check "  and runs no agentop" "0 0" "$(t_nlog probe) $(t_nlog argv)"
make_release v9.9.9 bad
run_install ok release --ref=v9.9.9
check "checksums: a hash that does not match dies" "1 yes" \
	"${T_ST} $(t_in 'checksum verification failed' err)"
check "  and runs no agentop" "0 0" "$(t_nlog probe) $(t_nlog argv)"

# --- the dump helper is never fatal ---
make_release v9.9.9
rm -f "${T_REL}/cortex-session-dump.py"
run_install ok release --ref=v9.9.9
check "dump helper: a failed fetch still hands off" "0 1" "${T_ST} $(t_nlog argv)"
check "  with no helper, whole or partial, in the stage" "0" \
	"$(t_dumps "$(t_stage)")"
make_release v9.9.9
printf '<html>404</html>\n' >"${T_REL}/cortex-session-dump.py"
run_install ok release --ref=v9.9.9
check "dump helper: a body without the python3 shebang still hands off" "0 1" "${T_ST} $(t_nlog argv)"
check "  and is not staged" "0" "$(t_dumps "$(t_stage)")"

# --- the exec probe: noexec TMPDIR, and an agentop too old to have setup ---
make_release v9.9.9
run_install noexec release --ref=v9.9.9
_p1=$(t_log probe | sed -n 1p)
_p2=$(t_log probe | sed -n 2p)
check "noexec: exit 126 probes a second time" "2" "$(t_nlog probe)"
check "  first from the stage under TMPDIR" "yes" \
	"$(t_under "${T_RUN}/tmp" "${_p1%/agentop}")"
check "  then from a re-stage under ~/.cortex/tmp" "yes" \
	"$(t_under "${T_RUN}/home/.cortex/tmp" "${_p2%/agentop}")"
check "  and hands off from the re-stage" "${_p2%/agentop}" "$(t_stage)"
check "  with the same argv" "${T_NOFLAGS}" "$(t_argv)"
check "  the re-stage keeps the binaries' modes" "agentop cortex cortex-session-dump" \
	"$(t_execs "${_p2%/agentop}")"
check "  and the first stage is gone" "gone" "$([ -e "${_p1%/agentop}" ] && echo kept || echo gone)"
run_install noexec-all release --ref=v9.9.9
check "noexec everywhere: dies, naming both dirs" "1 yes yes" \
	"${T_ST} $(t_in "${T_RUN}/tmp" err) $(t_in "${T_RUN}/home/.cortex/tmp" err)"
check "  and never hands off, nor leaves a stage behind" "0 0 0" \
	"$(t_nlog argv) $(ls -A "${T_RUN}/tmp" | wc -l | tr -d ' ') $(ls -A "${T_RUN}/home/.cortex/tmp" | wc -l | tr -d ' ')"

# An agentop from before setup exits 2 on `setup --help`. The guard names the release
# and the one-liner that runs that release's own installer.
run_install old release --ref=v9.9.9
check "old agentop: the guard dies" "1" "${T_ST}"
check "  saying why" "1" \
	"$(grep -cxF "error: the v9.9.9 agentop has no 'setup' command, which this installer needs." "${T_RUN}/err" || true)"
check "  with the piped --ref one-liner for that release" "1" \
	"$(grep -cxF '    curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh | sh -s -- --ref=v9.9.9' "${T_RUN}/err" || true)"
check "  and nothing runs after it: one probe, no handoff" "1 0" "$(t_nlog probe) $(t_nlog argv)"
check "  and the stage is cleaned up" "gone" \
	"$(_d=$(t_log probe); [ -e "${_d%/agentop}" ] && echo kept || echo gone)"
# The channel build is not a tag to pin, so the guard names the agentop instead.
make_release "${CHANNEL_TAG}"
run_install old release --ref=main
check "old agentop, channel build: the guard names the agentop it ran" "1 yes" \
	"${T_ST} $(t_in "the agentop at $(t_log probe) has no 'setup' command" err)"
check "  and offers no --ref one-liner for the channel" "no" "$(t_in "--ref=${CHANNEL_TAG}" err)"

# --- AUTHBRIDGE_SKIP_DOWNLOAD=1: repair what is installed, offline ---
run_install ok repair
check "repair: probes the installed agentop" "${T_RUN}/home/.local/bin/agentop" "$(t_log probe)"
check "  and execs its setup, with no --from and no counts" "[setup]" "$(t_argv)"
check "  touching no network" "" "$(cat "${T_RUN}/urls")"
run_install ok repair --claude-code --yes --no-service --install-only --no-modify-path
check "repair: the flags are passed on" \
	"[setup] [--claude-code] [--yes] [--no-service] [--install-only] [--no-modify-path]" "$(t_argv)"
run_install old repair
check "repair: an installed agentop without setup hits the guard" "1 yes" \
	"${T_ST} $(t_in "error: the agentop at ${T_RUN}/home/.local/bin/agentop has no 'setup' command" err)"
check "  and is never handed off to" "0" "$(t_nlog argv)"
run_install ok bare
check "repair: with no agentop installed it dies" "1 yes" \
	"${T_ST} $(t_in "AUTHBRIDGE_SKIP_DOWNLOAD=1 but ${T_RUN}/home/.local/bin/agentop is missing" err)"

# --- --stop: stops what runs here, and nothing else ---
# Piped, as the one-liner runs it, so the bootstrap is in play: --stop re-execs no
# released copy, since that one may predate the flag, and downloads nothing. With
# nothing running it says so, and exits 0. A release is there to download, so a --stop
# that fell through to the install would reach setup, and these checks would see it.
make_release v9.9.9
T_PIPE=1
run_install ok release --ref=v9.9.9 --stop
check "--stop, piped: exits 0, finding nothing to stop" "0 yes" \
	"${T_ST} $(t_in 'Cortex is not running (no service, no live pidfile, ports free).' out)"
check "  fetching nothing: no released script to re-exec, no release" "" "$(cat "${T_RUN}/urls")"
check "  and never probing or handing off to setup" "0 0" "$(t_nlog probe) $(t_nlog argv)"

# --- --no-modify-path is install.sh's own flag now ---
# The loop dies on the first unknown option, so reaching --bogus means
# --no-modify-path parsed.
run_install ok release --no-modify-path --bogus
check "--no-modify-path parses, and an unknown option still dies" "1 yes" \
	"${T_ST} $(t_in 'unknown option: --bogus' err)"
check "  before fetching anything" "" "$(cat "${T_RUN}/urls")"
run_install ok release --help
check "--help lists --no-modify-path" "1" "$(grep -c '^  --no-modify-path ' "${T_RUN}/out" || true)"

# --- make dev-install is the same handoff, from ./bin ---
# Setup copies the binaries, writes the config and starts the service, and its cleanup
# step is what remove_stale was, so the Makefile carries none of that any more.
# $(BIN_DIR), not ./bin: that is where the build writes, so the two cannot disagree.
_dev=$(awk '/^dev-install:/ {f = 1; next} f && !/^\t/ {exit} f' "${REPO_ROOT}/Makefile")
# shellcheck disable=SC2016 # $(BIN_DIR) is make's, matched literally
check "make dev-install hands \$(BIN_DIR) to agentop setup" "1" \
	"$(printf '%s\n' "${_dev}" | grep -cF '$(BIN_DIR)/agentop setup --from $(BIN_DIR) --yes --no-modify-path --restart' || true)"
check "  and does no install of its own" "0" \
	"$(printf '%s\n' "${_dev}" | grep -v '^	@#' | grep -cE 'cp |service install|service status|--write-config|DEV_BIN_DIR' || true)"
check "  and nothing in the Makefile uses remove_stale" "0" \
	"$(grep -c 'remove_stale' "${REPO_ROOT}/Makefile" || true)"

# --- authbridge/install.sh: the URL v0.7.0 and its docs give, which 404'd ---
#
# The curl stub serves a fake installer that reports how it was run, and records the
# URL. A v0.7.0 installer re-execs <tag>/authbridge/install.sh with
# AUTHBRIDGE_SCRIPT_REF=<tag>, so that tag's scripts/install.sh must be what runs.
STUB_SH="${REPO_ROOT}/authbridge/install.sh"
cat >"${TMP}/fake-installer.sh" <<'EOF'
printf 'zero=%s ref=%s argc=%s' "$0" "${AUTHBRIDGE_SCRIPT_REF:-}" "$#"
for _a in "$@"; do printf ' [%s]' "${_a}"; done
printf '\n'
EOF
with_stub() { # serve(ok|fail)  how(pipe|reexec)  args...
	_serve=$1 _how=$2
	shift 2
	mkdir -p "${TMP}/stubbin"
	{
		printf '#!/bin/sh\n'
		printf 'for a in "$@"; do case "$a" in http*) printf "%%s\\n" "$a" >>"%s" ;; esac; done\n' "${TMP}/stub-urls"
		if [ "${_serve}" = ok ]; then
			printf 'out=""; prev=""; for a in "$@"; do [ "$prev" = -o ] && out=$a; prev=$a; done\n'
			printf 'if [ -n "$out" ]; then cat "%s" >"$out"; else cat "%s"; fi\n' "${TMP}/fake-installer.sh" "${TMP}/fake-installer.sh"
		else
			printf 'exit 22\n'
		fi
	} >"${TMP}/stubbin/curl"
	chmod +x "${TMP}/stubbin/curl"
	: >"${TMP}/stub-urls"
	_st=0
	if [ "${_how}" = pipe ]; then
		PATH="${TMP}/stubbin:${PATH}" sh -s -- "$@" <"${STUB_SH}" 2>/dev/null || _st=$?
	else
		PATH="${TMP}/stubbin:${PATH}" AUTHBRIDGE_SCRIPT_REF=v9.9.9 sh "${STUB_SH}" "$@" 2>/dev/null || _st=$?
	fi
	printf 'status=%s url=%s\n' "${_st}" "$(cat "${TMP}/stub-urls")"
}
check "authbridge/install.sh piped: runs main's scripts/install.sh from stdin" \
	"zero=sh ref= argc=0
status=0 url=https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh" \
	"$(with_stub ok pipe)"
check "authbridge/install.sh re-exec'd by v0.7.0: runs that tag's scripts/install.sh" \
	"zero=sh ref=v9.9.9 argc=0
status=0 url=https://raw.githubusercontent.com/rossoctl/cortex/v9.9.9/scripts/install.sh" \
	"$(with_stub ok reexec)"
check "authbridge/install.sh passes arguments through, spaces intact" \
	"zero=sh ref= argc=2 [--no-service] [--ref=a b]
status=0 url=https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh" \
	"$(with_stub ok pipe --no-service '--ref=a b')"
check_fails "authbridge/install.sh: a failed download exits non-zero" \
	"$(with_stub fail pipe | sed -n 's/^status=\([0-9]*\) .*/\1/p')"

printf '\n%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = "0" ]
