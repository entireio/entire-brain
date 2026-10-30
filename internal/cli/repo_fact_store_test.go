package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func repoStoreFact(text, branch string) factRecord {
	paths := []string{"project.memory.storage"}
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return factRecord{ID: factRecordID(text, paths), Text: text, Paths: paths, Branch: branch,
		Origin: factOriginAuthored, Status: factStatusActive, CreatedAt: at, UpdatedAt: at,
		Provenance: []factAnchor{{SessionID: "session-" + branch, Commit: "commit-" + branch}}}
}

func writeLegacyView(t *testing.T, root, branch string, records []factRecord) {
	t.Helper()
	var b bytes.Buffer
	if err := factmerge.WriteNDJSON(&b, records); err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(root, factsFileRelPath(branch), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireRepoView(t *testing.T, root, branch string, want []factRecord) {
	t.Helper()
	got, err := loadFacts(root, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d records, want %d", branch, len(got), len(want))
	}
	if len(want) > 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %#v, want %#v", branch, got, want)
	}
}

func TestRepoFactStoreMigrationPreservesViewsAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	main := repoStoreFact("shared statement", "main")
	feature := main
	feature.Status = factmerge.StatusRetracted
	feature.Provenance = []factAnchor{{SessionID: "experiment", CheckpointID: "checkpoint"}}
	feature.Author = "teammate"
	feature.SupersededBy = "replacement"
	feature.UpdatedAt = feature.UpdatedAt.Add(time.Hour)
	candidate := repoStoreFact("unreviewed", "deleted/branch")
	candidate.Status = factmerge.StatusCandidate
	writeLegacyView(t, root, "main", []factRecord{main})
	// The directory is the view; record.Branch is deliberately different.
	writeLegacyView(t, root, "feature", []factRecord{feature, candidate})
	writeLegacyView(t, root, "empty", nil)
	legacy, _ := os.ReadFile(filepath.Join(root, factsFileRelPath("feature")))
	if err := migrateRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	requireRepoView(t, root, "main", []factRecord{main})
	want := []factRecord{feature, candidate}
	sortFactRecords(want)
	requireRepoView(t, root, "feature", want)
	requireRepoView(t, root, "empty", nil)
	s, present, err := readRepoFactStore(root)
	if err != nil || !present || len(s.Records) != 2 || len(s.Views) != 3 {
		t.Fatalf("migration did not consolidate identities: %+v, %v", s, err)
	}
	before, _ := os.ReadFile(filepath.Join(root, repoFactsPath))
	if err := migrateRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, repoFactsPath))
	if !bytes.Equal(before, after) {
		t.Fatal("migration is not a fixed point")
	}
	stillLegacy, _ := os.ReadFile(filepath.Join(root, factsFileRelPath("feature")))
	if !bytes.Equal(legacy, stillLegacy) {
		t.Fatal("migration changed the old binary's data")
	}
}

