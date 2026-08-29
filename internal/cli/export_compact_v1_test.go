package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v1 checkpoints DO carry a compact transcript. The CLI writes transcript.jsonl
// into the session directory next to full.jsonl and points at it from the root
// summary's sessions[].compact_transcript
// (cmd/entire/cli/checkpoint/persistent.go writeSessionToSubdirectory /
// writeCompactTranscript). The export reader ignored that pointer and always
// exported full.jsonl, so `refresh sessions` without --raw silently handed back
// the raw transcript while the manifest said "compact".

// compactV1Fixture scripts one v1 checkpoint with one session. When
// withCompact is false the session has no transcript.jsonl and the root summary
// omits the pointer, which is what an older CLI wrote.
func compactV1Fixture(t *testing.T, withCompact bool) (string, *fakeCommandRunner) {
	t.Helper()

	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	root := checkpointPath("aaa111aaa111")
	tree := root + "/metadata.json\n" + root + "/0/metadata.json\n" + root + "/0/full.jsonl\n"
	summary := `{"branch":"main","sessions":[{"metadata":"/` + root + `/0/metadata.json","transcript":"/` + root + `/0/full.jsonl"}]}`
	if withCompact {
		tree += root + "/0/transcript.jsonl\n"
		summary = `{"branch":"main","sessions":[{"metadata":"/` + root + `/0/metadata.json","transcript":"/` + root + `/0/full.jsonl","compact_transcript":"/` + root + `/0/transcript.jsonl"}]}`
	}

	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                  {stdout: tree},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef):                {},
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/metadata.json"):      {stdout: summary},
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/0/metadata.json"): {
			stdout: `{"checkpoint_id":"aaa111aaa111","session_id":"session-one","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/0/full.jsonl"):       {stdout: "{\"raw\":true}\n"},
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/0/transcript.jsonl"): {stdout: "{\"compact\":true}\n"},
	}}
	return repoDir, runner
}

func onlySession(t *testing.T, snapshot *checkpointSnapshot) selectedSession {
	t.Helper()
	if snapshot == nil {
		t.Fatal("no snapshot")
	}
	if len(snapshot.Selected) != 1 {
		t.Fatalf("selected %d sessions, want 1", len(snapshot.Selected))
	}
	for _, session := range snapshot.Selected {
		return session
	}
	return selectedSession{}
}

// TestLoadConfiguredCheckpointSnapshotExportsV1CompactTranscript pins that a
// compact export of v1 checkpoints reads the compact transcript the writer
// stored, rather than the raw log.
func TestLoadConfiguredCheckpointSnapshotExportsV1CompactTranscript(t *testing.T) {
	t.Parallel()

	repoDir, runner := compactV1Fixture(t, true)

	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 0, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		t.Fatalf("loadConfiguredCheckpointSnapshot: %v", err)
	}
	if snapshot.TranscriptMode != "compact" {
		t.Errorf("snapshot.TranscriptMode = %q, want %q", snapshot.TranscriptMode, "compact")
	}
	session := onlySession(t, snapshot)
	if !strings.HasSuffix(session.SourceTranscriptPath, "/transcript.jsonl") {
		t.Errorf("session transcript = %q, want the stored compact transcript", session.SourceTranscriptPath)
	}
	for _, member := range snapshot.SessionCheckpoints {
		if !strings.HasSuffix(member.TranscriptPath, "/transcript.jsonl") {
			t.Errorf("session-checkpoint transcript = %q, want the stored compact transcript", member.TranscriptPath)
		}
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "compact transcript unavailable") {
			t.Errorf("compact export claimed compact transcripts are unavailable: %q", warning)
		}
	}

	transcript, err := readSnapshotTranscriptFromSource(context.Background(), runner, snapshot, session.SourceKey, session.SourceTranscriptPath)
	if err != nil {
		t.Fatalf("read exported transcript: %v", err)
	}
	if !strings.Contains(string(transcript), `"compact"`) {
		t.Errorf("exported transcript = %q, want the compact bytes", transcript)
	}
}

// TestLoadConfiguredCheckpointSnapshotRawStillExportsFullJSONL keeps --raw on
// the raw log even when a compact transcript exists beside it.
func TestLoadConfiguredCheckpointSnapshotRawStillExportsFullJSONL(t *testing.T) {
	t.Parallel()

	repoDir, runner := compactV1Fixture(t, true)

	snapshot, _, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, true, 0, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		t.Fatalf("loadConfiguredCheckpointSnapshot: %v", err)
	}
	if snapshot.TranscriptMode != "raw" {
		t.Errorf("snapshot.TranscriptMode = %q, want %q", snapshot.TranscriptMode, "raw")
	}
	if session := onlySession(t, snapshot); !strings.HasSuffix(session.SourceTranscriptPath, "/full.jsonl") {
		t.Errorf("session transcript = %q, want the raw log", session.SourceTranscriptPath)
	}
}

// TestLoadConfiguredCheckpointSnapshotWarnsWhenCompactIsAbsent pins the
// fallback: a checkpoint written before compaction existed has no pointer, and
// the export says so instead of quietly labelling raw bytes "compact".
func TestLoadConfiguredCheckpointSnapshotWarnsWhenCompactIsAbsent(t *testing.T) {
	t.Parallel()

	repoDir, runner := compactV1Fixture(t, false)

	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 0, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		t.Fatalf("loadConfiguredCheckpointSnapshot: %v", err)
	}
	if snapshot.TranscriptMode != "raw" {
		t.Errorf("snapshot.TranscriptMode = %q, want %q — a raw export must not be labelled compact", snapshot.TranscriptMode, "raw")
	}
	if session := onlySession(t, snapshot); !strings.HasSuffix(session.SourceTranscriptPath, "/full.jsonl") {
		t.Errorf("session transcript = %q, want the raw log", session.SourceTranscriptPath)
	}

	var told bool
	for _, warning := range usefulExportWarnings(warnings, false) {
		if strings.Contains(warning, "compact") && strings.Contains(warning, "raw") {
			told = true
		}
	}
	if !told {
		t.Errorf("compact export fell back to raw with no user-visible warning: %v", usefulExportWarnings(warnings, false))
	}
}

// TestLoadConfiguredCheckpointSnapshotIgnoresDanglingCompactPointer pins that a
// summary pointing at a compact transcript the ref does not carry is treated as
// having none, rather than exporting a path that cannot be read.
func TestLoadConfiguredCheckpointSnapshotIgnoresDanglingCompactPointer(t *testing.T) {
	t.Parallel()

	repoDir, runner := compactV1Fixture(t, true)
	root := checkpointPath("aaa111aaa111")
	// Keep the pointer in the summary, drop the blob from the tree listing.
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{
		stdout: root + "/metadata.json\n" + root + "/0/metadata.json\n" + root + "/0/full.jsonl\n",
	}

	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 0, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		t.Fatalf("loadConfiguredCheckpointSnapshot: %v", err)
	}
	if snapshot.TranscriptMode != "raw" {
		t.Errorf("snapshot.TranscriptMode = %q, want %q for a dangling compact pointer", snapshot.TranscriptMode, "raw")
	}
	if session := onlySession(t, snapshot); !strings.HasSuffix(session.SourceTranscriptPath, "/full.jsonl") {
		t.Errorf("session transcript = %q, want the raw log", session.SourceTranscriptPath)
	}
	var told bool
	for _, warning := range usefulExportWarnings(warnings, false) {
		if strings.Contains(warning, "compact") {
			told = true
		}
	}
	if !told {
		t.Errorf("dangling compact pointer fell back to raw with no user-visible warning: %v", usefulExportWarnings(warnings, false))
	}
}
