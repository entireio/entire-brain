package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestHistoryTranscriptSingleReadYieldsDigestAndRecords locks the single-read
// property that scanHistoryFileWithContentSHA used to provide: one bounded,
// descriptor-rooted read produces BOTH the verified content digest and the
// parsed records, so a same-path rewrite between hashing and parsing cannot
// produce a digest of one generation and records of another.
func TestHistoryTranscriptSingleReadYieldsDigestAndRecords(t *testing.T) {
	document := `{
  "messages": [
    {"info":{"role":"assistant"},"parts":[{"type":"text","text":"Decision: retain document parsing."}]}
  ]
}`
	cases := []struct {
		name        string
		ext         string
		content     string
		wantSummary string
		wantScanErr bool
	}{
		{name: "jsonl", ext: ".jsonl", content: `{"type":"agent_message","message":"Decision: retain JSONL parsing."}` + "\n", wantSummary: "Decision: retain JSONL parsing."},
		{name: "document", ext: ".json", content: document, wantSummary: "Decision: retain document parsing."},
		{name: "markdown", ext: ".md", content: "Decision: retain markdown parsing.\n", wantSummary: "Decision: retain markdown parsing."},
		{name: "document-shaped fallback", ext: ".md", content: "{\nDecision: retain bounded fallback parsing.\n", wantSummary: "Decision: retain bounded fallback parsing."},
		{name: "oversized line", ext: ".txt", content: "Decision: " + strings.Repeat("x", historyMaxLineBytes+1) + "\n", wantScanErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			rel := "sessions/main/session" + tc.ext
			full := filepath.Join(brainDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			inventory, err := collectHistorySessionInventory(context.Background(), brainDir)
			if err != nil {
				t.Fatal(err)
			}
			defer inventory.Close()
			if len(inventory.files) != 1 {
				t.Fatalf("inventory = %d files, want 1", len(inventory.files))
			}
			file := inventory.files[0]
			content, digest, err := readHistorySessionInventoryFile(context.Background(), inventory, file)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			sum := sha256.Sum256([]byte(tc.content))
			if want := "sha256:" + hex.EncodeToString(sum[:]); digest != want {
				t.Fatalf("digest = %q, want %q", digest, want)
			}
			records, _, warnings, ok := scanSessionFileReaderRecords(context.Background(), bytes.NewReader(content), file.Path, file.Rel)
			if tc.wantScanErr {
				if ok || len(warnings) == 0 || !strings.Contains(strings.Join(warnings, " "), "token too long") {
					t.Fatalf("oversized scan = ok:%v warnings:%v, want a token-too-long refusal", ok, warnings)
				}
				return
			}
			if !ok {
				t.Fatalf("scan failed: %v", warnings)
			}
			found := false
			for _, record := range records {
				if record.Summary == tc.wantSummary {
					found = true
				}
			}
			if !found {
				t.Fatalf("records missing %q: %#v", tc.wantSummary, records)
			}
		})
	}
}

// TestHistoryTranscriptAggregateFingerprintIsStableAndContentBound locks the
// transcripts fingerprint: it commits the exact included set, is independent of
// enumeration order, and changes when any transcript's bytes change.
func TestHistoryTranscriptAggregateFingerprintIsStableAndContentBound(t *testing.T) {
	brainDir := t.TempDir()
	document := "{\n  \"messages\": [\n    {\"info\":{\"role\":\"assistant\"},\"parts\":[{\"type\":\"text\",\"text\":\"Decision: retain document parsing.\"}]}\n  ]\n}"
	contents := map[string]string{
		"a.jsonl": `{"type":"agent_message","message":"Decision: retain JSONL parsing."}` + "\n",
		"b.json":  document,
		"c.md":    "Decision: retain markdown parsing.\n",
		"d.md":    "{\nDecision: retain bounded fallback parsing.\n",
	}
	dir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range contents {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := collectHistorySessionDigests(context.Background(), brainDir, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(files) != len(contents) {
		t.Fatalf("collected %d files, want %d", len(files), len(contents))
	}
	first := historyTranscriptFilesFingerprint(files)
	if !validHistorySHA256(first) {
		t.Fatalf("fingerprint = %q, want a sha256", first)
	}
	// Enumeration order must not matter.
	shuffled := append([]historySessionFile(nil), files...)
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i].Rel > shuffled[j].Rel })
	if got := historyTranscriptFilesFingerprint(shuffled); got != first {
		t.Fatalf("fingerprint depends on order: %q vs %q", got, first)
	}
	// A single changed byte must change it.
	if err := os.WriteFile(filepath.Join(dir, "c.md"), []byte("Decision: retain markdown parsing!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := collectHistorySessionDigests(context.Background(), brainDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := historyTranscriptFilesFingerprint(changed); got == first {
		t.Fatal("fingerprint survived a transcript content change")
	}
	// A missing digest must refuse to produce a fingerprint at all.
	partial := append([]historySessionFile(nil), files...)
	partial[0].ContentDigest = ""
	if got := historyTranscriptFilesFingerprint(partial); got != "" {
		t.Fatalf("fingerprint over an unverified set = %q, want empty", got)
	}
}

