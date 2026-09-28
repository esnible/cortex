package session

import (
	"strings"
	"unicode"

	"github.com/rossoctl/cortex/core/capabilities"
	"github.com/rossoctl/cortex/core/pipeline"
)

const emptySessionTitle = "(empty session)"

// maxTitleLen caps a title, in RUNES. A user message is unbounded — the largest measured
// event carries ~190KB — and this is served on every /v1/sessions response, whose size is
// already the constraint that `?before=` exists for.
//
// NOT A DISPLAY-COLUMN BUDGET: 80 runes of CJK occupy 160 columns, so a client still has to
// truncate by width. Runes, not bytes, so a multi-byte title is not cut mid-character.
//
// Deliberately the same 80 as core/observe/claude.MaxTitleLen, and deliberately NOT that
// constant: that one is documented as the cap on a HARVESTED title and is exported so
// cmd/abctl's tests can hold a contract with the harvester. Importing it here would make the
// session store depend on the transcript harvester for a number, and would couple two caps
// that answer to different consumers. If they ever need to differ, they can.
const maxTitleLen = 80

// Title sources, best first. Within one rank the last match in event order wins.
const (
	rankRename    = 0
	rankUserQuery = 1
	rankUserMsg   = 2
	rankNone      = 3
)

const renamePrefix = "<command-name>/rename</command-name>"

// titleScanEvents caps how many of a session's NEWEST events sessionTitle examines, which is what
// makes ListSessions' lock hold bounded rather than proportional to the session.
//
// A CEILING, NOT A COPY. The call site holds the store's read lock against a writer that is Append
// on the proxy's request path, and without a bound the walk is O(events × message-content) with
// maxEvents unset by default and abctl polling every two seconds — ~5ms of lock hold on the repo's
// cited 5078-event session. Copying the events to walk them unlocked was the other option on the
// table and is worse: the copy is of exactly the large payload that makes this expensive, so it
// trades lock hold for an O(content) allocation on every poll, in a package that has just spent a
// lot of effort deleting that shape from the read path (see core/session/intern.go and
// core/sessionapi/snapshot.go).
//
// COSTS ALMOST NOTHING IN PRACTICE because of what the events contain: every inference request
// re-sends the whole conversation, so the newest event already holds every message of the session.
// A /rename from turn 3 of a 5000-turn session is still in the newest event and still found. What
// the cap can lose is a title that was only ever in an event now older than this window — an
// intent whose conversation has since been replaced wholesale, or a non-inference event kind. That
// is the same class of loss the store's own eviction already imposes, and the comment on the call
// site's Title field already tells clients this figure is recomputed and can change.
//
// 64 because the worst case per event is a full conversation's worth of message content, so the
// product is what matters rather than either factor; this keeps the bound near the cost of the
// benchmark shape that was measured (BenchmarkListSessions_Title, 300 events) rather than near the
// unbounded one.
const titleScanEvents = 64

// sessionTitle names a session from its own events: a /rename envelope if one is present,
// else a <user_query> block, else the last user message carrying text.
//
// SANITIZATION IS PART OF ACCEPTANCE, not a final flourish. A candidate that sanitizes to ""
// names nothing, and accepting one used to lock its rank and discard the real title behind it:
// sessionTitle([userEvent("the real ask"), userEvent("   ")]) answered "". The raw-content
// emptiness tests inside titleFrom cannot see this — whitespace and control runes are non-empty
// until sanitizeTitle folds them away — so the test has to happen after the fold.
//
// HERE RATHER THAN PER CANDIDATE, which is a cost decision: sanitizing every candidate as it is
// produced measured 326µs→651µs on BenchmarkListSessions_Title/user-text, because rank 2 is
// always beatable and so every user message of every event becomes a candidate. Sanitizing at
// the moment a candidate would REPLACE the incumbent runs it once per improvement — at most
// three times for a session, since rank only ever descends.
//
// BOUNDED BY titleScanEvents, which is what makes the call site's lock hold O(1) rather than
// O(session). See that constant.
func sessionTitle(events []pipeline.SessionEvent) string {
	if len(events) == 0 {
		return emptySessionTitle
	}
	stop := 0
	if len(events) > titleScanEvents {
		stop = len(events) - titleScanEvents
	}
	best, bestRank := "", rankNone
	for i := len(events) - 1; i >= stop; i-- {
		// Reverse, so the FIRST hit at a rank is the LAST one in event order — hence
		// `>=` rather than `>`: an earlier event must not displace it at equal rank.
		r, t := titleCandidate(&events[i])
		if t == "" || r >= bestRank {
			continue
		}
		// Fold BEFORE accepting, so a whitespace-only candidate does not claim the rank. It is
		// not merely that the title would be blank — claiming rank 0 would also break the walk
		// and hide a real /rename in an earlier event.
		//
		// REDUNDANT TODAY AND KEPT ANYWAY: titleCandidate now folds at its own acceptance points
		// (it has to — a blank candidate there hides a title in the SAME event), and the fold is
		// idempotent, so this re-folds an already-folded string of at most maxTitleLen runes. That
		// costs nothing measurable and keeps this loop's invariant true on its own terms rather
		// than by trusting its callee, which is what the blank-candidate bug cost twice.
		t = sanitizeTitle(t)
		if t == "" {
			continue
		}
		best, bestRank = t, r
		if bestRank == rankRename {
			break // nothing can outrank it
		}
	}
	if bestRank == rankNone {
		return ""
	}
	return best
}

