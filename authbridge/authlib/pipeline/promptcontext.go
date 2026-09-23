package pipeline

import "time"

// PromptContextOf is how full the session's context is, in prompt tokens: the MAIN AGENT's latest
// request, ignoring the subagents and one-shot calls interleaved with it.
//
// Zero when nothing can be said, which the gauge renders as an em dash rather than as an empty
// track — "not known" and "barely used" are different answers and the column has to keep them
// apart.
//
// A SESSION IS THREE KINDS OF CALLER, not one thread: Claude Code puts all of them under the same
// X-Claude-Code-Session-Id. One session's own events, in order, show what that does to a column
// that follows whoever spoke last — prompt tokens per response:
//
//	msgs     1491     3     3  1494     3     3  1497     2  1500  1503  1506     2  1509
//	context  830k  282k  282k  835k  282k  282k  837k    6k  840k  844k  848k    7k  851k
//
// It swung 83% to 0.7% between adjacent turns. So the column classifies, and two facts do it —
// neither of them sufficient alone:
//
//	                tool manifest   agentRole
//	main agent      present         main       <- the conversation, which is what this file wants
//	subagent        present         subagent   <- Task-spawned, and carries its own manifest
//	one-shot        ABSENT          main       <- monitor / classifier / title call
//
// THE MANIFEST EXCLUDES THE ONE-SHOTS. A request carrying no tools is a single completion rather
// than an agentic turn, and across 115 responses in three live sessions the split was total:
//
//	carries tools    73 responses   63-1572 messages   94k-897k context
//	no tools         42 responses     2-3   messages  6.5k-295k context
//
// Note the second row's ceiling, because it rules out the two cheaper filters. One of those calls
// carried 295k and another 421,220 against a true context of 998,334, so no size threshold
// separates them — and they declare themselves MAIN, since the CLI issues them rather than a
// subagent, so the role does not exclude them either.
//
// THE ROLE EXCLUDES THE SUBAGENTS, which the message count only approximated. Claude Code states
// the role outright (see InferenceExtension.AgentRole) and a compaction cannot change it,
// whereas subagent turns reached 177 and 186 messages against main threads at 188 and 288, with
// their contexts overlapping too — 198,899 against 217,121. The count separates the populations it
// happens to be ordered on, and only while it is.
//
// AMONG WHAT IS LEFT, THE LATEST WINS, by timestamp. That is safe only because both filters are
// exact rather than statistical: there is no window, so a main thread that goes SILENT while a
// subagent runs is never aged out, and no amount of subagent traffic is a candidate to fill one.
// Measured across 555 main-agent turns in six sessions the series steps upward and drops exactly
// once — at the compaction — so the column tracks the conversation, and follows it when it
// restarts on the next turn rather than on the next five hundred.
//
// WITH NO ROLE STATED — a proxy older than the field — the rule falls back to the most messages,
// ties to the latest: what this column did before, and the best an unstated stream supports. It
// pays the cost the role was added to remove. A compaction restarts the conversation at a low
// message count while the pre-compaction request stays retained with 2,468 of them, so the gauge
// keeps showing the old context, for the rest of the session if that request is never evicted. A
// stale figure still beats one that flips to a subagent's, and a window would not fix it — it
// would reintroduce the silence problem. The fallback, PromptContextFold.msgs and messageCount can
// all be deleted once the supported proxy floor publishes agentRole.
//
// A SESSION IS EITHER STATED OR IT IS NOT, and one stated turn settles it. The first one takes the
// column outright, however much longer an unstated predecessor was, because a figure chosen by a
// rule that cannot see subagents is not evidence about the conversation — and taking it on the
// spot is what makes an upgraded proxy's answer arrive on the next event. After that, unstated rows
// are not candidates: a session whose proxy states roles has no reason to produce one, and
// trusting it would hand the column to whatever sent it.
//
// THE PROMPT SIDE ONLY. PromptTokens is input + cache-read + cache-write; output is left out.
// Measured on the same sessions it is 0.003%-2.2% of the prompt, and 0.2% on the conversations
// near the limit — a fifth of one eighth-block at 1M, so counting it would change no pixel.
//
// Read off the RESPONSE because the provider is the only party that tokenizes — the same reason
// tokensCell looks forward from a request row to its pair.
//
// WHY THIS COMES FROM abctl'S OWN CACHE and not from the session summary: the summary carries no
// per-request field, so there is nothing to read. abctl subscribes to /v1/events unfiltered and
// appends every event under its session id, so any session with traffic since it attached has a
// conversation here.
//
// THE TIMELINE ANSWERS THROUGH THE COUNTS AND THE ROLE. abctl asks for `view=summary` on every
// timeline fetch, tail and page alike, and that projection drops the two SLICES this rule used to
// read — so summarizeEvent records their lengths first and toolCount/messageCount read either
// shape. Without them a delivered row cannot be read at all: measured on one live session's
// 200-event window, 41 of 62 inference responses carry a manifest unprojected and 0 of 62 do
// projected. agentRole needs none of that machinery: it is a scalar, and summarizeEvent copies the
// extension struct whole before nilling the slices, so it arrives on a projected row as-is.
//
// AND THE FIGURE OUTLIVES THE EVENTS IT WAS READ FROM, deliberately — see rebaseSessionContext. Two
// cases need that: a proxy built between this column and the counts projects without stating them,
// and the picker releases a live session's events for memory (keys.go) without learning anything
// about the session.
//
// A whole-slice fold, for callers with no running state to keep — the tests, and any future
// one-shot reader. The sessions pane goes through sessionContextFor instead.
func PromptContextOf(events []SessionEvent) int {
	var f PromptContextFold
	f.AddAll(events)
	return f.tokens
}

