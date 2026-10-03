package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		fakeCommandKey("git", "rev-parse", "HEAD"):            {stdout: "abc123abc123abc123abc123abc123abc123abcd\n"},
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

// localCheckpointFixture is one checkpoint of a session in the local store.
type localCheckpointFixture struct {
	checkpointID string
	sessionID    string
	turnID       string
	createdAt    string
	transcript   string
	// branch is the branch the checkpoint was made on; empty means "main". It is
	// what distinguishes one exported row of a multi-branch session from another.
	branch string
}

// addLocalCheckpoints scripts a local checkpoint store holding SEVERAL
// checkpoints, so a fact anchored to a mid-session one has something to resolve
// against. addLocalCheckpoint is the single-checkpoint case.
func (f verifyFixture) addLocalCheckpoints(t *testing.T, checkpoints []localCheckpointFixture) {
	t.Helper()
	var paths []string
	for _, checkpoint := range checkpoints {
		dir := checkpointPath(checkpoint.checkpointID)
		root := dir + "/metadata.json"
		session := dir + "/0/metadata.json"
		transcript := dir + "/0/" + v1TranscriptFileName
		branch := checkpoint.branch
		if branch == "" {
			branch = "main"
		}
		paths = append(paths, root, session, transcript)
		f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root)] = fakeCommandResponse{
			stdout: `{"branch":"` + branch + `","sessions":[{"metadata":"` + session + `","transcript":"` + transcript + `"}]}`,
		}
		f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+session)] = fakeCommandResponse{
			stdout: `{"checkpoint_id":"` + checkpoint.checkpointID + `","session_id":"` + checkpoint.sessionID +
				`","branch":"` + branch + `","turn_id":"` + checkpoint.turnID + `","created_at":"` + checkpoint.createdAt + `"}`,
		}
		f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+transcript)] = fakeCommandResponse{stdout: checkpoint.transcript}
	}
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{
		stdout: strings.Join(paths, "\n") + "\n",
	}
}

// TestVerifySharpenedAnchorOnAnEarlierCheckpointVerifies is the regression the
// provenance sharpener introduced: verification asserted that an anchor names
// its session's LATEST checkpoint, which is precisely what sharpening stops
// being true. Every sharpened fact then read back as stale even though its
// anchor was more accurate than the one that verified.
func TestVerifySharpenedAnchorOnAnEarlierCheckpointVerifies(t *testing.T) {
	f := newVerifyFixture(t)
	const (
		earlyID = "aaa111aaa111"
		lateID  = "bbb222bbb222"
	)
	const transcriptRel = "sessions/main/session.jsonl"
	early := `{"type":"user_message","message":"first turn"}` + "\n"
	full := early + `{"type":"user_message","message":"second turn"}` + "\n"

	f.writeBrainFile(t, transcriptRel, full)
	f.writeSessions(t, []exportSession{{
		SessionID:        "sess1",
		Branch:           "main",
		LatestCheckpoint: lateID,
		SessionIndex:     0,
		CreatedAt:        f.now,
		TurnID:           "turn2",
		TranscriptPath:   transcriptRel,
	}})
	f.addLocalCheckpoints(t, []localCheckpointFixture{
		{checkpointID: earlyID, sessionID: "sess1", turnID: "turn1", createdAt: "2026-06-09T11:00:00Z", transcript: early},
		{checkpointID: lateID, sessionID: "sess1", turnID: "turn2", createdAt: "2026-06-09T12:00:00Z", transcript: full},
	})

	// The sharpened anchor: an EARLIER checkpoint of the same session.
	f.writeFacts(t, "main", []factRecord{verifyFactFixture("fact:sharpened", "Provenance points at the checkpoint that changed the entity.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: earlyID, Transcript: transcriptRel, Line: 1,
	}})})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:sharpened", "--json")
	if err != nil {
		t.Fatalf("verify sharpened fact: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Results[0].Verdict != verifyVerdictVerified {
		t.Fatalf("a sharpened anchor did not verify: %+v", report.Results[0])
	}
	for _, check := range report.Results[0].Anchors[0].Checks {
		if check.Verdict != verifyVerdictVerified {
			t.Fatalf("check %s = %s (%s)", check.Name, check.Verdict, check.Reason)
		}
	}
}

