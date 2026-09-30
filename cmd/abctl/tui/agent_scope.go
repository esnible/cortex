package tui

import (
	"context"
	"strings"
	"time"

	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// fetchUsageScoped is one /v1/usage read for the spend band or drawer, narrowed to agent when set.
// Unscoped it is the plain read. Scoped it asks the server to narrow, and where the answer carries
// no agent echo — a server that predates agent= — narrows the group=agent series itself, the way
// the usage pane always has.
func fetchUsageScoped(ctx context.Context, client *apiclient.Client, window string, resolution time.Duration, agent string, group usage.Group) (*usage.Snapshot, error) {
	if agent == "" {
		return client.GetUsageWindow(ctx, window, resolution, "", group)
	}
	snap, err := client.GetUsageWindowForAgent(ctx, window, resolution, "", agent, group)
	if err != nil || snap.Agent == agent {
		return snap, err
	}
	all, err := client.GetUsageWindow(ctx, window, resolution, "", usage.GroupAgent)
	if err != nil {
		return nil, err
	}
	scoped := usage.NarrowToAgent(*all, agent, group)
	return &scoped, nil
}

// agentScopeName is the name sessions carry for an agent scope label: the product before its
// version. A label that is a raw User-Agent names no session.
func agentScopeName(label string) string {
	name, _, _ := strings.Cut(label, "/")
	return name
}

// spendAxes are the drawer's axes: every one, less the agent axis under a scope, where it would
// be a single row.
func (m *model) spendAxes() []usage.Group {
	if m.agentScope == "" {
		return spendDrawerAxes
	}
	out := make([]usage.Group, 0, len(spendDrawerAxes))
	for _, a := range spendDrawerAxes {
		if a != usage.GroupAgent {
			out = append(out, a)
		}
	}
	return out
}

// spendAxis is the drawer's axis. Under a scope an index left on the agent axis reads as the
// first axis instead.
func (m *model) spendAxis() usage.Group {
	a := m.spend.axis()
	if m.agentScope != "" && a == usage.GroupAgent {
		return spendDrawerAxes[0]
	}
	return a
}
