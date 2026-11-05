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

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

	args := []string{"recall", "--k", "128", "--json"}
	if binary == f.old {
		// The released client selects a branch store; the current build reads
		// the flat store and needs no branch at all.
		args = append(args, "--branch", branch)
	}
	out := f.run(binary, args...)
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

// legacyStorePath returns the released binary's branch store file under the
// old facts/<branch>-<hash>/facts.ndjson layout, creating the directory in a
// compatible shape when the released binary has not written one yet. Parked
// (.legacy-*) dirs never match: they are migration output, not live stores.
func (f *releasedMemoryFixture) legacyStorePath(root, branch string) string {
	f.t.Helper()
	prefix := strings.ReplaceAll(branch, "/", "-") + "-"
	entries, _ := os.ReadDir(filepath.Join(root, "facts"))
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && strings.HasPrefix(entry.Name(), prefix) {
			return filepath.Join(root, "facts", entry.Name(), "facts.ndjson")
		}
	}
	dir := filepath.Join(root, "facts", prefix+"compat00")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return filepath.Join(dir, "facts.ndjson")
}

// writeSchema1Fixture rebuilds the transient schema-1 store.json (records +
// views) from the live legacy branch files, the way the pre-release converter
// left it; the current build must still read and migrate that residue.
func writeSchema1Fixture(t *testing.T, root string) {
	t.Helper()
	records := map[string]factRecord{}
	selections := map[string]any{}
	entries, err := os.ReadDir(filepath.Join(root, "facts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, "facts", entry.Name(), "facts.ndjson"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		rows, err := factmerge.ParseNDJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		views := []map[string]string{}
		for _, rec := range rows {
			records[rec.ID] = rec
			views = append(views, map[string]string{"id": rec.ID})
		}
		selections[entry.Name()] = map[string]any{"entries": views}
	}
	payload, err := json.Marshal(map[string]any{"schema_version": 1, "records": records, "views": selections})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, factstore.RepositoryPath), payload, 0600); err != nil {
		t.Fatal(err)
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

// TestRepoFactStoreReleasedPrivacyDeletion pins the compatibility window for
// stores a RELEASED binary's `privacy purge` cleaned. The current binary has
// no privacy commands (deletion mirrors the checkpoint refs); what it must
// still guarantee is that an old purge's result stays clean through current
// reads and migrations, and that `memories storage migrate` removes the
// hidden schema 1 residue an old purge leaves behind.
func TestRepoFactStoreReleasedPrivacyDeletion(t *testing.T) {
	for _, mode := range []string{"legacy", "schema1-old-purge-new-migrate"} {
		t.Run(mode, func(t *testing.T) {
			f := newReleasedMemoryFixture(t)
			rec := f.remember(f.old, "RELEASED-PRIVATE-CANARY-84719", "main")
			root := f.brain()
			rec.Provenance = []factAnchor{{SessionID: "private-session", Transcript: "sessions/main/private.jsonl"}}
			var data bytes.Buffer
			if err := factmerge.WriteNDJSON(&data, []factRecord{rec}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.legacyStorePath(root, "main"), data.Bytes(), 0600); err != nil {
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
			if mode == "schema1-old-purge-new-migrate" {
				writeSchema1Fixture(t, root)
			}
			// Purges happen on the released layout BEFORE upgrading — after the
			// migration old binaries refuse to operate (the one-way door).
			f.purge(f.old, root, "private-session")
			if mode == "schema1-old-purge-new-migrate" {
				// The old purge cannot remove the hidden schema 1 snapshot
				// body; the current client's migration persists reconciliation
				// and removes the residue.
				f.run(f.current, "memories", "storage", "migrate", "--json")
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
			if mode == "legacy" {
				f.run(f.old, "privacy", "verify", "--json")
				f.requireVisible(f.old, "main", nil, []factRecord{rec})
			}
			f.requireVisible(f.current, "main", nil, []factRecord{rec})
		})
	}
}

func TestRepoFactStoreReleasedMemoryCommands(t *testing.T) {
	f := newReleasedMemoryFixture(t)
	a := f.remember(f.old, "old main memory", "main")
	b := f.remember(f.old, "feature memory for promotion", "feature")
	root := f.brain()
	// Include a schema 1 upgrade in the command lifecycle.
	writeSchema1Fixture(t, root)
	// promote and review were removed from the current build; the released
	// client still ships them, and the current build must read their writes.
	f.run(f.old, "memories", "promote", "--from", "feature", "--into", "main", "--json")
	for _, args := range [][]string{{"memories", "status", "--json"}, {"memories", "review", "--json"}, {"memories", "gc", "--json"}} {
		f.run(f.old, args...)
	}
	// Import through the old CLI, then read everywhere.
	importedPath := filepath.Join(f.root, "mem0.json")
	if err := os.WriteFile(importedPath, []byte(`[{"id":"foreign-1","memory":"imported compatibility assertion"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	f.run(f.old, "memories", "import", "--source", "mem0", "--file", importedPath, "--json")
	f.run(f.old, "memories", "retract", a.ID, "--json")
	f.run(f.old, "memories", "gc", "--force", "--json")

	// The current build folds every released-client write on its first
	// mutation (storage migrate here), and the whole surface reads it flat.
	f.run(f.current, "memories", "storage", "migrate", "--json")
	records, err := factstore.Load(root)
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
		t.Fatal("import did not persist through the migration")
	}
	f.requireVisible(f.current, "main", []factRecord{b, imported}, []factRecord{a})
	for _, args := range [][]string{{"brief", "memory storage", "--json"}, {"memories", "status", "--json"}, {"memories", "tree", "--json"}, {"memories", "map", "--json"}, {"memories", "vitality", "--json"}, {"memories", "files", "--dir", filepath.Join(f.root, "export"), "--json"}, {"memories", "gc", "--json"}} {
		f.run(f.current, args...)
	}
	f.run(f.current, "memories", "retract", imported.ID, "--json")
	f.run(f.current, "memories", "gc", "--force", "--json")
	f.requireVisible(f.current, "main", []factRecord{b}, []factRecord{a, imported})

	// The one-way door, half-open: a released client against the migrated
	// brain STILL WRITES a legacy branch file before its manifest check fails
	// (verified against the real binaries), and its reads warn and return
	// empty. The current build folds the straggler on its next mutation and
	// says so.
	// The released client reports the version refusal on stderr (carried in
	// the wrapped error), not stdout. Recall is checked BEFORE the straggler
	// write: the migrated corpus is invisible to the released client, but a
	// legacy branch file it half-writes afterwards is its own layout and its
	// recall does read that file back until the next fold parks it.
	recallOut, recallErr := f.call(f.old, "recall", "--branch", "main", "--k", "8", "--json")
	if combined := string(recallOut) + errText(recallErr); !strings.Contains(combined, "memory_unsupported_version") {
		t.Fatalf("released recall against a migrated brain must warn, got: %s", combined)
	}
	var recallResult struct {
		Facts []factRecord `json:"facts"`
	}
	if err := json.Unmarshal(recallOut, &recallResult); err == nil && len(recallResult.Facts) != 0 {
		t.Fatalf("released recall against a migrated brain must return no facts, got %d", len(recallResult.Facts))
	}
	out, rememberErr := f.call(f.old, "remember", "straggler after migration", "--branch", "main", "--path", "project.memory.storage", "--json")
	if combined := string(out) + errText(rememberErr); !strings.Contains(combined, "memory_unsupported_version") {
		t.Fatalf("released remember against a migrated brain must name the upgrade, got: %s", combined)
	}
	migrateOut := f.run(f.current, "memories", "storage", "migrate", "--json")
	var migrateResult struct {
		StragglerFiles int `json:"straggler_files"`
	}
	if err := json.Unmarshal(migrateOut, &migrateResult); err != nil {
		t.Fatal(err)
	}
	if migrateResult.StragglerFiles == 0 {
		t.Fatalf("the straggler write must be reported: %s", migrateOut)
	}
	straggled, err := factstore.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range straggled {
		if rec.Text == "straggler after migration" {
			found = true
		}
	}
	if !found {
		t.Fatal("the straggler write must be folded, not lost")
	}
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
	syncArgs := []string{"memories", "sync", "--memories-backend", "http", "--repo-id", "repo1", "--api-url", srv.URL, "--token", fakeJWT, "--json"}

	// Phase 1: the released client alone, including its failure/retry paths —
	// everything an old installation does BEFORE upgrading.
	a := f.remember(f.old, "old sync assertion", "main")
	f.run(f.old, syncArgs...)
	if !fake.has(a.ID) {
		t.Fatal("released sync did not push the local record")
	}
	f.requireVisible(f.old, "main", []factRecord{a, remote}, nil)
	fake.mu.Lock()
	fake.denyFeed = true
	fake.mu.Unlock()
	if _, err := f.call(f.old, syncArgs...); err == nil {
		t.Fatal("pull failure was hidden by the released client")
	}
	f.requireVisible(f.old, "main", []factRecord{a, remote}, nil)
	oldPending := f.remember(f.old, "pending upload from the released client", "main")
	fake.mu.Lock()
	fake.denyFeed = false
	fake.denyPush = true
	fake.mu.Unlock()
	if _, err := f.call(f.old, syncArgs...); err == nil {
		t.Fatal("push failure was hidden by the released client")
	}
	if fake.has(oldPending.ID) {
		t.Fatal("denied upload reached the server")
	}
	fake.mu.Lock()
	fake.denyPush = false
	fake.mu.Unlock()
	f.run(f.old, syncArgs...)
	if !fake.has(oldPending.ID) {
		t.Fatal("released retry did not upload the pending memory")
	}

	// Phase 2: the upgrade. The current build's first sync migrates the brain
	// (one-way door: the manifest advances past the released schema), keeps
	// everything the released client synced, and pushes its own writes.
	b := f.remember(f.current, "new sync assertion", "main")
	f.run(f.current, syncArgs...)
	if !fake.has(b.ID) {
		t.Fatal("current sync did not push the local record")
	}
	f.requireVisible(f.current, "main", []factRecord{a, b, oldPending, remote}, nil)

	// Retirements travel the wire both ways across the upgrade boundary: the
	// server's lifecycle state wins on pull, local retirements publish on push.
	f.run(f.current, "memories", "retract", b.ID, "--json")
	f.run(f.current, syncArgs...)
	f.requireVisible(f.current, "main", []factRecord{a, remote}, []factRecord{b})

	// An independent repository on either client receives the server's
	// lifecycle state.
	fake.setTrail(a.ID, "trail-shared")
	fake.setTrail(b.ID, "trail-shared")
	for _, reader := range []string{f.old, f.current} {
		peer := newReleasedMemoryFixture(t)
		peer.remember(reader, "peer local memory", "main")
		peer.run(reader, syncArgs...)
		// a is active on the server and carries the trail stamp, so the peer
		// receives it; b's retraction is the lifecycle state under test.
		peer.requireVisible(reader, "main", []factRecord{remote, a}, []factRecord{b})
	}

	// Phase 3: the current build's own failure/retry paths on the migrated
	// brain.
	fake.mu.Lock()
	fake.denyFeed = true
	fake.mu.Unlock()
	if _, err := f.call(f.current, syncArgs...); err == nil {
		t.Fatal("pull failure was hidden")
	}
	f.requireVisible(f.current, "main", []factRecord{a, remote}, []factRecord{b})
	pending := f.remember(f.current, "pending upload from the current client", "main")
	fake.mu.Lock()
	fake.denyFeed = false
	fake.denyPush = true
	fake.mu.Unlock()
	if _, err := f.call(f.current, syncArgs...); err == nil {
		t.Fatal("push failure was hidden")
	}
	if fake.has(pending.ID) {
		t.Fatal("denied upload reached the server")
	}
	f.requireVisible(f.current, "main", []factRecord{pending}, nil)
	fake.mu.Lock()
	fake.denyPush = false
	fake.mu.Unlock()
	f.run(f.current, syncArgs...)
	if !fake.has(pending.ID) {
		t.Fatal("retry did not upload pending memory")
	}
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
	// Splice the sessions source as raw JSON: writeBrainManifestAndReadme
	// stamps the CURRENT schema, which would close the one-way door before the
	// released client's phase.
	manifestPath := filepath.Join(root, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	sessionsJSON, err := json.Marshal(source.Sources.Sessions)
	if err != nil {
		t.Fatal(err)
	}
	var sessionsAny any
	if err := json.Unmarshal(sessionsJSON, &sessionsAny); err != nil {
		t.Fatal(err)
	}
	m["sources"].(map[string]any)["sessions"] = sessionsAny
	if raw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
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
		// Before the current build writes, both clients can read; after its
		// first write the manifest advances past the released schema and the
		// released client reads warn-and-empty (pinned elsewhere).
		readers := []string{f.old, f.current}
		if binary == f.current {
			readers = []string{f.current}
		}
		if err := os.WriteFile(response, []byte("preferences.coding.style\tUse sentence case in documentation.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		f.run(binary, "distill", "--force", "--agent", "command", "--agent-command", cat, "--agent-command", response, "--json")
		var distilled factRecord
		records, err := factstore.Load(root)
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
		for _, reader := range readers {
			f.requireVisible(reader, "main", []factRecord{distilled}, nil)
		}
		if err := os.WriteFile(response, []byte("Compatibility outline summary.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		f.run(binary, "memories", "outline", "--force", "--min-memories", "1", "--agent", "command", "--agent-command", cat, "--agent-command", response, "--json")
		for _, reader := range readers {
			if out := f.run(reader, "memories", "map", "--json"); !bytes.Contains(out, []byte("Compatibility outline summary.")) {
				t.Fatalf("outline not shared: %s", out)
			}
		}
		// Resolve a persisted proposal with the released client (the current
		// build removed the review queue), then inspect using both readers.
		if binary == f.old {
			target := f.remember(binary, "review target "+filepath.Base(binary), "main")
			candidate := f.remember(binary, "review replacement "+filepath.Base(binary), "main")
			queue := fmt.Sprintf(`{"action":%q,"candidate_id":%q,"target_id":%q,"confidence":0.5,"branch":"main"}`+"\n",
				factActionSupersede, candidate.ID, target.ID)
			queuePath := filepath.Join(filepath.Dir(f.legacyStorePath(root, "main")), "proposals.ndjson")
			if err := os.WriteFile(queuePath, []byte(queue), 0600); err != nil {
				t.Fatal(err)
			}
			f.run(binary, "memories", "review", "--apply", candidate.ID, "--json")
			for _, reader := range readers {
				f.requireVisible(reader, "main", []factRecord{candidate}, []factRecord{target})
			}
		}
	}
}
