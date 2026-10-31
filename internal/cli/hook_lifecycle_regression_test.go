package cli

import (
	"testing"
	"time"
)

func TestHookSessionEndDoesNotQueueWork(t *testing.T) {
	opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
	old := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { t.Error("session-end launched worker"); return nil }
	t.Cleanup(func() { memoryWorkerLaunch = old })
	before := privacyTreeDigest(t, brainDir)
	for _, args := range [][]string{
		{"hook", "session-end", "--session", "ended-session", "--agent", "none"},
		{"memory", "notify", "--event", "session_end", "--session", "ended-session"},
	} {
		stdout, stderr, err := executeSplit(t, NewRootCommand(opts), args...)
		if err != nil || stdout != "" || stderr != "" {
			t.Fatalf("legacy hook: %q %q %v", stdout, stderr, err)
		}
	}
	if privacyTreeDigest(t, brainDir) != before {
		t.Fatal("session-end changed stored state")
	}
}

func TestHookSessionEndWorkerOriginDoesNotRecurse(t *testing.T) {
	opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
	t.Setenv(memoryWorkerOriginEnv, "1")
	old := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { t.Error("worker-origin hook recursively launched worker"); return nil }
	t.Cleanup(func() { memoryWorkerLaunch = old })
	before := privacyTreeDigest(t, brainDir)
	stdout, stderr, err := executeSplit(t, NewRootCommand(opts), "hook", "session-end", "--session", "ended-session")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("worker-origin hook: stdout=%s stderr=%s err=%v", stdout, stderr, err)
	}
	if privacyTreeDigest(t, brainDir) != before {
		t.Fatal("worker-origin hook modified state")
	}
}
