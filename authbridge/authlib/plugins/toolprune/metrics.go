package toolprune

import (
	"fmt"
	"sort"
	"sync"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// metrics holds the plugin's counters. In-memory and per-process by design:
// this targets the single-laptop case, and staying free of a storage backend is
// what keeps the plugin dependency-free. Counters reset on restart, which is
// why every derived figure is reported alongside the sample behind it.
type metrics struct {
	mu sync.Mutex

	requestsSeen      uint64 // matched the path gate and carried a manifest
	requestsPruned    uint64 // body actually rewritten (enforce)
	requestsProjected uint64 // would have been rewritten (observe)

	toolsRemoved uint64
	perTool      map[string]uint64

	bytesRemoved uint64

	// Calibration sample for bytes -> tokens, gathered from response usage.
	promptTokens      uint64
	requestBytes      uint64
	requestsWithUsage uint64
}

func (m *metrics) seen() {
	m.mu.Lock()
	m.requestsSeen++
	m.mu.Unlock()
}

func (m *metrics) pruned(names []string, bytesRemoved int) {
	m.mu.Lock()
	m.requestsPruned++
	m.record(names, bytesRemoved)
	m.mu.Unlock()
}

func (m *metrics) projected(names []string, bytesRemoved int) {
	m.mu.Lock()
	m.requestsProjected++
	m.record(names, bytesRemoved)
	m.mu.Unlock()
}

// record must be called with mu held.
func (m *metrics) record(names []string, bytesRemoved int) {
	if m.perTool == nil {
		m.perTool = make(map[string]uint64)
	}
	for _, n := range names {
		m.perTool[n]++
	}
	m.toolsRemoved += uint64(len(names))
	if bytesRemoved > 0 {
		m.bytesRemoved += uint64(bytesRemoved)
	}
}

func (m *metrics) observeUsage(promptTokens, requestBytes int) {
	m.mu.Lock()
	m.promptTokens += uint64(promptTokens)
	m.requestBytes += uint64(requestBytes)
	m.requestsWithUsage++
	m.mu.Unlock()
}

// snapshot renders the counters as operator-facing metrics. Every derived row
// carries the sample it was computed from, so a figure can never be read as
// more certain than it is.
func (m *metrics) snapshot() []pipeline.Metric {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.requestsSeen == 0 && m.requestsPruned == 0 && m.requestsProjected == 0 {
		return nil
	}

	out := []pipeline.Metric{
		{Name: "requests seen", Value: float64(m.requestsSeen), Unit: "count"},
	}
	// Enforce and observe are mutually exclusive in practice (one policy per
	// plugin instance), but report whichever has fired so a mid-flight policy
	// change is visible rather than silently blended.
	if m.requestsPruned > 0 || m.requestsProjected == 0 {
		out = append(out, pipeline.Metric{
			Name: "requests pruned", Value: float64(m.requestsPruned), Unit: "count",
		})
	}
	if m.requestsProjected > 0 {
		out = append(out, pipeline.Metric{
			Name:  "requests projected",
			Value: float64(m.requestsProjected),
			Unit:  "count",
			Note:  "observe mode — body unchanged",
		})
	}
	out = append(out,
		pipeline.Metric{Name: "tools removed", Value: float64(m.toolsRemoved), Unit: "count"},
		pipeline.Metric{Name: "bytes removed", Value: float64(m.bytesRemoved), Unit: "bytes"},
	)

	acted := m.requestsPruned + m.requestsProjected
	if acted > 0 {
		perReq := float64(m.bytesRemoved) / float64(acted)
		out = append(out, pipeline.Metric{
			Name: "bytes removed / request", Value: perReq, Unit: "bytes",
		})
		// Calibrate bytes -> tokens on the operator's own traffic instead of
		// bundling a tokenizer or assuming a constant. With no usage sample
		// yet, report zero rather than dividing by zero.
		if m.requestBytes > 0 && m.promptTokens > 0 {
			ratio := float64(m.promptTokens) / float64(m.requestBytes)
			out = append(out, pipeline.Metric{
				Name:  "tokens saved / request",
				Value: perReq * ratio,
				Unit:  "tokens",
				Note:  fmt.Sprintf("estimate, n=%d", m.requestsWithUsage),
			})
		} else {
			out = append(out, pipeline.Metric{
				Name:  "tokens saved / request",
				Value: 0,
				Unit:  "tokens",
				Note:  "no usage sample yet",
			})
		}
	}

	// Per-tool attribution, sorted by count then name so the readout is
	// stable across calls and the biggest contributors come first.
	type kv struct {
		name string
		n    uint64
	}
	tools := make([]kv, 0, len(m.perTool))
	for k, v := range m.perTool {
		tools = append(tools, kv{k, v})
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].n != tools[j].n {
			return tools[i].n > tools[j].n
		}
		return tools[i].name < tools[j].name
	})
	for _, t := range tools {
		out = append(out, pipeline.Metric{
			Name: "removed: " + t.name, Value: float64(t.n), Unit: "count",
		})
	}
	return out
}
