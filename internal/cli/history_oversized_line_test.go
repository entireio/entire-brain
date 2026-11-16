package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// oversizedLineTranscript is a Claude Code session whose second line is a
// screenshot Read: the tool result carries the base64 image twice, so the line
// is over historyMaxLineBytes, the real shape behind issue 641. The exchange it
// sits in continues past it, and a second exchange follows.
func oversizedLineTranscript() string {
	image := strings.Repeat("A", historyMaxLineBytes/2+1024)
	return `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Why does the export drop the screenshot session?"}]}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + image + `"}}]}]},"toolUseResult":{"type":"image","file":{"base64":"` + image + `"}}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: skip the oversized line and keep the screenshot session."}]}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Does the second exchange survive too?"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: lines after the skipped one keep their numbers."}]}}` + "\n"
}

func writeOversizedLineSession(t *testing.T, brainDir, rel string) string {
	t.Helper()
	content := oversizedLineTranscript()
	if len(strings.Split(content, "\n")[1]) <= historyMaxLineBytes {
		t.Fatal("fixture line 2 must be over historyMaxLineBytes")
	}
	writeBundleTestFile(t, filepath.Join(brainDir, filepath.FromSlash(rel)), content)
	return content
}

func findHistoryRecord(records []historyRecord, kind, summaryPart string) (historyRecord, bool) {
	for _, record := range records {
		if record.Kind == kind && strings.Contains(record.Summary, summaryPart) {
			return record, true
		}
	}
	return historyRecord{}, false
}

// A line over historyMaxLineBytes used to end the scan with "bufio.Scanner:
// token too long", and the whole session dropped out of history and
// conversation memory. It now costs that one line.
func TestHistoryBuildKeepsSessionWithOversizedLine(t *testing.T) {
	brainDir := t.TempDir()
	rel := "sessions/main/20261112T090000Z_screenshot.jsonl"
	writeOversizedLineSession(t, brainDir, rel)
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)

	for build := 1; build <= 2; build++ {
		index, source, err := buildBrainHistoryIndex(brainDir, now, nil)
		if err != nil {
			t.Fatalf("build %d: %v", build, err)
		}
		for _, want := range []struct {
			kind, summary string
			line          int
		}{
			{"decision", "Decision: skip the oversized line", 3},
			{"decision", "Decision: lines after the skipped one", 5},
		} {
			record, ok := findHistoryRecord(index.Records, want.kind, want.summary)
			if !ok {
				t.Fatalf("build %d: history lost %q: %+v", build, want.summary, index.Records)
			}
			if record.Line != want.line {
				t.Fatalf("build %d: %q is on line %d, want %d (a skipped line still counts)", build, want.summary, record.Line, want.line)
			}
		}
		first, ok := findHistoryRecord(index.Records, conversationKind, "screenshot session")
		if !ok {
			t.Fatalf("build %d: conversation memory lost the exchange around the screenshot: %+v", build, index.Records)
		}
		if first.Line != 1 || first.EndLine != 3 || !first.ProjectionTruncated {
			t.Fatalf("build %d: exchange = lines %d-%d truncated=%v, want lines 1-3 marked truncated (it spans an unread line)", build, first.Line, first.EndLine, first.ProjectionTruncated)
		}
		second, ok := findHistoryRecord(index.Records, conversationKind, "second exchange")
		if !ok {
			t.Fatalf("build %d: conversation memory lost the second exchange: %+v", build, index.Records)
		}
		if second.Line != 4 || second.EndLine != 5 || second.ProjectionTruncated {
			t.Fatalf("build %d: second exchange = lines %d-%d truncated=%v, want lines 4-5, not truncated", build, second.Line, second.EndLine, second.ProjectionTruncated)
		}
		// Every build names the partial session, the one that reuses the scan
		// cache included.
		joined := strings.Join(source.Warnings, "\n")
		if !strings.Contains(joined, rel) || !strings.Contains(joined, "skipped 1 transcript line") {
			t.Fatalf("build %d: warnings = %q, want the skipped line attributed to %s", build, joined, rel)
		}
		if strings.Contains(joined, "token too long") {
			t.Fatalf("build %d: the session was still refused: %q", build, joined)
		}
	}
	if cache := loadHistoryScanCache(brainDir); cache.Files[rel].SkippedLines != 1 {
		t.Fatalf("scan cache entry = %+v, want skipped_lines 1", cache.Files[rel])
	}
}

// The short-term delta used to list the session in failed_files on every
// tick, and doctor kept warning that transcripts failed to scan.
func TestShortTermDeltaKeepsSessionWithOversizedLine(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	rel := "sessions/main/20261112T090000Z_screenshot.jsonl"
	writeOversizedLineSession(t, brainDir, rel)
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{
		SessionID: "sess-screenshot", Branch: "main", Agent: "claude", LatestCheckpoint: "cp3",
		TranscriptPath: rel, CreatedAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	})
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}

	stats := buildShortTerm(t, brainDir)
	if stats.Failed != 0 {
		t.Fatalf("stats.Failed = %d, want 0: one oversized line must not fail the transcript", stats.Failed)
	}
	overlay, state := loadHistoryShortTermState(brainDir, manifest.Sources.History)
	if state != shortTermStateCurrent || len(overlay.FailedFiles) != 0 {
		t.Fatalf("overlay state=%s failed=%v, want current with no failed files", state, overlay.FailedFiles)
	}
	entry, ok := overlay.Files[rel]
	if !ok {
		t.Fatalf("overlay lost the session: %v", overlay.Files)
	}
	if _, ok := findHistoryRecord(entry.Records, "decision", "Decision: skip the oversized line"); !ok {
		t.Fatalf("overlay records lost the decision after the oversized line: %+v", entry.Records)
	}
	if _, ok := findHistoryRecord(entry.Records, conversationKind, "screenshot session"); !ok {
		t.Fatalf("overlay records lost the exchange: %+v", entry.Records)
	}
}

// An exchange whose range spans an oversized line must still expand, from the
// same line numbers, and say that it is not the faithful full content.
func TestConversationExpansionReadsPastOversizedLine(t *testing.T) {
	brainDir := t.TempDir()
	rel := "sessions/main/20261112T090000Z_screenshot.jsonl"
	content := writeOversizedLineSession(t, brainDir, rel)

	scan, err := scanLineConversation(strings.NewReader(content))
	if err != nil {
		t.Fatalf("conversation scan refused the transcript: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	if want := "sha256:" + hex.EncodeToString(sum[:]); scan.SourceDigest != want {
		t.Fatalf("digest = %s, want %s: the skipped line's bytes must still be hashed", scan.SourceDigest, want)
	}
	records := conversationExchangeRecords(rel, scan)
	if len(records) != 2 {
		t.Fatalf("exchanges = %+v, want 2", records)
	}

	expansion, err := expandConversationExchange(brainDir, records[0])
	if err != nil {
		t.Fatalf("expand the exchange around the screenshot: %v", err)
	}
	if !strings.Contains(expansion.Request, "screenshot session") || !strings.Contains(expansion.Response, "keep the screenshot session") {
		t.Fatalf("expansion = %+v, want the request and the response after the skipped line", expansion)
	}
	if !expansion.Truncated {
		t.Fatal("an expansion over an unread line claimed to be the full content")
	}
	result := conversationGetResult(brainDir, records[0])
	if len(result.Caveats) != 1 || !result.Truncated {
		t.Fatalf("get result = %+v, want the expansion, marked truncated, with only the historical caveat", result)
	}

	expansion, err = expandConversationExchange(brainDir, records[1])
	if err != nil {
		t.Fatalf("expand the second exchange: %v", err)
	}
	if !strings.Contains(expansion.Response, "keep their numbers") || expansion.Truncated {
		t.Fatalf("second expansion = %+v, want its own response, not truncated", expansion)
	}
}

// The compact-fallback warning added every missing checkpoint to the session
// count, so a store whose sessions mostly lacked a compact transcript reported
// more missing sessions than it exported ("97 of 94").
func TestUseCompactTranscriptsCountsSessionsOnce(t *testing.T) {
	selected := map[string]selectedSession{
		"a": {SessionID: "a"},
		"b": {SessionID: "b"},
		"c": {SessionID: "c", CompactTranscriptPath: "c/transcript.jsonl"},
	}
	members := []sessionCheckpoint{
		{SessionID: "a", CheckpointID: "cp1"},
		{SessionID: "a", CheckpointID: "cp2"},
		{SessionID: "b", CheckpointID: "cp1"},
		{SessionID: "c", CheckpointID: "cp2", CompactTranscriptPath: "c/transcript.jsonl"},
	}
	mode, warnings := useCompactTranscripts(selected, members)
	if mode != "raw" {
		t.Fatalf("mode = %s, want raw", mode)
	}
	want := "compact transcripts unavailable for 2 of 3 exported v1 sessions and 3 of 4 session checkpoints; exported raw full.jsonl logs"
	if len(warnings) != 1 || warnings[0] != want {
		t.Fatalf("warnings = %q, want %q", warnings, want)
	}
	if got := usefulExportWarningText(warnings[0]); !strings.Contains(got, "(2 of 3 exported v1 sessions and 3 of 4 session checkpoints)") {
		t.Fatalf("readme text = %q", got)
	}

	// Every exported session has a compact transcript, but an earlier
	// checkpoint does not: still raw, and the warning says why.
	selected["a"] = selectedSession{SessionID: "a", CompactTranscriptPath: "a/transcript.jsonl"}
	selected["b"] = selectedSession{SessionID: "b", CompactTranscriptPath: "b/transcript.jsonl"}
	members = []sessionCheckpoint{
		{SessionID: "a", CheckpointID: "cp1"},
		{SessionID: "a", CheckpointID: "cp2", CompactTranscriptPath: "a/transcript.jsonl"},
		{SessionID: "b", CheckpointID: "cp1", CompactTranscriptPath: "b/transcript.jsonl"},
		{SessionID: "c", CheckpointID: "cp2", CompactTranscriptPath: "c/transcript.jsonl"},
	}
	mode, warnings = useCompactTranscripts(selected, members)
	want = "compact transcripts unavailable for 0 of 3 exported v1 sessions and 1 of 4 session checkpoints; exported raw full.jsonl logs"
	if mode != "raw" || len(warnings) != 1 || warnings[0] != want {
		t.Fatalf("mode = %s warnings = %q, want raw and %q", mode, warnings, want)
	}

	members[0].CompactTranscriptPath = "a/cp1-transcript.jsonl"
	if mode, _ := useCompactTranscripts(selected, members); mode != "compact" {
		t.Fatalf("mode = %s, want compact once every transcript has one", mode)
	}
}
