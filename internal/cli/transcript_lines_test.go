package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestTranscriptLinesBoundaries(t *testing.T) {
	for _, tc := range []struct {
		input         string
		text          string
		line, skipped int
	}{
		{"1234\nend", "end", 2, 0},
		{"1234\r\nend\r\n", "end", 2, 0},
		{"12345\n123456\r\nend", "end", 3, 2},
		{"12345", "", 1, 1},
		{"1234", "1234", 1, 0},
	} {
		t.Run(tc.input, func(t *testing.T) {
			s := newTranscriptLines(strings.NewReader(tc.input), 4)
			last := ""
			for s.Scan() {
				last = s.Text()
			}
			if s.Err() != nil || last != tc.text || s.line != tc.line || s.skipped != tc.skipped {
				t.Fatalf("last=%q line=%d skipped=%d err=%v", last, s.line, s.skipped, s.Err())
			}
		})
	}
}

type transcriptFailReader struct{}

func (transcriptFailReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestTranscriptLinesReadFailureAndCancellation(t *testing.T) {
	s := newTranscriptLines(io.MultiReader(strings.NewReader(strings.Repeat("x", 100000)), transcriptFailReader{}), 4)
	if s.Scan() || !errors.Is(s.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("error=%v", s.Err())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = newTranscriptLines(contextCheckingReader{ctx: ctx, r: strings.NewReader("ok\n")}, 4)
	if s.Scan() || !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("error=%v", s.Err())
	}
}

func TestInspectRawTextMatchesAfterOversizedLine(t *testing.T) {
	dir := t.TempDir()
	writeBundleTestFile(t, dir+"/notes.txt", strings.Repeat("x", brainInspectHistoryMaxLine+1)+"\nretainedcanary\n")
	report, err := inspectBrainRawText(dir, "all", "retainedcanary", 10)
	if err != nil || len(report.ScanErrors) != 0 || report.SkippedLines != 1 || len(report.Matches) != 1 || report.Matches[0].Line != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestInspectRawTextSkippedLinesArePartial(t *testing.T) {
	oversized := strings.Repeat("x", brainInspectHistoryMaxLine+1)
	for _, tc := range []struct {
		name, input string
		maxHits     int
		partial     bool
		matches     int
	}{
		{"skipped-only", oversized, 10, true, 0},
		{"retained-match", oversized + "\nretainedcanary\n", 10, true, 1},
		{"hit-limit", oversized + "\nretainedcanary\n", 1, true, 1},
		{"complete", "retainedcanary\n", 10, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBundleTestFile(t, dir+"/notes.txt", tc.input)
			report, err := inspectBrainRawText(dir, "all", "retainedcanary", tc.maxHits)
			if err != nil || report.Partial != tc.partial || len(report.Matches) != tc.matches {
				t.Fatalf("partial=%v matches=%d err=%v", report.Partial, len(report.Matches), err)
			}
		})
	}
}

func TestTranscriptLeadingWhitespaceDocument(t *testing.T) {
	data := "\n \t\r\n{\n  \"messages\": [\n    {\"info\":{\"role\":\"user\"},\"parts\":[{\"type\":\"text\",\"text\":\"Explain the decision\"}]},\n    {\"info\":{\"role\":\"assistant\"},\"parts\":[{\"type\":\"text\",\"text\":\"Decision: retained document\"}]}\n  ]\n}\n"
	dir := t.TempDir()
	rel := "sessions/main/document.json"
	writeBundleTestFile(t, dir+"/"+rel, data)
	records, err := scanHistoryFileReader(context.Background(), strings.NewReader(data), rel, ".json")
	if err != nil || len(records) == 0 {
		t.Fatalf("history=%+v err=%v", records, err)
	}
	scan, err := scanConversationTranscriptFileContext(context.Background(), strings.NewReader(data), rel)
	if err != nil || len(scan.Exchanges) != 1 {
		t.Fatalf("scan=%+v err=%v", scan, err)
	}
	record := conversationExchangeRecords(rel, scan)[0]
	if record.Line != 5 || record.EndLine != 9 || record.SourceDigest != conversationDigest([]byte(data)) {
		t.Fatal("raw digest lost")
	}
	expanded, err := expandConversationExchange(dir, record)
	if err != nil || expanded.Response != "Decision: retained document" {
		t.Fatalf("expanded=%+v err=%v", expanded, err)
	}
}

type cancelTranscriptReader struct {
	cancel    context.CancelFunc
	remaining int
}

func (r *cancelTranscriptReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	r.cancel()
	return n, nil
}
func TestHistoryCancellationWhileDraining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := scanHistoryLines(ctx, "session.txt", ".txt", &cancelTranscriptReader{cancel: cancel, remaining: historyMaxLineBytes * 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("drain error=%v", err)
	}
}

func TestTranscriptCapAcrossHistoryConversationAndExpansion(t *testing.T) {
	for _, extra := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			request := `{"type":"user","message":{"role":"user","content":"Explain retained boundaries"}}`
			response := `{"type":"assistant","message":{"role":"assistant","content":"Decision: boundary response"}}`
			response += strings.Repeat(" ", historyMaxLineBytes+extra-len(response))
			after := `{"type":"assistant","message":{"role":"assistant","content":"Decision: final response"}}`
			data := request + "\r\n" + response + "\r\n" + after
			dir := t.TempDir()
			rel := "sessions/main/boundary.jsonl"
			writeBundleTestFile(t, dir+"/"+rel, data)
			records, err := scanHistoryFileReader(context.Background(), strings.NewReader(data), rel, ".jsonl")
			wantRecords := 1
			wantSkipped := 1
			wantResponse := "Decision: final response"
			if extra == 0 {
				wantRecords = 2
				wantSkipped = 0
				wantResponse = "Decision: boundary response\nDecision: final response"
			}
			decisions := 0
			for _, r := range records {
				if r.Kind == "decision" {
					decisions++
				}
			}
			if err != nil || decisions != wantRecords {
				t.Fatalf("history decisions=%d err=%v", decisions, err)
			}
			scan, err := scanConversationTranscriptFileContext(context.Background(), strings.NewReader(data), rel)
			if err != nil || scan.SkippedLines != wantSkipped || len(scan.Exchanges) != 1 {
				t.Fatalf("scan=%+v err=%v", scan, err)
			}
			record := conversationExchangeRecords(rel, scan)[0]
			out, err := expandConversationExchange(dir, record)
			if err != nil || out.Response != wantResponse || record.Line != 1 || record.EndLine != 3 {
				t.Fatalf("out=%+v record=%+v err=%v", out, record, err)
			}
		})
	}
}
