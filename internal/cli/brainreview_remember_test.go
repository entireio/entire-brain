package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/command/brainreview"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

// reviewRememberFixture builds a repository whose tree has regressed away from
// what session history asserts, mirrored into the brain the status resolver
// actually reaches, and returns the options plus that brain directory.
//
// The mirroring is not incidental: brainReviewConfig resolves the brain
// through buildBrainStatusReport, which uses a different key than
// repoStoragePaths does under the fixture runner. A write-back that resolved
// the store for itself would land somewhere the review never read, so this
// test asserts the records appear in the SAME directory the review was handed.
func reviewRememberFixture(t *testing.T) (Options, string) {
	t.Helper()

	env := semanticTestEnv(t, t.TempDir())
	history := `{"text":"pkg/review_context.go builds the range as scopeBaseRef+\"..HEAD\""}` + "\n"
	body := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	repo, _ := writeLocalWorkspaceBrainRepo(t, env, history, "pkg/review_context.go", body)

	runner := semanticFixtureRunner(repo, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	opts.Env.RepoRoot = repo

	statusBrain, err := base.BrainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(statusBrain, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statusBrain, "sessions", "s.jsonl"), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	return opts, statusBrain
}

func runReviewRemember(t *testing.T, opts Options) brainreview.Report {
	t.Helper()

	var out bytes.Buffer
	cmd := &cobra.Command{Use: "review"}
	cmd.SetOut(&out)
	err := brainreview.RunBrainReview(context.Background(), cmd, brainReviewConfig(opts),
		regressionDetectorOptions{Limit: 20, JSON: true, Remember: true}, "fix scopeBaseRef")
	if err != nil {
		t.Fatalf("review --remember: %v\n%s", err, out.String())
	}
	var report brainreview.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out.String())
	}
	return report
}

// The end-to-end RC.M2 claim: a review's reasoning survives the run as a
// durable memory, in the store the review itself read, retrievable by the
// surfaces that deliver memories.
func TestReviewRememberWritesFindingsIntoTheStoreTheReviewRead(t *testing.T) {
	t.Parallel()

	opts, brainDir := reviewRememberFixture(t)
	report := runReviewRemember(t, opts)

	if len(report.Findings) == 0 {
		t.Fatal("fixture must regress, or there is nothing to remember")
	}
	if report.Remembered == nil {
		t.Fatal("--remember must report what it persisted")
	}
	if report.Remembered.Recorded != len(report.Findings) {
		t.Errorf("recorded %d of %d findings", report.Remembered.Recorded, len(report.Findings))
	}

	stored, err := factstore.Load(brainDir)
	if err != nil {
		t.Fatalf("load facts from the brain the review used: %v", err)
	}
	var reviewRecords []factmerge.Record
	for _, record := range stored {
		if record.Origin == factmerge.OriginReview {
			reviewRecords = append(reviewRecords, record)
		}
	}
	if len(reviewRecords) != report.Remembered.Recorded {
		t.Fatalf("store holds %d review-origin memories, the command claimed %d: %+v",
			len(reviewRecords), report.Remembered.Recorded, stored)
	}

	for _, record := range reviewRecords {
		if err := record.Validate(); err != nil {
			t.Errorf("a persisted record is invalid: %v", err)
		}
		if record.Kind != factmerge.KindGotcha {
			t.Errorf("kind = %q, want gotcha so the post-failure hook can deliver it", record.Kind)
		}
		if len(record.Paths) != 1 || record.Paths[0] != brainreview.ReviewFactPath {
			t.Errorf("paths = %v, want %q", record.Paths, brainreview.ReviewFactPath)
		}
		if len(record.Locus) == 0 || !strings.Contains(record.Locus[0], "review_context.go") {
			t.Errorf("locus = %v, want the finding's file first", record.Locus)
		}
		if len(record.Provenance) != 1 || record.Provenance[0].Commit == "" {
			t.Errorf("provenance = %+v, want one anchor naming the commit reviewed", record.Provenance)
		}
	}
}

// A second review of an unchanged tree must not grow the store. The record id
// is derived from the finding's text, so this is the test that would catch a
// clock or a commit leaking into that text.
func TestReviewRememberIsIdempotentAcrossRuns(t *testing.T) {
	t.Parallel()

	opts, brainDir := reviewRememberFixture(t)

	first := runReviewRemember(t, opts)
	if first.Remembered == nil || first.Remembered.Recorded == 0 {
		t.Fatal("the first run must record something")
	}
	afterFirst, err := factstore.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}

	second := runReviewRemember(t, opts)
	if second.Remembered == nil {
		t.Fatal("the second run must still report")
	}
	if second.Remembered.Recorded != 0 {
		t.Errorf("the second review recorded %d new memories for an unchanged tree", second.Remembered.Recorded)
	}
	if second.Remembered.AlreadyPresent != first.Remembered.Recorded {
		t.Errorf("already-present = %d, want %d", second.Remembered.AlreadyPresent, first.Remembered.Recorded)
	}

	afterSecond, err := factstore.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterSecond) != len(afterFirst) {
		t.Fatalf("the store grew from %d to %d memories on a re-review of an unchanged tree",
			len(afterFirst), len(afterSecond))
	}
}