func TestHistorySingleReadPreservesIndexAndBriefPacket(t *testing.T) {
	fixture := newBrainBriefRawHistoryEndToEndFixture(t)
	storage, err := repoStoragePaths(context.Background(), fixture.opts.Runner, fixture.opts.Env, fixture.opts.Env.RepoRoot)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	now := fixture.opts.Now().UTC()
	meaningful := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main", "20260716T235959Z-single-read.jsonl")
	content := `{"type":"agent_message","message":"Decision: ALPHA_IDENTIFIER keeps transcript digests coupled to parsed records."}` + "\n" +
		`{"type":"agent_message","message":"Validated ALPHA_IDENTIFIER history packets remain byte-identical."}` + "\n"
	if err := os.WriteFile(meaningful, []byte(content), 0o600); err != nil {
		t.Fatalf("write meaningful transcript: %v", err)
	}

	wantIndex, wantSource, err := buildBrainHistoryIndexHashThenScanReference(storage.BrainDir, now)
	if err != nil {
		t.Fatalf("reference build: %v", err)
	}
	if len(wantIndex.Records) == 0 {
		t.Fatal("comparison fixture produced no history records")
	}
	if err := publishHistoryComparisonIndex(storage.BrainDir, wantIndex, wantSource); err != nil {
		t.Fatalf("publish reference: %v", err)
	}
	wantPacket := runBrainBriefRawHistoryEndToEnd(t, fixture, brainBriefRawHistoryMatchesObserved)

	if err := os.Remove(filepath.Join(storage.BrainDir, filepath.FromSlash(historyScanCachePath))); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove scan cache: %v", err)
	}
	var progress [][2]int
	gotIndex, gotSource, err := buildBrainHistoryIndex(storage.BrainDir, now, func(done, total int) {
		progress = append(progress, [2]int{done, total})
	})
	if err != nil {
		t.Fatalf("single-read build: %v", err)
	}
	if !reflect.DeepEqual(gotIndex, wantIndex) {
		t.Fatalf("single-read index changed\ngot:  %#v\nwant: %#v", gotIndex, wantIndex)
	}
	if !reflect.DeepEqual(gotSource, wantSource) {
		t.Fatalf("single-read source changed\ngot:  %#v\nwant: %#v", gotSource, wantSource)
	}
	if len(progress) < 2 || progress[0][0] != 0 || progress[0][1] == 0 || progress[len(progress)-1] != [2]int{progress[0][1], progress[0][1]} {
		t.Fatalf("single-read progress = %v", progress)
	}
	if err := publishHistoryComparisonIndex(storage.BrainDir, gotIndex, gotSource); err != nil {
		t.Fatalf("publish single-read: %v", err)
	}
	gotPacket := runBrainBriefRawHistoryEndToEnd(t, fixture, brainBriefRawHistoryMatchesObserved)
	if gotPacket != wantPacket {
		t.Fatalf("brain brief packet changed after single-read history build\ngot:\n%s\nwant:\n%s", gotPacket, wantPacket)
	}
}

