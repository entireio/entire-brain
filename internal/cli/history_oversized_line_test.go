package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// The session is indexed without one line, so the overlay must not claim
	// to represent it fully, on this delta or on the next one that reuses the
	// entry unchanged.
	for pass := 1; pass <= 2; pass++ {
		if pass == 2 {
			if stats := buildShortTerm(t, brainDir); stats.Reused == 0 || stats.Failed != 0 {
				t.Fatalf("second delta stats = %+v, want the entry reused", stats)
			}
			overlay, _ = loadHistoryShortTermState(brainDir, manifest.Sources.History)
		}
		if got := overlay.Files[rel].SkippedLines; got != 1 {
			t.Fatalf("pass %d: overlay entry skipped_lines = %d, want 1", pass, got)
		}
		if overlay.complete() {
			t.Fatalf("pass %d: the overlay claimed complete coverage of a transcript it indexed without a line", pass)
		}
		if why := shortTermIncompleteness(overlay); !strings.Contains(why, "1 transcripts indexed without lines") {
			t.Fatalf("pass %d: incompleteness = %q, want the skipped line named", pass, why)
		}
	}
}

// pastedScreenshotTranscript is a Claude Code session in which the user's
// second turn (line 3) pastes a screenshot too large to read. The answer on
// line 4 belongs to that turn.
func pastedScreenshotTranscript() string {
	image := strings.Repeat("A", historyMaxLineBytes+1024)
	return `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Why does the export drop the screenshot session?"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: skip the oversized line and keep the screenshot session."}]}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Here is what the export shows now."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + image + `"}}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Answer to the unread turn: that screenshot shows a finished export."}]}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Does the second exchange survive too?"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: lines after the skipped one keep their numbers."}]}}` + "\n"
}

// A user turn too long to read is still a turn: the answer after it must not
// be attributed to the request before it.
func TestConversationScanEndsExchangeAtUnreadUserTurn(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	rel := "sessions/main/20261112T090000Z_pasted.jsonl"
	content := pastedScreenshotTranscript()
	writeBundleTestFile(t, filepath.Join(brainDir, filepath.FromSlash(rel)), content)

	scan, err := scanLineConversation(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Exchanges) != 2 || scan.Incomplete != 1 {
		t.Fatalf("exchanges = %+v incomplete = %d, want 2 exchanges and the unread turn counted incomplete", scan.Exchanges, scan.Incomplete)
	}
	first, second := scan.Exchanges[0], scan.Exchanges[1]
	if first.Line != 1 || first.EndLine != 2 || first.ResponseOverflow || strings.Contains(first.Response, "unread turn") {
		t.Fatalf("first exchange = %+v, want lines 1-2 without the answer to the unread turn", first)
	}
	if second.Line != 5 || second.EndLine != 6 || second.TurnOrdinal != 3 {
		t.Fatalf("second exchange = %+v, want lines 5-6 as turn 3", second)
	}

	records := conversationExchangeRecords(rel, scan)
	expansion, err := expandConversationExchange(brainDir, records[0])
	if err != nil {
		t.Fatal(err)
	}
	if expansion.Truncated || strings.Contains(expansion.Response, "unread turn") {
		t.Fatalf("expansion = %+v, want the first exchange whole, without the unread turn's answer", expansion)
	}
}

// codexImageTranscript is a Codex session whose second turn carries a
// screenshot. Codex records each user turn twice (response_item, then
// event_msg); itemImage and eventImage choose which copies carry the image
// that makes them too long to read.
func codexImageTranscript(t *testing.T, itemImage, eventImage bool) string {
	t.Helper()
	image := `"data:image/png;base64,` + strings.Repeat("A", historyMaxLineBytes+1024) + `"`
	user := func(text string, itemImage, eventImage bool) string {
		item := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}`
		event := `{"type":"event_msg","payload":{"type":"user_message","message":"` + text + `"`
		if itemImage {
			item += `,{"type":"input_image","image_url":` + image + `}`
		}
		if eventImage {
			event += `,"images":[` + image + `]`
		}
		return item + "]}}\n" + event + "}}\n"
	}
	assistant := func(text string) string {
		return `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}` + "\n"
	}
	content := user("First question about the export?", false, false) + assistant("First answer.") +
		user("Here is the screenshot.", itemImage, eventImage) + assistant("Answer to the screenshot turn.") +
		user("Second question about the export?", false, false) + assistant("Second answer.")
	for i, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("fixture line %d is not JSON", i+1)
		}
	}
	return content
}

// When both Codex copies of a turn are too long to read they are one unread
// turn, counted once.
func TestConversationScanCountsDuplicateUnreadTurnOnce(t *testing.T) {
	t.Parallel()
	scan, err := scanLineConversation(strings.NewReader(codexImageTranscript(t, true, true)))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Exchanges) != 2 || scan.Incomplete != 1 {
		t.Fatalf("exchanges = %+v incomplete = %d, want 2 exchanges and one incomplete turn", scan.Exchanges, scan.Incomplete)
	}
	if first := scan.Exchanges[0]; first.EndLine != 3 || strings.Contains(first.Response, "screenshot turn") {
		t.Fatalf("first exchange = %+v, want it to end before the unread turn", first)
	}
	if second := scan.Exchanges[1]; second.Line != 7 || second.TurnOrdinal != 3 || second.Response != "Second answer." {
		t.Fatalf("second exchange = %+v, want line 7, turn 3", second)
	}
}

// Codex wraps the image differently in its two copies of a turn, so near the
// limit only one copy can be too long. The readable copy is then the turn,
// whichever comes first, and it keeps its answer.
func TestConversationScanKeepsTurnWithOneReadableCodexCopy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                  string
		itemImage, eventImage bool
		requestLine           int
	}{
		{"event copy too long", false, true, 4},
		{"item copy too long", true, false, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scan, err := scanLineConversation(strings.NewReader(codexImageTranscript(t, tc.itemImage, tc.eventImage)))
			if err != nil {
				t.Fatal(err)
			}
			if len(scan.Exchanges) != 3 || scan.Incomplete != 0 {
				t.Fatalf("exchanges = %+v incomplete = %d, want 3 exchanges and none incomplete", scan.Exchanges, scan.Incomplete)
			}
			middle := scan.Exchanges[1]
			if middle.Request != "Here is the screenshot." || middle.Response != "Answer to the screenshot turn." || middle.Line != tc.requestLine || middle.EndLine != 6 || middle.TurnOrdinal != 2 {
				t.Fatalf("screenshot exchange = %+v, want its request from line %d and its answer, as turn 2", middle, tc.requestLine)
			}
			if last := scan.Exchanges[2]; last.TurnOrdinal != 3 {
				t.Fatalf("last exchange = %+v, want turn 3", last)
			}
		})
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