// TestVerifySpanningSessionRowsResolveDeterministically covers the case an
// unbranched fact reaches: one session id spanning SEVERAL exported rows. The
// row that is returned decides the transcript and the turn id every later check
// reads, and accepting a mid-session checkpoint turned that row from a stale
// tag into a VERIFIED answer — so picking whichever row the manifest happened
// to list first became a correctness question, not a cosmetic one.
func TestVerifySpanningSessionRowsResolveDeterministically(t *testing.T) {
	const (
		mainEarly = "aaa111aaa111"
		mainLate  = "bbb222bbb222"
		relLate   = "ccc333ccc333"
	)
	transcript := `{"type":"user_message","message":"turn"}` + "\n"
	mainRow := exportSession{
		SessionID: "sess1", Branch: "main", LatestCheckpoint: mainLate, SessionIndex: 0,
		TurnID: "turn-main", TranscriptPath: "sessions/main/s.jsonl",
	}
	releaseRow := exportSession{
		SessionID: "sess1", Branch: "release", LatestCheckpoint: relLate, SessionIndex: 0,
		TurnID: "turn-release", TranscriptPath: "sessions/release/s.jsonl",
	}
	// Two rows on the SAME branch: the local membership record cannot tell them
	// apart, so nothing may be promoted to verified off one of them.
	twinRow := mainRow
	twinRow.LatestCheckpoint = "ddd444ddd444"
	twinRow.TranscriptPath = "sessions/main/twin.jsonl"
	twinRow.TurnID = "turn-twin"

	anchor := factAnchor{SessionID: "sess1", CheckpointID: mainEarly}

	cases := []struct {
		name        string
		rows        []exportSession
		wantVerdict string
		wantRow     exportSession
	}{
		{
			name:        "the branch of the anchored checkpoint selects its row",
			rows:        []exportSession{mainRow, releaseRow},
			wantVerdict: verifyVerdictVerified,
			wantRow:     mainRow,
		},
		{
			name:        "two indistinguishable rows keep the conservative verdict",
			rows:        []exportSession{mainRow, twinRow},
			wantVerdict: verifyVerdictStale,
			wantRow:     mainRow, // deterministic pick: same branch, smaller latest checkpoint
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Both manifest orderings must answer identically: the array order of
			// an export is an assembly artifact, not evidence.
			for _, reversed := range []bool{false, true} {
				f := newVerifyFixture(t)
				rows := append([]exportSession(nil), tc.rows...)
				if reversed {
					rows[0], rows[1] = rows[1], rows[0]
				}
				f.addLocalCheckpoints(t, []localCheckpointFixture{
					{checkpointID: mainEarly, sessionID: "sess1", branch: "main", turnID: "turn-main", createdAt: "2026-06-09T11:00:00Z", transcript: transcript},
					{checkpointID: mainLate, sessionID: "sess1", branch: "main", turnID: "turn-main", createdAt: "2026-06-09T12:00:00Z", transcript: transcript},
					{checkpointID: relLate, sessionID: "sess1", branch: "release", turnID: "turn-release", createdAt: "2026-06-09T13:00:00Z", transcript: transcript},
				})
				v := &verifyContext{
					ctx:      context.Background(),
					opts:     f.opts,
					repoDir:  f.repoDir,
					brainDir: f.brainDir,
					branch:   "main",
					manifest: &exportManifest{
						DefaultBranch: "main",
						Sources:       &brainSources{Sessions: &sessionSourceManifest{Sessions: rows}},
					},
				}
				// factBranch is empty: the fact carries no branch of its own, which
				// is the only way both rows survive the filter.
				got, found, check := v.verifyExportedSession(anchor, "")
				if !found {
					t.Fatalf("reversed=%v: session not found: %+v", reversed, check)
				}
				if check.Verdict != tc.wantVerdict {
					t.Fatalf("reversed=%v: verdict = %s (%s), want %s", reversed, check.Verdict, check.Reason, tc.wantVerdict)
				}
				if got.TranscriptPath != tc.wantRow.TranscriptPath || got.TurnID != tc.wantRow.TurnID {
					t.Fatalf("reversed=%v: row = %+v, want %+v", reversed, got, tc.wantRow)
				}
			}
		})
	}
}