// buildBrainHistoryIndexHashThenScanReference pins the pre-optimization
// behavior: hash every safe file during collection, then reopen changed files
// for parsing. It intentionally omits the scan cache so the comparison covers a
// full rebuild.
func buildBrainHistoryIndexHashThenScanReference(outputDir string, now time.Time) (historyIndex, *historySourceManifest, error) {
	index := historyIndex{GeneratedAt: now}
	files, err := collectHistorySessionDigests(context.Background(), outputDir, nil)
	if err != nil {
		return index, nil, err
	}
	transcriptsFingerprint := historyTranscriptFilesFingerprint(files)
	manifest, _ := loadBrainManifest(outputDir)
	branchByPath := historyBranchByTranscriptPath(manifest)
	newCache := historyScanCache{Version: historyScanCacheVersion, Files: make(map[string]historyScanCacheEntry, len(files))}
	seenDecisions := map[string]struct{}{}
	for _, file := range files {
		rel, relErr := filepath.Rel(outputDir, file.Path)
		if relErr != nil {
			rel = file.Path
		}
		rel = filepath.ToSlash(rel)
		records, scanErr := scanHistoryFileHashThenScanReference(outputDir, file.Path)
		if scanErr != nil {
			index.Warnings = append(index.Warnings, scanErr.Error())
			continue
		}
		newCache.Files[rel] = historyScanCacheEntry{ContentDigest: file.ContentDigest, Records: records}
		records = annotateHistoryRecordBranches(records, rel, branchByPath)
		for _, record := range records {
			if record.Kind == "decision" {
				dedupeKey := normalizeHistorySearchText(record.Summary)
				if _, ok := seenDecisions[dedupeKey]; ok {
					continue
				}
				seenDecisions[dedupeKey] = struct{}{}
			}
			index.Records = append(index.Records, record)
		}
	}
	saveHistoryScanCache(outputDir, newCache)
	sort.Slice(index.Records, func(i, j int) bool {
		if index.Records[i].Kind != index.Records[j].Kind {
			return index.Records[i].Kind < index.Records[j].Kind
		}
		if index.Records[i].Path != index.Records[j].Path {
			return index.Records[i].Path < index.Records[j].Path
		}
		return index.Records[i].Line < index.Records[j].Line
	})
	source := &historySourceManifest{
		GeneratedAt:            now,
		IndexPath:              historyIndexPath,
		SessionsFingerprint:    brainSessionsFingerprint(outputDir),
		TranscriptsFingerprint: transcriptsFingerprint,
		Records:                len(index.Records),
		Warnings:               append([]string(nil), index.Warnings...),
	}
	for _, record := range index.Records {
		switch record.Kind {
		case "decision":
			source.Decisions++
		case "learning":
			source.Learnings++
		case "validation":
			source.Validations++
		case "tool_call":
			source.ToolCalls++
		case "code_fact":
			source.CodeFacts++
		}
	}
	return index, source, nil
}

func scanHistoryFileHashThenScanReference(outputDir, path string) ([]historyRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rel, _ := filepath.Rel(outputDir, path)
	rel = filepath.ToSlash(rel)
	if records, isDocument, err := scanDocumentHistoryFileHashThenScanReference(f, rel); err != nil {
		return nil, err
	} else if isDocument {
		return records, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return scanHistoryLines(context.Background(), rel, filepath.Ext(path), f)
}

func scanDocumentHistoryFileHashThenScanReference(f *os.File, rel string) ([]historyRecord, bool, error) {
	probe := make([]byte, 4096)
	n, readErr := f.Read(probe)
	if readErr != nil && readErr != io.EOF {
		return nil, false, readErr
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(probe[:n])), "\n")
	if !strings.HasPrefix(firstLine, "{") || json.Valid([]byte(firstLine)) {
		return nil, false, nil
	}
	data, err := safeReadAll(io.MultiReader(bytes.NewReader(probe[:n]), f), maxDocumentTranscriptBytes, "document transcript "+rel)
	if err != nil {
		return nil, false, err
	}
	messages, ok := parseDocumentConversation(string(data))
	if !ok {
		return nil, false, nil
	}
	return historyDocumentRecords(rel, messages), true, nil
}

func publishHistoryComparisonIndex(brainDir string, index historyIndex, source *historySourceManifest) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	publishedSource := *source
	publishedSource.IndexBytes = int64(len(data))
	publishedSource.IndexSHA256 = historyIndexBytesFingerprint(data)
	publishedSource.RecordsFingerprint = historyRecordsFingerprint(index.Records)
	if err := writeBrainRelativeFileAtomic(brainDir, historyIndexPath, data, 0o600); err != nil {
		return err
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.History = &publishedSource
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		return err
	}
	// Exercise packet generation from a freshly derived FTS index in both arms.
	for _, suffix := range []string{"", "-shm", "-wal"} {
		if err := os.Remove(historyFTSDBPath(brainDir) + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
