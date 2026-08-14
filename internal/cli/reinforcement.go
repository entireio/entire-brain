package cli

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Reinforcement classification (Pattern Consolidation, Phase 0 spike).
//
// An episode is a user request, the agent work that followed, and the NEXT
// substantive user turn. Reinforcement is the deterministic label this file
// assigns to that episode's outcome, used downstream by pattern strength scoring
// and pattern-card headers ("12 success, 2 corrected"). There is intentionally
// NO model call here: the refresh path that builds patterns must stay token-free,
// so the signal is read from cheap, high-precision evidence only.
//
// # RUBRIC
//
// Two evidence sources, combined by the precedence below:
//
//  1. The next substantive user turn (the "feedback" turn). High-precision phrase
//     cues classify it as a correction (negative) or an approval (positive).
//     Cues are deliberately narrow — a wrong label is worse than falling back to
//     neutral, and the false-`corrected` direction is the most damaging (it would
//     fabricate failure-mode evidence in skill bodies), so correction cues never
//     rely on a bare ambiguous word.
//  2. The agent's work segment (the turns between this request and the next). A
//     successful git commit — git prints its `[branch hash] subject` confirmation
//     line only when a commit lands — is a positive signal. This matters because,
//     empirically, users almost never type "thanks/lgtm" in CLI sessions; they
//     just move on, so next-turn approval cues have near-zero recall. A landed
//     commit that the user does not then push back on is the most reliable
//     token-free positive reinforcement available.
//
// PRECEDENCE (first match wins):
//
//  1. correction cue in feedback   -> corrected   (strongest negative; beats a
//     co-occurring "thanks but…",
//     and beats a committed work
//     segment the user then rejects)
//  2. approval cue in feedback     -> success
//  3. work segment committed        -> success
//  4. otherwise                    -> neutral      (no feedback turn, no cue, no commit)
//
// KNOWN GAPS (deferred, not implemented here):
//   - per-checkpoint Error -> a WorkFailed/corrected branch: the manifest session
//     record does not carry checkpointExportSession.Error, so the negative
//     work-outcome signal has no source yet.
//   - test/build-pass as an additional positive signal (needs reliable per-tool
//     command+output correlation; commit is the higher-precision proxy for now).
//   - "re-prompting the same intent" as a correction signal (needs cross-turn
//     intent comparison), and requiring an intervening assistant turn before
//     treating a user turn as feedback vs. a continuation.
const (
	reinforcementSuccess   = "success"
	reinforcementCorrected = "corrected"
	reinforcementNeutral   = "neutral"
)

// correctionCues are high-precision phrases that mark the feedback turn as
// negative: the prior agent work was wrong, rejected, or failed. Multi-word by
// design so a bare "wrong"/"no" inside a benign sentence does not misfire.
var correctionCues = []string{
	"that's wrong", "thats wrong", "that is wrong", "this is wrong",
	"that's incorrect", "that is incorrect", "this is incorrect",
	"that's not right", "that is not right",
	"not what i asked", "not what i wanted", "not what i meant",
	"that's not what i", "that is not what i",
	"doesn't work", "does not work", "didn't work", "did not work",
	"still failing", "still broken", "still doesn't", "still does not", "still not working",
	"you broke", "that broke", "broke the build", "broke the tests",
	"revert", "undo that", "undo the", "roll back", "rollback",
	"try again", "redo ", "that's a regression", "introduced a regression",
	"you missed", "you forgot", "that's incomplete",
}

// correctionPrefixes catch short negative openers that substring scanning would
// either miss or over-match. Checked against the normalized (lowercased,
// whitespace-collapsed) feedback. "no problem"/"no worries" are deliberately
// excluded so a polite acknowledgement is not read as a correction.
var correctionPrefixes = []string{
	"no,", "no.", "no it", "no that", "no this", "no don", "no the", "no -", "no the",
	"nope", "wrong,", "stop,", "stop.", "stop ", "ugh", "argh",
}

