package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFactsGCRootPreservesDryRunAndPrunesOnlyEligibleRecords(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	opts, brainDir := privacyRegressionCommandFixture(t, now)
	active := factFor(t, "active", []string{"project.tooling.stack"}, now)
	retracted := factFor(t, "retracted", []string{"project.tooling.stack"}, now)
	retracted.Status = factStatusRetracted
	old := factFor(t, "old superseded", []string{"project.tooling.stack"}, now.Add(-31*24*time.Hour))
	old.Status = factStatusSuperseded
	recent := factFor(t, "recent superseded", []string{"project.tooling.stack"}, now.Add(-24*time.Hour))
	recent.Status = factStatusSuperseded
	orphan := factFor(t, "active orphan", []string{"gone.sub.type"}, now)
	if err := writeFacts(brainDir, "main", []factRecord{active, retracted, old, recent, orphan}); err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(brainDir, "other", []factRecord{retracted}); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("other")))
	otherBefore, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	before := privacyTreeDigest(t, brainDir)
	for _, jsonOut := range []bool{false, true} {
		args := []string{"facts", "gc", "--retain", "720h"}
		if jsonOut {
			args = append(args, "--json")
		}
		out, err := execute(t, NewRootCommand(opts), args...)
		if err != nil {
			t.Fatal(err)
		}
		if jsonOut {
			assertFactsGCSummary(t, out, false, 2, 1, 3)
		} else if !strings.Contains(out, "would prune 2 fact(s)") || !strings.Contains(out, "re-run with --force") {
			t.Fatalf("dry-run output: %s", out)
		}
		if after := privacyTreeDigest(t, brainDir); after != before {
			t.Fatal("dry run modified persisted state")
		}
	}
	out, err := execute(t, NewRootCommand(opts), "facts", "gc", "--force", "--json")
	if err != nil {
		t.Fatal(err)
	}
	assertFactsGCSummary(t, out, true, 2, 1, 3)
	kept, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, f := range kept {
		ids[f.ID] = true
	}
	want := map[string]bool{active.ID: true, recent.ID: true, orphan.ID: true}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("kept identities=%v want %v", ids, want)
	}
	otherAfter, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(otherBefore) != string(otherAfter) {
		t.Fatal("gc modified an unselected branch")
	}
	out, err = execute(t, NewRootCommand(opts), "facts", "gc", "--force")
	if err != nil || !strings.Contains(out, "pruned 0 fact(s)") {
		t.Fatalf("repeat gc: %q err=%v", out, err)
	}
}

func assertFactsGCSummary(t *testing.T, out string, applied bool, pruned, orphans, remaining int) {
	t.Helper()
	var result struct {
		Branch                     string
		Applied                    bool
		Pruned, Orphans, Remaining int
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Branch != "main" || result.Applied != applied || result.Pruned != pruned || result.Orphans != orphans || result.Remaining != remaining {
		t.Fatalf("unexpected GC summary: %+v", result)
	}
}

func TestFactsGCRootFailsClosedOnMalformedStore(t *testing.T) {
	opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("main")))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const malformed = "{not valid json}\n"
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		args := []string{"facts", "gc", "--json"}
		if force {
			args = append(args, "--force")
		}
		stdout, _, err := executeSplit(t, NewRootCommand(opts), args...)
		if err == nil || stdout != "" {
			t.Fatalf("corrupt store reported success: %q err=%v", stdout, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != malformed {
			t.Fatalf("corrupt evidence changed: %q err=%v", got, err)
		}
	}
}
