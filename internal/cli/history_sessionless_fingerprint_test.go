package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A brain whose session export never produced a source -- `setup` on a machine
// with no host CLI is the ordinary way to get one -- still publishes a history
// projection and its receipt. That receipt used to read back as
// memory_source_stale immediately after being written, because the publisher
// recorded an EMPTY SessionsFingerprint and the checked reader treats empty as
// unproven. The prescribed remedy, `memory repair`, republished the same empty
// fingerprint it had just rejected, so the state was unrepairable by design.
func TestSessionlessBrainReadsBackItsOwnProjectionReceipt(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now,
		RepoKey: "test/sessionless", DefaultBranch: "main",
		// No Sessions source at all: the export failed or never ran.
		Sources: &brainSources{},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	// The sessions tree exists and is empty, exactly as a failed or
	// zero-session export leaves it.
	if err := os.MkdirAll(filepath.Join(brainDir, exportSessionsDirectory, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := writeBrainHistoryIndexAndSource(brainDir, now, nil)
	if err != nil {
		t.Fatalf("publish history projection: %v", err)
	}
	if source.SessionsFingerprint != sessionSourceFingerprintAbsent {
		t.Fatalf("sessionless brain must publish a nameable fingerprint, got %q", source.SessionsFingerprint)
	}
	if got := brainSessionsFingerprint(brainDir); got != sessionSourceFingerprintAbsent {
		t.Fatalf("current fingerprint = %q, want %q", got, sessionSourceFingerprintAbsent)
	}

	published, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	_, state, stateErr := loadProjectionStateChecked(brainDir, published.Sources.History)
	if state != projectionStateCurrent {
		t.Fatalf("freshly published receipt reads back as %q (%v); want %q",
			state, stateErr, projectionStateCurrent)
	}
}

// A receipt written by a binary that predates fingerprinting carries "". That
// stays unproven against a brain that HAS sessions, and is accepted only
// against one that provably has none -- the compatibility path that lets an
// already-broken brain heal without a repair run.
func TestEmptyRecordedFingerprintIsCurrentOnlyAgainstAnAbsentSource(t *testing.T) {
	for _, tc := range []struct {
		name              string
		recorded, current string
		want              bool
	}{
		{name: "legacy receipt, no sessions source", recorded: "", current: sessionSourceFingerprintAbsent, want: true},
		{name: "legacy receipt, real sessions source", recorded: "", current: "sha256:abc", want: false},
		{name: "unreadable manifest", recorded: "sha256:abc", current: "", want: false},
		{name: "matching", recorded: "sha256:abc", current: "sha256:abc", want: true},
		{name: "drifted", recorded: "sha256:abc", current: "sha256:def", want: false},
		{name: "absent both sides", recorded: sessionSourceFingerprintAbsent, current: sessionSourceFingerprintAbsent, want: true},
		{name: "was absent, now has sessions", recorded: sessionSourceFingerprintAbsent, current: "sha256:abc", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionSourceFingerprintCurrent(tc.recorded, tc.current); got != tc.want {
				t.Fatalf("sessionSourceFingerprintCurrent(%q, %q) = %v, want %v",
					tc.recorded, tc.current, got, tc.want)
			}
		})
	}
}

// A transcript carrying one line over historyMaxLineBytes drops out of the
// projection entirely -- records and exchanges both. The only evidence used to
// be a bare "bufio.Scanner: token too long" naming no file, so the session
// count silently disagreed with the manifest and no reader could tell which
// session had gone missing.
func TestOversizedTranscriptLineWarningNamesTheTranscript(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	rel := "sessions/main/20260901T000000Z_huge.jsonl"
	huge := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: ` +
		strings.Repeat("x", historyMaxLineBytes+1) + `"}]}}`
	writeBundleTestFile(t, filepath.Join(brainDir, filepath.FromSlash(rel)), huge+"\n")

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now,
		RepoKey: "test/huge", DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{
			GeneratedAt: now, DefaultBranch: "main",
			Sessions: []exportSession{{
				SessionID: "huge-sess", Branch: "main", Agent: "claude",
				LatestCheckpoint: "cp1", TranscriptPath: rel, CreatedAt: now,
			}},
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	source, err := writeBrainHistoryIndexAndSource(brainDir, now, nil)
	if err != nil {
		t.Fatalf("publish history projection: %v", err)
	}
	if source.Records != 0 {
		t.Fatalf("expected the oversized transcript to contribute nothing, got %d records", source.Records)
	}
	joined := strings.Join(source.Warnings, "\n")
	if !strings.Contains(joined, rel) {
		t.Fatalf("a dropped session must be attributable; warnings = %q", joined)
	}
}
