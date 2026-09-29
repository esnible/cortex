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
// WITHIN ONE RANK THE TIE-BREAK DEPENDS ON THE SCOPE, and the two differ: titleCandidate takes the
// LAST match among one event's messages, while Store.Append's fold across events keeps the FIRST
// unless the tie is at rankRename. See entry.Title for why that pairing is what it wants.
const (
	rankRename    = 0
	rankUserQuery = 1
	rankUserMsg   = 2
	rankNone      = 3
)

const renamePrefix = "<command-name>/rename</command-name>"

// titleCandidate returns the best-ranked title this event's user messages offer.
//
// ONE REVERSE SCAN, NO RETRIES — reverse so that the first acceptance at any rank is the LAST such
// message in event order, which is this scope's tie-break rule. Retrying the scan whenever
// titleFrom refused a pick was quadratic in messages-per-event, 31.9µs at n=50 rising to 7.20ms at
// n=800; BenchmarkSessionTitle_ReminderFanout is what holds that down.
//
// TWO CLASSES OF MESSAGE, because quickRank is a bound and a bound is not a rank:
//
//   - a rank-0 or rank-1 guess (rare, since the tags are) is SETTLED EAGERLY by calling titleFrom,
//     because such a guess can be demoted and must not be recorded before it is settled.
//   - a rank-2 guess (the majority) is DEFERRED to the second loop below: calling titleFrom on
//     every message cost +70%, so the scan remembers the index instead. This direction of the
//     bound does hold — a message with no <user_query> and no reminder-hidden /rename prefix has
//     nothing for the strip to uncover, because the strip's splice separator cannot assemble a tag
//     across the junction — and it is the only direction relied on anywhere. See quickRank.
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
		// REJECT A BLANK CANDIDATE BEFORE IT CLAIMS THE RANK, because no later screen can: a
		// caller sees ONE answer per event, so a blank winner here discards the whole event
		// rather than falling through to a real title inside it.
		//
		// foldsBlank rather than sanitizeTitle, which is why foldsBlank exists: this runs per
		// candidate message and folding here measured 326µs→635µs on
		// BenchmarkListSessions_Title/user-text. Only the winner is folded, by Store.Append.
		if foldsBlank(t) {
			continue
		}
		bestRank, bestTitle, bestIdx = r, t, i
		// PURE OPTIMIZATION — UNOBSERVABLE, so do not go looking for the test that pins it. Deleting
		// it changes no result, by two independent mechanisms: `r >= bestRank` above rejects every
		// later candidate once bestRank is 0 (no rank is better), and the deferred loop is gated on
		// `bestRank >= rankUserMsg`, which 0 fails. Verified by mutation over renames paired with
		// rank-1, rank-2, reminder-only and second-rename messages in both orders — byte-identical.
		// It is kept because the remaining scan is provably wasted work, not because it decides
		// anything.
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
	// `["early plain prose", "what does <user_query> mean here"]` is the case: index 1 guesses rank
	// 1, settles at 2, and is the last user message, so the deferred index 0 must not win.
	//
	// The prefix below deferredHead is visited by both loops. Skipping the indices the first loop
	// already settled is possible and measured slower than the redundancy (8.8µs against 12.3µs),
	// because the break below stops this loop within an index or two of deferredHead.
	//
	// THE BOUND IS rankUserMsg AND ONE DIRECTION OF IT IS OBSERVABLE. Tightening it to rankNone
	// skips the loop whenever the eager pass settled anything, and a settled DEMOTION is exactly
	// when the loop still has work: the demoted pick can sit earlier than the deferred one, so the
	// event gets titled by the earlier message against last-match-within-event.
	// TestSessionTitle_DeferredOnlyWithNothingSettled's last row is that case. Note rankNone is the
	// numerically largest rank, so `>= rankNone` is the stricter gate — the mutation reads backwards.
	//
	// Loosening it to rankUserQuery is EQUIVALENT BY CONSTRUCTION, so do not hunt for the test that
	// pins it. It differs only when bestRank == rankUserQuery, and every candidate this loop can
	// return settles at rankUserMsg — a rank-0/1 message was handled eagerly, and a rankNone one
	// carries t == "" and is dropped by foldsBlank above. So the first comparison below is
	// `rankUserMsg > rankUserQuery`, always true, and the loop breaks without returning anything.
	if deferredHead >= 0 && bestRank >= rankUserMsg {
		for i := deferredHead - 1; i >= 0; i-- {
			// No quickRank here: re-classifying costs a second scan of every message (+45% on
			// prose), and titleFrom is the authority anyway. A rank-0/1 message reaching this
			// point was already settled and rejected by the loop above.
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
			// `r > bestRank` is unreachable today: rankNone is the only rank worse than rankUserMsg,
			// and every titleFrom return carrying it also carries t == "", which foldsBlank
			// rejected one line above. Written as the full comparison anyway, so the rule — better
			// rank wins, then later index — lives in the code and not only in a comment.
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
// A SCREEN, NOT A RANK. titleFrom is the only authority on what a message settles at; this exists
// solely to keep titleFrom off the messages where the answer is almost always rank 2.
//
// IT DOES NOT UNDER-PROMISE, and the reason is load-bearing elsewhere. stripReminders splices the
// head before the first block onto the tail after the last, so in principle stripping could
// MANUFACTURE a tag the message did not carry as sent — "<user_qu" + a reminder +
// "ery>ask</user_query>" carries no "<user_query>" until the reminder is excised. It cannot, because
// the splice joins head to tail with a SPACE and no tag in the vocabulary contains one: a tag split
// across that junction reassembles as "<user_qu ery>" and matches nothing. Both halves of the old
// hazard — a synthesized <user_query> and a synthesized /rename envelope — settle at rank 2 with the
// mangled markup as their title, which is what TestSessionTitle_SpliceCannotManufactureATag pins.
//
// THAT MAKES THE SEPARATOR A SECURITY BOUNDARY, not just a readability fix, and it is the only
// thing standing between a client and a rank-0 title it never sent. Rank 0 is STICKY — the fold lets
// a rename override, so only a later genuine rename displaces one. Deleting the separator reopens
// the hole (verified: the spliced envelope titles a session "SPLICED"), which is why the test above
// exists in addition to the word-fusion row in TestSessionTitle_StripsReminder. See stripReminders.
//
// IT DOES OVER-PROMISE, in two ways both covered by tests: rank 1 for an UNTERMINATED
// <user_query>, where between() finds no close and titleFrom falls through to rank 2; and rank 0
// or 1 for a payload that is empty once extracted (`<command-args></command-args>`,
// `<user_query></user_query>`, `<user_query> </user_query>`), which settles at rankNone.
//
// So the bound holds in ONE direction only — the guess can be too GOOD but never too bad. A rank-0
// or rank-1 guess MUST therefore be settled by titleFrom before its rank is recorded, while a rank-2
// guess can be DEFERRED, since nothing the strip uncovers can improve on it. That asymmetry is what
// titleCandidate's two loops are shaped around, and it rests on the separator above: without it a
// rank-2 guess could settle at rank 0, and deferring would be unsound rather than merely lazy.
// Even so, the deferred arm calls titleFrom rather than recording the guess — a rank-2 guess can
// still settle at rankNone (a message that is nothing but reminders), which is not a rank a caller
// may assume away.
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
	// front, which is why the HasPrefix above misses one and this exists.
	//
	// BOTH TESTS ARE ANCHORED, and that is a cost decision. An unanchored Contains for the
	// 37-byte envelope, scanned over the whole message to answer "no" for almost every message,
	// cost +45% on 500-byte prose (325µs→472µs on a 300-turn session); gating it on an unanchored
	// reminder test cost the same, because the gate scans the whole message too. `<` is not a
	// usable discriminator either — prose carrying a code snippet has one.
	//
	// An envelope behind PROSE ("as I said, <command-name>/rename</command-name>…") is
	// deliberately not caught: titleFrom tests a prefix too, so rank 2 here agrees with what
	// titleFrom settles on.
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
	// A TRANSCRIPT ENVELOPE IS MACHINERY, NOT AN ASK. Observed live: a message opening with this tag
	// titled a session `\", \"` — the fold reduced a wall of quoted JSONL to its punctuation.
	// Discarded rather than ranked, so the walk reaches a real title behind it.
	//
	// ANCHORED, and the anchor is the whole rule: of the user messages in local transcripts that
	// mention this tag, the only one does so as PROSE — a bug report quoting it, which is a perfectly
	// good title — so an unanchored match would discard exactly the message a reader wants. No
	// leading-whitespace tolerance either, for the same reason.
	if strings.HasPrefix(content, transcriptOpen) {
		return rankNone, ""
	}
	// AN ENVELOPE THAT IS THE WHOLE MESSAGE YIELDS ITS BODY OR NOTHING, and must never fall through
	// to the generic rank-2 arm, because falling through takes the LITERAL MARKUP as the title. That
	// is non-blank, so foldsBlank cannot reject it, so under Append's first-wins fold it claims a rank
	// and blocks the session's real title for the rest of its life. rankNone instead lets the walk
	// reach a real title behind the envelope. TestSessionTitle_EmptyCommandArgs and the
	// "user_query with empty body" row of TestSessionTitle_BlankAfterSanitizeFallsThrough pin the
	// two halves. Naming the shape instead ("empty <user_query>") is the same bug wearing a label:
	// still non-blank, still claims a rank, and at rank 1 it would outrank genuine prose outright.
	//
	// "THE WHOLE MESSAGE" is load-bearing in both arms and each anchors it differently — see the
	// user_query arm. Markup embedded in prose is a title, not machinery.
	if strings.HasPrefix(content, renamePrefix) {
		if t := between(content, "<command-args>", "</command-args>"); t != "" {
			return rankRename, t
		}
		return rankNone, ""
	}
	if strings.Contains(content, "<user_query>") {
		if t := between(content, "<user_query>", "</user_query>"); t != "" {
			return rankUserQuery, t
		}
		// ONLY A WHOLE-MESSAGE ENVELOPE IS DISCARDED, and the equality is the anchor — the /rename arm
		// above anchors with HasPrefix because its body follows the tag, while an empty envelope has
		// no body to follow, so the whole message is the tag pair. A tag pair EMBEDDED in prose is a
		// different thing entirely and must stay nameable: "why does <user_query></user_query> render
		// empty in my logs?" is a perfectly good title, and an unanchored test discarded exactly the
		// message a reader wants. So is an unterminated open — "what does <user_query> mean in this
		// code" — which between() reports identically, since "" means empty-body and missing-close
		// alike; the equality separates all three without having to tell those two apart.
		//
		// NO ORDER CHECK IS NEEDED, because equality cannot be order-confused. Searching for the
		// close from the open's index is the correction stripReminders needs, and the hazard is real
		// in the shape this replaced ("a</user_query>b<user_query>c" read as a terminated envelope) —
		// but against an exact whole-string match there is no second occurrence to mis-pair, so an
		// index-relative search here would be unreachable. Verified over 400k randomized tag/prose
		// fragments: adding it changes no result.
		if strings.TrimSpace(content) == "<user_query></user_query>" {
			return rankNone, ""
		}
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
// DELIBERATELY UNBALANCED, and that is a DoS fix. Pairing each open with its own close by depth
// re-scanned the tail once per nesting level: a 190KB message of 11,444 nested open tags took
// 593ms, and this runs under the store's write lock on the proxy request path. Two index scans
// cannot go quadratic regardless of what the content nests — 250µs on that same fixture with no
// close tag anywhere (the worst case: LastIndex scans the whole payload for a close that is not
// there), 31ns once a close is present.
//
// ONE ACCEPTED LOSS: PROSE BETWEEN TWO BLOCKS IS DROPPED. "<sr>a</sr>mid<sr>b</sr>tail" yields
// "tail" — keeping the outermost pair rather than excising each block separately.
// The alternative is named precisely because it is easy to overstate: per-block excision would
// yield "midtail", NOT "mid tail", since `mid` and `tail` are not adjacent to one separator but to
// two different blocks. So the trade is one title against another equally mangled one, not against
// a clean read — which is most of why it is acceptable. It costs a title, on a field documented as
// a suggestion, and nothing else. Measured across 550 local transcripts — 2804 user text messages,
// 253 reminder-bearing — that shape is 8 of them, against 242 reminder-only and 3 with an unclosed
// open. (An earlier comment here claimed 67 of 68 and used it to justify the splice; it did not
// reproduce.)
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
		// NO CLOSE AFTER THE FIRST OPEN, so there is no block here to excise — just a tag name
		// sitting in prose. Return the message UNCHANGED rather than its head, matching titleFrom's
		// policy for an unclosed <user_query> and for the same reason.
		//
		// Returning s[:i] is what this did, and it was a bug: "why is <system-reminder> leaking into
		// my session titles?" served "why is", which is non-blank, so under Append's first-wins fold
		// it claimed rank 2 and then permanently blocked the session's real title. Costs nothing
		// either way — this branch is reached by the same two index scans, and returning s skips the
		// slice copy s[:i] sets up.
		//
		// The compared value is i, not 0: a close sitting in prose BEFORE the first open (j >= 0
		// but j < i) is not the end of a block either, and splicing on it would run backwards.
		return s
	}
	// A SPACE, because the head and tail were never adjacent in the message and joining them bare
	// FUSES THE WORDS ACROSS THE GAP: "my question<sr>noise</sr>and the follow-up" served
	// "my questionand the follow-up". The harness always emits a newline beside its own blocks, so
	// nothing it injects fuses — every would-fuse message in the 550-transcript corpus (6 of the 10
	// with prose on both sides) is someone writing ABOUT the tag, with it quoted mid-sentence. That
	// is exactly the message whose title must survive, so the separator is not only for pathological
	// input.
	//
	// IT HAS A SECOND, LOAD-BEARING ROLE: it is what stops the splice MANUFACTURING a tag. A tag
	// split across the junction ("<user_qu" + block + "ery>ask</user_query>", or a halved /rename
	// envelope) would reassemble if head met tail bare, letting a client synthesize a rank-1 title —
	// or, worse, a STICKY rank-0 one — out of markup it never sent. No tag in the vocabulary contains
	// a space, so the separator defeats every such splice. Deleting it reopens that hole, and the
	// word-fusion case above is NOT what would catch it: TestSessionTitle_SpliceCannotManufactureATag
	// is. quickRank's bound depends on this, so do not "simplify" the separator away.
	//
	// NOT WHEN THE HEAD IS EMPTY, and this guard is load-bearing rather than a micro-optimization.
	// It is tempting to splice unconditionally on the grounds that sanitizeTitle drops leading
	// whitespace and TrimRights the tail, so no title can show the difference — but titleFrom reads
	// this string BEFORE anything trims it, and its rank-0 and transcript arms are HasPrefix tests
	// that a leading space defeats. Spliced unconditionally, every reminder-prefixed /rename demoted
	// from rank 0 to rank 2; TestSessionTitle_ReminderBeforeRename, _LaterReminderPrefixedRenameWins
	// and _TranscriptEnvelopeDiscarded all catch it. A LEADING BLOCK IS ALSO THE COMMON CASE — 242 of
	// the 253 reminder-bearing messages in the corpus are reminder-only, which lands here with an
	// empty head and must stay exactly "" for foldsBlank to screen it.
	//
	// THE MIRROR CASE IS DELIBERATELY NOT GUARDED. A trailing block leaves a trailing space, and no
	// anchored test looks at the end of the string: every arm either extracts up to a close tag or is
	// TrimRighted by sanitizeTitle, including at the clip boundary (a title exactly at maxTitleLen
	// followed by a block keeps all 80 runes). Verified by mutation — adding `|| tail == ""` changes
	// no test and no title on any shape reachable here — so it would be a clause no test could pin.
	head := s[:i]
	if head == "" {
		return s[j+len(reminderClose):]
	}
	return head + " " + s[j+len(reminderClose):]
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
// Applied ONCE, to the winner — not per candidate and not per event. Both were measured: either
// one roughly doubles BenchmarkListSessions_Title/user-text (326µs→635-651µs), because rank 2 never
// terminates the walk so every message would pay a fold. foldsBlank supplies the one thing that
// does have to happen earlier — a candidate folding to blank must not claim a rank — as a predicate
// that builds no string.
//
// Rank never depends on whitespace, so comparing raw strings is safe. And applied to the
// EXTRACTED title, never the haystack: folding U+0085 to a space turns "<\u0085command-name>"
// into "< command-name>", which is no longer a tag.
//
// O(maxTitleLen), NOT O(len(s)), and that is what lets it stay under the store's write lock. It
// stops as soon as maxTitleLen runes are emitted, so a 190KB candidate costs the same as an 80-rune
// one: 916µs and 958KB of allocation became 366ns and 320B. The 320 is not a measurement to re-take
// but `maxTitleLen * 4`, the b.Grow below — one allocation of a fixed size, whatever the input.
//
// NO LOOKAHEAD IS NEEDED PAST THE STOP, which is the reason this is correct and not merely fast.
// The fold is a left-to-right rewrite that never revisits an emitted rune, so runes 1..80 are final
// when the 80th is written. The only thing a later rune could affect is the TrimRight, and it
// cannot: if rune 80 is a folded space the trim removes it whether or not more input follows, and if
// it is a kept rune the trim stops there regardless. Counting EMITTED runes rather than input
// consumed is likewise load-bearing — a whitespace run of any length emits at most one.
//
// A PLAIN RUNE BOUNDARY, which the early stop does not change: it can sever a combining mark from
// its base, leaving a dangling accent. core/observe/claude's clip is exempt because its scrubRunes
// drops every combining mark first; this one keeps them, so "café" survives and the cut is the
// price. Grapheme clusters need a segmentation library core/ does not have.
func sanitizeTitle(s string) string {
	var b strings.Builder
	// Sized to the OUTPUT, not the input. 4 bytes per rune is the max UTF-8 encoding, so this is
	// one allocation for any input, where b.Grow(len(s)) was proportional to a hostile message.
	b.Grow(maxTitleLen * 4)
	emitted := 0
	prevSpace := true // drops leading whitespace
	for _, r := range s {
		if emitted == maxTitleLen {
			break
		}
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
				emitted++
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
		emitted++
	}
	// TrimRight, not clipTitle: the loop cannot emit more than maxTitleLen runes, so there is
	// nothing left to clip. The trim is still needed because the stop can land just after a
	// folded space, exactly as the old cut could.
	return strings.TrimRight(b.String(), " ")
}

// foldsBlank reports whether sanitizeTitle(s) would be "" — that is, whether s holds no rune the
// fold keeps. Exactly sanitizeTitle's own predicate, negated: change one and change both.
//
// Separate from sanitizeTitle because titleCandidate must REJECT a blank candidate without folding
// it. Folding there instead is a 2x on prose (326µs→635µs on BenchmarkListSessions_Title/user-text):
// sanitizeTitle is 2.8µs on a 500-byte message and titleCandidate examines every message of every
// appended event, where Store.Append folds only a candidate that already won its rank. This
// allocates nothing and returns at the first kept rune, so a real message pays one rune's worth of
// work.
func foldsBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && !pipeline.IsControlRune(r) {
			return false
		}
	}
	return true
}