func TestRepoFactStoreCoreWithoutCompatibility(t *testing.T) {
	root := t.TempDir()
	s := emptyRepoFactStore()
	a := repoStoreFact("one", "main")
	b := a
	b.Branch = "experiment"
	b.Provenance = nil
	b.RelatedIDs = []string{"related"}
	for dir, records := range map[string][]factRecord{"main": {a}, "experiment": {b}} {
		if err := s.replaceView(dir, records, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRepoFactStore(root, &s); err != nil {
		t.Fatal(err)
	}
	roundTrip, _, err := readRepoFactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := roundTrip.view("experiment")
	if err != nil || !reflect.DeepEqual(got, []factRecord{b}) || len(roundTrip.Records) != 1 {
		t.Fatalf("core round trip: %+v, %v", got, err)
	}
	if err := roundTrip.replaceView("main", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := writeRepoFactStore(root, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if len(roundTrip.Records) != 1 {
		t.Fatal("removing one view removed another view's body")
	}
	if err := roundTrip.replaceView("experiment", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := writeRepoFactStore(root, &roundTrip); err != nil || len(roundTrip.Records) != 0 {
		t.Fatalf("unreferenced bodies survived compaction: %v", err)
	}
}

func TestRepoFactStoreInterruptedCommitRecoversFromLegacyRedo(t *testing.T) {
	root := t.TempDir()
	old := repoStoreFact("before", "main")
	if err := writeFacts(root, "main", []factRecord{old}); err != nil {
		t.Fatal(err)
	}
	next := repoStoreFact("after", "main")
	injected := errors.New("injected disk failure after projection commit")
	err := commitRepoFactView(root, repoFactViewDir("main"), []factRecord{old, next}, func(string, *repoFactStore) error { return injected })
	if !errors.Is(err, injected) {
		t.Fatalf("failure was hidden: %v", err)
	}
	want := []factRecord{old, next}
	sortFactRecords(want)
	requireRepoView(t, root, "main", want)
	if err := migrateRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, factsFileRelPath("main"))); err != nil {
		t.Fatal(err)
	}
	requireRepoView(t, root, "main", want)
}

func TestRepoFactStoreProjectionDeletionAndExplicitDeletion(t *testing.T) {
	root := t.TempDir()
	a := repoStoreFact("retained body", "main")
	if err := writeFacts(root, "main", []factRecord{a}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, factsFileRelPath("main"))); err != nil {
		t.Fatal(err)
	}
	requireRepoView(t, root, "main", []factRecord{a})
	writeLegacyView(t, root, "main", nil)
	requireRepoView(t, root, "main", nil)
	if err := migrateRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	s, _, _ := readRepoFactStore(root)
	if len(s.Records) != 0 {
		t.Fatal("explicit legacy deletion retained the body")
	}
}

func TestRepoFactStoreLegacyRetirementReplayAndReauthor(t *testing.T) {
	root := t.TempDir()
	a := repoStoreFact("lifecycle", "main")
	retired := a
	retired.Status = factmerge.StatusRetracted
	retired.UpdatedAt = a.UpdatedAt.Add(time.Hour)
	if err := writeFacts(root, "main", []factRecord{retired}); err != nil {
		t.Fatal(err)
	}
	writeLegacyView(t, root, "main", []factRecord{a})
	requireRepoView(t, root, "main", []factRecord{retired})
	a.UpdatedAt = retired.UpdatedAt.Add(time.Hour)
	writeLegacyView(t, root, "main", []factRecord{a})
	requireRepoView(t, root, "main", []factRecord{a})
}

func TestRepoFactStoreRejectsUnsafeOrForwardStateBeforeWritingLegacy(t *testing.T) {
	for _, kind := range []string{"future", "unknown", "corrupt", "dangling", "missing-version", "missing-records", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			a := repoStoreFact("do not overwrite", "main")
			writeLegacyView(t, root, "main", []factRecord{a})
			path := filepath.Join(root, repoFactsPath)
			switch kind {
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				data := map[string]string{
					"future":          `{"schema_version":999,"records":{},"views":{}}`,
					"unknown":         `{"schema_version":1,"records":{},"views":{},"new_field":true}`,
					"corrupt":         `{`,
					"dangling":        `{"schema_version":1,"records":{},"views":{"main":{"entries":[{"id":"missing"}]}}}`,
					"missing-version": `{"records":{},"views":{}}`,
					"missing-records": `{"schema_version":1,"views":{}}`,
				}[kind]
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(filepath.Join(root, factsFileRelPath("main")))
			if err := writeFacts(root, "main", nil); err == nil {
				t.Fatal("unsafe store was overwritten")
			}
			after, _ := os.ReadFile(filepath.Join(root, factsFileRelPath("main")))
			if !bytes.Equal(before, after) {
				t.Fatal("refused write changed legacy data")
			}
		})
	}
}

func TestRepoFactStoreRollbackPreservesNewAndOldWrites(t *testing.T) {
	root := t.TempDir()
	a, b := repoStoreFact("old", "main"), repoStoreFact("new", "feature")
	writeLegacyView(t, root, "main", []factRecord{a})
	if err := writeFacts(root, "feature", []factRecord{b}); err != nil {
		t.Fatal(err)
	}
	a.Status = factmerge.StatusRetracted
	a.UpdatedAt = a.UpdatedAt.Add(time.Hour)
	writeLegacyView(t, root, "main", []factRecord{a})
	if err := exportLegacyRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, repoFactsPath)); !os.IsNotExist(err) {
		t.Fatalf("snapshot survived rollback: %v", err)
	}
	requireRepoView(t, root, "main", []factRecord{a})
	requireRepoView(t, root, "feature", []factRecord{b})
	if err := migrateRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	requireRepoView(t, root, "main", []factRecord{a})
	requireRepoView(t, root, "feature", []factRecord{b})
}

