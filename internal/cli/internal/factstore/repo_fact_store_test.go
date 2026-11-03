package factstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func repoStoreFact(text, branch string) factmerge.Record {
	paths := []string{"project.memory.storage"}
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return factmerge.Record{ID: factmerge.RecordID(text, paths), Text: text, Paths: paths, Branch: branch,
		Origin: OriginAuthored, Status: factmerge.StatusActive, CreatedAt: at, UpdatedAt: at,
		Provenance: []factmerge.Anchor{{SessionID: "session-" + branch, Commit: "commit-" + branch}}}
}

// writeLegacyFile plants a legacy branch NDJSON file the way pre-flat layouts
// and released clients do. The dir name is arbitrary: folding reads every
// facts/<dir>/facts.ndjson regardless of the slug that named it.
func writeLegacyFile(t *testing.T, root, dir string, records []factmerge.Record) {
	t.Helper()
	var b bytes.Buffer
	if err := factmerge.WriteNDJSON(&b, records); err != nil {
		t.Fatal(err)
	}
	rel := filepath.ToSlash(filepath.Join(DirName, dir, FileName))
	if err := brainstore.WriteBrainRelativeFileAtomic(root, rel, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadByID(t *testing.T, root string) map[string]factmerge.Record {
	t.Helper()
	records, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]factmerge.Record, len(records))
	for _, rec := range records {
		out[rec.ID] = rec
	}
	return out
}

func legacyDirsOnDisk(t *testing.T, root string) []string {
	t.Helper()
	dirs, err := legacyBranchDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func parkedDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, DirName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var parked []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), legacyParkPrefix) {
			parked = append(parked, entry.Name())
		}
	}
	return parked
}

func TestFlatStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	a := repoStoreFact("first memory", "main")
	b := repoStoreFact("second memory", "feature")
	if err := Write(root, []factmerge.Record{a, b}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(got), got)
	}
	byID := loadByID(t, root)
	if byID[a.ID].Text != a.Text || byID[a.ID].Branch != "main" ||
		len(byID[a.ID].Provenance) != 1 || byID[a.ID].Provenance[0].SessionID != "session-main" {
		t.Fatalf("payload fields lost: %+v", byID[a.ID])
	}
	if byID[b.ID].Branch != "feature" {
		t.Fatalf("branch metadata lost: %+v", byID[b.ID])
	}
}

func TestFlatStoreLoadMissingIsEmpty(t *testing.T) {
	records, err := Load(t.TempDir())
	if err != nil || len(records) != 0 {
		t.Fatalf("missing store should be empty: %v, %v", records, err)
	}
}

func TestFlatStoreForwardVersionFailsClosed(t *testing.T) {
	root := t.TempDir()
	forward := []byte(`{"schema_version":99,"records":{}}` + "\n")
	if err := brainstore.WriteBrainRelativeFileAtomic(root, RepositoryPath, forward, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("forward-version load must fail closed, got %v", err)
	}
	if err := Write(root, []factmerge.Record{repoStoreFact("clobber", "main")}); err == nil {
		t.Fatal("write must refuse to replace a newer store")
	}
	if _, err := MigrateRepository(root); err == nil {
		t.Fatal("migration must refuse a newer store")
	}
	after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(RepositoryPath)))
	if err != nil || !bytes.Equal(after, forward) {
		t.Fatalf("a newer store was modified: %v", err)
	}
}

func TestFlatStoreCorruptionFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := brainstore.WriteBrainRelativeFileAtomic(root, RepositoryPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("corrupt store must fail closed")
	}

	// A key standing over another record's body is a substitution: the fact
	// the key names is gone, and the store must say so rather than serve it.
	root = t.TempDir()
	a := repoStoreFact("kept memory", "main")
	b := repoStoreFact("overwritten memory", "main")
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	store := fmt.Sprintf(`{"schema_version":3,"records":{%q:%s,%q:%s}}`, a.ID, raw, b.ID, raw)
	if err := brainstore.WriteBrainRelativeFileAtomic(root, RepositoryPath, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "carries id") {
		t.Fatalf("substituted record must fail closed, got %v", err)
	}
}

func TestFlatStoreWriteRefusesRecordWithoutID(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, []factmerge.Record{{Text: "no id"}}); err == nil {
		t.Fatal("a record without an id is unaddressable and must be refused")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(RepositoryPath))); !os.IsNotExist(err) {
		t.Fatalf("refused write must not create a store: %v", err)
	}
}