// approvalCues mark the feedback turn as positive. Single-word entries are
// restricted to terms that are unambiguous on their own; softer words ("great",
// "nice", "good") are only included in multi-word forms to avoid
// approval-with-caveat false positives.
var approvalCues = []string{
	"thanks", "thank you", "thank u",
	"perfect", "looks good", "looks great", "lgtm",
	"that works", "it works", "works now", "works great", "that did it",
	"ship it", "merge it", "awesome", "exactly", "that's correct", "that is correct",
	"great work", "nice work", "good job", "well done", "love it", "you nailed it",
}

// classifyReinforcement labels one episode from its reinforcement signal.
// Deterministic and allocation-light; safe to call per episode during refresh.
func classifyReinforcement(sig reinforcementSignal) string {
	feedback := normalizeFeedback(sig.FeedbackText)
	if hasCorrectionCue(feedback) {
		return reinforcementCorrected
	}
	if hasApprovalCue(feedback) {
		return reinforcementSuccess
	}
	if sig.WorkCommitted {
		return reinforcementSuccess
	}
	return reinforcementNeutral
}

// reinforcementSignal is the minimal evidence the classifier needs about one
// episode. The episode layer populates it: FeedbackText from the next substantive
// user turn, WorkCommitted from a git commit confirmation in this episode's work
// segment.
type reinforcementSignal struct {
	FeedbackText  string // next substantive user turn; "" when the session ended
	WorkCommitted bool   // the agent's work segment landed a git commit
}