// TestVerifyAnchorOnAForeignCheckpointIsStale is the other half: accepting a
// non-latest checkpoint must not become accepting ANY checkpoint. One that the
// session does not own is still stale.
func TestVerifyAnchorOnAForeignCheckpointIsStale(t *testing.T) {
	f := newVerifyFixture(t)
	const (
		foreignID = "ccc333ccc333"
		lateID    = "bbb222bbb222"
	)
	const transcriptRel = "sessions/main/session.jsonl"
	transcript := `{"type":"user_message","message":"only turn"}` + "\n"

	f.writeBrainFile(t, transcriptRel, transcript)
	f.writeSessions(t, []exportSession{{
		SessionID:        "sess1",
		Branch:           "main",
		LatestCheckpoint: lateID,
		SessionIndex:     0,
		CreatedAt:        f.now,
		TranscriptPath:   transcriptRel,
	}})
	f.addLocalCheckpoints(t, []localCheckpointFixture{
		{checkpointID: lateID, sessionID: "sess1", createdAt: "2026-06-09T12:00:00Z", transcript: transcript},
		{checkpointID: foreignID, sessionID: "sess2", createdAt: "2026-06-09T13:00:00Z", transcript: transcript},
	})
	f.writeFacts(t, "main", []factRecord{verifyFactFixture("fact:foreign", "This anchor names another session's checkpoint.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: foreignID, Transcript: transcriptRel, Line: 1,
	}})})

	cmd := NewRootCommand(f.opts)
	out, _ := execute(t, cmd, "verify", "fact:foreign", "--json")
	report := parseVerifyReport(t, out)
	if got := findVerifyCheck(report.Results[0].Anchors[0].Checks, "session"); got.Verdict != verifyVerdictStale {
		t.Fatalf("session check = %+v, want stale", got)
	}
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

