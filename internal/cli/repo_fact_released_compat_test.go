package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// These tests execute real stable release binaries, not old serializer mocks.
// All state and network targets are isolated from the developer's environment.
type releasedMemoryFixture struct {
	t                        *testing.T
	root, repo, old, current string
	env                      []string
}

func newReleasedMemoryFixture(t *testing.T) *releasedMemoryFixture {
	t.Helper()
	old, current := os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_BINARY"), os.Getenv("ENTIRE_BRAIN_TEST_BINARY")
	if old == "" || current == "" {
		t.Skip("run scripts/test-memory-storage-compat.sh")
	}
	root := t.TempDir()
	f := &releasedMemoryFixture{t: t, root: root, repo: filepath.Join(root, "repo"), old: old, current: current}
	if err := os.MkdirAll(f.repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"PATH", "TMPDIR", "LANG", "SYSTEMROOT"} {
		if val, ok := os.LookupEnv(key); ok {
			f.env = append(f.env, key+"="+val)
		}
	}
	for key, value := range map[string]string{"ENTIRE_REPO_ROOT": f.repo, "ENTIRE_PLUGIN_DATA_DIR": filepath.Join(root, "data"), "ENTIRE_PLUGIN_CACHE_DIR": filepath.Join(root, "cache"), "ENTIRE_PLUGIN_STATE_DIR": filepath.Join(root, "state"), "ENTIRE_PLUGIN_CONFIG_DIR": filepath.Join(root, "config"), "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		f.env = append(f.env, key+"="+value)
	}
	f.run("git", "init", "-q", "-b", "main")
	f.run("git", "-c", "user.name=Compatibility Test", "-c", "user.email=compat@example.invalid", "commit", "-q", "--allow-empty", "-m", "fixture")
	f.run("git", "remote", "add", "origin", "https://github.com/entireio/memory-store-compat-fixture.git")
	f.run("git", "branch", "feature")
	return f
}

