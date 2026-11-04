package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The store is repo-wide: a memory recorded while working on one branch is
// recalled from every other branch. (The pre-flat behavior — recall returning
// nothing on a fresh branch while other branches held memories — was a
// recorded gotcha, and this pins its fix.) An explicit --branch narrows to
// records stamped with that branch.
func TestRecallSeesMemoriesRecordedOnOtherBranches(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	opts, _, _ := rememberReviveEnv(t, now)

	out, err := execute(t, NewRootCommand(opts), "remember",
		"The flat store is repo-wide.", "--branch", "feat-a", "--path", "project.memory.storage", "--json")
	if err != nil {
		t.Fatalf("remember: %v\n%s", err, out)
	}

	recall := func(args ...string) []factRecord {
		t.Helper()
		out, err := execute(t, NewRootCommand(opts), append([]string{"recall", "flat store", "--k", "16", "--json"}, args...)...)
		if err != nil {
			t.Fatalf("recall: %v\n%s", err, out)
		}
		var result struct {
			Facts []factRecord `json:"facts"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("decode recall: %v\n%s", err, out)
		}
		return result.Facts
	}

	flat := recall()
	found := false
	for _, f := range flat {
		if strings.Contains(f.Text, "repo-wide") {
			found = true
			if f.Branch != "feat-a" {
				t.Fatalf("branch metadata lost: %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("a memory stamped feat-a must be recalled without a branch filter, got %d facts", len(flat))
	}

	if facts := recall("--branch", "feat-a"); len(facts) == 0 {
		t.Fatal("an explicit matching --branch filter must still return the memory")
	}
	if facts := recall("--branch", "feat-b"); len(facts) != 0 {
		t.Fatalf("a non-matching --branch filter must return nothing, got %d", len(facts))
	}
}