// titleCandidate returns the best-ranked title this event's user messages offer.
//
// ONE REVERSE SCAN, NO RETRIES. The previous shape picked a candidate by quickRank and retried
// the whole scan whenever titleFrom refused the pick, which was quadratic in messages-per-event
// on exactly the reminder-bearing path this file added: measured 31.9µs at n=50 rising to 7.20ms
// at n=800, a clean 4x per doubling and ~1088x the cost of the same event with no demotions. It
// runs under the store's read lock, whose writer side is Append on the proxy's request path.
//
// Reverse, so the first acceptance at any rank is the LAST such message in event order — the
// rule stated at the top of this file. That replaces the old forward loop's `<=` tie-break,
// which was subtly wrong in the other direction: see the /rename case in #6 below.
//
// TWO CLASSES OF MESSAGE, because quickRank is only an upper bound and a bound is not a rank:
//
//   - quickRank says rank 0 or 1 (rare): the guess is SETTLED EAGERLY by calling titleFrom, and
//     whatever rank comes back is used. A guess of 0 or 1 that titleFrom demotes to 2 must not
//     be recorded as 0 or 1 — that is #4 (an unterminated <user_query> guesses 1 and settles at
//     2) and #5 (a demoted-but-non-empty pick discarded a better-ranked message in the same
//     event). Eager settling costs one titleFrom per rank-0/1 message, and those are rare
//     precisely because the tags are.
//   - quickRank says rank 2 (the majority): DEFERRED. Calling titleFrom here is what cost +70%,
//     so the scan only remembers the index and settles it at the end if nothing better appeared.
//     A deferred rank-2 message can only settle at 2 or at rankNone, never better — a message
//     with no <user_query> and no reminder-hidden /rename prefix has nothing for the strip to
//     uncover. That is the one direction of quickRank's bound that does hold, and it is the only
//     direction this relies on.
func titleCandidate(e *pipeline.SessionEvent) (int, string) {
	if e.Inference == nil {
		return rankNone, ""
	}
	msgs := e.Inference.Messages
	bestRank, bestTitle, bestIdx := rankNone, "", -1
	// deferredHead is the index one past the newest rank-2 guess not yet settled. The deferred
	// candidates are scanned lazily below, latest first, because a rank-2 guess can still settle
	// at rankNone — a message that is nothing but reminders guesses 2 and names nothing — and the
	// next one back must then get its turn. Storing a single index lost exactly that case.
	deferredHead := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != capabilities.RoleUser {
			continue
		}
		switch quickRank(msgs[i].Content) {
		case rankNone:
			continue
		case rankUserMsg:
			if deferredHead < 0 {
				deferredHead = i + 1
			}
			continue
		}
		// A rank-0 or rank-1 guess. Settle it now; titleFrom may demote it to 2 or refuse it.
		r, t := titleFrom(msgs[i].Content)
		if t == "" || r >= bestRank {
			continue
		}
		// REJECT A BLANK CANDIDATE BEFORE IT CLAIMS THE RANK. sessionTitle's own blank check cannot
		// cover this: it sees one answer per event, so a blank winner here does not fall through to
		// a real title in the SAME event — it discards the event. `<user_query>   </user_query>`
		// standing after a real ask used to title the session "" rather than the ask, and a blank
		// /rename arg did the same at rank 0.
		//
		// TESTS WITHOUT FOLDING, and that is the whole reason foldsBlank exists. Folding here
		// instead measured 326µs→635µs on BenchmarkListSessions_Title/user-text: one fold per
		// RETAINED EVENT rather than one per session, and sanitizeTitle is 2.8µs on a 500-byte
		// message (101 calls against 1, at maxEvents=100). The winner is folded once, by
		// sessionTitle, which is where the only affordable fold has always been.
		if foldsBlank(t) {
			continue
		}
		bestRank, bestTitle, bestIdx = r, t, i
		if bestRank == rankRename {
			return bestRank, bestTitle // nothing in this event can outrank it
		}
	}
	// Settle the deferred rank-2 candidates only if nothing better was found — and, at equal rank,
	// only if the deferred one is LATER in event order, per the last-match rule.
	//
	// THE INDEX COMPARISON IS NOT REDUNDANT, though the old comment here claimed the reverse scan
	// made it so. The scan visits messages newest-first, so the FIRST rank-2 guess it meets is the
	// newest one — but a rank-0/1 guess that titleFrom demotes to rank 2 can sit anywhere, including
	// after deferredHead, and then it is the later of the two. `["early plain prose", "what does
	// <user_query> mean here"]` is the whole bug: index 1 guesses rank 1, settles at 2, and is the
	// last user message — yet the deferred index 0 was returned unconditionally.
	//
	// AT MOST TWO PASSES, not one: messages between deferredHead and the end are visited only by
	// the first loop, but those below it can be visited by both. Still linear — two passes over a
	// prefix, no retries — which is the property that matters; the old "one pass total" claim was
	// simply wrong about which messages each loop touches.
	if deferredHead >= 0 && bestRank >= rankUserMsg {
		for i := deferredHead - 1; i >= 0; i-- {
			// No quickRank here. Re-classifying costs a second scan of every message, which is
			// what a first cut of this did and it showed: +45% on prose. titleFrom is the
			// authority anyway, and a message it names is a valid rank-2 candidate whatever the
			// guess would have been — a rank-0/1 message that reaches this point was already
			// settled and rejected by the loop above.
			if msgs[i].Role != capabilities.RoleUser {
				continue
			}
			r, t := titleFrom(msgs[i].Content)
			// foldsBlank, NOT sanitizeTitle — see the eager arm above for the 2x this avoids.
			if foldsBlank(t) {
				// Named nothing (nothing but reminders, or blank after the fold). Keep walking
				// back — an earlier message in this same event may still title it, which is the
				// within-event fall-through TestSessionTitle_DemotedPickFallsBackWithinEvent pins.
				continue
			}
			// UNREACHABLE TODAY AND KEPT DELIBERATELY: `r > bestRank` cannot fire, because this
			// loop only runs when bestRank >= rankUserMsg and titleFrom cannot return worse than
			// rankUserMsg once foldsBlank has rejected the blanks. Written as the full comparison
			// anyway so the rule is "better rank wins, then later index wins" in the code and not
			// only in this comment — a future arm that can settle at a worse rank would otherwise
			// silently take the wrong branch. Verified dead by mutation: dropping this disjunct
			// leaves every test passing.
			if r > bestRank || (r == bestRank && i < bestIdx) {
				break // the already-settled pick is better ranked, or equally ranked and later
			}
			return r, t
		}
	}
	return bestRank, bestTitle
}