func TestFlatStoreWriteParksLegacyDirs(t *testing.T) {
	root := t.TempDir()
	legacy := repoStoreFact("legacy memory", "main")
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{legacy})

	// The caller's contract: load (which folds legacy), mutate, write.
	records, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	fresh := repoStoreFact("fresh memory", "main")
	if err := Write(root, append(records, fresh)); err != nil {
		t.Fatal(err)
	}
	if dirs := legacyDirsOnDisk(t, root); len(dirs) != 0 {
		t.Fatalf("legacy dirs must be parked after a write: %v", dirs)
	}
	if parked := parkedDirs(t, root); len(parked) != 1 {
		t.Fatalf("legacy dir must be parked, not deleted: %v", parked)
	}
	byID := loadByID(t, root)
	if _, ok := byID[legacy.ID]; !ok {
		t.Fatalf("legacy record lost: %+v", byID)
	}
	if _, ok := byID[fresh.ID]; !ok {
		t.Fatalf("fresh record lost: %+v", byID)
	}
}

func TestFlatStoreLoadFoldsLegacyWithoutWriting(t *testing.T) {
	root := t.TempDir()
	marker := []byte(`{"schema_version":2,"storage":"legacy_views"}` + "\n")
	if err := brainstore.WriteBrainRelativeFileAtomic(root, RepositoryPath, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	shared := repoStoreFact("shared memory", "main")
	mirrored := shared
	mirrored.Branch = "feature"
	mirrored.Provenance = []factmerge.Anchor{{SessionID: "session-feature", Commit: "commit-feature"}}
	other := repoStoreFact("feature memory", "feature")
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{shared})
	writeLegacyFile(t, root, "feature-def456", []factmerge.Record{mirrored, other})

	byID := loadByID(t, root)
	if len(byID) != 2 {
		t.Fatalf("expected the shared id to fold once: %+v", byID)
	}
	if got := byID[shared.ID]; len(got.Provenance) != 2 {
		t.Fatalf("fold must union provenance across copies: %+v", got.Provenance)
	}

	// Load is read-only: the marker and the legacy files are untouched.
	after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(RepositoryPath)))
	if err != nil || !bytes.Equal(after, marker) {
		t.Fatalf("load mutated the store file: %v", err)
	}
	if dirs := legacyDirsOnDisk(t, root); len(dirs) != 2 {
		t.Fatalf("load must not move legacy dirs: %v", dirs)
	}
}

