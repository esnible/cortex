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
// WITH NO ROLE STATED the rule falls back to the most messages, ties to the latest, and then to the
// larger context: what this column did before, plus a final tie-break for the one pair the
// sequential form left to arrival order (see better). It pays the cost the role was added to remove.
// A compaction restarts the conversation at a low message count while the pre-compaction request
// stays retained with 2,468 of them, so the gauge keeps showing the old context, for the rest of the
// session if that request is never evicted. A stale figure still beats one that flips to a
// subagent's, and a window would not fix it — it would reintroduce the silence problem.
//
// AND THAT FALLBACK IS PERMANENT, not a version-skew relic to be deleted. An earlier version of
// this comment said the fallback, PromptContextFold.msgs and messageCount could all go "once the
// supported proxy floor publishes agentRole" — which reads the field as a proxy capability. It is
// not one: inferenceparser.agentRole returns "" for a request with no system message at all
// (/v1/completions), for one whose first system line lacks the required billing-header prefix, and
// on an unparseable body, so the answer depends on the CLIENT. Its own doc puts it plainly —
// "every client that is not Claude Code". So there is no proxy version at which the unstated arm
// stops being reachable, and anything resting on the assumption that there is should stop.
//
// A SESSION IS EITHER STATED OR IT IS NOT, and any stated turn outranks every unstated one,
// however much longer the unstated one was: a figure chosen by a rule that cannot see subagents is
// not evidence about the conversation. So an upgraded proxy's first stated turn takes the column on
// the event it arrives on, and an unstated row after it never takes it back — a session whose proxy
// states roles has no reason to produce one, and trusting it would hand the column to whatever sent
// it. Expressed as the top rank of a total order rather than as a one-way latch, so that which of
// the two arrived first cannot matter (see better).
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
	// msgs is the FALLBACK rule's LEADING comparator, and the stated arm's final determinism
	// filler. PERMANENT, not transitional: stated depends on the client rather than on the proxy
	// version (see PromptContextOf), so the unstated arm is a live path and this field is published
	// on the wire as PromptContext.Msgs.
	msgs int
	// stated says the WINNING candidate declared a role, which is what decides which rule ranked
	// it — latest-wins against most-messages. It is carried on the fold rather than recomputed
	// because it is half of what better() compares: an unstated row must lose to a stated one
	// whichever order the two arrive in, and nothing in the unstated row says so.
	stated bool
	// at is WHEN the winning response arrived, and it exists because of the rebase. It is the
	// stated arm's primary comparator and the unstated arm's tie-break on message count — but a
	// rebase folds new events on top of a winner that is no longer in the slice, so arrival order
	// says nothing about which of the two came first and the timestamp has to be kept. Without
	// this, opening an OLDER turn of equal length replaced a newer remembered figure (reproduced
	// at 445k against a true 500k).
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

// candidate is one event's claim on the column: extracted, then compared.
type candidate struct {
	tokens int
	msgs   int
	at     time.Time
	stated bool
}

// candidateOf extracts an event's claim, or a zero candidate if it makes none.
func candidateOf(e *SessionEvent) candidate {
	// RESPONSES ONLY, stated rather than relied on. The token counts arrive on the response
	// pass, so a request snapshot carries zeroes and would be dropped by the PromptTokens
	// check below anyway — but that is an accident of when SnapshotInference copies, not
	// something this rule said. Checking the phase makes the doc above load-bearing and
	// halves the candidates.
	if e.Phase != SessionResponse || e.Inference == nil {
		return candidate{}
	}
	// The tool manifest is the first filter: no tools means a one-shot completion, read through
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
		return candidate{}
	}
	// The role is the second filter. See the doc comment above for why a subagent cannot be
	// filtered by size or by message count instead.
	if e.Inference.AgentRole == AgentRoleSubagent {
		return candidate{}
	}
	n := PromptTokens(e.Inference)
	if n <= 0 {
		return candidate{}
	}
	return candidate{
		tokens: n,
		msgs:   messageCount(e.Inference),
		at:     e.At,
		stated: e.Inference.AgentRole != "",
	}
}