// quickRank guesses a message's rank without stripping reminders, cheaply enough to run on
// every message of every event.
//
// A LOWER BOUND ON COST, NOT A RANK. It is reliable in ONE direction only: a message it calls
// rank 2 cannot settle better than 2, because it contains neither <user_query> nor a /rename
// prefix anywhere, and stripping reminders cannot introduce either. Callers may defer such a
// message and settle it later.
//
// IN THE OTHER DIRECTION IT OVER-PROMISES, and the comment here used to claim otherwise —
// "never worse" was false in two ways, both now covered by tests:
//
//   - rank 1 for an UNTERMINATED <user_query>: Contains finds the open tag, but between() needs
//     a closing tag and returns "", so titleFrom falls through to rank 2.
//   - rank 0 or 1 for a message whose payload is empty once extracted (`<command-args></command-args>`,
//     `<user_query> </user_query>`), which settles at rankNone.
//
// So a rank-0 or rank-1 guess MUST be settled by titleFrom before its rank is recorded.
//
// Contains for BOTH tags, not HasPrefix for the /rename envelope. HasPrefix systematically
// under-rated a reminder-prefixed /rename to rank 2, which is how a later genuine /rename lost
// to an earlier one inside the same event. The prefix test still lives in titleFrom, where the
// reminders have already been stripped and a prefix is the right question.
func quickRank(content string) int {
	if content == "" {
		return rankNone
	}
	if strings.HasPrefix(content, renamePrefix) {
		return rankRename
	}
	if strings.Contains(content, "<user_query>") {
		return rankUserQuery
	}
	// A REMINDER AT THE FRONT is the only thing that can displace a /rename envelope from the
	// front, which is why the HasPrefix above misses one and this exists. Without it a later
	// reminder-prefixed /rename lost to an earlier bare one inside the same event, against this
	// file's stated last-match-wins rule.
	//
	// BOTH TESTS ARE ANCHORED, and that is a cost decision as much as a correctness one. An
	// unanchored Contains for the envelope — 37 bytes, scanned over the whole message to answer
	// "no" for almost every message — cost +45% on 500-byte prose (325µs→472µs on a 300-turn
	// session), and gating it on an unanchored Contains for the reminder cost the same, because
	// the gate scans the whole message too. Anchoring the gate restores parity. `<` is not a
	// usable discriminator: ordinary prose carrying a code snippet has one, and an IndexByte('<')
	// fast path measured 906µs on exactly that fixture while looking like 248µs on a fixture
	// whose prose had no '<' at all.
	//
	// What this deliberately does NOT catch is an envelope behind PROSE rather than behind a
	// reminder ("as I said, <command-name>/rename</command-name>…"). titleFrom tests a prefix too,
	// so such a message never named a session at rank 0 under any version of this code; quickRank
	// guessing rank 2 for it agrees with what titleFrom would settle on.
	if strings.HasPrefix(content, reminderOpen) && strings.Contains(content, renamePrefix) {
		return rankRename
	}
	return rankUserMsg
}

