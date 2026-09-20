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

func TestReinforcementConversationalPrefixesPreserveCorrections(t *testing.T) {
	for _, text := range []string{"Now that's wrong", "Now that’s wrong", "Can you revert that?", "Could you undo that change?", "Next, please try again", "Can you now revert this commit?"} {
		for _, committed := range []bool{false, true} {
			if got := classifyReinforcement(reinforcementSignal{FeedbackText: text, WorkCommitted: committed}); got != reinforcementCorrected {
				t.Errorf("%q committed=%v: got %s, want corrected", text, committed, got)
			}
		}
	}
	for _, text := range []string{"Can you add a try again button?", "Could you document how it works?", "Now please add rollback support", "Next add a test for it works", "Can you not revert that?", "Now do not undo that change"} {
		if got := classifyReinforcement(reinforcementSignal{FeedbackText: text}); got != reinforcementNeutral {
			t.Errorf("%q: got %s, want neutral", text, got)
		}
	}
}

func TestReinforcementStandaloneRevertRequests(t *testing.T) {
	for _, text := range []string{"Revert", "revert!", "Please revert.", "Can you revert?", "Could you please revert?", "Now revert", "Revert, please", "Roll back.", "Please rollback"} {
		for _, committed := range []bool{false, true} {
			if got := classifyReinforcement(reinforcementSignal{FeedbackText: text, WorkCommitted: committed}); got != reinforcementCorrected {
				t.Errorf("%q committed=%v: got %s, want corrected", text, committed, got)
			}
		}
	}
	for _, text := range []string{"Do not revert", "Please don't revert", "Can you not revert?", "No need to revert", "Now add revert support", "Please document revert", "Can you add a revert button?", "The command is revert", "Revert support is useful", "Should I revert?"} {
		if got := classifyReinforcement(reinforcementSignal{FeedbackText: text}); got != reinforcementNeutral {
			t.Errorf("%q: got %s, want neutral", text, got)
		}
	}
}

func TestReinforcementTaskRequestPreservesLaterCorrection(t *testing.T) {
	for _, text := range []string{"Please implement caching properly, this still doesn't work", "Add the missing cache; that's wrong", "Write the handler again. You missed the fallback", "Create the replacement, it does not work", "Document the fix, still failing", "Please implement caching properly because this still doesn’t work", "Please implement caching, and this still does not work", "Please implement caching\nthis still does not work"} {
		for _, committed := range []bool{false, true} {
			if got := classifyReinforcement(reinforcementSignal{FeedbackText: text, WorkCommitted: committed}); got != reinforcementCorrected {
				t.Errorf("%q committed=%v: got %s", text, committed, got)
			}
		}
	}
	for _, text := range []string{"Add a button, label it try again", "Write docs, explain why the example says it doesn't work", "Implement the message 'this still doesn't work'", "Add caching, this is not a reason to revert that", "Document the example, do not revert that", "Add a test, this should never say it works", `Add the label "retry, and this still does not work"`, "Add the label 'retry, and this still doesn't work'", "Implement an example because this should not say it works"} {
		if got := classifyReinforcement(reinforcementSignal{FeedbackText: text}); got != reinforcementNeutral {
			t.Errorf("task mention/negation %q: got %s", text, got)
		}
	}
}
