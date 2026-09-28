package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBriefFactCacheFlushFailureKeepsPacketAndFactsRepairable(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	facts, err := loadFacts(fixture.brainDir, "feature")
	if err != nil || len(facts) != 1 {
		t.Fatalf("fixture facts=%+v err=%v", facts, err)
	}
	factsPath := filepath.Join(fixture.brainDir, filepath.FromSlash(factsFileRelPath("feature")))
	before := fileDigest(t, factsPath)
	cacheDir := filepath.Join(fixture.brainDir, filepath.FromSlash(factsBranchRelDir("feature")), embedStoreDirName)
	original := defaultEmbedder()
	fake := &fakeFusionEmbedder{}
	defaultEmbedderInst = fake
	t.Cleanup(func() { defaultEmbedderInst = original })
	faulted := false
	var faultErr error
	fake.fail = func(string) bool {
		if !faulted {
			faulted = true
			// Emulate cache storage becoming unavailable during an external embedding
			// call, after read preflight. Only this fixture's derived cache is replaced.
			if faultErr = os.RemoveAll(cacheDir); faultErr == nil {
				faultErr = os.WriteFile(cacheDir, []byte("cache parent unavailable"), 0o600)
			}
		}
		return false
	}
	run := func(profilePath string, wantErrors int) {
		t.Helper()
		packet, err := execute(t, NewRootCommand(fixture.opts), "brief", "PRIVATE_FACT_PAYLOAD", "--json", "--profile-json", profilePath)
		if err != nil {
			t.Fatalf("cache failure escaped best-effort boundary: %v\n%s", err, packet)
		}
		var report brainBriefJSONReport
		if err := json.Unmarshal([]byte(packet), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Facts) != 1 || report.Facts[0].ID != facts[0].ID {
			t.Fatalf("cache failure lost fact: %+v", report.Facts)
		}
		data, err := os.ReadFile(profilePath)
		if err != nil {
			t.Fatal(err)
		}
		var profile brainBriefProfile
		if err := json.Unmarshal(data, &profile); err != nil {
			t.Fatal(err)
		}
		if !profile.Facts.CacheFlush.Invoked || profile.Facts.CacheFlush.ErrorCount != wantErrors {
			t.Fatalf("cache flush profile=%+v want errors=%d", profile.Facts.CacheFlush, wantErrors)
		}
		if fileDigest(t, factsPath) != before {
			t.Fatal("cache publication changed durable facts")
		}
	}
	run(filepath.Join(t.TempDir(), "failed-flush.json"), 1)
	if !faulted || faultErr != nil {
		t.Fatalf("cache fault injected=%v err=%v", faulted, faultErr)
	}
	if data, err := os.ReadFile(cacheDir); err != nil || string(data) != "cache parent unavailable" {
		t.Fatalf("cache obstruction changed: %q %v", data, err)
	}
	fake.fail = nil
	if err := os.Remove(cacheDir); err != nil {
		t.Fatal(err)
	}
	run(filepath.Join(t.TempDir(), "repaired-flush.json"), 0)
	store := newVectorStore(fixture.brainDir, "feature", factEmbeddingModelID(fake.ID()), fake.Dim())
	if vectors := store.load(); len(vectors) != 1 || len(vectors[facts[0].ID]) != fake.Dim() {
		t.Fatalf("retry failed to publish exact fact vector: %+v", vectors)
	}
}
