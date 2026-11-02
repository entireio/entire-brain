package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// addLocalCompactCheckpoint scripts a v1 checkpoint whose session carries BOTH
// the raw full.jsonl and the compact transcript.jsonl the CLI stores beside it.
func (f verifyFixture) addLocalCompactCheckpoint(t *testing.T, checkpointID, sessionID, turnID, rawTranscript, compactTranscript string) {
	t.Helper()
	dir := checkpointPath(checkpointID)
	rootMetadata := dir + "/metadata.json"
	sessionMetadata := dir + "/0/metadata.json"
	rawPath := dir + "/0/" + v1TranscriptFileName
	compactPath := dir + "/0/" + v1CompactTranscriptFileName
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{
		stdout: strings.Join([]string{rootMetadata, sessionMetadata, rawPath, compactPath}, "\n") + "\n",
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+rootMetadata)] = fakeCommandResponse{
		stdout: `{"branch":"main","sessions":[{"metadata":"` + sessionMetadata + `","transcript":"` + rawPath + `","compact_transcript":"` + compactPath + `"}]}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+sessionMetadata)] = fakeCommandResponse{
		stdout: `{"session_id":"` + sessionID + `","branch":"main","turn_id":"` + turnID + `"}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+rawPath)] = fakeCommandResponse{stdout: rawTranscript}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+compactPath)] = fakeCommandResponse{stdout: compactTranscript}
}

