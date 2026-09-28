package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRefreshCommandUpdatesFactEvidenceWithoutTranscriptChanges(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	// Stub only external Git/checkpoint discovery; refresh writes and reads real
	// transcripts, fact files, manifests and the published SQLite corpus.
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "branch", "--show-current")] = fakeCommandResponse{stdout: "main\n"}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all")] = fakeCommandResponse{stdout: `[{"checkpoint_id":"aaa111aaa111","date":"2026-01-01T00:00:00Z","is_logs_only":true}]`}
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111")] = fakeCommandResponse{stdout: `{"checkpoint_id":"aaa111aaa111","branch":"main","checkpoints_count":1,"session_count":1,"sessions":[{"index":0,"session_id":"session-one","agent":"Claude","branch":"main","created_at":"2026-01-01T00:00:00Z"}]}`}
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--raw-transcript", "--session-index", "0", "aaa111aaa111")] = fakeCommandResponse{stdout: claudeReviewTranscript}
	opts := Options{
		Version: "test", Runner: runner,
		Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()},
		Now: func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	}
	refresh := func() {
		t.Helper()
		out, stderr, err := executeSplit(t, NewRootCommand(opts), "refresh", "--entire-binary", "entire-test", "--semantic=false", "--agent", "none", "--raw")
		if err != nil || strings.Contains(stderr, "warning: pattern corpus:") {
			t.Fatalf("refresh failed: %v\nstdout=%s\nstderr=%s", err, out, stderr)
		}
	}
	refresh()
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	brainDir := storage.BrainDir
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) != 1 {
		t.Fatalf("missing persisted session: %+v", manifest.Sources)
	}
	transcript := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Sessions.Sessions[0].TranscriptPath))
	original, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(transcript)
	if err != nil {
		t.Fatal(err)
	}
	assertUnchanged := func() {
		t.Helper()
		got, err := os.ReadFile(transcript)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(transcript)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) || !info.ModTime().Equal(originalInfo.ModTime()) {
			t.Fatal("refresh changed transcript; fact-only invalidation was not exercised")
		}
	}
	links := func() []string {
		t.Helper()
		db := openCorpus(t, brainDir)
		defer db.Close()
		rows, err := db.Query(`SELECT DISTINCT fact_id FROM episode_facts ORDER BY fact_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	fact := factRecord{ID: "fact:original", Text: "review release readiness requires staging", Paths: []string{"release.validation"}, Branch: "main", Status: "active", Origin: "distilled"}
	if err := writeFacts(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	refresh()
	assertUnchanged()
	if got := links(); !reflect.DeepEqual(got, []string{fact.ID}) {
		t.Fatalf("initial public refresh did not link active fact: %v", got)
	}
	fact.Status = "retracted"
	if err := writeFacts(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	if got := links(); !reflect.DeepEqual(got, []string{fact.ID}) {
		t.Fatalf("fixture changed corpus before refresh: %v", got)
	}
	refresh()
	assertUnchanged()
	if got := links(); len(got) != 0 {
		t.Fatalf("public refresh retained retracted corroboration: %v", got)
	}
	fact.ID = "fact:replacement"
	fact.Status = "active"
	if err := writeFacts(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	if got := links(); len(got) != 0 {
		t.Fatalf("fixture linked replacement before refresh: %v", got)
	}
	refresh()
	assertUnchanged()
	if got := links(); !reflect.DeepEqual(got, []string{fact.ID}) {
		t.Fatalf("public refresh did not link new fact to unchanged transcript: %v", got)
	}
}