// PromptContextFold is the running answer over the events folded so far: the winning figure and
// the message count that won it, plus how many events have been folded in.
//
// A RUNNING answer rather than a rescan, because the caller's shape demands it: the sessions row
// loop asks for every session, rebuildSessionsTable runs on every streamed event, and retention is
// unbounded, so a full scan per call is O(events) per session per event on the DEFAULT pane. A
// rescan measured 6.1ms and 3.49MB per call at 100k events — 60ms and 35MB for ten such sessions on
// one arriving event. TestSessionContextFor_DoesNotRescanTheFoldedPrefix pins that it does not
// happen; BenchmarkSessionContextPerEvent says what it costs.
//
// A REMEMBERED MAXIMUM, not a cache of a pure function over m.events — and the difference is
// load-bearing, not a convenience. The events abctl holds for a session can stop carrying the
// evidence the figure was read from, and a cache would be invalidated by that and come back empty.
// Three ways it happens: a proxy that projects without stating the counts (the version window
// between abctl's CONTEXT column and InferenceExtension.MessageCount), the picker
// releasing a live session's events for memory, and server-side FIFO eviction dropping the turn the
// figure came from. What it costs is stated with the compaction trade-off above — a figure this
// holds is the largest conversation abctl has SEEN for the session, which may be larger than
// anything it still holds.
type PromptContextFold struct {
	n      int // events folded so far
	tokens int
	// msgs is the FALLBACK rule's comparator and is not read once stated is true. It goes with
	// messageCount when the unstated arm does.
	msgs int
	// stated says a role-declaring candidate has been folded for this session, which is what
	// decides WHICH rule runs — latest-wins against most-messages. It is a property of the
	// session's traffic rather than of one event, which is why it lives here: an unstated row
	// arriving after a stated one must be skipped, and nothing in that row says so.
	stated bool
	// at is WHEN the winning response arrived, and it exists because of the rebase. Ties go to
	// the latest, which a single forward fold expresses as arrival order — but a rebase folds
	// new events on top of a winner that is no longer in the slice, so arrival order says
	// nothing about which of the two came first. Without this, opening an OLDER turn of equal
	// length replaced a newer remembered figure (reproduced at 445k against a true 500k).
	at time.Time
}

// AddAll is the existing loop body, folding a slice of events into f, in order.
//
// AddAll advances n; Add does not: n is a cursor into a caller's slice, and a per-event caller has
// no slice. The store leaves n at zero forever.
func (f *PromptContextFold) AddAll(events []SessionEvent) {
	for i := range events {
		f.Add(&events[i])
	}
	f.n += len(events)
}