// better reports whether a outranks b under THE rule, expressed as a TOTAL ORDER rather than as
// the sequential latch this replaced.
//
//	stated ≻ unstated                      one stated turn settles it, however much longer an
//	                                       unstated predecessor was — a figure chosen by a rule
//	                                       that cannot see subagents is not evidence
//	  within stated:    (at, tokens, msgs)
//	  within unstated:  (msgs, at, tokens)
//
// A max over a total order is commutative AND associative, which is what makes the fold a
// monoid: replay order cannot change the answer, so a future restore-then-continue needs no
// ordering guarantee.
//
// EACH ARM'S LAST COMPARATOR IS DETERMINISM FILLER and nothing else — msgs where the role is
// stated, tokens where it is not. Both arms APPEND to the rule they inherited rather than replace
// a comparator in it, which is the whole safety property of this reformulation: the leading
// comparators are the rule, and the trailing one only settles pairs the rule cannot separate, so
// the STORED struct comes out fully determined. Putting tokens AHEAD of at in the unstated arm was
// tried and is wrong: a view=summary timeline that projects without MessageCount ties every
// candidate at msgs == 0, so the second comparator decides the entire answer there, and
// largest-context-wins pins a pre-compaction figure — the exact stale-figure failure this column
// exists to avoid.
//
// FULLY-TIED CANDIDATES KEEP THE INCUMBENT, which is not a bug: better returns false both ways for
// two candidates equal on every field it compares, so Add does not replace, and two such candidates
// are interchangeable for every purpose this package has.
//
// At and not Seq, though a reviewer asked for Seq: the store's counter restarts at 1 when a session
// is evicted and re-created under the same id (authlib/session/store.go), so Seq cannot order across
// that boundary and wall-clock time can. Same reason a paging client sorts pages by At.
//
// THE PREDECESSOR'S ONE DISAGREEMENT, for the record: it compared with `!Before`, so a later
// ARRIVAL took a tie among unstated turns that agreed on both message count and timestamp — 100k or
// 200k for the same session depending only on fold order. tokens now settles that pair, AFTER at
// rather than before it, so `at` keeps the role it had and the delta is confined to genuinely
// identical (msgs, at) pairs. TestPromptContextFold_ExactTimestampTiesAreDeterministic pins it.
func better(a, b candidate) bool {
	if a.stated != b.stated {
		return a.stated
	}
	if a.stated {
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		if a.tokens != b.tokens {
			return a.tokens > b.tokens
		}
		return a.msgs > b.msgs
	}
	if a.msgs != b.msgs {
		return a.msgs > b.msgs
	}
	if !a.at.Equal(b.at) {
		return a.at.After(b.at)
	}
	return a.tokens > b.tokens
}

// Add folds one event into f, keeping the MAXIMUM under better's total order.
//
// A max rather than a sequential scan with a latch, and that is the whole of this function's
// claim: arrival order says nothing once a rebase folds new events onto a winner that is no longer
// in the slice, and a restore from disk has no order to offer at all.
func (f *PromptContextFold) Add(e *SessionEvent) {
	c := candidateOf(e)
	if c.tokens == 0 {
		return
	}
	// The zero fold is the monoid's identity, stated explicitly even though better() already
	// handles it: every candidate reaching here beats the zero fold on SOME comparator, whatever
	// its role, so this disjunct changes no outcome today. Following the arms — a stated candidate
	// wins on stated; an unstated one wins on msgs, or where its own count is 0 falls through and
	// wins on at, since any representable timestamp is after time.Time{}; and if its At is unset
	// too it wins on tokens, which the guard above proved non-zero. It is kept so that a future
	// reordering of better()'s comparators cannot quietly make an empty fold a legitimate operand:
	// the identity is a property of the monoid, not an accident of which field is compared last.
	if f.tokens == 0 || better(c, f.current()) {
		f.tokens, f.msgs, f.at, f.stated = c.tokens, c.msgs, c.at, c.stated
	}
}