func (f *releasedMemoryFixture) call(binary string, args ...string) ([]byte, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = f.repo, f.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		f.t.Logf("%s %v: %v\n%s\n%s", filepath.Base(binary), args, err, out, stderr.String())
		err = fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out, err
}
func (f *releasedMemoryFixture) run(binary string, args ...string) []byte {
	f.t.Helper()
	out, err := f.call(binary, args...)
	if err != nil {
		f.t.Fatalf("command failed: %v", err)
	}
	return out
}
func (f *releasedMemoryFixture) remember(binary, text, branch string) factRecord {
	f.t.Helper()
	var rec factRecord
	if err := json.Unmarshal(f.run(binary, "remember", text, "--branch", branch, "--path", "project.memory.storage", "--json"), &rec); err != nil {
		f.t.Fatal(err)
	}
	return rec
}
func (f *releasedMemoryFixture) brain() string {
	f.t.Helper()
	return strings.TrimSpace(string(f.run(f.current, "path")))
}
func (f *releasedMemoryFixture) requireVisible(binary, branch string, present, absent []factRecord) {
	f.t.Helper()

	out := f.run(binary, "recall", "--branch", branch, "--k", "128", "--json")
	var result struct {
		Facts []factRecord `json:"facts"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		f.t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, rec := range result.Facts {
		ids[rec.ID] = true
	}
	for _, r := range present {
		if !ids[r.ID] {
			f.t.Fatalf("missing %s from recall: %s", r.ID, out)
		}
	}
	for _, r := range absent {
		if ids[r.ID] {
			f.t.Fatalf("unexpected %s in recall: %s", r.ID, out)
		}
	}

}

// Stable releases compare filesystem mtimes when verifying a regenerated scan
// cache. On coarse filesystem clocks, a clean rebuild can share the tombstone's
// timestamp. Retry the documented idempotent purge only for that exact collision;
// older caches, other privacy findings, and repeated failures remain fatal.
func (f *releasedMemoryFixture) purge(binary, brainDir, session string) {
	f.t.Helper()
	for attempt := 0; attempt < 5; attempt++ {
		_, err := f.call(binary, "privacy", "purge", session, "--json")
		if err == nil {
			return
		}
		cache, cacheErr := os.Stat(filepath.Join(brainDir, "history", "scan-cache.json.gz"))
		stone, stoneErr := os.Stat(filepath.Join(brainDir, "history", "tombstones.json"))
		collision := cacheErr == nil && stoneErr == nil && cache.ModTime().Equal(stone.ModTime())
		if !strings.Contains(err.Error(), "verification found 1 violations (first: derived_store history/scan-cache.json.gz does not postdate the newest tombstone;") || !collision || attempt == 4 {
			f.t.Fatalf("privacy purge failed (equal filesystem timestamps=%v): %v", collision, err)
		}
		f.t.Logf("retrying purge after equal cache/tombstone timestamps: %s", cache.ModTime())
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRepoFactStoreReleasedPrivacyDeletion(t *testing.T) {
	for _, mode := range []string{"legacy", "converted-old-purge", "converted-new-purge", "schema1-old-purge-new-repair"} {
		t.Run(mode, func(t *testing.T) {
			f := newReleasedMemoryFixture(t)
			rec := f.remember(f.old, "RELEASED-PRIVATE-CANARY-84719", "main")
			root := f.brain()
			rec.Provenance = []factAnchor{{SessionID: "private-session", Transcript: "sessions/main/private.jsonl"}}
			var data bytes.Buffer
			if err := factmerge.WriteNDJSON(&data, []factRecord{rec}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, factstore.FileRelPath("main")), data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			rel := "sessions/main/private.jsonl"
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, rel), []byte(`{"type":"user","message":{"role":"user","content":"`+rec.Text+`"}}`+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(root, "manifest.json")
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			m["sources"].(map[string]any)["sessions"] = map[string]any{"generated_at": "2026-10-30T00:00:00Z", "default_branch": "main", "transcript_mode": "raw", "scope": "all", "sessions": []any{map[string]any{"session_id": "private-session", "branch": "main", "agent": "claude-code", "latest_checkpoint_id": "cp1", "created_at": "2026-10-29T00:00:00Z", "transcript_path": rel}}}
			raw, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if mode != "legacy" {
				f.run(f.current, "memories", "storage", "migrate", "--json")
			}
			if mode == "schema1-old-purge-new-repair" {
				writeSchema1Fixture(t, root)
			}
			purger := f.old
			if mode == "converted-new-purge" {
				purger = f.current
			}
			f.purge(purger, root, "private-session")
			if mode == "schema1-old-purge-new-repair" {
				if out, err := f.call(f.current, "privacy", "verify", "--json"); err == nil || !bytes.Contains(out, []byte(`"clean": false`)) {
					t.Fatalf("hidden snapshot was not detected: %s, %v", out, err)
				}
				f.purge(f.current, root, "private-session")
			}
			if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if bytes.Contains(data, []byte(rec.Text)) {
					t.Errorf("purge left private body: %s", path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, binary := range []string{f.old, f.current} {
				f.run(binary, "privacy", "verify", "--json")
				f.requireVisible(binary, "main", nil, []factRecord{rec})
			}
		})
	}
}

func TestRepoFactStoreReleasedMemoryCommands(t *testing.T) {
	f := newReleasedMemoryFixture(t)
	a := f.remember(f.old, "old main memory", "main")
	b := f.remember(f.current, "feature memory for promotion", "feature")
	root := f.brain()
	// Include a schema 1 upgrade in the command lifecycle.
	writeSchema1Fixture(t, root)
	f.run(f.current, "memories", "storage", "migrate", "--json")
	for _, binary := range []string{f.old, f.current} {
		f.run(binary, "memories", "promote", "--from", "feature", "--into", "main", "--json")
		f.requireVisible(binary, "main", []factRecord{a, b}, nil)
		for _, args := range [][]string{{"brief", "memory storage", "--json"}, {"memories", "status", "--json"}, {"memories", "tree", "--json"}, {"memories", "map", "--json"}, {"memories", "review", "--json"}, {"memories", "vitality", "--json"}, {"memories", "files", "--dir", filepath.Join(f.root, "export"), "--json"}, {"memories", "gc", "--json"}} {
			f.run(binary, args...)
		}
	}
	// Import through the old CLI, then read and remove through the new CLI.
	importedPath := filepath.Join(f.root, "mem0.json")
	if err := os.WriteFile(importedPath, []byte(`[{"id":"foreign-1","memory":"imported compatibility assertion"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	f.run(f.old, "memories", "import", "--source", "mem0", "--file", importedPath, "--json")
	records, err := factstore.Load(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	var imported factRecord
	for _, rec := range records {
		if rec.Text == "imported compatibility assertion" {
			imported = rec
		}
	}
	if imported.ID == "" {
		t.Fatal("import did not persist")
	}
	for _, binary := range []string{f.current, f.old} {
		f.requireVisible(binary, "main", []factRecord{imported}, nil)
	}
	f.run(f.old, "memories", "retract", a.ID, "--json")
	f.run(f.current, "memories", "retract", imported.ID, "--json")
	f.run(f.old, "memories", "gc", "--force", "--json")
	for _, binary := range []string{f.old, f.current} {
		f.requireVisible(binary, "main", []factRecord{b}, []factRecord{a, imported})
	}
	f.run(f.current, "memories", "storage", "rollback", "--json")
	c := f.remember(f.old, "after downgrade", "main")
	f.run(f.current, "memories", "storage", "migrate", "--json")
	f.requireVisible(f.current, "main", []factRecord{b, c}, []factRecord{a, imported})
}

func TestRepoFactStoreReleasedHostedSync(t *testing.T) {
	f := newReleasedMemoryFixture(t)
	fake := &hostedFake{callerBy: "acct-caller", callerHandle: "caller"}
	srv := newHostedFakeServer(t, fake)
	// Multiple feed pages exercise shared cursors, not just an empty exchange.
	var remote factRecord
	for i := 0; i < 55; i++ {
		remote = hostedSyncTestFact("team memory "+strings.Repeat("x", i+1), time.Now())
		fake.seed(remote, "trail-team", "acct-team", "team")
	}
	a := f.remember(f.old, "old sync assertion", "main")
	f.remember(f.current, "feature local assertion", "feature")
	syncArgs := []string{"memories", "sync", "--memories-backend", "http", "--repo-id", "repo1", "--api-url", srv.URL, "--token", fakeJWT, "--json"}
	for _, binary := range []string{f.old, f.current, f.old} {
		f.run(binary, syncArgs...)
		if !fake.has(a.ID) {
			t.Fatal("sync did not push local record")
		}
		f.requireVisible(binary, "main", []factRecord{a, remote}, nil)
	}
	// Retraction must survive new-to-old and old-to-new exchanges.
	b := f.remember(f.current, "new sync assertion", "main")
	f.run(f.current, syncArgs...)
	f.run(f.old, "memories", "retract", b.ID, "--json")
	f.run(f.old, syncArgs...)
	f.run(f.current, syncArgs...)
	f.requireVisible(f.current, "main", []factRecord{a, remote}, []factRecord{b})
	f.run(f.current, "memories", "retract", a.ID, "--json")
	f.run(f.current, syncArgs...)
	f.run(f.old, syncArgs...)
	f.requireVisible(f.old, "main", []factRecord{remote}, []factRecord{a, b})
	// An independent repository must receive the server's lifecycle state too.
	fake.setTrail(a.ID, "trail-shared")
	fake.setTrail(b.ID, "trail-shared")
	for _, reader := range []string{f.old, f.current} {
		peer := newReleasedMemoryFixture(t)
		peer.remember(reader, "peer local memory", "main")
		peer.run(reader, syncArgs...)
		peer.requireVisible(reader, "main", []factRecord{remote}, []factRecord{a, b})
	}

	// Failure and retry must preserve readable local data across both clients.
	for _, binary := range []string{f.old, f.current} {
		fake.mu.Lock()
		fake.denyFeed = true
		fake.mu.Unlock()
		if _, err := f.call(binary, syncArgs...); err == nil {
			t.Fatal("pull failure was hidden")
		}
		f.requireVisible(binary, "main", []factRecord{remote}, []factRecord{a, b})
		// Acknowledged versions need no POST. Create a pending version so
		// both clients must exercise the denied-upload and retry paths.
		pending := f.remember(binary, "pending upload from "+filepath.Base(binary), "main")
		fake.mu.Lock()
		fake.denyFeed = false
		fake.denyPush = true
		fake.mu.Unlock()
		if _, err := f.call(binary, syncArgs...); err == nil {
			t.Fatal("push failure was hidden")
		}
		if fake.has(pending.ID) {
			t.Fatal("denied upload reached the server")
		}
		f.requireVisible(binary, "main", []factRecord{pending}, nil)
		fake.mu.Lock()
		fake.denyPush = false
		fake.mu.Unlock()
		f.run(binary, syncArgs...)
		if !fake.has(pending.ID) {
			t.Fatal("retry did not upload pending memory")
		}
	}
	f.run(f.current, "memories", "storage", "rollback", "--json")
	f.run(f.old, syncArgs...)
	f.run(f.current, "memories", "storage", "migrate", "--json")
	f.run(f.current, syncArgs...)
	f.requireVisible(f.current, "main", []factRecord{remote}, []factRecord{a, b})
}

func TestRepoFactStoreReleasedDistillOutlineAndReview(t *testing.T) {
	f := newReleasedMemoryFixture(t)
	f.remember(f.old, "authored initialization memory", "main")
	root := f.brain()
	fixture := writeDistillFixture(t, time.Now())
	source, err := bmanifest.Load(fixture)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := bmanifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions = source.Sources.Sessions
	if err := writeBrainManifestAndReadme(root, *manifest); err != nil {
		t.Fatal(err)
	}
	for _, s := range source.Sources.Sessions.Sessions {
		data, err := os.ReadFile(filepath.Join(fixture, s.TranscriptPath))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, s.TranscriptPath)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Deterministic agent responses test the released command/storage path.
	// No external model or credentials are used.
	response := filepath.Join(f.root, "agent-response.txt")
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{f.old, f.current} {
		if err := os.WriteFile(response, []byte("preferences.coding.style\tUse sentence case in documentation.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		f.run(binary, "distill", "--force", "--agent", "command", "--agent-command", cat, "--agent-command", response, "--json")
		var distilled factRecord
		records, err := factstore.Load(root, "main")
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range records {
			if rec.Text == "Use sentence case in documentation." {
				distilled = rec
			}
		}
		if distilled.Origin != factstore.OriginDistilled || len(distilled.Provenance) == 0 {
			t.Fatalf("distill lost body/provenance: %+v", distilled)
		}
		for _, reader := range []string{f.old, f.current} {
			f.requireVisible(reader, "main", []factRecord{distilled}, nil)
		}
		if err := os.WriteFile(response, []byte("Compatibility outline summary.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		f.run(binary, "memories", "outline", "--force", "--min-memories", "1", "--agent", "command", "--agent-command", cat, "--agent-command", response, "--json")
		for _, reader := range []string{f.old, f.current} {
			if out := f.run(reader, "memories", "map", "--json"); !bytes.Contains(out, []byte("Compatibility outline summary.")) {
				t.Fatalf("outline not shared: %s", out)
			}
		}
		// Resolve a persisted proposal with this client, then inspect using both.
		target := f.remember(binary, "review target "+filepath.Base(binary), "main")
		candidate := f.remember(binary, "review replacement "+filepath.Base(binary), "main")
		if err := writeFactProposals(root, "main", []factProposal{{Action: factActionSupersede, CandidateID: candidate.ID, TargetID: target.ID, Confidence: 0.5, Branch: "main"}}); err != nil {
			t.Fatal(err)
		}
		f.run(binary, "memories", "review", "--apply", candidate.ID, "--json")
		for _, reader := range []string{f.old, f.current} {
			f.requireVisible(reader, "main", []factRecord{candidate}, []factRecord{target})
		}
	}
}
