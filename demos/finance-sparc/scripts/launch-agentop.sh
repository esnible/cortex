#!/bin/bash
# Launch agentop against the finance-agent's authbridge sidecar session API.
# agentop ships a Namespaces → Pods picker that spawns its own kubectl
# port-forward; this wrapper just verifies prerequisites and runs it.
set -euo pipefail

NAMESPACE=${1:-team1}
AGENT_NAME=${2:-finance-agent}
AGENTOP_BIN=${AGENTOP_BIN:-/tmp/agentop-finance-sparc-demo}

if [[ ! -x "$AGENTOP_BIN" ]]; then
  echo "ERROR: agentop binary not found at $AGENTOP_BIN." >&2
  echo "       Run \`make show-result\` (which depends on build-agentop) first." >&2
  exit 1
fi
if ! kubectl -n "$NAMESPACE" get deploy "$AGENT_NAME" >/dev/null 2>&1; then
  echo "ERROR: deployment $NAMESPACE/$AGENT_NAME not found. Run 'make demo' first." >&2
  exit 1
fi
"$AGENTOP_BIN"
