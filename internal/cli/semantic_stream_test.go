package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// semanticStreamLeanHeader is a minimal streaming header: identity fields only,
// no aggregate metadata (that lives in the trailing summary).
const semanticStreamLeanHeader = `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.2.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111"}`

func semanticStreamSummaryFixture() string {
	lines := []string{
		semanticStreamLeanHeader,
		`{"record_type":"file","file_path":"internal/auth/token.go","blob":"abc123","language":"Go"}`,
		`{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}`,
		`{"record_type":"relation","from_id":"caller","to_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","type":"CALLS","confidence":1}`,
		`{"record_type":"external","id":"external:package:fmt","kind":"package","name":"fmt"}`,
		`{"record_type":"future_thing","id":"x","note":"forward compatible payload"}`,
		`{"record_type":"summary","languages":["Go"],"capabilities":["go"],"profile":"default","relation_set":["CALLS"],"skipped_relation_families":["routes"],"completeness":"partial","profile_limits":{"max_files":1000},"stats":{"files":1,"symbols":1,"relations":1,"externals":1},"warnings":[{"code":"summary_warning","severity":"warning","detail":"from summary"}],"partial_failures":[]}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestScanSemanticStreamCapturesSummaryAndCounts(t *testing.T) {
	out := &bytes.Buffer{}
	res, err := scanSemanticStream(strings.NewReader(semanticStreamSummaryFixture()), out, semanticStreamScanConfig{})
	if err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if !res.haveHeader {
		t.Fatal("expected header to be parsed")
	}
	if res.summary == nil {
		t.Fatal("expected trailing summary to be captured")
	}
	if res.counts.Symbols != 1 || res.counts.Relations != 1 || res.counts.Files != 1 {
		t.Fatalf("unexpected counts: %+v", res.counts)
	}
	if res.stream.Externals != 1 {
		t.Fatalf("expected 1 external, got %d", res.stream.Externals)
	}
	if res.stream.Unknown != 1 {
		t.Fatalf("expected 1 unknown record, got %d", res.stream.Unknown)
	}
	if len(res.extraWarnings) != 1 || res.extraWarnings[0].Code != "provider_unknown_record_type" {
		t.Fatalf("expected unknown-record warning, got %+v", res.extraWarnings)
	}
	if got := res.summary.Completeness; got != "partial" {
		t.Fatalf("summary completeness = %q, want partial", got)
	}
}

func TestScanSemanticStreamPreservesUnknownAndExternalVerbatim(t *testing.T) {
	out := &bytes.Buffer{}
	if _, err := scanSemanticStream(strings.NewReader(semanticStreamSummaryFixture()), out, semanticStreamScanConfig{}); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	if !strings.Contains(out.String(), `"record_type":"future_thing"`) {
		t.Fatalf("unknown record was not preserved verbatim:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"forward compatible payload"`) {
		t.Fatalf("unknown record fields were dropped:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"record_type":"external"`) {
		t.Fatalf("external record was not preserved:\n%s", out.String())
	}
}

func TestScanSemanticStreamFilteredArtifactIsValidNDJSON(t *testing.T) {
	out := &bytes.Buffer{}
	if _, err := scanSemanticStream(strings.NewReader(semanticStreamSummaryFixture()), out, semanticStreamScanConfig{}); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected multiple NDJSON lines, got %d", len(lines))
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i+1, err, line)
		}
	}
	// First line is the header (no record_type); last line is the summary.
	var first map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	if _, ok := first["record_type"]; ok {
		t.Fatalf("first line should be the header, got %s", lines[0])
	}
	var last map[string]any
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if last["record_type"] != "summary" {
		t.Fatalf("last line should be the summary, got %s", lines[len(lines)-1])
	}
}

func TestScanSemanticStreamReportsMalformedLine(t *testing.T) {
	input := semanticStreamLeanHeader + "\n" + `{"record_type":"symbol" BROKEN` + "\n"
	_, err := scanSemanticStream(strings.NewReader(input), io.Discard, semanticStreamScanConfig{})
	if err == nil {
		t.Fatal("expected error for malformed line")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error should identify the malformed line: %v", err)
	}
}

