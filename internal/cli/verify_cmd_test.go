package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type verifyFixture struct {
	repoDir  string
	brainDir string
	storage  repoStorage
	runner   *fakeCommandRunner
	opts     Options
	now      time.Time
}

func newVerifyFixture(t *testing.T) verifyFixture {
	t.Helper()
	repoDir := t.TempDir()
	dataDir := t.TempDir()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:      repoDir,
			PluginDataDir: dataDir,
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	return verifyFixture{repoDir: repoDir, brainDir: storage.BrainDir, storage: storage, runner: runner, opts: opts, now: now}
}

func (f verifyFixture) writeSessions(t *testing.T, sessions []exportSession) {
	t.Helper()
	manifest := exportManifest{
		SchemaVersion:      brainManifestSchemaVersion,
		GeneratedAt:        f.now,
		RepoRoot:           f.repoDir,
		RepoKey:            f.storage.Key,
		DefaultBranch:      "main",
		TranscriptMode:     "raw",
		Scope:              exportScopeAll,
		Sessions:           sessions,
		CheckpointsScanned: len(sessions),
	}
	if err := writeBrainManifestAndReadme(f.brainDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func (f verifyFixture) writeFacts(t *testing.T, branch string, facts []factRecord) {
	t.Helper()
	if err := writeFacts(f.brainDir, branch, facts); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := updateFactSourceManifest(f.brainDir, f.now); err != nil {
		t.Fatalf("update facts manifest: %v", err)
	}
}

func (f verifyFixture) writeBrainFile(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(f.brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (f verifyFixture) addLocalCheckpoint(t *testing.T, checkpointID, sessionID, turnID, transcript string) {
	t.Helper()
	checkpointDir := checkpointPath(checkpointID)
	rootMetadata := checkpointDir + "/metadata.json"
	sessionMetadata := checkpointDir + "/0/metadata.json"
	transcriptPath := checkpointDir + "/0/" + v1TranscriptFileName
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{
		stdout: strings.Join([]string{rootMetadata, sessionMetadata, transcriptPath}, "\n") + "\n",
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+rootMetadata)] = fakeCommandResponse{
		stdout: `{"branch":"main","sessions":[{"metadata":"` + sessionMetadata + `","transcript":"` + transcriptPath + `"}]}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+sessionMetadata)] = fakeCommandResponse{
		stdout: `{"session_id":"` + sessionID + `","branch":"main","turn_id":"` + turnID + `"}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+transcriptPath)] = fakeCommandResponse{stdout: transcript}
}

func verifyFactFixture(id, text, branch, origin, status string, now time.Time, anchors []factAnchor) factRecord {
	paths := normalizeFactPaths([]string{"architecture.boundaries.rationale"})
	if id == "" {
		id = factRecordID(text, paths)
	}
	return factRecord{
		ID:         id,
		Paths:      paths,
		Text:       text,
		Branch:     branch,
		Origin:     origin,
		Status:     status,
		Provenance: anchors,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func parseVerifyReport(t *testing.T, out string) verifyReport {
	t.Helper()
	var report verifyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse verify json: %v\n%s", err, out)
	}
	return report
}

func TestVerifyDistilledFactAtCheckpointGranularity(t *testing.T) {
	f := newVerifyFixture(t)
	const checkpointID = "aaa111aaa111"
	const transcriptRel = "sessions/main/session.jsonl"
	transcript := `{"type":"user_message","message":"Use the local checkpoint reader."}` + "\n"
	f.writeBrainFile(t, transcriptRel, transcript)
	f.writeSessions(t, []exportSession{{
		SessionID:        "sess1",
		Branch:           "main",
		LatestCheckpoint: checkpointID,
		SessionIndex:     0,
		CreatedAt:        f.now,
		TurnID:           "turn1",
		TranscriptPath:   transcriptRel,
	}})
	f.addLocalCheckpoint(t, checkpointID, "sess1", "turn1", transcript)
	fact := verifyFactFixture("fact:verified", "The verifier reuses the local checkpoint reader.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: checkpointID, TurnID: "turn1", Transcript: transcriptRel, Line: 1,
	}})
	f.writeFacts(t, "main", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:verified", "--json")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Verified != 1 || report.Results[0].Verdict != verifyVerdictVerified {
		t.Fatalf("expected verified report, got %+v", report)
	}
	if len(report.Results[0].Limitations) == 0 || report.Results[0].Limitations[0].Reason != verifyTurnSigningLimitation {
		t.Fatalf("missing turn-signing limitation: %+v", report.Results[0].Limitations)
	}
}

func TestVerifyReportsOrphanedAnchorAndStillEmitsJSON(t *testing.T) {
	f := newVerifyFixture(t)
	f.writeSessions(t, nil)
	fact := verifyFactFixture("fact:orphan", "The fact cites a missing session.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "missing", CheckpointID: "aaa111aaa111", Transcript: "sessions/main/missing.jsonl", Line: 1,
	}})
	f.writeFacts(t, "main", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:orphan", "--json")
	if err == nil || !errors.Is(err, errVerifyIssues) {
		t.Fatalf("expected verify issue error, got %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Orphaned != 1 || report.Results[0].Verdict != verifyVerdictOrphaned {
		t.Fatalf("expected orphaned report, got %+v", report)
	}
}

func TestVerifyAuthoredFactsAreUnverifiableUnlessCommitMissing(t *testing.T) {
	f := newVerifyFixture(t)
	reachableCommit := "1111111111111111111111111111111111111111"
	missingCommit := "2222222222222222222222222222222222222222"
	f.runner.responses[fakeCommandKey("git", "cat-file", "-e", reachableCommit+"^{commit}")] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "for-each-ref", "--format=%(refname)", "--contains", reachableCommit, "refs/heads", "refs/remotes", "refs/tags")] = fakeCommandResponse{stdout: "refs/heads/main\n"}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-e", missingCommit+"^{commit}")] = fakeCommandResponse{err: os.ErrNotExist}
	f.writeSessions(t, nil)
	authored := verifyFactFixture("fact:authored", "Authored facts may cite only a commit.", "main", factOriginAuthored, factStatusActive, f.now, []factAnchor{{Commit: reachableCommit}})
	missing := verifyFactFixture("fact:missing-commit", "Missing commit anchors are orphaned.", "main", factOriginAuthored, factStatusActive, f.now, []factAnchor{{Commit: missingCommit}})
	f.writeFacts(t, "main", []factRecord{authored, missing})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "--json")
	if err == nil || !errors.Is(err, errVerifyIssues) {
		t.Fatalf("expected verify issue error, got %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	byID := map[string]verifyFactResult{}
	for _, result := range report.Results {
		byID[result.Fact.ID] = result
	}
	if byID["fact:authored"].Verdict != verifyVerdictUnverifiableHere {
		t.Fatalf("authored verdict = %+v", byID["fact:authored"])
	}
	if byID["fact:missing-commit"].Verdict != verifyVerdictOrphaned {
		t.Fatalf("missing commit verdict = %+v", byID["fact:missing-commit"])
	}
}

func TestVerifyReportsStaleTranscript(t *testing.T) {
	f := newVerifyFixture(t)
	const checkpointID = "aaa111aaa111"
	const transcriptRel = "sessions/main/session.jsonl"
	f.writeBrainFile(t, transcriptRel, "old transcript\n")
	f.writeSessions(t, []exportSession{{
		SessionID:        "sess1",
		Branch:           "main",
		LatestCheckpoint: checkpointID,
		SessionIndex:     0,
		CreatedAt:        f.now,
		TranscriptPath:   transcriptRel,
	}})
	f.addLocalCheckpoint(t, checkpointID, "sess1", "", "new transcript\n")
	fact := verifyFactFixture("fact:stale", "The exported transcript can drift from its checkpoint source.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: checkpointID, Transcript: transcriptRel, Line: 1,
	}})
	f.writeFacts(t, "main", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:stale", "--json")
	if err == nil || !errors.Is(err, errVerifyIssues) {
		t.Fatalf("expected verify issue error, got %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Stale != 1 || report.Results[0].Verdict != verifyVerdictStale {
		t.Fatalf("expected stale report, got %+v", report)
	}
}

func TestVerifySelectionModes(t *testing.T) {
	f := newVerifyFixture(t)
	active := verifyFactFixture("fact:active", "Lexical verify query matches this active fact.", "main", factOriginAuthored, factStatusActive, f.now, nil)
	superseded := verifyFactFixture("fact:superseded", "Lexical verify query matches this superseded fact.", "main", factOriginAuthored, factStatusSuperseded, f.now.Add(-time.Hour), nil)
	f.writeSessions(t, nil)
	f.writeFacts(t, "main", []factRecord{active, superseded})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "--json")
	if err != nil {
		t.Fatalf("verify no-arg: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Facts != 1 || report.Results[0].Fact.ID != "fact:active" {
		t.Fatalf("no-arg should verify only active facts, got %+v", report.Results)
	}

	cmd = NewRootCommand(f.opts)
	out, err = execute(t, cmd, "verify", "Lexical verify query", "--all", "--json")
	if err != nil {
		t.Fatalf("verify query --all: %v\n%s", err, out)
	}
	report = parseVerifyReport(t, out)
	if report.Summary.Facts != 2 {
		t.Fatalf("query --all should verify both facts, got %+v", report.Results)
	}
}

func TestVerifyJSONUsesEmptyResultArray(t *testing.T) {
	f := newVerifyFixture(t)
	f.writeSessions(t, nil)
	f.writeFacts(t, "main", nil)

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "no matching fact text", "--json")
	if err != nil {
		t.Fatalf("verify empty query: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Facts != 0 || len(report.Results) != 0 {
		t.Fatalf("expected empty report, got %+v", report)
	}
	if report.Results == nil {
		t.Fatalf("expected empty JSON results array, got nil slice from %s", out)
	}
}

func TestVerifyDoesNotFetchConfiguredCheckpointRemote(t *testing.T) {
	f := newVerifyFixture(t)
	settingsDir := filepath.Join(f.repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"example/checkpoints"}}}`), 0o600); err != nil {
		t.Fatalf("settings: %v", err)
	}
	const transcriptRel = "sessions/main/session.jsonl"
	f.writeBrainFile(t, transcriptRel, "local export only\n")
	f.writeSessions(t, []exportSession{{
		SessionID:        "sess1",
		Branch:           "main",
		LatestCheckpoint: "aaa111aaa111",
		SessionIndex:     0,
		CreatedAt:        f.now,
		TranscriptPath:   transcriptRel,
	}})
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	fact := verifyFactFixture("fact:local-only", "Verification must not fetch checkpoint remotes.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: "aaa111aaa111", Transcript: transcriptRel, Line: 1,
	}})
	f.writeFacts(t, "main", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:local-only", "--json")
	if err != nil {
		t.Fatalf("unverifiable local refs should not fail: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.UnverifiableHere != 1 || report.Results[0].Verdict != verifyVerdictUnverifiableHere {
		t.Fatalf("expected unverifiable local-only report, got %+v", report)
	}
	for _, call := range f.runner.calls {
		if call.name == "git" && len(call.args) > 0 && call.args[0] == "fetch" {
			t.Fatalf("verify must not fetch: %+v", call)
		}
		if call.name == "entire" || call.name == "entire-test" {
			t.Fatalf("verify must not shell to entire checkpoint explain: %+v", call)
		}
	}
}

func TestStatusIncludesVerificationSummary(t *testing.T) {
	f := newVerifyFixture(t)
	f.runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "headsha\n"}
	f.runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}
	f.writeSessions(t, nil)
	f.writeFacts(t, "main", []factRecord{
		verifyFactFixture("fact:status", "Status summarizes fact verification.", "main", factOriginAuthored, factStatusActive, f.now, nil),
	})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "status", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var report brainStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse status json: %v\n%s", err, out)
	}
	if report.Verification == nil || report.Verification.UnverifiableHere != 1 {
		t.Fatalf("missing verification summary: %+v", report.Verification)
	}
}