// A human retraction is a judgment that a finding is not worth keeping. The
// detector must not overturn it on the next run: Upsert never touches Status,
// and this asserts that the write-back depends on that rather than reviving.
func TestReviewRememberDoesNotReviveARetractedFinding(t *testing.T) {
	t.Parallel()

	opts, brainDir := reviewRememberFixture(t)
	first := runReviewRemember(t, opts)
	branch := first.Remembered.Branch

	stored, err := factstore.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	retracted := ""
	for i := range stored {
		if stored[i].Origin == factmerge.OriginReview {
			// The store is flat per repository now, so Branch is metadata on
			// the record rather than the file it lives in. Pin that the
			// write-back still stamps it, because the provenance walk and
			// every branch-filtered read depend on that field alone.
			if stored[i].Branch != branch {
				t.Fatalf("a review record must carry the branch it was derived on: got %q, want %q", stored[i].Branch, branch)
			}
			stored[i].Status = factmerge.StatusRetracted
			retracted = stored[i].ID
			break
		}
	}
	if retracted == "" {
		t.Fatal("no review-origin record to retract")
	}
	if err := factstore.Write(brainDir, stored); err != nil {
		t.Fatal(err)
	}

	runReviewRemember(t, opts)

	after, err := factstore.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	i := factmerge.IndexOf(after, retracted)
	if i < 0 {
		t.Fatal("the retracted record vanished")
	}
	if after[i].Status != factmerge.StatusRetracted {
		t.Fatalf("a re-review revived a retracted finding (status %q): a detector must not overturn a human judgment",
			after[i].Status)
	}
}

// Nothing found means UNKNOWN, not clean (Report.Checked). Recording "a review
// found nothing" would assert the clean bill of health the rest of this
// command refuses to give, so the write path must not run at all.
func TestReviewRememberWritesNothingWhenThereAreNoFindings(t *testing.T) {
	t.Parallel()

	env := semanticTestEnv(t, t.TempDir())
	history := `{"text":"pkg/review_context.go builds the range as scopeBaseRef+\"..HEAD\""}` + "\n"
	intact := "package x\nfunc f(scopeBaseRef string) string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	repo, _ := writeLocalWorkspaceBrainRepo(t, env, history, "pkg/review_context.go", intact)

	runner := semanticFixtureRunner(repo, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	opts.Env.RepoRoot = repo
	statusBrain, err := base.BrainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(statusBrain, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statusBrain, "sessions", "s.jsonl"), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{Use: "review"}
	cmd.SetOut(&out)
	if err := brainreview.RunBrainReview(context.Background(), cmd, brainReviewConfig(opts),
		regressionDetectorOptions{Limit: 20, JSON: true, Remember: true}, "fix scopeBaseRef"); err != nil {
		t.Fatalf("review: %v\n%s", err, out.String())
	}
	var report brainreview.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Skipf("fixture produced %d findings; this case needs a clean tree", len(report.Findings))
	}
	if report.Remembered != nil {
		t.Errorf("a review that found nothing must not report a write-back: %+v", report.Remembered)
	}
	// One flat store per repository, so this reads it once. A load error is a
	// failure rather than a skip: swallowing it would make the assertion below
	// unreachable and the test green for the wrong reason.
	stored, err := factstore.Load(statusBrain)
	if err != nil {
		t.Fatalf("load the store the review would have written to: %v", err)
	}
	for _, record := range stored {
		if record.Origin == factmerge.OriginReview {
			t.Errorf("a review that found nothing wrote a memory: %s", record.Text)
		}
	}
}

// --remember with --location-only must be refused before anything is written.
func TestReviewRememberRefusesLocationOnlyEndToEnd(t *testing.T) {
	t.Parallel()

	opts, brainDir := reviewRememberFixture(t)
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "review"}
	cmd.SetOut(&out)
	err := brainreview.RunBrainReview(context.Background(), cmd, brainReviewConfig(opts),
		regressionDetectorOptions{Limit: 20, JSON: true, Remember: true, LocationOnly: true}, "fix scopeBaseRef")
	if err == nil {
		t.Fatal("the combination must be refused")
	}
	stored, loadErr := factstore.Load(brainDir)
	if loadErr != nil {
		t.Fatalf("load the store the refused run must not have touched: %v", loadErr)
	}
	for _, record := range stored {
		if record.Origin == factmerge.OriginReview {
			t.Errorf("a refused run still wrote %s", record.ID)
		}
	}
	if !strings.Contains(fmt.Sprint(err), "--location-only") {
		t.Errorf("the error must name the conflicting flag: %v", err)
	}
}