func TestFlatStoreSchema2MarkerValidation(t *testing.T) {
	root := t.TempDir()
	if err := brainstore.WriteBrainRelativeFileAtomic(root, RepositoryPath, []byte(`{"schema_version":2,"storage":"something_else"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "storage mode") {
		t.Fatalf("invalid marker must fail closed, got %v", err)
	}
}

func TestRepositoryStatusReportsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	writeLegacyFile(t, root, "main-abc123", []factmerge.Record{repoStoreFact("legacy memory", "main")})
	status, err := RepositoryStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Records != 1 || status.Views != 1 || !status.Compatibility || status.Format != "legacy" {
		t.Fatalf("legacy status wrong: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(RepositoryPath))); !os.IsNotExist(err) {
		t.Fatalf("status must not write the store: %v", err)
	}
	if _, err := MigrateRepository(root); err != nil {
		t.Fatal(err)
	}
	status, err = RepositoryStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Records != 1 || status.Views != 0 || status.Compatibility || status.Format != "repository" || status.SchemaVersion != repoFactsVersion {
		t.Fatalf("migrated status wrong: %+v", status)
	}
}

// Randomized no-loss walk: whatever mix of flat writes and legacy files lands
// on disk, every id ever written stays loadable, and migration never loses one.
func TestFlatStoreRandomizedLegacyAndFlatMutations(t *testing.T) {
	root := t.TempDir()
	rng := rand.New(rand.NewPCG(42, 73))
	wanted := map[string]bool{}
	for step := 0; step < 60; step++ {
		text := fmt.Sprintf("memory %d", rng.IntN(20))
		branch := fmt.Sprintf("branch-%d", rng.IntN(3))
		rec := repoStoreFact(text, branch)
		rec.UpdatedAt = rec.UpdatedAt.Add(time.Duration(step) * time.Minute)
		wanted[rec.ID] = true
		if rng.IntN(2) == 0 {
			// One dir per write: a real legacy writer appends to its branch
			// file, and this fixture must never overwrite earlier records.
			writeLegacyFile(t, root, fmt.Sprintf("dir-%d", step), []factmerge.Record{rec})
		} else {
			records, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := Write(root, append(records, rec)); err != nil {
				t.Fatal(err)
			}
		}
		if step%7 == 0 {
			if _, err := MigrateRepository(root); err != nil {
				t.Fatal(err)
			}
		}
		byID := loadByID(t, root)
		for id := range wanted {
			if _, ok := byID[id]; !ok {
				t.Fatalf("step %d lost %s", step, id)
			}
		}
	}
	if _, err := MigrateRepository(root); err != nil {
		t.Fatal(err)
	}
	if got := len(loadByID(t, root)); got != len(wanted) {
		t.Fatalf("got %d records, want %d", got, len(wanted))
	}
}

func FuzzFlatStoreRoundTrip(f *testing.F) {
	f.Add("remember this", "main", 0)
	f.Add("remember that", "feature/x", 2)
	statuses := []string{factmerge.StatusActive, factmerge.StatusRetracted, factmerge.StatusCandidate, factmerge.StatusSuperseded}
	f.Fuzz(func(t *testing.T, text, branch string, statusIdx int) {
		if strings.TrimSpace(text) == "" {
			t.Skip()
		}
		root := t.TempDir()
		rec := repoStoreFact(text, branch)
		rec.Status = statuses[((statusIdx%len(statuses))+len(statuses))%len(statuses)]
		if err := Write(root, []factmerge.Record{rec}); err != nil {
			t.Fatal(err)
		}
		got, err := Load(root)
		if err != nil || len(got) != 1 {
			t.Fatalf("round trip: %v %v", got, err)
		}
		if got[0].ID != rec.ID || got[0].Text != rec.Text || got[0].Branch != rec.Branch || got[0].Status != rec.Status {
			t.Fatalf("round trip mismatch: %+v vs %+v", got[0], rec)
		}
	})
}

// --- released-binary coverage (scripts/test-memory-storage-compat.sh) --------
//
// These exercise the one-way door against REAL released binaries: a pre-flat
// client keeps reading and writing legacy branch files — even after migration,
// because its write path appends before its manifest check fails — and the
// flat store folds whatever it wrote. CLI-level behavior of the CURRENT binary
// (recall output, command surface) is covered by the released-compat suite in
// internal/cli, not here.

func TestRepoFactStoreReleasedBinary(t *testing.T) {
	if os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_VERSION") == "v0.1.0" {
		t.Skip("v0.1.0 has no global memory store; repository compatibility is tested separately")
	}
	binary := os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_BINARY")
	if binary == "" {
		t.Skip("run scripts/test-memory-storage-compat.sh for real released-binary coverage")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	env := append(os.Environ(), "ENTIRE_PLUGIN_DATA_DIR="+data, "ENTIRE_PLUGIN_CACHE_DIR="+filepath.Join(root, "cache"), "ENTIRE_PLUGIN_STATE_DIR="+filepath.Join(root, "state"), "ENTIRE_PLUGIN_CONFIG_DIR="+filepath.Join(root, "config"))
	run := func(check func(error, []byte) bool, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir, cmd.Env = root, env
		out, err := cmd.CombinedOutput()
		if check == nil && err != nil {
			t.Fatalf("released CLI %v: %v\n%s", args, err, out)
		}
		if check != nil && !check(err, out) {
			t.Fatalf("released CLI %v unexpected result: %v\n%s", args, err, out)
		}
		return out
	}
	var old factmerge.Record
	if err := json.Unmarshal(run(nil, "remember", "old binary memory", "--global", "--path", "project.memory.storage", "--json"), &old); err != nil {
		t.Fatal(err)
	}
	run(nil, "memories", "retract", old.ID, "--global", "--json")
	brain := filepath.Join(data, globalStoreDirName)

	// Pre-migration: the fold sees the released client's write AND its
	// retraction (retirement wins over nothing here; it is simply the record).
	byID := loadByID(t, brain)
	if got, ok := byID[old.ID]; !ok || got.Status != factmerge.StatusRetracted {
		t.Fatalf("released write/retraction not folded: %+v", byID)
	}

	summary, err := MigrateRepository(brain)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Folded == 0 || len(summary.MovedDirs) == 0 {
		t.Fatalf("migration did not fold the released client's store: %+v", summary)
	}

	// Post-migration: the released client's next write is a straggler in a
	// fresh legacy file (its own layout), and the standing reconcile folds it.
	// Its command may fail on the manifest step AFTER writing; both outcomes
	// leave the record on disk (verified against v0.2.0 and v0.4.0).
	run(func(error, []byte) bool { return true }, "remember", "straggler memory", "--global", "--path", "project.memory.storage", "--json")
	if dirs := legacyDirsOnDisk(t, brain); len(dirs) == 0 {
		t.Skip("this released client stopped writing legacy files post-migration; straggler path not reachable")
	}
	summary, err = MigrateRepository(brain)
	if err != nil {
		t.Fatal(err)
	}
	if summary.StragglerFiles == 0 {
		t.Fatalf("straggler write was not reported: %+v", summary)
	}
	byID = loadByID(t, brain)
	found := false
	for _, rec := range byID {
		if rec.Text == "straggler memory" {
			found = true
		}
	}
	if !found {
		t.Fatalf("straggler memory not folded: %+v", byID)
	}
}

func TestRepoFactStoreReleasedMixedProcesses(t *testing.T) {
	if os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_VERSION") == "v0.1.0" {
		t.Skip("v0.1.0 has no global memory store; repository compatibility is tested separately")
	}
	old := os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_BINARY")
	if old == "" {
		t.Skip("run scripts/test-memory-storage-compat.sh")
	}
	root := t.TempDir()
	env := append(os.Environ(), "ENTIRE_PLUGIN_DATA_DIR="+filepath.Join(root, "data"), "ENTIRE_PLUGIN_CACHE_DIR="+filepath.Join(root, "cache"), "ENTIRE_PLUGIN_STATE_DIR="+filepath.Join(root, "state"), "ENTIRE_PLUGIN_CONFIG_DIR="+filepath.Join(root, "config"))
	const writers = 8
	brain := filepath.Join(root, "data", globalStoreDirName)
	for i := range writers {
		// Interleave released-client writes with flat-store writes the way a
		// half-upgraded machine does. Serial on purpose: the released client
		// serializes on the brain write lock it takes itself; the flat write
		// here stands in for the flipped CLI, whose callers hold that lock.
		if i%2 == 0 {
			cmd := exec.Command(old, "remember", fmt.Sprintf("mixed writer %d", i), "--global", "--path", "project.memory.storage", "--json")
			cmd.Dir, cmd.Env = root, env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("writer %d: %v: %s", i, err, out)
			}
			continue
		}
		records, err := Load(brain)
		if err != nil {
			t.Fatal(err)
		}
		if err := Write(brain, append(records, repoStoreFact(fmt.Sprintf("mixed writer %d", i), GlobalBranch))); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(loadByID(t, brain)); got != writers {
		t.Fatalf("lost mixed-process writes: %d of %d", got, writers)
	}
	for range 2 {
		if _, err := MigrateRepository(brain); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(loadByID(t, brain)); got != writers {
		t.Fatalf("migration lost mixed-process writes: %d of %d", got, writers)
	}
}

func TestRepoFactStoreReleasedRepositoryVisibility(t *testing.T) {
	old := os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_BINARY")
	if old == "" {
		t.Skip("run scripts/test-memory-storage-compat.sh")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "ENTIRE_REPO_ROOT="+repo, "ENTIRE_PLUGIN_DATA_DIR="+filepath.Join(root, "data"), "ENTIRE_PLUGIN_CACHE_DIR="+filepath.Join(root, "cache"), "ENTIRE_PLUGIN_STATE_DIR="+filepath.Join(root, "state"), "ENTIRE_PLUGIN_CONFIG_DIR="+filepath.Join(root, "config"), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	run := func(binary string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(binary), args, err, out)
		}
		return out
	}
	run("git", "init", "-q", "-b", "main")
	run("git", "-c", "user.name=Memory Test", "-c", "user.email=memory@example.invalid", "commit", "-q", "--allow-empty", "-m", "fixture")
	run("git", "remote", "add", "origin", "https://github.com/entireio/memory-store-compat-fixture.git")
	run("git", "branch", "feature")
	var a, b factmerge.Record
	if err := json.Unmarshal(run(old, "remember", "repository main assertion", "--path", "project.memory.storage", "--json"), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run(old, "remember", "repository feature assertion", "--branch", "feature", "--path", "project.memory.storage", "--json"), &b); err != nil {
		t.Fatal(err)
	}
	run(old, "facts", "retract", a.ID, "--branch", "main", "--json")

	// Memories are repo-wide in the flat store: both branches' records fold
	// into one set, and the released client's retraction survives the fold.
	brain := strings.TrimSpace(string(run(old, "path")))
	dataDir := filepath.Join(root, "data")
	rel, err := filepath.Rel(dataDir, brain)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("released client reported a brain outside the fixture data directory: %q (relative path %q, error %v)", brain, rel, err)
	}
	if info, err := os.Stat(brain); err != nil || !info.IsDir() {
		t.Fatalf("released client reported an unreadable brain directory %q: %v", brain, err)
	}
	byID := loadByID(t, brain)
	if got, ok := byID[a.ID]; !ok || got.Status != factmerge.StatusRetracted {
		t.Fatalf("main-branch retraction not folded: %+v", byID)
	}
	if got, ok := byID[b.ID]; !ok || got.Status != factmerge.StatusActive || got.Branch != "feature" {
		t.Fatalf("feature-branch record not folded with branch metadata: %+v", byID)
	}
	summary, err := MigrateRepository(brain)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Folded < 2 {
		t.Fatalf("migration folded too little: %+v", summary)
	}
	after := loadByID(t, brain)
	if len(after) != len(byID) {
		t.Fatalf("migration changed the record set: %d vs %d", len(after), len(byID))
	}
}
