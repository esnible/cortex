package pipeline

import (
	"context"
	"testing"
)

// finisherCountPlugin counts OnFinish calls so a test can tell which
// pipeline's plugins a RunFinish reached.
type finisherCountPlugin struct {
	holderTestPlugin
	finishes int
}

func (f *finisherCountPlugin) OnFinish(_ context.Context, _ *Context) {
	f.finishes++
}

// TestHolder_ReloadMidRequestKeepsPhasesOnOnePipeline: a reload between a
// request's Run and its RunFinish must not move the request onto the new
// pipeline. pctx.dispatched holds indexes into the OLD pipeline's plugin
// slice; RunFinish on the NEW pipeline indexes its own slice with them —
// which panics outright when the new chain is shorter, and otherwise runs
// finish/response hooks on plugins that never saw OnRequest (#1561).
// CONNECT tunnels and SSE streams hold requests open for minutes, so any
// reload that shrinks the chain in that window crashes the sidecar.
func TestHolder_ReloadMidRequestKeepsPhasesOnOnePipeline(t *testing.T) {
	a := &finisherCountPlugin{}
	b := &finisherCountPlugin{}
	c := &finisherCountPlugin{}
	old := mustPipeline(t, a, b, c)

	h := NewHolder(old)

	// Request runs on the 3-plugin chain.
	pctx := &Context{Direction: Outbound}
	if act := h.Run(context.Background(), pctx); act.Type != Continue {
		t.Fatalf("Run: %v", act.Type)
	}

	// Reload swaps in a 1-plugin chain while the request is open.
	h.Store(mustPipeline(t, &finisherCountPlugin{}))

	// Response and finish must land on the OLD pipeline's plugins. Today
	// this panics (index out of range) or finishes the wrong plugins.
	h.RunResponse(context.Background(), pctx)
	h.RunFinish(context.Background(), pctx, Outcome{StatusCode: 200})

	if a.finishes+b.finishes+c.finishes != 3 {
		t.Errorf("OnFinish counts = %d,%d,%d, want 1 each: the request must finish on the plugins that saw it",
			a.finishes, b.finishes, c.finishes)
	}
	if a.respCount.Load()+b.respCount.Load()+c.respCount.Load() != 3 {
		t.Errorf("OnResponse counts = %d,%d,%d, want 1 each",
			a.respCount.Load(), b.respCount.Load(), c.respCount.Load())
	}
}