func TestRepoFactStoreConcurrentViewsDoNotLoseUpdates(t *testing.T) {
	root := t.TempDir()
	const workers = 12
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			branch := fmt.Sprintf("worktree-%d", i)
			errs <- writeFacts(root, branch, []factRecord{repoStoreFact(branch, branch)})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s, _, err := readRepoFactStore(root)
	if err != nil || len(s.Records) != workers || len(s.Views) != workers {
		t.Fatalf("lost concurrent writes: %d records, %v", len(s.Records), err)
	}
}

// Run via scripts/test-memory-storage-compat.sh, which builds a pinned released
// binary. Ordinary unit tests do not fetch/build historical dependencies.
func TestRepoFactStoreReleasedBinary(t *testing.T) {
	binary := os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_BINARY")
	if binary == "" {
		t.Skip("run scripts/test-memory-storage-compat.sh for real released-binary coverage")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	env := append(os.Environ(), "ENTIRE_PLUGIN_DATA_DIR="+data, "ENTIRE_PLUGIN_CACHE_DIR="+filepath.Join(root, "cache"), "ENTIRE_PLUGIN_STATE_DIR="+filepath.Join(root, "state"), "ENTIRE_PLUGIN_CONFIG_DIR="+filepath.Join(root, "config"))
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir, cmd.Env = root, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("released CLI %v: %v\n%s", args, err, out)
		}
		return out
	}
	var old factRecord
	if err := json.Unmarshal(run("remember", "old binary memory", "--global", "--path", "project.memory.storage", "--json"), &old); err != nil {
		t.Fatal(err)
	}
	brain := filepath.Join(data, globalStoreDirName)
	if err := migrateRepoFacts(brain); err != nil {
		t.Fatal(err)
	}
	fresh := repoStoreFact("new binary memory", globalFactBranch)
	if err := writeFacts(brain, globalFactBranch, []factRecord{old, fresh}); err != nil {
		t.Fatal(err)
	}
	out := run("memories", "global", "--json")
	if !strings.Contains(string(out), fresh.ID) {
		t.Fatalf("old reader missed new memory: %s", out)
	}
	run("memories", "retract", old.ID, "--global", "--json")
	got, err := loadFacts(brain, globalFactBranch)
	if err != nil {
		t.Fatal(err)
	}
	idx := indexOfFact(got, old.ID)
	if idx < 0 || got[idx].Status != factmerge.StatusRetracted {
		t.Fatalf("old retraction lost: %+v", got)
	}
	if err := exportLegacyRepoFacts(brain); err != nil {
		t.Fatal(err)
	}
	run("remember", "after downgrade", "--global", "--path", "project.memory.storage", "--json")
	if err := migrateRepoFacts(brain); err != nil {
		t.Fatal(err)
	}
	got, err = loadFacts(brain, globalFactBranch)
	if err != nil || len(got) != 3 {
		t.Fatalf("upgrade/downgrade lost records: %+v, %v", got, err)
	}
}

