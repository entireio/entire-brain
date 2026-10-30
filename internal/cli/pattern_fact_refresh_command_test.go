package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestRefreshCommandUpdatesFactEvidenceWithoutTranscriptChanges(t *testing.T) {
	opts, brainDir, repoDir := filelessCheckpointFixtureWithTranscript(t, claudeReviewTranscript)
	runner := opts.Runner
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
	brainDir = storage.BrainDir
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) != 1 {
		t.Fatalf("missing persisted session: %+v", manifest.Sources)
	}
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	original, err := readCanonicalHistoryTranscript(context.Background(), brainDir, rel)
	if err != nil {
		t.Fatal(err)
	}
	assertUnchanged := func() {
		t.Helper()
		got, err := readCanonicalHistoryTranscript(context.Background(), brainDir, rel)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatalf("checkpoint changed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(brainDir, rel)); !os.IsNotExist(err) {
			t.Fatalf("refresh materialized transcript: %v", err)
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
	if err := factstore.Write(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	refresh()
	assertUnchanged()
	if got := links(); !reflect.DeepEqual(got, []string{fact.ID}) {
		t.Fatalf("initial public refresh did not link active fact: %v", got)
	}
	fact.Status = "retracted"
	if err := factstore.Write(brainDir, "main", []factRecord{fact}); err != nil {
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
	if err := factstore.Write(brainDir, "main", []factRecord{fact}); err != nil {
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
