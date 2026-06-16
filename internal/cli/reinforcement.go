package cli

import (
	"encoding/json"
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
// RUBRIC
//
// Two evidence sources, combined by the precedence below:
//
//  1. The next substantive user turn (the "feedback" turn). High-precision phrase
//     cues classify it as a correction (negative) or an approval (positive).
//     Cues are deliberately narrow — a wrong label is worse than falling back to
//     neutral, and the false-`corrected` direction is the most damaging (it would
//     fabricate failure-mode evidence in skill bodies), so correction cues never
//     rely on a bare ambiguous word.
//  2. Checkpoint metadata: a non-empty checkpointExportSession.Error means the
//     agent's own run failed. That is a negative outcome even absent user words.
//
// PRECEDENCE (first match wins):
//
//  1. correction cue in feedback   -> corrected   (strongest negative; beats a
//                                                   co-occurring "thanks but…")
//  2. approval cue in feedback     -> success
//  3. agent run errored            -> corrected
//  4. otherwise                    -> neutral      (no feedback turn, or no cue)
//
// KNOWN GAPS (deferred to the episode layer in Phase 1, not implemented here):
//   - "re-prompting the same intent" as a correction signal needs intent
//     comparison across turns, which the episode layer provides.
//   - requiring an intervening assistant turn before treating a user turn as
//     feedback (vs. a continuation/clarification of the prior request).
//   - linking a following commit/checkpoint success as a positive signal.
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
	if sig.AgentErrored {
		return reinforcementCorrected
	}
	return reinforcementNeutral
}

// reinforcementSignal is the minimal evidence the classifier needs about one
// episode. The episode layer (Phase 1) populates it: FeedbackText from the next
// substantive user turn, AgentErrored from the judged checkpoint's Error field.
type reinforcementSignal struct {
	FeedbackText string // next substantive user turn; "" when the session ended
	AgentErrored bool   // checkpointExportSession.Error was non-empty
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
		signals = append(signals, reinforcementSignal{FeedbackText: feedback})
	}
	return signals
}

// substantiveUserTurns returns the real user requests in transcript order,
// skipping injected wrappers. Unlike firstUserRequest it collects every turn and
// does not truncate, so cue scanning sees the full text.
func substantiveUserTurns(transcript string) []string {
	if messages, ok := parseDocumentConversation(transcript); ok {
		var turns []string
		for _, message := range messages {
			if message.Role != "user" || message.Text == "" || isWrapperRequest(message.Text) {
				continue
			}
			turns = append(turns, message.Text)
		}
		return turns
	}
	var turns []string
	for _, line := range strings.Split(transcript, "\n") {
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
		turns = append(turns, strings.Join(strings.Fields(text), " "))
	}
	return turns
}
