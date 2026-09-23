package pipeline

// Fixtures for the prompt-context rule. DELIBERATELY A SECOND COPY of the set in
// cmd/abctl/tui/sessions_context_test.go, not a shared helper package.
//
// authbridge has no exported test-helper package anywhere, and introducing the first one to
// avoid copying ~94 lines would cost ~450 lines of churn across 80 call sites. Drift between
// the copies is loud or harmless, never silently wrong: a manifest that goes missing makes the
// fold return 0 and fails every test that reads it, while 27-tools-versus-1 changes nothing
// because the rule only asks whether a manifest is non-empty.
//
// The tui copy also has a job this one does not: it states NO role, so the tests over there
// keep exercising the unstated fallback rule on purpose.

import (
	"fmt"
	"time"
)

// toolsOf builds a manifest of n tools. Only its LENGTH matters to PromptContextOf: a request that
// carries any tools is an agentic conversation, one that carries none is a one-shot completion.
func toolsOf(n int) []InferenceTool {
	out := make([]InferenceTool, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, InferenceTool{Name: fmt.Sprintf("Tool%02d", i)})
	}
	return out
}

// exchange is a request/response pair as the store records one: the manifest and the message count
// on both sides, the token counts on the response, since the provider is the only party that
// tokenizes.
func exchange(id string, at time.Time, msgs, ntools, input, cacheRead int) []SessionEvent {
	inf := func() *InferenceExtension {
		return &InferenceExtension{
			Model:    "claude-opus-5",
			Messages: make([]InferenceMessage, msgs),
			Tools:    toolsOf(ntools),
		}
	}
	respInf := inf()
	respInf.InputTokens, respInf.CacheReadTokens = input, cacheRead
	return []SessionEvent{
		{At: at, RequestID: id, Phase: SessionRequest,
			Direction: Outbound, Inference: inf()},
		{At: at.Add(time.Second), RequestID: id, Phase: SessionResponse,
			Direction: Outbound, Inference: respInf},
	}
}

// conversation is an agentic turn: 27 tools, as every main-thread request measured carried.
func conversation(id string, at time.Time, msgs, context int) []SessionEvent {
	return exchange(id, at, msgs, 27, 300, context-300)
}

// oneShot is a title / quota / summary call: no tools, three messages, and — the part that
// matters — a context that can be large.
func oneShot(id string, at time.Time, context int) []SessionEvent {
	return exchange(id, at, 3, 0, 300, context-300)
}

// roled states the caller's role on every event of a turn, which is what a proxy that reads
// Claude Code's billing-header line publishes.
//
// COPIES FIRST, unlike the tui original, which mutated its argument and returned it. That reads
// as pure and is not: `base := conversation(...)` followed by `subagent(base)` would restamp
// base too. Safe there only because every caller happens to pass a fresh turn.
func roled(evs []SessionEvent, role AgentRole) []SessionEvent {
	out := make([]SessionEvent, len(evs))
	for i := range evs {
		out[i] = evs[i]
		if evs[i].Inference != nil {
			inf := *evs[i].Inference
			inf.AgentRole = role
			out[i].Inference = &inf
		}
	}
	return out
}

// mainAgent is a conversation turn whose request declared itself the interactive thread.
func mainAgent(id string, at time.Time, msgs, context int) []SessionEvent {
	return roled(conversation(id, at, msgs, context), AgentRoleMain)
}

// subagent is a Task-spawned agent's turn. It carries its OWN tool manifest, which is why the
// manifest alone cannot filter it out — measured at 11 tools against the main thread's 27.
func subagent(id string, at time.Time, msgs, context int) []SessionEvent {
	return roled(conversation(id, at, msgs, context), AgentRoleSubagent)
}
