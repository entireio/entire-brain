package cli

import "testing"

func TestReinforcementRejectsFalseCues(t *testing.T) {
	for _, x := range []struct{ text, want string }{{"Perfect, no need to revert anything.", reinforcementSuccess}, {"This is imperfect.", reinforcementNeutral}, {"I cannot say that it works.", reinforcementNeutral}, {"Now add rollback support.", reinforcementNeutral},
		{"Now add a test for it works.", reinforcementNeutral},
		{"Please add a try again button.", reinforcementNeutral},
		{"Do not undo that change.", reinforcementNeutral},
		{"Not exactly what I needed.", reinforcementNeutral},
		{"Thanks but I need to check it.", reinforcementNeutral},
		{"Revert this commit.", reinforcementCorrected},
		{"It works.", reinforcementSuccess}} {
		got := classifyReinforcement(reinforcementSignal{FeedbackText: x.text})
		if got != x.want {
			t.Errorf("false cue: %q => %q", x.text, got)
		}
		t.Logf("%q => %s", x.text, got)
	}
}
