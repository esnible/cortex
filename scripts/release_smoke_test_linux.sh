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

# Always piped, never invoked as a local file: install.sh's own re-exec bootstrap
# only re-fetches and re-runs the tagged copy of itself (matching the requested
# --ref) when $0 is not a readable file — the exact condition a real `curl | sh`
# user hits. Running the checked-out scripts/install.sh directly would skip that
# and test main's install.sh logic against an old tag's binaries, which is not
# what a real installer run for that tag ever looked like.
install_cortex() {
	curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh \
		| sh -s -- --ref="$1" --yes
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
	out="$(mktemp)"
	"${ABCTL}" service status | tee "${out}"
	assert_contains "${out}" "healthy:" "abctl service status did not report healthy"
	rm -f "${out}"
}

# The most recent STABLE release that isn't the tag under test — the realistic
# "what a user who hasn't upgraded in a while" starting point. Not hardcoded: a
# fixed "known good" version would drift out of the release list over time and
# stop being the second-most-recent release, silently testing a narrower jump
# than intended.
OLDER_TAG="$(gh release list --exclude-drafts --exclude-pre-releases --limit 10 \
	--json tagName -q '.[].tagName' | grep -v -x "${TAG}" | head -1 || true)"

log "Testing ${TAG} (upgrading from: ${OLDER_TAG:-none found; first release})"

INSTALL_TAG="${OLDER_TAG:-${TAG}}"
log "Fresh install: ${INSTALL_TAG}"
install_cortex "${INSTALL_TAG}"
assert_healthy

if [ -n "${OLDER_TAG:-}" ] && [ "${OLDER_TAG}" != "${TAG}" ]; then
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
reinstall_out="$(mktemp)"
install_cortex "${TAG}" | tee "${reinstall_out}"
assert_contains "${reinstall_out}" "Already current" "re-running install was not a no-op"
rm -f "${reinstall_out}"

log "Uninstall"
uninstall_out="$(mktemp)"
"${ABCTL}" service uninstall --yes | tee "${uninstall_out}"
assert_contains "${uninstall_out}" "Removed" "uninstall did not report success"
rm -f "${uninstall_out}"
if [ -f "${UNIT}" ]; then
	echo "FAIL: unit file still present after uninstall: ${UNIT}" >&2
	exit 1
fi
if [ ! -f "${CFG}" ]; then
	echo "FAIL: uninstall removed ${CFG}; it promises to leave config untouched" >&2
	exit 1
fi

log "Linux release smoke test passed for ${TAG}"
