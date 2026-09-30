#!/bin/sh
# release_smoke_test_linux.sh — the Linux half of the release smoke test (#957).
#
# Exercises install.sh + abctl service exactly as a real user would, against a
# real published release: fresh install, upgrade over an existing install
# (config preserved), a no-op re-run, and a clean uninstall. Run against a real
# tag/release, not a local build — there is no supported "install from this
# on-disk tarball" mode in install.sh, so this always downloads for real.
#
# Usage: release_smoke_test_linux.sh <tag-under-test>
#
# Deliberately NOT covered here: driving a real request through the proxy and
# asserting a parsed event with a non-zero token count (#957's third bullet).
# That needs the unattended/headless capture path from #955, which does not
# exist yet — tracked there, not attempted here.
set -eu

TAG="${1:?usage: release_smoke_test_linux.sh <tag-under-test>}"
ABCTL="${HOME}/.local/bin/abctl"
CFG="${HOME}/.cortex/config.yaml"
UNIT="${HOME}/.config/systemd/user/cortex.service"

# One temp dir, cleaned up on any exit (success, an assertion's exit 1, or an
# uncaught error under set -e) — the same pattern scripts/install_test.sh and
# scripts/dev/verify-moved-ca-diagnostics.sh already use, rather than a
# scattered rm -f after each individual mktemp that an early exit would skip.
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

# Downloads to a temp file first rather than piping curl straight into sh:
# /bin/sh on ubuntu-latest is dash, which has no pipefail, so a failed
# download (a 404, a network blip) would otherwise leave sh reading empty
# stdin and the pipeline exiting 0 — the failure would only surface later, at
# assert_healthy, pointing at "the service is unhealthy" rather than "the
# download failed."
#
# Still never invoked as a local file, though: install.sh's own re-exec
# bootstrap only re-fetches and re-runs the tagged copy of itself (matching
# the requested --ref) when $0 is not a readable file — the exact condition a
# real `curl | sh` user hits. Reading the script from a temp file via stdin
# redirection (rather than running it as `sh /path/to/tmpfile`) keeps that
# property: $0 stays "sh", not a file path.
install_cortex() {
	tmp="$(mktemp "${TMP_DIR}/install.XXXXXX")"
	if ! curl -fsSL -o "${tmp}" https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh; then
		echo "FAIL: could not download install.sh" >&2
		exit 1
	fi
	if [ ! -s "${tmp}" ]; then
		echo "FAIL: downloaded install.sh is empty" >&2
		exit 1
	fi
	sh -s -- --ref="$1" --yes <"${tmp}"
}

log() { printf '\n== %s ==\n' "$1"; }

assert_contains() {
	# assert_contains <file> <needle> <description>
	if ! grep -q -- "$2" "$1"; then
		echo "FAIL: $3" >&2
		echo "--- $1 ---" >&2
		cat "$1" >&2
		exit 1
	fi
}

assert_healthy() {
	out="$(mktemp "${TMP_DIR}/status.XXXXXX")"
	"${ABCTL}" service status | tee "${out}"
	assert_contains "${out}" "healthy:" "abctl service status did not report healthy"
}

# The most recent STABLE release created strictly BEFORE the tag under test —
# the realistic "what a user who hasn't upgraded in a while" starting point.
# Not hardcoded: a fixed "known good" version would drift out of the release
# list over time and stop being the second-most-recent release, silently
# testing a narrower jump than intended.
#
# "Before TAG", not just "the most recent other release": this workflow can be
# triggered by re-running an OLD completed Release Binaries run, at which
# point newer stable releases may already exist. Picking "the most recent
# other release" in that case would select something NEWER than TAG, silently
# turning the "upgrade" step into an unlabeled downgrade — and for a release
# old enough (pre-rename artifact names, no install.sh at any path it
# probes), the fresh install of that "older" release would fail outright.
#
# createdAt, not publishedAt: main-latest is a moving tag whose assets get
# clobbered on every push to main, and its createdAt is refreshed by that —
# publishedAt is not, staying frozen at whenever the release was first
# created (2026-09-09 here, confirmed against the live API). Anchoring on
# publishedAt would permanently exclude every stable release published after
# that first day from ever being picked for a main-latest run. createdAt
# holds for a normal, non-rolling release too — verified against the live API
# that it lands a few minutes before that release's own publishedAt, so
# nothing here regresses for the v* case.
#
# set -eu alone won't catch a failure inside a pipeline under dash (no
# pipefail), so each gh call below is checked explicitly rather than trusted
# to abort the script on its own.
if ! tag_created_at="$(gh release view "${TAG}" --json createdAt -q '.createdAt')"; then
	echo "FAIL: could not resolve the creation date for release ${TAG}" >&2
	exit 1
