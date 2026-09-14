package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRecallEvidenceLargeOutputBudgets(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		sessions, textBytes, budget int
	}{
		{"single_block_above_old_ceiling", 1, 40 * 1024, 64 * 1024},
		{"shared_pool_across_sessions", 8, 10 * 1024, 128 * 1024},
		{"near_maximum_output", 1, 1000 * 1024, 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, brain := evidenceFixture(t)
			m, err := loadBrainManifest(brain)
			if err != nil {
				t.Fatal(err)
			}
			m.Sources.Sessions.Sessions = nil
			texts := map[string]string{}
			for i := 0; i < tc.sessions; i++ {
				id := fmt.Sprintf("large-%d", i)
				rel := "sessions/" + id + ".txt"
				texts[id] = "target " + strings.Repeat("x", tc.textBytes)
				if err := os.WriteFile(filepath.Join(brain, rel), []byte(texts[id]), 0600); err != nil {
					t.Fatal(err)
				}
				m.Sources.Sessions.Sessions = append(m.Sources.Sessions.Sessions, exportSession{SessionID: id, Branch: "main", TranscriptPath: rel})
			}
			if err := writeBrainManifestAndReadme(brain, *m); err != nil {
				t.Fatal(err)
			}
			out, err := execute(t, newRecallCommand(opts), "target", "--evidence", "--json", "--evidence-bytes", strconv.Itoa(tc.budget))
			if err != nil {
				t.Fatal(err)
			}
			var got evidenceRecallResult
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if got.ReturnedCount != tc.sessions || got.InputTruncated || got.Truncated {
				t.Fatalf("large budget lost source blocks: returned=%d input_truncated=%v truncated=%v warnings=%v", got.ReturnedCount, got.InputTruncated, got.Truncated, got.Warnings)
			}
			encoded, err := json.Marshal(got.Evidence)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) <= 24*1024 || len(encoded) > tc.budget || got.EvidenceBytes != len(encoded) {
				t.Fatalf("wrong byte accounting: %d reported=%d budget=%d", len(encoded), got.EvidenceBytes, tc.budget)
			}
			for _, span := range got.Evidence {
				text, ok := texts[span.SessionID]
				if !ok || span.Text != text || span.StartByte != 0 || span.EndByte != len(text) || span.SourceSHA256 != evidenceHash([]byte(text)) || span.ContentSHA256 != span.SourceSHA256 {
					t.Fatal("large output changed source citation")
				}
			}
		})
	}
}

func TestRecallEvidenceRejectsAmbiguityBeyondScanLimit(t *testing.T) {
	opts, brain := evidenceFixture(t)
	m, err := loadBrainManifest(brain)
	if err != nil {
		t.Fatal(err)
	}
	first := m.Sources.Sessions.Sessions[0]
	for i := 1; i <= 128; i++ {
		s := first
		s.SessionID = fmt.Sprintf("session-%d", i)
		m.Sources.Sessions.Sessions = append(m.Sources.Sessions.Sessions, s)
	}
	if err := writeBrainManifestAndReadme(brain, *m); err != nil {
		t.Fatal(err)
	}
	bounded, err := collectEvidence(context.Background(), brain, "main", "target", 128, 1024*1024)
	if err != nil || bounded.SessionsScanned != 128 || len(bounded.Spans) > 128 || !bounded.InputTruncated {
		t.Fatalf("larger budget bypassed scan/block limits: scanned=%d spans=%d truncated=%v err=%v", bounded.SessionsScanned, len(bounded.Spans), bounded.InputTruncated, err)
	}
	conflict := first
	conflict.TranscriptPath = "sessions/different.jsonl"
	m.Sources.Sessions.Sessions = append(m.Sources.Sessions.Sessions, conflict)
	if err := writeBrainManifestAndReadme(brain, *m); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, newRecallCommand(opts), "target", "--evidence", "--json")
	if err == nil || !strings.Contains(err.Error(), "ambiguous session identity") || strings.Contains(out, `"evidence":`) {
		t.Fatalf("ambiguous manifest emitted source evidence: err=%v", err)
	}
}

func FuzzRecallEvidencePacking(f *testing.F) {
	f.Add("<tag> café ಕನ್ನಡ\r\n\"quoted\"\x00", uint32(8192))
	f.Add("", uint32(2))
	f.Add("\xff\xfe", uint32(1048576))
	f.Add(strings.Repeat("x", 32*1024), uint32(24*1024))
	f.Fuzz(func(t *testing.T, text string, requested uint32) {
		if len(text) > 64*1024 {
			t.Skip()
		}
		budget := 2 + int(requested%(1024*1024-1))
		candidates := []evidenceSpan{
			{ID: "first", Text: text, Path: "sessions/<quoted>\".txt"},
			{ID: "second", Text: "small complete block"},
			{ID: "third", Text: text, Timestamp: text},
		}
		spans, omitted, size, err := packDeterministicEvidence(candidates, budget)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(spans)
		if err != nil || !json.Valid(encoded) || len(encoded) != size || size > budget {
			t.Fatalf("invalid JSON/accounting: size=%d encoded=%d budget=%d err=%v", size, len(encoded), budget, err)
		}
		seen := map[string]bool{}
		last := -1
		for _, span := range spans {
			found := false
			for i, original := range candidates {
				if span.ID == original.ID {
					if i <= last || span != original {
						t.Fatal("source span modified or reordered")
					}
					last, found = i, true
				}
			}
			if !found || seen[span.ID] {
				t.Fatal("invented or repeated span")
			}
			seen[span.ID] = true
		}
		for _, id := range omitted {
			if seen[id] {
				t.Fatal("omitted span was also returned")
			}
			seen[id] = true
		}
		if len(seen) != len(candidates) {
			t.Fatal("span lost without omission reporting")
		}
	})
}

func TestRecallEvidenceHiddenStructuredContent(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"analysis","content":[{"type":"output_text","text":"target SECRET"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"target SECRET"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"target SECRET"},{"type":"text","text":"target public"}]}}`,
		`{"role":"system","content":"target SECRET"}`,
	}, "\n")
	fields, partial := evidenceFields([]byte(data), "session.jsonl")
	if partial || len(fields) != 1 || fields[0].text != "target public" || fields[0].line != 3 || fields[0].pointer != "/message/content/1/text" {
		t.Fatalf("hidden content or incorrect anchor: %+v partial=%v", fields, partial)
	}
	fields, partial = evidenceFields([]byte("target \xff invalid UTF-8"), "session.txt")
	if !partial || len(fields) != 0 {
		t.Fatal("invalid source bytes were rewritten into evidence")
	}
}