func TestScanSemanticStreamProcessesRecordsIncrementally(t *testing.T) {
	records := []string{
		`{"record_type":"file","file_path":"a.go","language":"Go"}`,
		`{"record_type":"symbol","id":"s1","kind":"function","name":"A","file_path":"a.go","stable_id_version":"1"}`,
		`{"record_type":"relation","from_id":"caller","to_id":"s1","type":"CALLS","confidence":1}`,
		`{"record_type":"summary","languages":["Go"]}`,
	}
	pr, pw := io.Pipe()
	proceed := make(chan struct{})
	cfg := semanticStreamScanConfig{
		onRecord: func(string) {
			// Block until the producer acknowledges, proving the consumer reaches
			// this record before the next one is even written. If the scanner
			// buffered the whole stream first, the producer would never write
			// past record 1 and this would not complete.
			proceed <- struct{}{}
		},
	}
	go func() {
		fmt.Fprintln(pw, semanticStreamLeanHeader)
		for _, rec := range records {
			fmt.Fprintln(pw, rec)
			<-proceed
		}
		_ = pw.Close()
	}()

	done := make(chan semanticStreamResult, 1)
	go func() {
		res, err := scanSemanticStream(pr, io.Discard, cfg)
		if err != nil {
			t.Errorf("scan stream: %v", err)
		}
		done <- res
	}()

	select {
	case res := <-done:
		if res.counts.Symbols != 1 || res.counts.Relations != 1 {
			t.Fatalf("unexpected counts: %+v", res.counts)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("streaming did not complete; records were not processed incrementally")
	}
}

// generatedSemanticStream lazily emits a header, n relation records, and a
// trailing summary without ever materializing the whole stream in memory, so
// the test can prove the scanner does not buffer proportionally.
type generatedSemanticStream struct {
	n       int
	i       int
	pending []byte
	bytes   int64
}

func (g *generatedSemanticStream) line(i int) (string, bool) {
	switch {
	case i == 0:
		return semanticStreamLeanHeader, true
	case i <= g.n:
		return fmt.Sprintf(`{"record_type":"relation","from_id":"caller-%d","to_id":"callee-%d","type":"CALLS","confidence":1}`, i, i), true
	case i == g.n+1:
		return `{"record_type":"summary","languages":["Go"]}`, true
	default:
		return "", false
	}
}

func (g *generatedSemanticStream) Read(p []byte) (int, error) {
	for len(g.pending) == 0 {
		line, ok := g.line(g.i)
		if !ok {
			return 0, io.EOF
		}
		g.i++
		g.pending = []byte(line + "\n")
	}
	n := copy(p, g.pending)
	g.pending = g.pending[n:]
	g.bytes += int64(n)
	return n, nil
}

func TestScanSemanticStreamDoesNotBufferProportionally(t *testing.T) {
	const records = 300_000
	gen := &generatedSemanticStream{n: records}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	res, err := scanSemanticStream(gen, io.Discard, semanticStreamScanConfig{})
	if err != nil {
		t.Fatalf("scan stream: %v", err)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	if res.counts.Relations != records {
		t.Fatalf("relation count = %d, want %d", res.counts.Relations, records)
	}
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if heapDelta < 0 {
		heapDelta = 0
	}
	// The stream is tens of megabytes; retained heap must be a tiny fraction of
	// it (relations are not retained in any map, so growth should be ~flat).
	if heapDelta*4 > gen.bytes {
		t.Fatalf("retained heap %d bytes is not bounded vs %d streamed bytes", heapDelta, gen.bytes)
	}
}

func TestStreamSemanticSnapshotReportsProviderFailureAfterPartialOutput(t *testing.T) {
	repoDir := t.TempDir()
	partial := semanticStreamLeanHeader + "\n" +
		`{"record_type":"symbol","id":"s1","kind":"function","name":"A","file_path":"a.go","stable_id_version":"1"}` + "\n"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"): {
			stdout: partial,
			stderr: "provider crashed mid-stream",
			err:    fmt.Errorf("exit status 1"),
		},
	}}
	out := &bytes.Buffer{}
	res, err := streamSemanticSnapshot(context.Background(), runner, repoDir, semanticIndexOptions{semBinary: "entire"}, nil, brainIgnore{}, out)
	if err == nil {
		t.Fatal("expected provider failure error")
	}
	if !strings.Contains(err.Error(), "semantic provider snapshot failed") {
		t.Fatalf("error should report provider failure: %v", err)
	}
	if !strings.Contains(err.Error(), "provider crashed mid-stream") {
		t.Fatalf("error should include provider stderr: %v", err)
	}
	// Partial records observed before the failure are still counted, but the
	// caller (runSemanticIndex) aborts on the error and never persists them.
	if res.counts.Symbols != 1 {
		t.Fatalf("expected partial symbol count, got %+v", res.counts)
	}
}

func TestSemanticIndexUsesSummaryMetadataOverLeanHeader(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticStreamSummaryFixture())
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     time.Now,
	}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	source := mustSemanticSource(t, env)
	if !source.SummaryPresent {
		t.Fatal("expected SummaryPresent to be true")
	}
	if len(source.Languages) != 1 || source.Languages[0] != "Go" {
		t.Fatalf("languages from summary not applied: %+v", source.Languages)
	}
	if source.Completeness != "partial" {
		t.Fatalf("completeness from summary not applied: %q", source.Completeness)
	}
	if source.Profile != "default" {
		t.Fatalf("profile from summary not applied: %q", source.Profile)
	}
	if len(source.RelationSet) != 1 || source.RelationSet[0] != "CALLS" {
		t.Fatalf("relation set from summary not applied: %+v", source.RelationSet)
	}
	if len(source.SkippedRelationFamilies) != 1 || source.SkippedRelationFamilies[0] != "routes" {
		t.Fatalf("skipped relation families from summary not applied: %+v", source.SkippedRelationFamilies)
	}
	if source.Externals != 1 {
		t.Fatalf("external count not recorded: %d", source.Externals)
	}
	if !semanticWarningsContainCode(source.Warnings, "summary_warning") {
		t.Fatalf("summary warning missing from manifest (lean header had none): %+v", source.Warnings)
	}
	if !semanticWarningsContainCode(source.Warnings, "provider_unknown_record_type") {
		t.Fatalf("unknown-record warning missing from manifest: %+v", source.Warnings)
	}
}

func TestSemanticIndexPersistsSummaryRecordInSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticStreamSummaryFixture())
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	source := mustSemanticSource(t, env)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(source.SnapshotPath))
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("persisted snapshot has invalid NDJSON line: %v\n%s", err, line)
		}
	}
	if !strings.Contains(string(data), `"record_type":"summary"`) {
		t.Fatalf("persisted snapshot missing summary record:\n%s", string(data))
	}
}