// current is the running answer as a candidate, so one order compares both sides of the max.
func (f PromptContextFold) current() candidate {
	return candidate{tokens: f.tokens, msgs: f.msgs, at: f.at, stated: f.stated}
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

// PromptContext is the fold's PUBLISHABLE state: enough to merge two of them, which a client
// holding its own figure must do, and which a future restore-then-continue would do.
//
// LOSSLESS WITH RESPECT TO THE ORDERING, carrying every field better() compares — so the published
// order IS the fold's order rather than a coarsening of it, and MergePromptContext is the same max
// over the same comparator. Because those four fields are now the WHOLE struct, better() is a total
// order on this type, which it was not while a comparator was missing from it.
//
// NOT A WHOLE FOLD, and this is not a claim that a restore path exists: n is deliberately
// unpublished (it is a cursor into a caller's slice, not part of the answer) and there is no
// PromptContext-to-fold constructor. What losslessness buys is that two published figures can be
// COMPARED exactly, which is what a restore would need of them — not that one can be resumed from.
//
// MSGS IS PUBLISHED, AND THE REASON IT WAS NOT IS FALSE. An earlier draft dropped it as the
// FALLBACK rule's comparator, "slated for deletion once the supported proxy floor publishes
// agentRole". Stated depends on the CLIENT, not on the proxy version:
// inferenceparser.agentRole returns "" for a request with no system message at all
// (/v1/completions), for one whose first system line lacks the required billing-header prefix,
// and on an unparseable body — "every client that is not Claude Code", as its own doc says. So an
// unstated figure is a live state on a CURRENT proxy rather than a version-skew relic, the
// unstated-versus-unstated arm is a real path, and msgs is permanent.
//
// Omitting it therefore bought nothing and cost exactness. It left two combining operations over
// two DIFFERENT total orders which had to be kept in agreement by hand — the drift risk this
// design rejects everywhere else, and one that had already produced two reasoning errors here.
// The cost of carrying it is one int on the wire.
type PromptContext struct {
	Tokens int       `json:"tokens"`
	Stated bool      `json:"stated"` // which rule ranked it; stated always beats unstated
	At     time.Time `json:"at"`     // the winning turn's arrival, for latest-wins
	Msgs   int       `json:"msgs"`   // the unstated rule's LEADING comparator; see better
}

// candidate projects a published figure back onto the fold's comparison type, and is the whole
// mechanism by which the wire order and the fold order cannot disagree: there is ONE ordering,
// better(), and both are views of it. Total in both directions because PromptContext is lossless.
func (p *PromptContext) candidate() candidate {
	return candidate{tokens: p.Tokens, msgs: p.Msgs, at: p.At, stated: p.Stated}
}

// Publish projects the fold for the wire, or nil when nothing can be said.
//
// NIL RATHER THAN A ZERO STRUCT, so the field is absent under omitempty. A session with only
// one-shot calls has no conversation to measure and is indistinguishable from one nobody has
// observed; both must render as an em dash rather than as a figure. Same standing rule
// SessionSummary.CostMicros states for its own omitempty.
func (f PromptContextFold) Publish() *PromptContext {
	if f.tokens == 0 {
		return nil
	}
	return &PromptContext{Tokens: f.tokens, Stated: f.stated, At: f.at, Msgs: f.msgs}
}

// MergePromptContext combines two published figures, nil meaning "nothing known".
//
// THE SAME ORDER AS THE FOLD'S, and it DEFERS to better() rather than restating it. PromptContext
// is lossless (see there), so there is exactly one total order here, not two that have to be kept
// in agreement by hand:
//
//	stated ≻ unstated             a figure from a rule that cannot see subagents is not
//	                              evidence, at any size
//	  within stated:    (At, Tokens, Msgs)
//	  within unstated:  (Msgs, At, Tokens)
//
// A HAND-WRITTEN SECOND COPY WAS THE EARLIER SHAPE, and it is worth recording why it went. With
// Msgs unpublished this function had a single arm serving both classes, documented as "(At, Tokens)
// either way" — true only because the comparator that distinguishes the two arms had been dropped
// from the wire. Publishing Msgs makes the orders genuinely differ, which would mean maintaining
// better()'s two arms twice; deferring instead makes disagreement impossible by construction.
//
// A max over a total order, so MergePromptContext is commutative, associative, and has nil as its
// identity. That is what lets a client merge the server's figure with its own and need no version
// detection: an old proxy sends nothing, and nothing is a valid operand.
//
// FULLY-TIED OPERANDS KEEP a, exactly as the fold keeps its incumbent: better() is false both ways
// for two figures equal on every field it compares, and two such figures are interchangeable for
// every purpose this package has.
//
// SPELLED OUT IN FULL, and a free function rather than a method. Bare `Merge` was rejected: this
// package also owns pipelines, extensions, sessions, events and snapshots, and `pipeline.Merge(a,
// b)` reads at the call site as "merge two pipelines". A method — `server.Merge(local)` — was
// rejected too, and for a sharper reason than symmetry: nil is the identity of this monoid, so a
// method would have to be callable on a NIL RECEIVER to accept the operand an old proxy actually
// sends. That works in Go and it is a footgun, because nothing at the call site warns the next
// reader that the receiver may be nil; two plainly nilable arguments say so in the signature.
func MergePromptContext(a, b *PromptContext) *PromptContext {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case better(b.candidate(), a.candidate()):
		return b
	default:
		return a
	}
}