func normalizeFeedback(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func hasCorrectionCue(normalized string) bool {
	if normalized == "" {
		return false
	}
	if normalized == "no" || normalized == "nope" || normalized == "wrong" {
		return true
	}
	if strings.HasPrefix(normalized, "no problem") || strings.HasPrefix(normalized, "no worries") {
		return false
	}
	for _, p := range correctionPrefixes {
		if strings.HasPrefix(normalized, p) {
			return true
		}
	}
	for _, c := range correctionCues {
		if strings.Contains(normalized, c) {
			return true
		}
	}
	return false
}

func hasApprovalCue(normalized string) bool {
	for _, c := range approvalCues {
		if strings.Contains(normalized, c) {
			return true
		}
	}
	return false
}

// reinforcementSignalsFromTranscript derives one signal per episode boundary in a
// single-session transcript: each substantive user turn is feedback on the work
// that followed the previous one, so the signals are the user turns after the
// first. AgentErrored is left false here — it is a per-checkpoint signal joined in
// by the real pipeline; this helper exists for the deterministic next-user-turn
// path and for fixtures. Supports the Codex, Claude, pi and opencode shapes via
// the same dialect helpers distill/history use.
func reinforcementSignalsFromTranscript(transcript string) []reinforcementSignal {
	turns := substantiveUserTurns(transcript)
	if len(turns) < 2 {
		return nil
	}
	signals := make([]reinforcementSignal, 0, len(turns)-1)
	for _, feedback := range turns[1:] {
		signals = append(signals, reinforcementSignal{FeedbackText: feedback.Text})
	}
	return signals
}

// commitConfirmationPattern matches a git commit confirmation line
// ("[main 7249258] Subject"). git emits this only when a commit lands, and it
// appears literally in the raw transcript (inside the tool-output JSON string)
// regardless of agent dialect — so a raw scan of the work segment is a
// high-precision, dialect-agnostic "work committed" signal. The trailing space
// before the subject avoids matching bare bracketed hex elsewhere.
var commitConfirmationPattern = regexp.MustCompile(`\[[A-Za-z0-9._/+-]+ [0-9a-f]{7,40}\] `)

// workCommitted reports whether the episode's work segment shows a landed commit.
func workCommitted(workText string) bool {
	return commitConfirmationPattern.MatchString(workText)
}

// episodeSegment is a substantive user request paired with the raw text of the
// agent work that followed it, up to the next substantive request. WorkText is
// kept raw (the verbatim non-user transcript lines) so commit detection sees tool
// output without per-dialect extraction.
type episodeSegment struct {
	Request  userTurn
	WorkText string
}

// transcriptEpisodeSegments splits a transcript into episodes: each substantive
// user turn opens a segment, and every following non-user turn (assistant, tool
// output, reasoning, injected wrappers) accrues to that segment's work text until
// the next substantive user turn. Document-form (opencode) transcripts expose
// only conversation text via the shared parser, so tool output — and thus commit
// detection — is best-effort there; JSONL dialects (Codex, Claude, pi) carry it.
//
// NOTE: this deliberately DIVERGES from the conversation exchange parser
// (scanConversationTranscript) even though both split on substantive user
// turns. Episodes keep RAW work text (verbatim tool output feeds commit
// detection) and count hook-injected requests as turn boundaries; exchanges
// keep only visible assistant narrative and filter hook injections and
// API-error envelopes (isConversationInjectedRequest /
// isConversationNoiseNarrative). The shared kernel is a dozen lines of loop
// scaffolding; converging them would either change one side's measured
// semantics or produce an abstraction with more parameters than shared code
// (the Phase 2 convergence question was evaluated and closed this way; see
// also the intentionally-divergent query stopword regimes for the precedent).
// Keep the two parsers' dialect switches in sync when adding a transcript
// format: transcriptUserText covers requests for both.
func transcriptEpisodeSegments(transcript string) []episodeSegment {
	var (
		segments []episodeSegment
		open     bool
		current  episodeSegment
		work     strings.Builder
	)
	flush := func() {
		if open {
			current.WorkText = work.String()
			segments = append(segments, current)
			work.Reset()
		}
	}

	if messages, ok := parseDocumentConversation(transcript); ok {
		for _, m := range messages {
			if m.Role == "user" && m.Text != "" && !isWrapperRequest(m.Text) {
				flush()
				current = episodeSegment{Request: userTurn{Text: m.Text, Line: m.Line}}
				open = true
				continue
			}
			if open && m.Text != "" {
				work.WriteString(m.Text)
				work.WriteByte('\n')
			}
		}
		flush()
		return segments
	}

	for i, raw := range strings.Split(transcript, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		text := strings.TrimSpace(transcriptUserText(obj))
		if text != "" && !isWrapperRequest(text) {
			flush()
			current = episodeSegment{Request: userTurn{Text: strings.Join(strings.Fields(text), " "), Line: i + 1}}
			open = true
			continue
		}
		if open {
			work.WriteString(raw)
			work.WriteByte('\n')
		}
	}
	flush()
	return segments
}

// userTurn is a substantive user request with its anchor line in the transcript.
type userTurn struct {
	Text string
	Line int // 1-based; 0 when the dialect does not expose a line
}

// substantiveUserTurns returns the real user requests in transcript order,
// skipping injected wrappers. Unlike firstUserRequest it collects every turn and
// does not truncate, so cue scanning sees the full text; it also carries the
// anchor line so the episode layer can record a source pointer.
func substantiveUserTurns(transcript string) []userTurn {
	if messages, ok := parseDocumentConversation(transcript); ok {
		var turns []userTurn
		for _, message := range messages {
			if message.Role != "user" || message.Text == "" || isWrapperRequest(message.Text) {
				continue
			}
			turns = append(turns, userTurn{Text: message.Text, Line: message.Line})
		}
		return turns
	}
	var turns []userTurn
	for i, line := range strings.Split(transcript, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		text := strings.TrimSpace(transcriptUserText(obj))
		if text == "" || isWrapperRequest(text) {
			continue
		}
		turns = append(turns, userTurn{Text: strings.Join(strings.Fields(text), " "), Line: i + 1})
	}
	return turns
}
