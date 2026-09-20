package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The transcript ingest path reads RAW AGENT OUTPUT: JSONL a third-party agent
// wrote, or a pretty-printed session document. It is the largest untrusted input
// the brain reads on every distill, and the existing FuzzParseDocumentConversation
// covers only the document parser in isolation. These harnesses drive the whole
// dispatcher (probe -> document-or-line decision -> scan) and the distill
// pre-processor, checking parser panics, digest determinism and valid source ranges.
// Cooperative deadlines and bounded seeds do not measure peak allocation.
//
//	go test ./internal/cli -run xxx -fuzz FuzzName

// FuzzScanConversationTranscript drives the real entry point history and distill
// use, including the 4 KiB probe and the document/line fallback.
func FuzzScanConversationTranscript(f *testing.F) {
	f.Add([]byte(`{"role":"user","content":"hi"}` + "\n" + `{"role":"assistant","content":"yo"}` + "\n"))
	f.Add([]byte("{\n  \"messages\": [\n    {\"info\":{\"role\":\"user\"},\"parts\":[{\"type\":\"text\",\"text\":\"hi\"}]}\n  ]\n}"))
	f.Add([]byte("{\n  \"messages\": [\n"))
	f.Add([]byte("{" + strings.Repeat(" ", 5000) + "\n}"))
	f.Add([]byte("\x00\x01\x02"))
	f.Add([]byte(""))
	f.Add(append([]byte(`{"type":"user","message":{"content":"`), append(bytes.Repeat([]byte("x"), 2*1024*1024), []byte(`"}}`)...)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := scanConversationTranscriptFileContext(ctx, bytes.NewReader(data), "fuzz.jsonl"); err == nil {
			// A successful scan must also survive being scanned again: the
			// source digest is a stability claim about the same bytes.
			if _, err := scanConversationTranscriptFileContext(ctx, bytes.NewReader(data), "fuzz.jsonl"); err != nil {
				t.Fatalf("re-scanning identical bytes failed the second time: %v", err)
			}
		}
	})
}

// FuzzScanLineConversationDigest asserts the line scanner's source digest is a
// pure function of the bytes consumed. The digest is what later proves an
// expansion re-parsed the same content, so an unstable digest silently
// invalidates every stored anchor.
func FuzzScanLineConversationDigest(f *testing.F) {
	f.Add([]byte(`{"role":"user","content":"a"}` + "\n" + `{"role":"assistant","content":"b"}`))
	f.Add([]byte("not json\n{\n"))
	f.Add([]byte("\n\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		first, err1 := scanLineConversation(bytes.NewReader(data))
		second, err2 := scanLineConversation(bytes.NewReader(data))
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("scanning identical bytes disagreed on error: %v vs %v", err1, err2)
		}
		if err1 != nil {
			return
		}
		if first.SourceDigest != second.SourceDigest {
			t.Fatalf("source digest is not a function of the bytes: %q vs %q", first.SourceDigest, second.SourceDigest)
		}
		if len(first.Exchanges) != len(second.Exchanges) {
			t.Fatalf("exchange count is not deterministic: %d vs %d", len(first.Exchanges), len(second.Exchanges))
		}
	})
}

// FuzzDistillDocumentConversation drives the distill pre-processor, whose
// contract is a strict line-for-line correspondence with the input document: a
// fact's provenance anchor points at a line number in the ORIGINAL transcript, so
// an off-by-one here mis-attributes evidence.
func FuzzDistillDocumentConversation(f *testing.F) {
	f.Add("{\n  \"messages\": [\n    {\"info\":{\"role\":\"user\"},\"parts\":[{\"type\":\"text\",\"text\":\"hi\"}]}\n  ]\n}")
	f.Add("{\n\"messages\":[]\n}")
	f.Add("{")
	f.Add("")
	f.Fuzz(func(t *testing.T, content string) {
		out, ok := distillDocumentConversation(content)
		if !ok {
			return
		}
		inLines := strings.Count(content, "\n") + 1
		outLines := strings.Count(out, "\n") + 1
		if inLines != outLines {
			t.Fatalf("line correspondence broken: input has %d lines, output %d", inLines, outLines)
		}
	})
}

// FuzzParseDocumentConversationLines pins the anchor invariant of the document
// parser: every reported message line must be a real 1-based line of the input,
// or a stored anchor points outside the file it cites.
func FuzzParseDocumentConversationLines(f *testing.F) {
	f.Add("{\n  \"messages\": [\n    {\"info\":{\"role\":\"user\"},\"parts\":[{\"type\":\"text\",\"text\":\"hi\"}]}\n  ]\n}")
	f.Add("{\n\"messages\":[{\"info\":{\"role\":\"assistant\"},\"parts\":[{\"type\":\"tool\"}]}]\n}")
	f.Add("{\n\"a\":1,\n\"messages\":[")
	f.Fuzz(func(t *testing.T, content string) {
		messages, ok := parseDocumentConversation(content)
		if !ok {
			return
		}
		maxLine := strings.Count(content, "\n") + 1
		for _, m := range messages {
			if m.Line < 1 || m.Line > maxLine {
				t.Fatalf("message anchored at line %d, outside the document's 1..%d", m.Line, maxLine)
			}
		}
	})
}
