package cli

import (
	"fmt"
	"testing"
)

// reinforcementFixture is one labeled episode signal. Fixtures span both agents'
// phrasings and all three labels; they are the eval guardrail for the Phase 0
// deterministic classifier and the regression guard if cues change later.
type reinforcementFixture struct {
	name      string
	feedback  string
	committed bool
	want      string
}

var reinforcementFixtures = []reinforcementFixture{
	// success: explicit approval in the feedback turn.
	{"thanks-exact", "thanks, that's exactly what I needed", false, reinforcementSuccess},
	{"perfect-ship", "perfect, ship it", false, reinforcementSuccess},
	{"looks-good-merge", "looks good to me, merge it", false, reinforcementSuccess},
	{"works-now", "that works now, nice work", false, reinforcementSuccess},
	{"lgtm", "LGTM", false, reinforcementSuccess},
	{"great-work", "great work, the tests pass", false, reinforcementSuccess},
	{"that-did-it", "that did it, thanks", false, reinforcementSuccess},

	// corrected: explicit negative feedback.
	{"no-revert", "no, that's wrong — revert it", false, reinforcementCorrected},
	{"didnt-work", "that didn't work, still failing on CI", false, reinforcementCorrected},
	{"you-broke", "you broke the build", false, reinforcementCorrected},
	{"nope", "nope, not what I asked for", false, reinforcementCorrected},
	{"try-again", "try again, this time use the existing helper", false, reinforcementCorrected},
	{"undo", "undo that change please", false, reinforcementCorrected},
	{"stop-wrong-file", "stop, that's the wrong file", false, reinforcementCorrected},
	{"incorrect-detail", "this is incorrect, the retry count should be 3", false, reinforcementCorrected},
	// correction beats a co-occurring thanks (the "most damaging" direction must
	// still resolve to the negative label).
	{"thanks-but-broken", "thanks but it still doesn't quite match the style", false, reinforcementCorrected},
	// correction also beats a committed work segment the user then rejects.
	{"committed-but-corrected", "no, that's wrong — revert it", true, reinforcementCorrected},

	// success: work segment landed a commit and the user did not push back.
	{"committed-no-feedback", "", true, reinforcementSuccess},
	{"committed-then-next-task", "now add logging to the fetcher", true, reinforcementSuccess},

	// neutral: no feedback, or a follow-up that carries no approval/correction.
	{"empty", "", false, reinforcementNeutral},
	{"new-request", "can you also add logging to the fetcher", false, reinforcementNeutral},
	{"question", "what does this function do?", false, reinforcementNeutral},
	{"next-task", "now do the same for the writer", false, reinforcementNeutral},
	{"ok-cool", "ok cool", false, reinforcementNeutral},
	// the no-problem guard: a polite acknowledgement is not a correction.
	{"no-problem", "no problem, I'll wait for the next pass", false, reinforcementNeutral},
}

func TestClassifyReinforcement(t *testing.T) {
	var total, correct, predictedCorrected, correctedHits, successAsCorrected int
	for _, f := range reinforcementFixtures {
		got := classifyReinforcement(reinforcementSignal{FeedbackText: f.feedback, WorkCommitted: f.committed})
		total++
		if got == f.want {
			correct++
		} else {
			t.Errorf("%s: classifyReinforcement(%q, committed=%v) = %q, want %q", f.name, f.feedback, f.committed, got, f.want)
		}
		if got == reinforcementCorrected {
			predictedCorrected++
			if f.want == reinforcementCorrected {
				correctedHits++
			}
		}
		// The most damaging confusion: a genuine approval labeled as a failure.
		if f.want == reinforcementSuccess && got == reinforcementCorrected {
			successAsCorrected++
		}
	}

	accuracy := float64(correct) / float64(total)
	correctedPrecision := 1.0
	if predictedCorrected > 0 {
		correctedPrecision = float64(correctedHits) / float64(predictedCorrected)
	}
	t.Logf("reinforcement eval: accuracy=%.3f (%d/%d), corrected precision=%.3f, success→corrected=%d",
		accuracy, correct, total, correctedPrecision, successAsCorrected)

	// Phase 0 exit criteria: overall accuracy >= 0.85, corrected precision >= 0.90,
	// and zero approvals mislabeled as failures.
	if accuracy < 0.85 {
		t.Errorf("accuracy %.3f below target 0.85", accuracy)
	}
	if correctedPrecision < 0.90 {
		t.Errorf("corrected precision %.3f below target 0.90", correctedPrecision)
	}
	if successAsCorrected != 0 {
		t.Errorf("%d approval(s) mislabeled as corrected; must be 0", successAsCorrected)
	}
}

// TestReinforcementSignalsFromTranscript proves end-to-end extraction: the next
// substantive user turn is recovered across every transcript dialect and feeds
// the classifier. Each transcript is request → agent work → feedback.
func TestReinforcementSignalsFromTranscript(t *testing.T) {
	cases := []struct {
		name       string
		transcript string
		want       string // expected label of the single episode's feedback turn
	}{
		{
			name: "claude-user-assistant",
			transcript: `{"type":"user","message":{"content":"add retry backoff to the fetcher"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Added exponential backoff."}]}}
{"type":"user","message":{"content":"that didn't work, still failing"}}`,
			want: reinforcementCorrected,
		},
		{
			name: "claude-message-dialect",
			transcript: `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"make it cohesive"}]}}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"perfect, ship it"}]}}`,
			want: reinforcementSuccess,
		},
		{
			name: "codex-event-msg",
			transcript: `{"type":"event_msg","payload":{"type":"user_message","message":"why does flush stay dirty"}}
{"type":"event_msg","payload":{"type":"agent_message","message":"because the lock is held"}}
{"type":"event_msg","payload":{"type":"user_message","message":"no, that's wrong - revert it"}}`,
			want: reinforcementCorrected,
		},
		{
			name: "codex-response-item",
			transcript: `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"add a benchmark"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"added"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"thanks, that did it"}]}}`,
			want: reinforcementSuccess,
		},
		{
			// Document-form (opencode) transcripts are pretty-printed, so the first
			// line is not standalone-valid JSON — that is how the parser tells them
			// apart from JSONL.
			name: "opencode-document",
			transcript: `{
  "info": {"id": "ses_x"},
  "messages": [
    {"info": {"role": "user"}, "parts": [{"type": "text", "text": "Remove the header bottom border"}]},
    {"info": {"role": "assistant"}, "parts": [{"type": "text", "text": "Removed both borders."}]},
    {"info": {"role": "user"}, "parts": [{"type": "text", "text": "you broke the layout"}]}
  ]
}`,
			want: reinforcementCorrected,
		},
		{
			// An injected wrapper user turn between the work and the real feedback
			// must be skipped, not treated as the feedback turn.
			name: "wrapper-skipped",
			transcript: `{"type":"user","message":{"content":"add caching"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}
{"type":"user","message":{"content":"<system-reminder>be concise</system-reminder>"}}
{"type":"user","message":{"content":"perfect, thanks"}}`,
			want: reinforcementSuccess,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signals := reinforcementSignalsFromTranscript(tc.transcript)
			if len(signals) != 1 {
				t.Fatalf("got %d signals, want 1: %s", len(signals), describeSignals(signals))
			}
			if got := classifyReinforcement(signals[0]); got != tc.want {
				t.Errorf("feedback %q classified %q, want %q", signals[0].FeedbackText, got, tc.want)
			}
		})
	}
}

func describeSignals(signals []reinforcementSignal) string {
	out := ""
	for _, s := range signals {
		out += fmt.Sprintf("[%q] ", s.FeedbackText)
	}
	return out
}
