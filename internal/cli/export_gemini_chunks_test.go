package cli

import (
	"context"
	"encoding/json"
	"testing"
)

// The CLI splits an oversized transcript into `full.jsonl` plus numbered
// `full.jsonl.NNN` parts and reassembles them format-aware: JSONL agents get a
// newline join, and Gemini — the one agent whose transcript is a standalone
// JSON document — gets its per-chunk {"messages":[...]} arrays concatenated into
// ONE document (cmd/entire/cli/agent/chunking.go, agent/geminicli/gemini.go).
//
// The export reader did neither: it returned chunk 0 alone whenever the base
// path was in the tree, and byte-joined with "\n" otherwise, which for Gemini
// yields two top-level JSON documents in one file.

const (
	geminiChunkOne = `{"messages":[{"role":"user","content":"first"}]}`
	geminiChunkTwo = `{"messages":[{"role":"model","content":"second"},{"role":"user","content":"third"}]}`
)

// chunkedTranscriptFixture builds a source whose session transcript is stored as
// the base file plus one numbered chunk, exactly as the CLI writes it.
func chunkedTranscriptFixture(t *testing.T, chunkZero, chunkOne string) (checkpointSnapshotSource, *fakeCommandRunner, string) {
	t.Helper()
	const root = "aa/aaaaaaaaaa"
	base := root + "/0/full.jsonl"
	source := checkpointSnapshotSource{
		GitDir:      t.TempDir(),
		Ref:         v1MainRef,
		VirtualRoot: root,
		ActualRoot:  root,
		TreePaths:   map[string]struct{}{base: {}, base + ".001": {}},
	}
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+base):        {stdout: chunkZero},
			fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+base+".001"): {stdout: chunkOne},
		},
	}
	return source, runner, base
}

func assertMergedGeminiDocument(t *testing.T, data []byte) {
	t.Helper()
	var doc struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("reassembled Gemini transcript is not valid JSON: %v\ngot: %s", err, data)
	}
	want := []string{"first", "second", "third"}
	if len(doc.Messages) != len(want) {
		t.Fatalf("reassembled Gemini transcript has %d messages, want %d\ngot: %s", len(doc.Messages), len(want), data)
	}
	for i, message := range doc.Messages {
		if message.Content != want[i] {
			t.Errorf("message %d content = %q, want %q (chunk order not preserved)", i, message.Content, want[i])
		}
	}
}

// TestReadCheckpointSourceTranscript_MergesGeminiChunks pins that the per-source
// reader, which every local checkpoint store union export goes through,
// reassembles a Gemini transcript into one valid JSON document.
func TestReadCheckpointSourceTranscript_MergesGeminiChunks(t *testing.T) {
	t.Parallel()

	source, runner, base := chunkedTranscriptFixture(t, geminiChunkOne, geminiChunkTwo)

	got, err := readCheckpointSourceTranscript(context.Background(), runner, source, base)
	if err != nil {
		t.Fatalf("readCheckpointSourceTranscript: %v", err)
	}
	assertMergedGeminiDocument(t, got)
}

// TestReadSnapshotTranscript_MergesGeminiChunks pins the same contract on the
// snapshot-wide reader that `brain verify` and the single-ref snapshot paths use.
func TestReadSnapshotTranscript_MergesGeminiChunks(t *testing.T) {
	t.Parallel()

	source, runner, base := chunkedTranscriptFixture(t, geminiChunkOne, geminiChunkTwo)
	snapshot := &checkpointSnapshot{GitDir: source.GitDir, Ref: source.Ref, TreePaths: source.TreePaths}

	got, err := readSnapshotTranscript(context.Background(), runner, snapshot, base)
	if err != nil {
		t.Fatalf("readSnapshotTranscript: %v", err)
	}
	assertMergedGeminiDocument(t, got)
}

// TestReadCheckpointSourceTranscript_JoinsJSONLChunks pins that JSONL agents
// keep the newline join, and that the trailing chunks are not dropped.
func TestReadCheckpointSourceTranscript_JoinsJSONLChunks(t *testing.T) {
	t.Parallel()

	source, runner, base := chunkedTranscriptFixture(t, `{"type":"user"}`, `{"type":"assistant"}`)

	got, err := readCheckpointSourceTranscript(context.Background(), runner, source, base)
	if err != nil {
		t.Fatalf("readCheckpointSourceTranscript: %v", err)
	}
	want := "{\"type\":\"user\"}\n{\"type\":\"assistant\"}"
	if string(got) != want {
		t.Fatalf("JSONL chunks = %q, want %q", got, want)
	}
}

// TestReadSnapshotTranscript_JoinsJSONLChunks pins the same for the
// snapshot-wide reader.
func TestReadSnapshotTranscript_JoinsJSONLChunks(t *testing.T) {
	t.Parallel()

	source, runner, base := chunkedTranscriptFixture(t, `{"type":"user"}`, `{"type":"assistant"}`)
	snapshot := &checkpointSnapshot{GitDir: source.GitDir, Ref: source.Ref, TreePaths: source.TreePaths}

	got, err := readSnapshotTranscript(context.Background(), runner, snapshot, base)
	if err != nil {
		t.Fatalf("readSnapshotTranscript: %v", err)
	}
	want := "{\"type\":\"user\"}\n{\"type\":\"assistant\"}"
	if string(got) != want {
		t.Fatalf("JSONL chunks = %q, want %q", got, want)
	}
}

// TestReadCheckpointSourceTranscript_UnchunkedIsByteIdentical keeps the
// single-file case byte-for-byte unchanged, Gemini shape included.
func TestReadCheckpointSourceTranscript_UnchunkedIsByteIdentical(t *testing.T) {
	t.Parallel()

	const root = "aa/aaaaaaaaaa"
	base := root + "/0/full.jsonl"
	source := checkpointSnapshotSource{
		GitDir:      t.TempDir(),
		Ref:         v1MainRef,
		VirtualRoot: root,
		ActualRoot:  root,
		TreePaths:   map[string]struct{}{base: {}},
	}
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+base): {stdout: geminiChunkOne + "\n"},
		},
	}

	got, err := readCheckpointSourceTranscript(context.Background(), runner, source, base)
	if err != nil {
		t.Fatalf("readCheckpointSourceTranscript: %v", err)
	}
	if string(got) != geminiChunkOne+"\n" {
		t.Fatalf("unchunked transcript = %q, want it returned verbatim", got)
	}
}