// titleFrom ranks one user message and returns the title it offers.
//
// <system-reminder> blocks are excised first, before every rank arm rather than only the
// rank-2 one. All three arms need it and two are wrong without it: a reminder nested INSIDE a
// <user_query> would ride along into the title, and a <user_query> nested inside a REMINDER —
// one quoting an earlier turn is enough — would be mistaken for the real ask. The /rename arm
// tests a PREFIX, so a reminder in front of an envelope hides it entirely.
//
// THE STRIP IS NOT FREE, which is why titleCandidate does not call this on every message.
// Excising a tag that can sit anywhere means scanning the whole message and building a new
// string; calling it per message cost +70% on BenchmarkListSessions_Title/user-text, which walks
// ~45k messages of ~500 bytes. quickRank screens instead, and this settles the rank of the few
// messages whose guess is not reliable — see titleCandidate for which those are.
func titleFrom(content string) (int, string) {
	content = stripReminders(content)
	// A TRANSCRIPT ENVELOPE IS MACHINERY, NOT AN ASK, and it names nothing. Observed live: a
	// message opening with this tag titled a session `\", \"` — the fold reduced a wall of quoted
	// JSONL to its punctuation, which is both meaningless and unrecognisable as the session's
	// subject. Discarded rather than ranked, so the walk reaches a real title behind it, the same
	// way an argument-less /rename does.
	//
	// ANCHORED, and the anchor is the whole rule. Of the user messages in local transcripts that
	// mention this tag, the only one does so as PROSE — a bug report quoting it, which is a
	// perfectly good title — so an unanchored match would discard exactly the message a reader
	// wants. After stripReminders, so a reminder in front of an envelope does not hide it, and
	// after no trimming: leading whitespace before the tag is not a shape the harness emits, and
	// tolerating it would start widening the match toward the prose case.
	if strings.HasPrefix(content, transcriptOpen) {
		return rankNone, ""
	}
	if strings.HasPrefix(content, renamePrefix) {
		// A /rename envelope is machinery, not prose, so it yields its ARGUMENT OR NOTHING
		// and never falls through to a lower rank. Falling through was the first attempt and
		// it was worse than the bug it avoided: the generic rank-2 arm below then took the
		// whole literal "<command-name>/rename</command-name><command-args></command-args>"
		// as the title. Returning rankNone lets the walk reach a real title behind it, which
		// is the thing an argument-less /rename must not defeat.
		if t := between(content, "<command-args>", "</command-args>"); t != "" {
			return rankRename, t
		}
		return rankNone, ""
	}
	if t := between(content, "<user_query>", "</user_query>"); t != "" {
		return rankUserQuery, t
	}
	// A user-role message whose payload was a tool result or an image flattens to "" (see
	// pipeline.InferenceMessage.ContentBytes) — it is the last message of every agentic turn
	// and must not be chosen as the title. A message that was nothing BUT reminders reaches
	// here as "" too, and falls through by the same route: rankNone, so the walk finds a real
	// title behind it.
	if content != "" {
		return rankUserMsg, content
	}
	return rankNone, ""
}

