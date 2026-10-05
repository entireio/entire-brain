package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCappedHistoryFixture lays down a brain whose raw history trips
// regressionScanHistory's distinct-signal cap inside the FIRST file, so the
// second file (which carries a real, distinct invariant) is never opened.
func writeCappedHistoryFixture(t *testing.T) string {
	t.Helper()
	brainDir := t.TempDir()
	sessDir := filepath.Join(brainDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var bulk strings.Builder
	for i := 0; i < 420; i++ {
		// change signal: identifier adjacent to a distinctive literal operand
		fmt.Fprintf(&bulk, `{"text":"the range must stay scopeBaseRef+\"oprnd%04d\" here"}`+"\n", i)
		// delete signal: a field assignment whose target contains the identifier
		fmt.Fprintf(&bulk, `{"text":"keep state.scopeBaseRef = rhsval%04d after the update"}`+"\n", i)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "a_bulk.jsonl"), []byte(bulk.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessDir, "z_tail.jsonl"), []byte(tail), 0o644); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

// Bug 1: the raw-history walk stops at a hard cap via filepath.SkipAll and says
// nothing, so "No suspected regressions" is printed over a PARTIAL scan.
func TestRegressionHistoryScanCapIsSurfacedNotSilent(t *testing.T) {
	brainDir := writeCappedHistoryFixture(t)
	repoRoot := t.TempDir()
	full := filepath.Join(repoRoot, "pkg", "review_context.go")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	// Current tree LOST the invariant that only the un-scanned tail file records.
	if err := os.WriteFile(full, []byte("package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, warnings, _ := detectRegressionAnomaliesCapped(brainDir, repoRoot, nil, "fix scopeBaseRef base scope", 20, false)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "stopped early") && strings.Contains(w, "partial") {
			found = true
		}
	}
	if !found {
		t.Fatalf("capped history scan must warn that the result is partial; warnings = %q", warnings)
	}

	// The headline may not read as a clean bill of health over a truncated scan.
	if line := regressionNoAnomaliesLine("q", 5, true); strings.HasPrefix(line, "No suspected regressions") {
		t.Fatalf("truncated scan must not print the unqualified headline: %q", line)
	}
	if line := regressionNoAnomaliesLine("q", 5, false); !strings.HasPrefix(line, "No suspected regressions") {
		t.Fatalf("a complete scan with no findings must keep the plain headline: %q", line)
	}
}

// Bug 2: usefulExportWarnings drops whole classes of warning unless --debug and
// the printed count describes the FILTERED set, so an export with unreadable
// checkpoint snapshots reads as complete.
func TestExportWarningSuppressionIsDisclosed(t *testing.T) {
	raw := []string{
		"checkpoint snapshot unavailable: object 0bad1dea missing from pack",
		"checkpoint snapshot unavailable: object 0bad1deb missing from pack",
		"checkpoint author index unavailable: no refs enumerable",
	}
	shown, suppressed, total := usefulExportWarningsDetailed(raw, false)
	if total != 3 {
		t.Fatalf("raw warning total must be 3, got %d", total)
	}

	// The printed summary must state the REAL total and disclose the suppression.
	summary := strings.Join(exportWarningSummaryLines(shown, suppressed, total), "\n")
	if !strings.Contains(summary, "warnings: 1 shown of 3 total") {
		t.Fatalf("summary must report the true total, not the filtered count:\n%s", summary)
	}
	if !strings.Contains(summary, "2 warning(s) suppressed") || !strings.Contains(summary, "--debug") || !strings.Contains(summary, "suppressed_warnings") {
		t.Fatalf("summary must say how many were suppressed and how to read them:\n%s", summary)
	}

	// ...and the dropped text itself must be recoverable without --debug, via the manifest.
	if len(suppressed) != 2 || !strings.Contains(suppressed[0], "0bad1dea") || !strings.Contains(suppressed[1], "0bad1deb") {
		t.Fatalf("the raw suppressed warnings must be recoverable verbatim, got %q", suppressed)
	}
	// --debug must surface them inline and suppress nothing.
	_, debugSuppressed, _ := usefulExportWarningsDetailed(raw, true)
	if len(debugSuppressed) != 0 {
		t.Fatalf("--debug must suppress nothing, got %q", debugSuppressed)
	}
}

func TestExportWarningSummaryOnlyClaimsIncompleteForUnreadableSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        []string
		incomplete bool
	}{
		{"discovery notice", []string{"discovered checkpoint refs from configured checkpoint remote"}, false},
		{"transcript notice", []string{"exporting raw transcripts directly from checkpoint storage"}, false},
		{"unreadable snapshot", []string{"checkpoint snapshot unavailable: missing object"}, true},
		{"mixed", []string{"discovered checkpoint refs from configured checkpoint remote", "checkpoint snapshot unavailable: missing object"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shown, suppressed, total := usefulExportWarningsDetailed(tc.raw, false)
			summary := strings.Join(exportWarningSummaryLines(shown, suppressed, total), "\n")
			if got := strings.Contains(summary, "INCOMPLETE"); got != tc.incomplete {
				t.Fatalf("incomplete claim = %t, want %t:\n%s", got, tc.incomplete, summary)
			}
			if !strings.Contains(summary, "suppressed_warnings") {
				t.Fatalf("summary lost the suppressed warning location:\n%s", summary)
			}
		})
	}
}

// Bug 3: capWarnings is applied BEFORE the manifest write, so dropped warning
// text exists nowhere and no flag recovers it.
func TestDistillWarningsPersistUncappedToManifest(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	const failing = maxDistillWarnings + 10

	brainDir := t.TempDir()
	sessions := make([]exportSession, 0, failing+1)
	for i := 0; i <= failing; i++ {
		id := fmt.Sprintf("s%03d", i)
		rel := "sessions/main/" + id + ".jsonl"
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("turn one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, exportSession{
			SessionID: id, Branch: "main", LatestCheckpoint: "cp" + id,
			TranscriptPath: rel, CreatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	// One success (so the run is not aborted as a total agent failure), the rest fail.
	var mu chan struct{} = make(chan struct{}, 1)
	mu <- struct{}{}
	succeeded := false
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		<-mu
		first := !succeeded
		succeeded = true
		mu <- struct{}{}
		if first {
			return "preferences.coding.style\tThe user prefers concise commits.\n", nil
		}
		return "", fmt.Errorf("synthetic agent failure")
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: fakeRun, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, jobs: 1}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if len(source.Warnings) <= maxDistillWarnings {
		t.Fatalf("manifest must persist every warning (expected > %d from %d failing agent calls), got %d: %q",
			maxDistillWarnings, failing, len(source.Warnings), source.Warnings)
	}
	for _, w := range source.Warnings {
		if strings.Contains(w, "more warnings") {
			t.Fatalf("the display cap must not reach the persisted manifest: %q", w)
		}
	}
	// The cap still bounds the TERMINAL rendering, and says where the rest live.
	shown := distillWarningsForDisplay(source.Warnings)
	if len(shown) != maxDistillWarnings+1 {
		t.Fatalf("display must stay capped at %d entries + overflow line, got %d", maxDistillWarnings, len(shown))
	}
	if !strings.Contains(shown[len(shown)-1], "recorded in the brain manifest") {
		t.Fatalf("the overflow line must say where the full list lives, got %q", shown[len(shown)-1])
	}
}