func findVerifyCheck(checks []verifyCheck, name string) verifyCheck {
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	return verifyCheck{}
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

func TestVerifyDistilledFactFromGitRefsCheckpoint(t *testing.T) {
	f := newVerifyFixture(t)
	const checkpointID = "01KVBJCWYA4YW6J5M9GP655HZN"
	settingsDir := filepath.Join(f.repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-refs"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := checkpointRefPrefix + checkpointID[len(checkpointID)-2:] + "/" + checkpointID
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	f.runner.responses[fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix)] = fakeCommandResponse{stdout: ref + "\n"}
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", ref)] = fakeCommandResponse{stdout: "metadata.json\n0/metadata.json\n0/full.jsonl\n"}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", ref+":metadata.json")] = fakeCommandResponse{
		stdout: `{"branch":"main","sessions":[{"metadata":"/0/metadata.json","transcript":"/0/full.jsonl"}]}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", ref+":0/metadata.json")] = fakeCommandResponse{
		stdout: `{"checkpoint_id":"` + checkpointID + `","session_id":"sess-refs","branch":"main","turn_id":"turn-refs","created_at":"2026-06-09T11:00:00Z"}`,
	}
	transcript := `{"type":"user_message","message":"Verify the per-checkpoint ref."}` + "\n"
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", ref+":0/full.jsonl")] = fakeCommandResponse{stdout: transcript}

	const transcriptRel = "sessions/main/git-refs.jsonl"
	f.writeBrainFile(t, transcriptRel, transcript)
	f.writeSessions(t, []exportSession{{
		SessionID:        "sess-refs",
		Branch:           "main",
		LatestCheckpoint: checkpointID,
		SessionIndex:     0,
		CreatedAt:        f.now,
		TurnID:           "turn-refs",
		TranscriptPath:   transcriptRel,
	}})
	f.writeFacts(t, "main", []factRecord{verifyFactFixture("fact:git-refs", "Git-refs checkpoints are verifiable locally.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess-refs", CheckpointID: checkpointID, TurnID: "turn-refs", Transcript: transcriptRel, Line: 1,
	}})})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:git-refs", "--json")
	if err != nil {
		t.Fatalf("verify git-refs fact: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Verified != 1 || report.Results[0].Verdict != verifyVerdictVerified {
		t.Fatalf("expected verified git-refs anchor, got %+v", report)
	}
	for _, call := range f.runner.calls {
		if call.name == "entire" || (call.name == "git" && len(call.args) > 0 && call.args[0] == "fetch") {
			t.Fatalf("local verification crossed an egress boundary: %+v", call)
		}
		if call.name == "git" && len(call.args) > 0 && (call.args[0] == "ls-tree" || call.args[0] == "for-each-ref" || call.args[0] == "cat-file") && call.env[gitNoLazyFetchEnv] != "1" {
			t.Fatalf("local verification object read lacks lazy-fetch guard: %+v", call)
		}
	}
}

func TestVerifyTranscriptFallbackRetainsCheckpointSource(t *testing.T) {
	const (
		oldID = "aaa111aaa111"
		newID = "bbb222bbb222"
	)
	oldPath := snapshotTranscriptPath(oldID, 0, v1TranscriptFileName)
	newPath := snapshotTranscriptPath(newID, 0, v1TranscriptFileName)
	snapshot := &checkpointSnapshot{
		TranscriptFileName: v1TranscriptFileName,
		Selected: map[string]selectedSession{
			selectedSessionKey("main", "same-session"): {
				CheckpointID: newID,
				SessionID:    "same-session",
				SessionIndex: 0,
				SourceKey:    "new-source",
			},
		},
		TreePaths: map[string]struct{}{oldPath: {}, newPath: {}},
		Sources: map[string]checkpointSnapshotSource{
			"old-source": {TreePaths: map[string]struct{}{oldPath: {}}},
			"new-source": {TreePaths: map[string]struct{}{newPath: {}}},
		},
	}
	v := verifyContext{}
	path, sourceKey := v.snapshotSourceTranscriptPath(snapshot, factAnchor{SessionID: "same-session", CheckpointID: oldID}, exportSession{SessionIndex: 0})
	if path != oldPath || sourceKey != "old-source" {
		t.Fatalf("old checkpoint fallback = (%q, %q), want (%q, old-source)", path, sourceKey, oldPath)
	}
}

func TestVerifyCheckpointDistinguishesUnreadableFromMissing(t *testing.T) {
	const (
		readableID = "aaa111aaa111"
		corruptID  = "bbb222bbb222"
	)
	v := verifyContext{
		snapshotLoaded: true,
		snapshot: &checkpointSnapshot{
			Sources: map[string]checkpointSnapshotSource{
				"source\x00" + readableID: {TreePaths: map[string]struct{}{}},
			},
			UnreadableCheckpoints: map[string]struct{}{corruptID: {}},
		},
	}
	if got := v.verifyCheckpoint(readableID); got.Verdict != verifyVerdictVerified {
		t.Fatalf("readable sibling = %+v", got)
	}
	if got := v.verifyCheckpoint(corruptID); got.Verdict != verifyVerdictUnverifiableHere || !strings.Contains(got.Reason, "unreadable") {
		t.Fatalf("corrupt checkpoint = %+v", got)
	}
	if got := v.verifyCheckpoint("ccc333ccc333"); got.Verdict != verifyVerdictOrphaned {
		t.Fatalf("missing checkpoint = %+v", got)
	}
}

func TestVerifyCommitDisablesLazyObjectFetch(t *testing.T) {
	const commit = "aaa111aaa111"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-e", commit+"^{commit}"): {},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", "--contains", commit, "refs/heads", "refs/remotes", "refs/tags"): {
			stdout: "refs/heads/main\n",
		},
	}}
	v := verifyContext{ctx: context.Background(), opts: Options{Runner: runner}, repoDir: "/repo"}
	if got := v.verifyCommitUncached(commit); got.Verdict != verifyVerdictVerified {
		t.Fatalf("commit verification = %+v", got)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("verification calls = %+v", runner.calls)
	}
	for _, call := range runner.calls {
		if call.env[gitNoLazyFetchEnv] != "1" {
			t.Fatalf("verification object read lacks lazy-fetch guard: %+v", call)
		}
	}
}

func TestVerifyDoesNotMutateStoredAnchorMetadata(t *testing.T) {
	f := newVerifyFixture(t)
	const checkpointID = "aaa111aaa111"
	const transcriptRel = "sessions/main/session.jsonl"
	transcript := `{"type":"user_message","message":"Verification reports are read-only."}` + "\n"
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
	f.writeFacts(t, "main", []factRecord{
		verifyFactFixture("fact:readonly", "Verify should not persist anchor verdicts.", "main", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
			SessionID: "sess1", CheckpointID: checkpointID, TurnID: "turn1", Transcript: transcriptRel, Line: 1,
		}}),
	})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:readonly", "--json")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Results[0].Verdict != verifyVerdictVerified {
		t.Fatalf("expected verified report, got %+v", report.Results[0])
	}
	stored, err := loadFacts(f.brainDir, "main")
	if err != nil {
		t.Fatalf("reload facts: %v", err)
	}
	if len(stored) != 1 || len(stored[0].Provenance) != 1 {
		t.Fatalf("unexpected stored facts: %+v", stored)
	}
	if stored[0].Provenance[0].Verified {
		t.Fatalf("verify mutated stored anchor metadata: %+v", stored[0].Provenance[0])
	}
	manifest, err := loadBrainManifest(f.brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Facts == nil {
		t.Fatalf("missing fact source manifest: %+v", manifest.Sources)
	}
	if manifest.Sources.Facts.Verified != 0 || manifest.Sources.Facts.Unsigned != 1 {
		t.Fatalf("verify should not mutate manifest anchor counts: %+v", manifest.Sources.Facts)
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

func TestVerifySessionLookupIsBranchScoped(t *testing.T) {
	f := newVerifyFixture(t)
	mainTranscript := "sessions/main/shared.jsonl"
	featureTranscript := "sessions/feature/shared.jsonl"
	f.writeBrainFile(t, mainTranscript, `{"type":"user_message","message":"main checkpoint"}`+"\n")
	f.writeBrainFile(t, featureTranscript, `{"type":"user_message","message":"feature checkpoint"}`+"\n")
	f.writeSessions(t, []exportSession{
		{SessionID: "shared", Branch: "main", LatestCheckpoint: "cp-main", TranscriptPath: mainTranscript, CreatedAt: f.now},
		{SessionID: "shared", Branch: "feature", LatestCheckpoint: "cp-feature", TranscriptPath: featureTranscript, CreatedAt: f.now.Add(time.Minute)},
	})
	fact := verifyFactFixture("fact:feature", "Feature branch fact cites shared session.", "feature", factOriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "shared", CheckpointID: "cp-main", Transcript: featureTranscript, Line: 1,
	}})
	f.writeFacts(t, "feature", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:feature", "--branch", "feature", "--json")
	if err == nil || !errors.Is(err, errVerifyIssues) {
		t.Fatalf("expected stale/orphaned verify issue, got %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Results[0].Verdict != verifyVerdictStale {
		t.Fatalf("feature fact should compare against feature session, got %+v", report.Results[0])
	}
	sessionCheck := findVerifyCheck(report.Results[0].Anchors[0].Checks, "session")
	if sessionCheck.Verdict != verifyVerdictStale || !strings.Contains(sessionCheck.Reason, "cp-feature") {
		t.Fatalf("session check should report feature checkpoint, got %+v", sessionCheck)
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

func TestVerifyStrictFlagsFailOnUnverifiableAfterJSON(t *testing.T) {
	for _, flag := range []string{"--strict", "--fail-on-unverifiable"} {
		t.Run(flag, func(t *testing.T) {
			f := newVerifyFixture(t)
			f.writeSessions(t, nil)
			f.writeFacts(t, "main", []factRecord{
				verifyFactFixture("fact:unverifiable", "Strict verify should gate locally unverifiable facts.", "main", factOriginAuthored, factStatusActive, f.now, nil),
			})

			cmd := NewRootCommand(f.opts)
			out, err := execute(t, cmd, "verify", "fact:unverifiable", flag, "--json")
			if err == nil || !errors.Is(err, errVerifyStrictIssues) {
				t.Fatalf("expected strict unverifiable error, got %v\n%s", err, out)
			}
			report := parseVerifyReport(t, out)
			if report.Summary.UnverifiableHere != 1 || report.Results[0].Verdict != verifyVerdictUnverifiableHere {
				t.Fatalf("expected unverifiable report before strict failure, got %+v", report)
			}
		})
	}
}

// Verification is charged to --details: the walk it runs is too expensive to
// sit on the default status path (see populateBrainStatusVerification, #325).
func TestStatusDetailsIncludesVerificationSummary(t *testing.T) {
	f := newVerifyFixture(t)
	f.runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "headsha\n"}
	f.runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "diff-index", "-M", "--shortstat", "HEAD")] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}
	f.writeSessions(t, nil)
	f.writeFacts(t, "main", []factRecord{
		verifyFactFixture("fact:status", "Status summarizes fact verification.", "main", factOriginAuthored, factStatusActive, f.now, nil),
	})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "status", "--json", "--details")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var report brainStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse status json: %v\n%s", err, out)
	}
	if report.Facts == nil || report.Facts.Verification == nil || report.Facts.Verification.UnverifiableHere != 1 {
		t.Fatalf("missing verification summary: %+v", report.Facts)
	}
}

// TestVerifySamplingAndCommitMemoization covers the brain-status hot path:
// sampling caps verification to the most recent facts (with the corpus size
// recorded so a sampled summary cannot be misread as full verification), and
// commit checks are memoized — facts sharing a checkpoint commit must not
// re-shell git per fact.
func TestVerifySamplingAndCommitMemoization(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	facts := make([]factRecord, 0, 30)
	for i := 0; i < 30; i++ {
		facts = append(facts, factRecord{
			ID:        fmt.Sprintf("fact:%02d", i),
			Text:      fmt.Sprintf("fact number %d", i),
			Status:    factStatusActive,
			UpdatedAt: now.Add(time.Duration(i) * time.Minute),
		})
	}
	selected, _, err := selectFactsForVerify(facts, "", verifyCommandOptions{limit: 10, sample: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 10 {
		t.Fatalf("sample should cap at 10, got %d", len(selected))
	}
	if selected[0].ID != "fact:29" {
		t.Fatalf("sample should be most-recent-first, got %s", selected[0].ID)
	}
	// Without sample, no-target mode still verifies everything (the verify
	// command's contract is unchanged).
	all, _, err := selectFactsForVerify(facts, "", verifyCommandOptions{limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 30 {
		t.Fatalf("no-target verify must keep checking everything, got %d", len(all))
	}

	// Memoization: two checks of one commit cost one git round, not two.
	runner := &countingRunner{}
	vctx := &verifyContext{ctx: context.Background(), opts: Options{Runner: runner, Now: time.Now}, repoDir: t.TempDir()}
	vctx.verifyCommit("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	callsAfterFirst := runner.calls
	vctx.verifyCommit("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if runner.calls != callsAfterFirst {
		t.Fatalf("repeated commit check must be memoized: %d -> %d git calls", callsAfterFirst, runner.calls)
	}
}

type countingRunner struct{ calls int }

func (r *countingRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	r.calls++
	return nil, nil, fmt.Errorf("not a repo")
}
