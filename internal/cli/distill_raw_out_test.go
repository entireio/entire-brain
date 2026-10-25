package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type rawEvalLine struct {
	Type         string   `json:"type"`
	SessionID    string   `json:"session_id"`
	Branch       string   `json:"branch"`
	ChunkIndex   int      `json:"chunk_index"`
	ChunkCount   int      `json:"chunk_count"`
	FactsEmitted int      `json:"facts_emitted"`
	Cap          int      `json:"cap"`
	FactID       string   `json:"fact_id"`
	Kind         string   `json:"kind"`
	Paths        []string `json:"paths"`
	Text         string   `json:"text"`
	CheckpointID string   `json:"checkpoint_id"`
}

func readRawEvalLines(t *testing.T, path string) (chunks, facts []rawEvalLine) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open raw output: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line rawEvalLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad JSONL line %q: %v", sc.Text(), err)
		}
		switch line.Type {
		case "chunk":
			chunks = append(chunks, line)
		case "fact":
			facts = append(facts, line)
		default:
			t.Fatalf("unknown line type %q", line.Type)
		}
	}
	return chunks, facts
}

func TestDistillRawFactsOutSkipsReconcileAndStore(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	var calls atomic.Int64
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls.Add(1)
		return "gotcha\tproject.tooling.stack\tThe project uses Go.\n", nil
	}
	rawPath := filepath.Join(t.TempDir(), "raw.jsonl")
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: 8, timeout: time.Minute, rawFactsOut: rawPath}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("raw run: %v", err)
	}
	firstCalls := calls.Load()
	if firstCalls < 2 {
		t.Fatalf("fixture should produce at least 2 extraction calls, got %d", firstCalls)
	}
	if source.ReconcileCalls != 0 {
		t.Errorf("ReconcileCalls = %d, want 0", source.ReconcileCalls)
	}

	chunks, facts := readRawEvalLines(t, rawPath)
	if int64(len(chunks)) != firstCalls {
		t.Errorf("chunk lines = %d, want one per extraction call (%d)", len(chunks), firstCalls)
	}
	if int64(len(facts)) != firstCalls {
		t.Errorf("fact lines = %d, want %d (one fact per call)", len(facts), firstCalls)
	}
	for _, c := range chunks {
		if c.SessionID == "" || c.Branch == "" || c.ChunkIndex < 1 || c.ChunkCount < c.ChunkIndex {
			t.Errorf("bad chunk line: %+v", c)
		}
		if c.FactsEmitted != 1 || c.Cap != factsMaxPerChunk {
			t.Errorf("chunk line counts wrong: %+v", c)
		}
	}
	for _, f := range facts {
		if f.FactID == "" || f.Kind != "gotcha" || len(f.Paths) != 1 || f.Paths[0] != "project.tooling.stack" || f.Text != "The project uses Go." {
			t.Errorf("bad fact line: %+v", f)
		}
	}

	for _, branch := range []string{"main", "feature"} {
		stored, err := loadFacts(brainDir, branch)
		if err != nil {
			t.Fatalf("loadFacts %s: %v", branch, err)
		}
		if len(stored) != 0 {
			t.Errorf("branch %s: store gained %d facts, want 0", branch, len(stored))
		}
	}

	// No fingerprint was cached: a second raw run re-processes every session.
	opts.rawFactsOut = filepath.Join(t.TempDir(), "raw2.jsonl")
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("second raw run: %v", err)
	}
	if calls.Load() != 2*firstCalls {
		t.Errorf("second run made %d calls, want %d (sessions must not be cached)", calls.Load()-firstCalls, firstCalls)
	}
}

func TestDistillRawFactsOutLeavesCacheAndManifestUntouched(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, rawFactsOut: filepath.Join(t.TempDir(), "raw.jsonl")}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("raw run: %v", err)
	}
	if cache := loadDistillCache(brainDir); len(cache.Sessions) != 0 {
		t.Errorf("raw run persisted distill cache entries: %+v", cache.Sessions)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources != nil && manifest.Sources.Facts != nil {
		t.Errorf("raw run stamped manifest facts source: %+v", manifest.Sources.Facts)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(factsTaxonomyPath))); err == nil {
		t.Error("raw run wrote the facts taxonomy")
	}
}

// TestDistillRawFactsOutIgnoresChunkCache: a partly cached session (stale
// fingerprint, some chunk hashes still current) must re-emit EVERY chunk in
// raw mode — chunk-cache filtering would silently undercount facts and chunks.
func TestDistillRawFactsOutIgnoresChunkCache(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	s1ChunkLines := func(rawPath string) int {
		chunks, _ := readRawEvalLines(t, rawPath)
		n := 0
		for _, c := range chunks {
			if c.SessionID == "s1" {
				n++
			}
		}
		return n
	}

	// Control: raw run on a fresh store with the extended transcript.
	freshBrain := writeDistillFixture(t, now)
	extend := func(brainDir string) {
		path := filepath.Join(brainDir, "sessions", "main", "s1.jsonl")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("turn three\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	extend(freshBrain)
	freshRaw := filepath.Join(t.TempDir(), "fresh.jsonl")
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: 12, timeout: time.Minute, rawFactsOut: freshRaw}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), freshBrain, opts, now); err != nil {
		t.Fatalf("fresh raw run: %v", err)
	}
	want := s1ChunkLines(freshRaw)
	if want < 2 {
		t.Fatalf("fixture too small to exercise chunk cache: %d chunks", want)
	}

	// Primed store: a normal run caches s1's original chunks, then the
	// transcript grows, so the fingerprint misses but old chunk hashes hit.
	primedBrain := writeDistillFixture(t, now)
	primeOpts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: 12, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), primedBrain, primeOpts, now); err != nil {
		t.Fatalf("priming run: %v", err)
	}
	extend(primedBrain)
	primedRaw := filepath.Join(t.TempDir(), "primed.jsonl")
	opts.rawFactsOut = primedRaw
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), primedBrain, opts, now); err != nil {
		t.Fatalf("primed raw run: %v", err)
	}
	if got := s1ChunkLines(primedRaw); got != want {
		t.Errorf("raw run on primed store emitted %d s1 chunk lines, want %d (chunk cache must not filter raw output)", got, want)
	}
}

func TestDistillRawFactsOutFlagIsHidden(t *testing.T) {
	cmd := newDistillCommand(Options{})
	flag := cmd.Flags().Lookup("raw-facts-out")
	if flag == nil {
		t.Fatal("--raw-facts-out flag missing")
	}
	if !flag.Hidden {
		t.Error("--raw-facts-out must be hidden")
	}
}