// Add folds one event into f.
//
// Forward, and the later turn wins — by TIMESTAMP, not by arrival order. Timestamp is the SOLE
// comparator where the role is stated and the tie-break where it is not, so `at` carries both
// rules. Arrival order says nothing once a rebase folds new events onto a winner that is no longer
// in the slice, so an older turn would otherwise take it.
//
// At and not Seq, though a reviewer asked for Seq: the store's counter restarts at 1 when a session
// is evicted and re-created under the same id (authlib/session/store.go), so Seq cannot order across
// that boundary and wall-clock time can. Same reason a paging client sorts pages by At.
//
// `!Before` rather than `After`, so two candidates sharing a timestamp still resolve by arrival
// order the way a pure fold did.
func (f *PromptContextFold) Add(e *SessionEvent) {
	// RESPONSES ONLY, stated rather than relied on. The token counts arrive on the response
	// pass, so a request snapshot carries zeroes and would be dropped by the PromptTokens
	// check below anyway — but that is an accident of when SnapshotInference copies, not
	// something this loop said. Checking the phase makes the doc above load-bearing and
	// halves the candidates.
	if e.Phase != SessionResponse || e.Inference == nil {
		return
	}
	// The tool manifest is the filter: no tools means a one-shot completion, read through
	// toolCount so a projected event answers too.
	//
	// THE REQUEST SIDE IS NOT CONSULTED, because no copy on the way here can change a
	// manifest's LENGTH — and enumerating the copies is the argument. There are three:
	// SnapshotInference is `c := *ext` and shares the array; session.Interner CLONES it
	// per event before interning the descriptions and schemas in place, precisely because
	// the response-phase event aliases the same array (authlib/session/intern.go), and a
	// clone preserves length; summarizeEvent drops it after recording that length as
	// ToolCount. So a request and its response cannot disagree about whether a manifest
	// was there, pairing them would be dead code, and the map that needs is what made
	// this function allocate on every call.
	if toolCount(e.Inference) == 0 {
		return
	}
	n := PromptTokens(e.Inference)
	if n <= 0 {
		return
	}
	// The role is the second filter, and the three arms below are the whole rule. See the
	// doc comment above for why a subagent cannot be filtered by size or by message count,
	// and why one stated turn switches the session's rule for good.
	switch role := e.Inference.AgentRole; {
	case role == AgentRoleSubagent:
		return
	case role != "" && !f.stated:
		f.tokens, f.msgs, f.at, f.stated = n, messageCount(e.Inference), e.At, true
	case role != "":
		if !e.At.Before(f.at) {
			f.tokens, f.at = n, e.At
		}
	case f.stated:
		return
	default:
		if msgs := messageCount(e.Inference); msgs > f.msgs ||
			(msgs == f.msgs && !e.At.Before(f.at)) {
			f.tokens, f.msgs, f.at = n, msgs, e.At
		}
	}
}

// Tokens is the winning figure folded so far.
func (f PromptContextFold) Tokens() int { return f.tokens }

// Folded is how many events have been folded in — abctl's slice-length check reads this to decide
// whether a caller's slice has grown, shrunk, or is a wholesale replacement.
func (f PromptContextFold) Folded() int { return f.n }

// ResetFolded zeroes the slice cursor while KEEPING the figure, for a caller whose slice was
// replaced rather than appended to. See abctl's rebaseSessionContext: dropping the figure there
// blanks a live session's gauge, because a view=summary timeline may carry no candidate at all.
func (f *PromptContextFold) ResetFolded() { f.n = 0 }

// toolCount and messageCount answer the two questions this file asks of a conversation, on either
// shape the API delivers.
//
// A PROJECTED EVENT CARRIES COUNTS INSTEAD OF SLICES. sessionapi.summarizeEvent nils Messages and
// Tools — they are 99.5% of an event — and records their lengths first, because those two lengths
// are the whole of what this rule needs. len() first, then the count, because zero on the count
// means "not stated" rather than "none": an old proxy ignores `view` and returns full events, and a
// proxy built between the CONTEXT column and the counts projects without setting them.
//
// That last case is why the gauge remembers its own figure (PromptContextFold) rather than trusting
// whatever it currently holds: against such a proxy neither source answers, and a remembered figure
// from the stream is all there is.
func toolCount(inf *InferenceExtension) int {
	if n := len(inf.Tools); n > 0 {
		return n
	}
	return inf.ToolCount
}

func messageCount(inf *InferenceExtension) int {
	if n := len(inf.Messages); n > 0 {
		return n
	}
	return inf.MessageCount
}
