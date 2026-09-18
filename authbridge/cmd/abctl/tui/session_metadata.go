package tui

// SessionMetadata is what a coding agent knows about one of its own sessions that
// Cortex does not.
//
// Cortex buckets traffic by session id and nothing more: `/v1/sessions` returns a
// UUID, and a UUID does not tell an operator which session it is. The agent that
// produced the traffic already knows — Claude Code writes a per-session transcript
// carrying an AI-generated title and the directory it ran in — so this is the shape
// that carries those facts back, keyed by the same id Cortex uses.
//
// Every field is optional. A harvester that cannot determine one must leave it empty
// rather than invent a value, and a consumer must render an entry that has only some
// of them: the fields come from an agent's own on-disk layout, which is not a
// contract anyone here controls.
//
// The tags here are the package's first `json:` tags — every other tagged type in
// tui is YAML, because those are abctl's own settings. This one is written as JSON to
// ~/.cortex/session-metadata.json. Key names stay camelCase to match settings.go's
// sortColumn / sortDesc rather than introducing a second convention.
type SessionMetadata struct {
	// Title is a human-readable name for the session. From Claude Code this is the
	// model-generated title when the transcript carries one, and otherwise the
	// working directory it ran in — so it may well be a path rather than a phrase,
	// and a consumer that assumes prose will be wrong most of the time (measured: 2
	// of 109 local sessions had a real title).
	Title string `json:"title,omitempty"`
	// AgentType names the agent that produced the session, e.g. "Claude Code". A
	// plain display string, not an enum: the set of agents Cortex fronts is open,
	// and a consumer showing this has no decision to make on it.
	AgentType string `json:"agentType,omitempty"`
	// AgentConfigDir is the directory the metadata was harvested from — the parent
	// of the agent's own per-session layout. Recorded per entry rather than once per
	// file so a future harvester can merge two agents, or two config dirs of one
	// agent, into the same map without the entries becoming ambiguous.
	AgentConfigDir string `json:"agentConfigDir,omitempty"`
	// LogFile is the transcript this entry was read from, as a path. The one field
	// that lets a reader go and check: a title that looks wrong is answerable by
	// opening the file it came from.
	LogFile string `json:"logFile,omitempty"`
}