const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
	// transcriptOpen is matched as a PREFIX only, and with no attribute tolerance: the opening
	// tag observed in the harness payload is exactly this, and every widening of the match moves
	// it toward the prose case that must keep working. See titleFrom.
	transcriptOpen = "<transcript>"
)

// stripReminders excises every <system-reminder>…</system-reminder> span and returns what
// surrounds them. The harness injects these blocks into the user turn it is attached to, so
// without this the block itself becomes the title and the real prompt — which sits after it —
// is never reached.
//
// SPLICES rather than keeping only the tail: of the user turns in local transcripts that carry
// a reminder alongside real prose, 67 of 68 have text on BOTH sides of the block. The seam is
// left as-is; sanitizeTitle folds "before\n" + "\nafter" to one space later.
//
// Called once at the top of titleFrom, so every rank arm sees content with the blocks already
// gone — see the comment there for why that matters to all three and not just rank 2.
//
// NESTED BLOCKS ARE MATCHED BY DEPTH, so an inner block does not terminate the outer one. The
// non-nesting version leaked the outer closing tag and dropped the text between the two open
// tags: "prose <sr>a<sr>b</sr> outer-tail</sr> real ask" left
// "prose  outer-tail</system-reminder> real ask" — a stray tag in a title, and the reason to
// track depth rather than pair each open with the next close. "<sr>a<sr>b</sr>c" now excises the
// whole span and yields "", where before it yielded "c".
//
// An UNTERMINATED open tag is left alone, matching the policy between() fixes for <user_query>:
// the tag name can appear in ordinary prose (someone discussing this very code), so the whole
// message stays a reasonable title rather than being cut at a tag that closes nothing. With depth
// tracking that extends to a block whose closes do not balance its opens: the remainder from the
// outermost unbalanced open is kept verbatim, tags and all.
//
// Matched literally in lowercase, which is the only form the harness emits into an inference
// payload. core/observe/claude canonicalises tag names because a capitalised variant reached
// its filter from a transcript; that is a different producer, and importing its machinery here
// would be the wrong dependency direction for a string scan.
//
// Runs on RAW content, before sanitizeTitle — folding U+0085 to a space would turn
// "<\u0085system-reminder>" into a non-tag and hide the block from this scan.
func stripReminders(s string) string {
	// The common case is no reminder at all, and this runs on every message of every event: a
	// session whose best rank is 2 can never terminate the reverse walk early, since rank 2 is
	// always beatable. One Index over the message beats building a string for each.
	if !strings.Contains(s, reminderOpen) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.Index(s, reminderOpen)
		if i < 0 {
			break
		}
		// Walk forward from the open tag, tracking depth, so a nested open tag consumes its own
		// close rather than letting the inner close terminate the outer block.
		depth, rest := 1, s[i+len(reminderOpen):]
		for depth > 0 {
			nextOpen := strings.Index(rest, reminderOpen)
			nextClose := strings.Index(rest, reminderClose)
			if nextClose < 0 {
				break // unbalanced from here on
			}
			if nextOpen >= 0 && nextOpen < nextClose {
				depth++
				rest = rest[nextOpen+len(reminderOpen):]
				continue
			}
			depth--
			rest = rest[nextClose+len(reminderClose):]
		}
		if depth > 0 {
			break // unterminated: keep the remainder verbatim, tags and all
		}
		b.WriteString(s[:i])
		s = rest
	}
	b.WriteString(s)
	return b.String()
}