fi

if ! OLDER_TAG="$(gh release list --exclude-drafts --exclude-pre-releases --limit 20 \
	--json tagName,createdAt \
	-q "[.[] | select(.createdAt < \"${tag_created_at}\")] | sort_by(.createdAt) | last | .tagName // \"\"")"; then
	echo "FAIL: gh release list failed" >&2
	exit 1
fi

log "Testing ${TAG} (upgrading from: ${OLDER_TAG:-none found; first release})"

INSTALL_TAG="${OLDER_TAG:-${TAG}}"
log "Fresh install: ${INSTALL_TAG}"
install_cortex "${INSTALL_TAG}"
assert_healthy

if [ -n "${OLDER_TAG}" ]; then
	# A marker only this test writes, to prove config survives the upgrade
	# untouched (beyond migrateConfig's own additive listener pins).
	marker="smoke-test-marker-${TAG}"
	printf '# %s\n' "${marker}" >>"${CFG}"

	log "Upgrade: ${INSTALL_TAG} -> ${TAG}"
	install_cortex "${TAG}"
	assert_contains "${CFG}" "${marker}" "config marker did not survive the upgrade"
	assert_healthy
fi

log "No-op re-run: ${TAG}"
# Redirected, not piped through tee: install_cortex is a function with its own
# early `exit 1` on a failed download, and each side of a pipe runs in its own
# subshell — an exit inside install_cortex on the left of a pipe would only
# kill that subshell, with the pipeline's own exit status coming from tee
# (which sees a closed/empty stdin and exits 0 regardless), silently hiding
# exactly the failure this function's error handling exists to surface.
reinstall_out="$(mktemp "${TMP_DIR}/reinstall.XXXXXX")"
if ! install_cortex "${TAG}" >"${reinstall_out}" 2>&1; then
	echo "FAIL: re-running install failed" >&2
	cat "${reinstall_out}" >&2
	exit 1
fi
cat "${reinstall_out}"
assert_contains "${reinstall_out}" "Already current" "re-running install was not a no-op"

log "Uninstall"
# Existence alone survives truncation or a rewrite — the promise being tested
# is that the file is left UNTOUCHED, so a checksum from just before uninstall
# is what actually proves that, not just that something is still there
# afterward.
cfg_before="$(cksum <"${CFG}")"
uninstall_out="$(mktemp "${TMP_DIR}/uninstall.XXXXXX")"
"${ABCTL}" service uninstall --yes | tee "${uninstall_out}"
assert_contains "${uninstall_out}" "Removed" "uninstall did not report success"
if [ -f "${UNIT}" ]; then
	echo "FAIL: unit file still present after uninstall: ${UNIT}" >&2
	exit 1
fi
if [ ! -f "${CFG}" ]; then
	echo "FAIL: uninstall removed ${CFG}; it promises to leave config untouched" >&2
	exit 1
fi
cfg_after="$(cksum <"${CFG}")"
if [ "${cfg_before}" != "${cfg_after}" ]; then
	echo "FAIL: uninstall changed the contents of ${CFG}; it promises to leave it untouched" >&2
	exit 1
fi

log "Linux release smoke test passed for ${TAG}"
