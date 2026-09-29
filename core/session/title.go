package session

import (
	"strings"
	"unicode"

	"github.com/rossoctl/cortex/core/capabilities"
	"github.com/rossoctl/cortex/core/pipeline"
)

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

// Title sources, best first. Lower wins; rankNone means "named nothing".
//
// WITHIN ONE RANK THE TIE-BREAK DEPENDS ON THE CALLER, and the two in tree differ: sessionTitle
// takes the LAST match in event order, Store.Append's fold keeps the FIRST unless the tie is at
// rankRename. See entry.Title.
const (
	rankRename    = 0
	rankUserQuery = 1
	rankUserMsg   = 2
	rankNone      = 3
)

const renamePrefix = "<command-name>/rename</command-name>"

// sessionTitle names a session from a slice of its events: a /rename envelope if one is present,
// else a <user_query> block, else the LAST user message carrying text.
//
// TEST-ONLY, AND IT IS NOT WHAT /v1/sessions SERVES. Store.Append folds the title incrementally
// into entry.Title instead, because this walk could not be afforded under the store's read lock —
// see the Title call site in ListSessions for the 38.1s measurement that moved it. What survives
// here is the per-event picker plus the cross-event ranking rule, which is the cheapest way to
// exercise titleCandidate / titleFrom / stripReminders over a sequence.
//
// THE TWO RULES DISAGREE ON PURPOSE, so do not read this as a model of the served value. This
// answers "last match in event order wins": over two prose messages it picks the SECOND. The fold
// is first-wins except that a /rename always overrides, so it picks the FIRST. Both are pinned —
// this by the tests below, the fold by tests that go through Append/ListSessions — and a test
// added here proves nothing about the wire.
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
func sessionTitle(events []pipeline.SessionEvent) string {
	best, bestRank := "", rankNone
	for i := len(events) - 1; i >= 0; i-- {
		// Reverse, so the FIRST hit at a rank is the LAST one in event order — hence
		// `>=` rather than `>`: an earlier event must not displace it at equal rank.
		r, t := titleCandidate(&events[i])
		if t == "" || r >= bestRank {
			continue
		}
		// Fold BEFORE accepting, so a whitespace-only candidate does not claim the rank — claiming
		// rank 0 would also break the walk and hide a real /rename in an earlier event. Redundant
		// while titleCandidate screens with foldsBlank, and kept so this loop's invariant holds on
		// its own terms rather than by trusting its callee.
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
	// THE INDEX COMPARISON IS NOT REDUNDANT. The scan visits messages newest-first, so the first
	// rank-2 guess it meets is the newest one — but a rank-0/1 guess that titleFrom demotes to rank
	// 2 can sit anywhere, including after deferredHead, and then it is the later of the two.
	// `["early plain prose", "what does <user_query> mean here"]` is the bug: index 1 guesses rank
	// 1, settles at 2, and is the last user message — yet the deferred index 0 won regardless.
	//
	// AT MOST TWO PASSES over a prefix, no retries: messages above deferredHead are visited only by
	// the first loop, those below it by both. A review asked whether the second visit's titleFrom
	// could be skipped for indices the first loop already settled. It can, and it is not worth the
	// bookkeeping: constructing the overlap (a demoted message below a plain-prose guess, 300
	// messages alternating) measures 8.8µs against 12.3µs for the all-demoted shape, because the
	// break below stops this loop within an index or two of deferredHead. The redundancy is real
	// and costs less than the counter that would avoid it.
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
			// `r > bestRank` cannot fire today (this loop runs only when bestRank >= rankUserMsg,
			// and titleFrom cannot settle worse once foldsBlank has rejected the blanks). Written
			// as the full comparison so the rule — better rank wins, then later index — lives in
			// the code rather than only here.
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
// A SCREEN, NOT A RANK, AND NOT AN INVARIANT IN EITHER DIRECTION. titleFrom is the only
// authority on what a message settles at; this exists solely to keep titleFrom off the messages
// where the answer is almost always rank 2. An earlier comment here claimed rank 2 was reliable
// — that the guess could never be beaten, because such a message contains neither <user_query>
// nor a /rename prefix and stripping reminders could not introduce one. The last clause is
// false: stripReminders SPLICES, joining the head before the first block to the tail after the
// last, so "<user_qu" + "<system-reminder>…</system-reminder>" + "ery>ask</user_query>" carries
// no "<user_query>" as sent and does once stripped. Contrived rather than observed, and the cost
// of being wrong is one under-ranked title, so it is not worth a scan to close.
//
// IT OVER-PROMISES TOO, in two ways both covered by tests:
//
//   - rank 1 for an UNTERMINATED <user_query>: Contains finds the open tag, but between() needs
//     a closing tag and returns "", so titleFrom falls through to rank 2.
//   - rank 0 or 1 for a message whose payload is empty once extracted (`<command-args></command-args>`,
//     `<user_query> </user_query>`), which settles at rankNone.
//
// So a rank-0 or rank-1 guess MUST be settled by titleFrom before its rank is recorded, and a
// rank-2 guess may be deferred — but a caller must not treat a deferred message's rank as known
// without calling titleFrom, which is why titleCandidate's deferred arm calls it rather than
// trusting the screen.
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

// stripReminders returns what surrounds the reminder blocks: everything before the FIRST
// <system-reminder> joined to everything after the LAST </system-reminder>. The harness injects
// these blocks into the user turn it is attached to, so without this the block itself becomes the
// title and the real prompt — which sits after it — is never reached.
//
// DELIBERATELY UNBALANCED, and that is a DoS fix rather than a simplification. The previous version
// paired each open with its own close by tracking depth, which re-scanned the tail once per nesting
// level: a 190KB message of nested open tags (11,444 of them, all attacker-supplied) took 593ms
// here, and ListSessions called this under s.mu.RLock() whose writer side is Store.Append on the
// proxy's request path — 38.1s of lock hold over a 64-event window of such events.
//
// Two index scans cannot go quadratic regardless of what the content nests. On that same 190KB
// fixture, measured three ways because the shapes differ by four orders of magnitude and only the
// worst one is worth quoting: 250µs with no close tag anywhere (Index finds the open at byte 0,
// then LastIndex scans the whole payload backward for a close that is not there — THE WORST CASE,
// and 2400x better than the 593ms it replaces), 31ns once a close is present, 2.4µs for content
// with no reminder at all. Through the real path a single hostile Append is 452µs and the
// ListSessions that was 38.1s is now 1.6µs.
//
// THE LOCK HOLD WAS FIXED TWICE OVER, and this is the half that still matters. The title is now
// folded at Append, so ListSessions no longer calls this at all — but Append does, on the request
// path, still holding the write lock. A quadratic scan there is the same denial of service with a
// different call stack, which is why the ceiling that once bounded the read side was not a fix.
//
// TWO ACCEPTED LOSSES, both real and both pinned by tests rather than left to be rediscovered:
//
//   - PROSE BETWEEN TWO BLOCKS IS DROPPED. "<sr>a</sr>mid<sr>b</sr>tail" yields "tail", not
//     "mid tail". The 67-of-68 evidence that user turns carry prose on both sides of a block
//     argued for excising each block and splicing; this keeps the outermost pair instead. It
//     costs a title, on a field documented as a suggestion, and nothing else.
//   - AN UNTERMINATED OPEN TRUNCATES. "what does <system-reminder> mean" yields "what does ",
//     where the old version returned it verbatim — so this is narrower than the policy
//     TestSessionTitle_UnclosedTag states for <user_query>, which keeps the raw string because
//     the tag name can appear in ordinary prose. Unlike a blank result this does NOT fall
//     through to an earlier real title: "what does " is non-blank and wins. Tolerated because
//     the alternative is scanning for balance, which is the thing that was quadratic.
//
// Called once at the top of titleFrom, so every rank arm sees content with the blocks already
// gone — see the comment there for why that matters to all three and not just rank 2.
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
	i := strings.Index(s, reminderOpen)
	if i < 0 {
		return s
	}
	// LastIndex, not Index: pairing the first open with the FIRST close is what leaks a stray
	// "</system-reminder>" into the title when the blocks nest, since the inner close arrives
	// first. Searching the whole string from the right is one scan either way.
	j := strings.LastIndex(s, reminderClose)
	if j < i {
		// No close after the first open — including a close that sits in prose BEFORE it, which
		// is why this compares against i rather than testing j < 0. Keep the head.
		return s[:i]
	}
	return s[:i] + s[j+len(reminderClose):]
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
