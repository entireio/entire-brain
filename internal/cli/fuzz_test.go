package cli

import (
	"bufio"
	"bytes"
	"io"
	"testing"
)

// These fuzz harnesses assert a hard robustness invariant for every parser that
// ingests untrusted input: arbitrary bytes must never panic the process. They do
// not assert correctness of the parse — only that malformed, truncated, or
// hostile input is handled by returning an error/empty result rather than
// crashing. Run with: go test ./internal/cli -run xxx -fuzz FuzzName

func FuzzReadMCPMessage(f *testing.F) {
	f.Add([]byte("Content-Length: 2\r\n\r\n{}"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"))
	f.Add([]byte("Content-Length: -1\r\n\r\n"))
	f.Add([]byte("Content-Length: 999999999\r\n\r\nshort"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// May loop over multiple frames until EOF/error; bounded by input length.
		r := bufio.NewReader(bytes.NewReader(data))
		for i := 0; i < 64; i++ {
			if _, _, err := readMCPMessage(r); err != nil {
				return
			}
		}
	})
}

func FuzzScanSemanticStream(f *testing.F) {
	f.Add([]byte(`{"schema_version":"1"}` + "\n" + `{"record_type":"file","file_path":"a.go"}` + "\n"))
	f.Add([]byte(`{"record_type":"symbol" BROKEN`))
	f.Add([]byte("\x00\x01\x02 not json at all"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = scanSemanticStream(bytes.NewReader(data), io.Discard, semanticStreamScanConfig{})
	})
}

func FuzzParseFactLine(f *testing.F) {
	f.Add("decision/architecture\tWe chose X over Y")
	f.Add("\t\t\t")
	f.Add("no-tab-here")
	f.Fuzz(func(t *testing.T, line string) {
		_, _, _ = parseFactLine(line)
	})
}

func FuzzParseDocumentConversation(f *testing.F) {
	f.Add(`{"role":"user","content":"hi"}` + "\n" + `{"role":"assistant","content":"yo"}`)
	f.Add("{not json")
	f.Add("")
	f.Fuzz(func(t *testing.T, content string) {
		_, _ = parseDocumentConversation(content)
	})
}
