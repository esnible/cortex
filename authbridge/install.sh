#!/bin/sh
# The installer lives at scripts/install.sh. This path is the one v0.7.0, its README
# and the rossoctl.dev docs give, and the one a v0.7.0 installer re-execs for a newer
# release; it runs scripts/install.sh from the same ref, with the same arguments.
#
# Read from stdin, not run as a file: that is how scripts/install.sh knows it came from
# `curl | sh` and runs the newest release's copy of itself.
set -eu
ref="${AUTHBRIDGE_SCRIPT_REF:-main}"
tmp=$(mktemp)
trap 'rm -f "${tmp}"' EXIT
curl -fsSL "https://raw.githubusercontent.com/rossoctl/cortex/${ref}/scripts/install.sh" -o "${tmp}"
sh -s -- "$@" <"${tmp}"