// addLocalMixedStore scripts a store holding the compact-capable checkpoint
// the fact anchors to PLUS a second session whose checkpoint has no compact
// pointer — the state that flips useCompactTranscripts' all-or-nothing switch
// back to raw for the whole snapshot.
func (f verifyFixture) addLocalMixedStore(t *testing.T, checkpointID, sessionID, turnID, rawTranscript, compactTranscript string) {
	t.Helper()
	dir := checkpointPath(checkpointID)
	rootMetadata := dir + "/metadata.json"
	sessionMetadata := dir + "/0/metadata.json"
	rawPath := dir + "/0/" + v1TranscriptFileName
	compactPath := dir + "/0/" + v1CompactTranscriptFileName
	otherDir := checkpointPath("bbb222bbb222")
	otherRoot := otherDir + "/metadata.json"
	otherSession := otherDir + "/0/metadata.json"
	otherRaw := otherDir + "/0/" + v1TranscriptFileName
	f.runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{
		stdout: strings.Join([]string{rootMetadata, sessionMetadata, rawPath, compactPath, otherRoot, otherSession, otherRaw}, "\n") + "\n",
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+rootMetadata)] = fakeCommandResponse{
		stdout: `{"branch":"main","sessions":[{"metadata":"` + sessionMetadata + `","transcript":"` + rawPath + `","compact_transcript":"` + compactPath + `"}]}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+sessionMetadata)] = fakeCommandResponse{
		stdout: `{"session_id":"` + sessionID + `","branch":"main","turn_id":"` + turnID + `"}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+rawPath)] = fakeCommandResponse{stdout: rawTranscript}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+compactPath)] = fakeCommandResponse{stdout: compactTranscript}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+otherRoot)] = fakeCommandResponse{
		stdout: `{"branch":"main","sessions":[{"metadata":"` + otherSession + `","transcript":"` + otherRaw + `"}]}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+otherSession)] = fakeCommandResponse{
		stdout: `{"session_id":"sess-other","branch":"main","turn_id":"turn-other"}`,
	}
	f.runner.responses[fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+otherRaw)] = fakeCommandResponse{stdout: "other raw transcript\n"}
}

func anchorCheck(report verifyReport, name string) (verifyCheck, bool) {
	for _, result := range report.Results {
		for _, anchor := range result.Anchors {
			for _, check := range anchor.Checks {
				if check.Name == name {
					return check, true
				}
			}
		}
	}
	return verifyCheck{}, false
}

// A compact-mode export carries the checkpoint's compact transcript, so verify
// must compare it against the local COMPACT flavor, not raw full.jsonl.
// Regression for the customer-reported cycle (COR-2289): default (compact)
// refresh marked every session-anchored memory stale with "exported transcript
// differs from local checkpoint transcript"; only --raw exports verified.
func TestVerifyCompactExportComparesCompactCheckpointTranscript(t *testing.T) {
	f := newVerifyFixture(t)
	const checkpointID = "aaa111aaa111"
	const transcriptRel = "sessions/main/session.jsonl"
	raw := `{"type":"user","message":{"role":"user","content":"Use the local checkpoint reader."}}` + "\n"
	compact := etfTranscript()

	f.writeBrainFile(t, transcriptRel, compact)
	if err := writeBrainManifestAndReadme(f.brainDir, compactManifest(f, checkpointID, transcriptRel)); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	f.addLocalCompactCheckpoint(t, checkpointID, "sess1", "turn1", raw, compact)
	fact := verifyFactFixture("fact:compact", "Compact exports verify against the compact transcript.", "main", factstore.OriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: checkpointID, TurnID: "turn1", Transcript: transcriptRel, Line: 1,
	}})
	f.writeFacts(t, "main", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:compact", "--json")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Verified != 1 || report.Results[0].Verdict != verifyVerdictVerified {
		t.Fatalf("expected verified report, got %+v", report)
	}
}

// compactManifest is the manifest TestVerifyCompactExportComparesCompactCheckpointTranscript
// writes, shared by the mixed-store regressions.
func compactManifest(f verifyFixture, checkpointID, transcriptRel string) exportManifest {
	return exportManifest{
		SchemaVersion:  bmanifest.SchemaVersion,
		GeneratedAt:    f.now,
		RepoRoot:       f.repoDir,
		RepoKey:        f.storage.Key,
		DefaultBranch:  "main",
		TranscriptMode: "compact",
		Scope:          exportScopeAll,
		Sessions: []exportSession{{
			SessionID:        "sess1",
			Branch:           "main",
			LatestCheckpoint: checkpointID,
			SessionIndex:     0,
			CreatedAt:        f.now,
			TurnID:           "turn1",
			TranscriptPath:   transcriptRel,
		}},
		CheckpointsScanned: 1,
	}
}

// A compact export's MATERIALIZED transcript file (a pre-COR-2128 brain or a
// standalone --output bundle) cannot be byte-compared when the local store has
// since gained a session without a compact pointer: useCompactTranscripts then
// resolves the whole snapshot as raw, and compact-vs-raw bytes never match.
// That mismatch is a resolution limit, not evidence the memory went stale —
// the verdict must say so instead of re-reporting COR-2289's false stale.
func TestVerifyCompactExportWithMixedLocalStoreIsUnverifiableNotStale(t *testing.T) {
	f := newVerifyFixture(t)
	const checkpointID = "aaa111aaa111"
	const transcriptRel = "sessions/main/session.jsonl"
	raw := `{"type":"user","message":{"role":"user","content":"Use the local checkpoint reader."}}` + "\n"
	compact := etfTranscript()

	f.writeBrainFile(t, transcriptRel, compact)
	if err := writeBrainManifestAndReadme(f.brainDir, compactManifest(f, checkpointID, transcriptRel)); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	f.addLocalMixedStore(t, checkpointID, "sess1", "turn1", raw, compact)
	fact := verifyFactFixture("fact:compact", "Compact exports verify against the compact transcript.", "main", factstore.OriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: checkpointID, TurnID: "turn1", Transcript: transcriptRel, Line: 1,
	}})
	f.writeFacts(t, "main", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:compact", "--json")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Stale != 0 {
		t.Fatalf("a flavor-resolution limit must not report stale, got %+v", report)
	}
	check, ok := anchorCheck(report, "checkpoint_transcript")
	if !ok {
		t.Fatalf("expected a checkpoint_transcript check, got %+v", report)
	}
	if check.Verdict != verifyVerdictUnverifiableHere {
		t.Fatalf("expected unverifiable-here, got %+v", check)
	}
}

// A checkpoint-backed brain reads the anchored transcript OUT OF the
// checkpoint store, so byte-comparing it back against that same store proves
// nothing — both sides run the same loader with the same flavor flag. The
// check is skipped entirely there; existence, readability, and line checks
// carry the verification.
func TestVerifyCheckpointBackedBrainSkipsTranscriptByteCompare(t *testing.T) {
	f := newVerifyFixture(t)
	const checkpointID = "aaa111aaa111"
	const transcriptRel = "sessions/main/session.jsonl"
	raw := `{"type":"user","message":{"role":"user","content":"Use the local checkpoint reader."}}` + "\n"
	compact := etfTranscript()

	manifest := compactManifest(f, checkpointID, transcriptRel)
	manifest.TranscriptStorage = transcriptStorageCheckpoint
	if err := writeBrainManifestAndReadme(f.brainDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// The shared store registry wires ExecRunner; the checkpoint-backed read
	// must go through the fixture's scripted git instead.
	storeKey := filepath.Clean(f.brainDir)
	transcriptStores.Store(storeKey, newTranscriptStore(f.runner, f.brainDir))
	t.Cleanup(func() { transcriptStores.Delete(storeKey) })
	f.addLocalMixedStore(t, checkpointID, "sess1", "turn1", raw, compact)
	fact := verifyFactFixture("fact:compact", "Checkpoint-backed brains skip the byte compare.", "main", factstore.OriginDistilled, factStatusActive, f.now, []factAnchor{{
		SessionID: "sess1", CheckpointID: checkpointID, TurnID: "turn1", Transcript: transcriptRel, Line: 1,
	}})
	f.writeFacts(t, "main", []factRecord{fact})

	cmd := NewRootCommand(f.opts)
	out, err := execute(t, cmd, "verify", "fact:compact", "--json")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	report := parseVerifyReport(t, out)
	if report.Summary.Verified != 1 || report.Results[0].Verdict != verifyVerdictVerified {
		t.Fatalf("expected verified report, got %+v", report)
	}
	if check, ok := anchorCheck(report, "checkpoint_transcript"); ok {
		t.Fatalf("checkpoint-backed brains must not byte-compare, got %+v", check)
	}
}