// between returns the text bracketed by open and closing, or "" if either is absent.
//
// Hand-parsed rather than a regexp: matching a tag pair wants a backreference, which RE2
// lacks. A local copy of core/observe/claude's unexported helper — importing a transcript
// harvester into the session store for a string helper is the wrong dependency direction.
// Unlike the original this one does not TrimSpace; sanitizeTitle trims at the end.
func between(s, open, closing string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// sanitizeTitle folds a title to a single line: every whitespace or control rune becomes at
// most one U+0020, and the result is clipped to maxTitleLen runes.
//
// Applied ONCE, to the winner — not per candidate and not per event, both of which were measured
// and rejected. Per candidate cost 326µs→651µs on BenchmarkListSessions_Title/user-text (rank 2
// never terminates the walk, so every message pays a full O(content) fold); per event, at each of
// titleCandidate's acceptance points, cost 326µs→635µs for the same reason at retained-event scale.
// This function is 2.8µs on a 500-byte message, so where the number of calls goes, the cost goes.
//
// What still has to happen earlier is the BLANK TEST — a candidate folding to blank must not claim
// a rank, or it hides a real title in its own event — and foldsBlank does exactly that predicate
// without building the folded string.
//
// Rank never depends on whitespace, so comparing raw strings is safe. And applied to the
// EXTRACTED title, never the haystack: folding U+0085 to a space turns "<\u0085command-name>"
// into "< command-name>", which is no longer a tag.
func sanitizeTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // drops leading whitespace
	for _, r := range s {
		// TWO PREDICATES, and each catches runes the other misses. pipeline.IsControlRune covers
		// C0/C1/DEL plus the BIDI and zero-width runes that reorder or hide their surroundings.
		// unicode.IsSpace covers \n\r\t, the exotic spaces (U+00A0, U+3000, the U+2000 block) —
		// and, most importantly for a single-line guarantee, U+2028 LINE SEPARATOR and U+2029
		// PARAGRAPH SEPARATOR, which are the runes most likely to break a title across lines and
		// are NOT control runes. Both FOLD rather than drop, so "one\ntwo" does not become
		// "onetwo".
		if unicode.IsSpace(r) || pipeline.IsControlRune(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return clipTitle(strings.TrimRight(b.String(), " "))
}

// foldsBlank reports whether sanitizeTitle(s) would be "" — that is, whether s holds no rune the
// fold keeps. Exactly sanitizeTitle's own predicate, negated: change one and change both.
//
// Separate from sanitizeTitle because titleCandidate must REJECT a blank candidate without folding
// it. Folding there instead is a 2x on prose (326µs→635µs on BenchmarkListSessions_Title/user-text):
// sanitizeTitle is 2.8µs on a 500-byte message and titleCandidate runs once per retained event,
// where sessionTitle folds once per session. This allocates nothing and returns at the first kept
// rune, so a real message pays one rune's worth of work.
func foldsBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && !pipeline.IsControlRune(r) {
			return false
		}
	}
	return true
}

// clipTitle caps s at maxTitleLen runes.
//
// A PLAIN RUNE CUT, which can sever a combining mark from its base character — "e" plus a
// combining acute cut between the two leaves a dangling accent on whatever now precedes it.
// core/observe/claude's clipTitle is exempt because its scrubRunes drops every combining
// mark, modifier and regional indicator first; this sanitizer keeps them, so "café" survives
// as "café" and the cut is the price. Measuring in grapheme clusters instead needs a
// segmentation library core/ does not have, and the cut lands at rune 80 of a title that is
// almost never that long.
//
// TrimRight runs again here because the cut can land just after a folded space.
func clipTitle(s string) string {
	r := []rune(s)
	if len(r) <= maxTitleLen {
		return s
	}
	return strings.TrimRight(string(r[:maxTitleLen]), " ")
}