// TokensMergedWith is f's figure merged with a PUBLISHED one: the same figure
// MergePromptContext(p, f.Publish()) carries, or zero where that is nil, without publishing f.
//
// IT EXISTS FOR THE ALLOCATION, and the measurement is the whole justification — this file does not
// add a second entry point for tidiness. abctl's sessions row loop asks EVERY visible session for
// its gauge on every rebuild, and a rebuild is one streamed event or one poll, so anything per-row
// here is on the same hot path the fold itself exists to protect. Going through Publish() put a
// 48-byte *PromptContext on the heap per row per rebuild. On
// BenchmarkSessionContextPerEvent/folded/10000, ten sessions per iteration and one of them with a
// turn to fold:
//
//	184 ns/op    0 allocs/op   the int-returning path, before a server figure existed
//	356 ns/op   10 allocs/op   merged by publishing f — exactly one allocation per row
//	253 ns/op    0 allocs/op   as shipped, through this method
//
// Those first two were taken before the benchmark grew its second case, whose slice index costs the
// baseline about a nanosecond. That second case, folded+server/10000, is where every row carries a
// figure as a current proxy sends it: 342 ns/op and 0 allocs/op, so the comparison itself costs
// about 9ns a row and allocates nothing.
//
// THE LITERAL CANNOT STAY ON THE STACK, which is why this is a signature change rather than a
// compiler hint. Publish() inlines, but MergePromptContext does not — cost 110 against a budget of
// 80 — and its parameters flow to its result, so the escape analysis spills the published struct
// whatever the caller looks like. A fold is a VALUE, so comparing out of one allocates nothing.
//
// STILL ONE ORDERING. This defers to better() exactly as MergePromptContext does, so the two are
// views of the same total order rather than two implementations of it — the property this file
// protects everywhere, and the reason Msgs is on the wire at all. Only the identity handling is
// restated, two lines of it, and TestPromptContextFold_TokensMergedWithAgreesWithMergePromptContext
// pins the agreement over every combination of stated, unstated and absent.
//
// THE COMPARISON IS INVERTED relative to MergePromptContext, and saying so is cheaper than leaving
// the next reader to derive it: that function asks better(b, a) and keeps a — the SERVER figure, at
// abctl's call site — where this asks better(p, f.current()) and keeps the FOLD. Both return the
// same int, for a reason narrow enough to state outright: better() is false in both directions only
// for candidates equal on every field it compares, and it compares all four of PromptContext's, so a
// tie means the two token counts are equal as well and which operand is kept cannot show.
//
// SO THE EQUIVALENCE RESTS ON PromptContext STAYING LOSSLESS — the same invariant MergePromptContext's
// own doc asserts, load-bearing here for a second reason. A comparator added to better() and NOT
// published would make a tie with UNEQUAL tokens reachable, and these two would then disagree. The
// sweep test cannot catch that: it builds its folds one-to-one from PromptContext values, so an
// unpublished fold field is unconstructible in that fixture. Anything adding a field to better()
// owes this method a look.
//
// RETURNS THE FIGURE, not the winner, and that is not a shortcut: the only caller draws a gauge from
// an int, and handing back a *PromptContext would put the allocation straight back.
func (f PromptContextFold) TokensMergedWith(p *PromptContext) int {
	if p == nil {
		return f.tokens
	}
	// A zero fold is the monoid's identity here as everywhere: Publish() would return nil for it,
	// and nil is what MergePromptContext returns the other operand for.
	if f.tokens == 0 || better(p.candidate(), f.current()) {
		return p.Tokens
	}
	return f.tokens
}

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