func TestRepoFactStoreRandomizedLegacyAndNativeMutations(t *testing.T) {
	root := t.TempDir()
	rng := rand.New(rand.NewPCG(42, 73))
	model := map[string][]factRecord{}
	for step := 0; step < 100; step++ {
		branch := fmt.Sprintf("branch-%d", rng.IntN(4))
		records := append([]factRecord(nil), model[branch]...)
		if len(records) > 0 && rng.IntN(4) == 0 {
			records = records[1:]
		} else {
			rec := repoStoreFact(fmt.Sprintf("shared-%d", rng.IntN(8)), branch)
			rec.UpdatedAt = rec.UpdatedAt.Add(time.Duration(step+1) * time.Hour)
			rec.Status = []string{factmerge.StatusActive, factmerge.StatusRetracted, factmerge.StatusCandidate, factmerge.StatusSuperseded}[rng.IntN(4)]
			if i := indexOfFact(records, rec.ID); i >= 0 {
				records[i] = rec
			} else {
				records = append(records, rec)
			}
		}
		sortFactRecords(records)
		model[branch] = records
		if rng.IntN(2) == 0 {
			writeLegacyView(t, root, branch, records)
		} else if err := writeFacts(root, branch, records); err != nil {
			t.Fatal(err)
		}
		for view, want := range model {
			requireRepoView(t, root, view, want)
		}
		if step%7 == 0 {
			if err := migrateRepoFacts(root); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := migrateRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	s, _, err := readRepoFactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[string]bool{}
	for _, records := range model {
		for _, rec := range records {
			distinct[rec.ID] = true
		}
	}
	if len(s.Records) != len(distinct) {
		t.Fatalf("duplicate/orphan bodies: got %d want %d", len(s.Records), len(distinct))
	}
}

func TestRepoFactStorePrivacyPurgesMirrorsAndMissingProjections(t *testing.T) {
	root := t.TempDir()
	a := repoStoreFact("private unique statement", "main")
	for _, view := range []string{"main", "feature"} {
		if err := writeFacts(root, view, []factRecord{a}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(root, factsFileRelPath("feature"))); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, factsDirName, ".store.json.tmp-crashed")
	if err := os.WriteFile(orphan, []byte(a.Text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := purgeSessionFacts(root, "session-main", nil); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"main", "feature"} {
		requireRepoView(t, root, view, nil)
	}
	raw, err := os.ReadFile(filepath.Join(root, repoFactsPath))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(a.Text)) || bytes.Contains(raw, []byte("session-main")) {
		t.Fatal("privacy deletion left content in native representatives/overrides")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("purge retained interrupted snapshot: %v", err)
	}
}

func TestRepoFactStoreBundleKeepsLegacyContract(t *testing.T) {
	root := t.TempDir()
	if err := writeFacts(root, "main", []factRecord{repoStoreFact("bundle", "main")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, factsDirName, ".store.json.tmp-crashed"), []byte("private partial native snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, layer := range brainBundleLayers {
		if layer.name != bundleLayerFacts {
			continue
		}
		paths, err := collectBrainBundleLayerPaths(root, layer)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || filepath.ToSlash(paths[0]) != factsFileRelPath("main") {
			t.Fatalf("bundle leaks native storage or drops old data: %v", paths)
		}
		if err := os.Remove(filepath.Join(root, paths[0])); err != nil {
			t.Fatal(err)
		}
		paths, err = collectBrainBundleLayerPaths(root, layer)
		if err != nil || len(paths) != 1 {
			t.Fatalf("missing projection lost export: %v, %v", paths, err)
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := addBundlePath(tw, root, paths[0]); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(&buf)
		if _, err := tr.Next(); err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(tr)
		if err != nil || !bytes.Contains(payload, []byte(`"text":"bundle"`)) {
			t.Fatalf("missing projection exported wrong bytes: %s, %v", payload, err)
		}
	}
}

func TestRepoFactStoreCrashAfterProjection(t *testing.T) {
	if root := os.Getenv("ENTIRE_BRAIN_TEST_CRASH_STORE"); root != "" {
		a := repoStoreFact("survives process death", "main")
		_ = commitRepoFactView(root, repoFactViewDir("main"), []factRecord{a}, func(string, *repoFactStore) error { os.Exit(23); return nil })
		os.Exit(24)
	}
	root := t.TempDir()
	if err := writeFacts(root, "main", nil); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRepoFactStoreCrashAfterProjection$")
	cmd.Env = append(os.Environ(), "ENTIRE_BRAIN_TEST_CRASH_STORE="+root)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("child did not reach crash boundary: %v", err)
	}
	requireRepoView(t, root, "main", []factRecord{repoStoreFact("survives process death", "main")})
	if err := migrateRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	if err := exportLegacyRepoFacts(root); err != nil {
		t.Fatal(err)
	}
	requireRepoView(t, root, "main", []factRecord{repoStoreFact("survives process death", "main")})
}

func TestRepoFactStoreReleasedMixedProcesses(t *testing.T) {
	old, current := os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_BINARY"), os.Getenv("ENTIRE_BRAIN_TEST_BINARY")
	if old == "" || current == "" {
		t.Skip("run scripts/test-memory-storage-compat.sh")
	}
	root := t.TempDir()
	env := append(os.Environ(), "ENTIRE_PLUGIN_DATA_DIR="+filepath.Join(root, "data"), "ENTIRE_PLUGIN_CACHE_DIR="+filepath.Join(root, "cache"), "ENTIRE_PLUGIN_STATE_DIR="+filepath.Join(root, "state"), "ENTIRE_PLUGIN_CONFIG_DIR="+filepath.Join(root, "config"))
	const writers = 8
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			binary := current
			if i%2 == 0 {
				binary = old
			}
			cmd := exec.Command(binary, "remember", fmt.Sprintf("mixed writer %d", i), "--global", "--path", "project.memory.storage", "--json")
			cmd.Dir, cmd.Env = root, env
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("writer %d: %w: %s", i, err, out)
			} else {
				errs <- nil
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	brain := filepath.Join(root, "data", globalStoreDirName)
	got, err := loadFacts(brain, globalFactBranch)
	if err != nil || len(got) != writers {
		t.Fatalf("lost mixed-process writes: %d, %v", len(got), err)
	}
	for _, action := range []string{"migrate", "rollback", "rollback", "migrate"} {
		cmd := exec.Command(current, "memories", "storage", action, "--global", "--json")
		cmd.Dir, cmd.Env = root, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", action, err, out)
		}
	}
	got, err = loadFacts(brain, globalFactBranch)
	if err != nil || len(got) != writers {
		t.Fatalf("lost rollback writes: %d, %v", len(got), err)
	}
}

func TestRepoFactStoreReleasedRepositoryVisibility(t *testing.T) {
	old, current := os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_BINARY"), os.Getenv("ENTIRE_BRAIN_TEST_BINARY")
	if old == "" || current == "" {
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
	var a, b factRecord
	if err := json.Unmarshal(run(old, "remember", "repository main assertion", "--path", "project.memory.storage", "--json"), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run(current, "remember", "repository feature assertion", "--branch", "feature", "--path", "project.memory.storage", "--json"), &b); err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{old, current} {
		out := run(binary, "recall", "--branch", "main", "--no-global", "--json")
		if !bytes.Contains(out, []byte(a.ID)) || bytes.Contains(out, []byte(b.ID)) {
			t.Fatalf("main visibility changed: %s", out)
		}
		out = run(binary, "recall", "--branch", "feature", "--no-global", "--json")
		if !bytes.Contains(out, []byte(b.ID)) || bytes.Contains(out, []byte(a.ID)) {
			t.Fatalf("feature visibility changed: %s", out)
		}
	}
	run(old, "memories", "retract", a.ID, "--branch", "main", "--json")
	out := run(current, "recall", "--branch", "main", "--no-global", "--json")
	if bytes.Contains(out, []byte(a.ID)) {
		t.Fatalf("old retraction leaked into new recall: %s", out)
	}
	run(current, "memories", "storage", "rollback", "--json")
	out = run(old, "recall", "--branch", "feature", "--no-global", "--json")
	if !bytes.Contains(out, []byte(b.ID)) {
		t.Fatalf("downgrade lost feature memory: %s", out)
	}
	run(current, "memories", "storage", "migrate", "--json")
}

func FuzzRepoFactStoreRoundTrip(f *testing.F) {
	f.Add("memory", "author", "session", true)
	f.Add("", "", "", false)
	f.Fuzz(func(t *testing.T, text, author, session string, retired bool) {
		if len(text)+len(author)+len(session) > 4096 {
			t.Skip()
		}
		a := repoStoreFact(text, "main")
		b := a
		b.Author, b.Provenance = author, []factAnchor{{SessionID: session}}
		if retired {
			b.Status = factmerge.StatusRetracted
		}
		// JSON normalizes invalid UTF-8; the public write path rejects it.
		var buf bytes.Buffer
		if err := factmerge.WriteNDJSON(&buf, []factRecord{a, b}); err != nil {
			return
		}
		s := emptyRepoFactStore()
		if err := s.replaceView("a", []factRecord{a}, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.replaceView("b", []factRecord{b}, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.compact(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var decoded repoFactStore
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		got, err := decoded.view("b")
		if err != nil || !reflect.DeepEqual(got, []factRecord{b}) {
			t.Fatalf("view changed: %+v, %v", got, err)
		}
	})
}

func BenchmarkRepoFactStoreSharedView(b *testing.B) {
	root := b.TempDir()
	s := emptyRepoFactStore()
	records := make([]factRecord, 1000)
	for i := range records {
		records[i] = repoStoreFact(fmt.Sprintf("shared observation %d", i), "main")
	}
	for i := range 16 {
		if err := s.replaceView(repoFactViewDir(fmt.Sprintf("branch-%d", i)), records, ""); err != nil {
			b.Fatal(err)
		}
	}
	if err := writeRepoFactStore(root, &s); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		got, err := loadFacts(root, "branch-0")
		if err != nil || len(got) != len(records) {
			b.Fatalf("read: %d, %v", len(got), err)
		}
	}
}
