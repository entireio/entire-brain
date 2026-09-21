package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// A benchmark's failure mode is not crashing, it is reporting a number that
// flatters the thing it measures. These tests are mostly about the arithmetic
// and the labelling underneath the published figures: a denominator that
// silently excludes files makes every ratio look better, a percentile computed
// wrongly understates the tail, and a blank column heading leaves an
// authoritative-looking number attached to nothing.

func TestSourceLinesCountsTheLastLineWithoutANewline(t *testing.T) {
	// Off by one on every file that does not end in a newline. It inflates
	// bytes-per-line and lines-per-second by understating the denominator, in
	// our favour, which is the direction a benchmark must never be wrong in.
	dir := t.TempDir()
	for name, content := range map[string]string{
		"trailing.go":    "package main\nfunc main() {}\n",
		"no-trailing.go": "package main\nfunc main() {}",
		"empty.go":       "",
		"one-line.go":    "package main",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]int64{
		"trailing.go":    2,
		"no-trailing.go": 2,
		"empty.go":       0,
		"one-line.go":    1,
	} {
		lines, _, err := countFileLines(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if lines != want {
			t.Fatalf("%s counted %d lines, want %d", name, lines, want)
		}
	}
}

func TestCorpusExcludesWhatIsNotTheCodebase(t *testing.T) {
	// Vendored dependencies and build output are in the tree and are not the
	// codebase. Counting them inflates the denominator, which makes bytes per
	// line and lines per second both look better than they are.
	for path, counted := range map[string]bool{
		"internal/cli/bench.go":          true,
		"cmd/main.go":                    true,
		"docs/design.md":                 true,
		"vendor/golang.org/x/net/net.go": false,
		"node_modules/react/index.js":    false,
		"web/node_modules/x/y.ts":        false,
		"third_party/zlib/zlib.c":        false,
		"internal/cli/testdata/big.json": false,
		"assets/logo.png":                false,
		"dist/app.wasm":                  false,
		"lib/libfoo.dylib":               false,
	} {
		if got := isCountableSourcePath(path); got != counted {
			t.Fatalf("isCountableSourcePath(%q) = %v, want %v", path, got, counted)
		}
	}
}

func TestSymlinkIsNotCountedTwice(t *testing.T) {
	// A tracked symlink pointing at a tracked file would otherwise contribute
	// its target's lines a second time, inflating the corpus.
	dir := t.TempDir()
	real := filepath.Join(dir, "real.go")
	if err := os.WriteFile(real, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.go")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := countFileLines(link); err == nil {
		t.Fatal("a symlink was counted as a source file, double-counting its target")
	}
}

func TestBrainSizeSumsAndAttributes(t *testing.T) {
	// A total with no breakdown cannot be acted on: "193 MB" does not say which
	// component to go and look at.
	dir := t.TempDir()
	write := func(rel string, size int) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("semantic/index.db", 4000)
	write("semantic/vectors.bin", 2000)
	write("facts/main.ndjson", 500)
	write("manifest.json", 100)

	total, components, err := measureBrainSize(dir)
	if err != nil {
		t.Fatalf("measureBrainSize: %v", err)
	}
	if total != 6600 {
		t.Fatalf("total = %d, want 6600", total)
	}
	if len(components) == 0 || components[0].Name != "semantic" || components[0].Bytes != 6000 {
		t.Fatalf("components should lead with semantic at 6000: %+v", components)
	}
	var sum int64
	for _, c := range components {
		sum += c.Bytes
	}
	if sum != total {
		t.Fatalf("components sum to %d but the total is %d", sum, total)
	}
}

func TestPercentileDoesNotUnderstateTheTail(t *testing.T) {
	// The tail is the interesting part of a latency distribution, and an
	// off-by-one here reports a p99 that is really a p95.
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 100}
	if got := percentile(sorted, 0.50); got != 5 {
		t.Fatalf("p50 = %v, want 5", got)
	}
	if got := percentile(sorted, 0.99); got != 100 {
		t.Fatalf("p99 = %v, want the outlier (100)", got)
	}
	// A single sample is every percentile of itself.
	if got := percentile([]float64{7}, 0.99); got != 7 {
		t.Fatalf("p99 of one sample = %v, want 7", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("percentile of nothing = %v, want 0", got)
	}
}

func TestLatencySummaryReportsTheWorstCase(t *testing.T) {
	stats := summarizeLatency([]float64{5, 1, 3, 200, 2})
	if stats.Samples != 5 {
		t.Fatalf("samples = %d", stats.Samples)
	}
	if stats.MinMS != 1 || stats.MaxMS != 200 {
		t.Fatalf("min/max = %v/%v, want 1/200", stats.MinMS, stats.MaxMS)
	}
	// The mean must include the outlier. A "mean" that quietly drops one would
	// be the single easiest way to publish a better number than we have.
	if stats.MeanMS != 42.2 {
		t.Fatalf("mean = %v, want 42.2", stats.MeanMS)
	}
	if empty := summarizeLatency(nil); empty.Samples != 0 || empty.P50MS != 0 {
		t.Fatalf("an empty sample set reported %+v", empty)
	}
}

func TestEveryRetrievalModeHasAName(t *testing.T) {
	// retrievalMode is an int, so string(mode) yields a rune rather than a
	// name. That shipped blank column headings with authoritative-looking
	// numbers underneath them, which is worse than no table at all.
	seen := map[string]bool{}
	for _, mode := range []retrievalMode{modeLexical, modeVector, modeHybrid} {
		name := retrievalModeName(mode)
		// "Non-empty" is not enough: string(rune(mode)) yields "\x00", which is
		// non-empty, unique, and renders as nothing in a terminal. The name has
		// to be a label somebody can read in a table.
		if len(name) < 3 {
			t.Fatalf("mode %d has no usable name: %q", int(mode), name)
		}
		for _, r := range name {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				t.Fatalf("mode %d name %q contains %q, which is not printable in a table", int(mode), name, r)
			}
		}
		if seen[name] {
			t.Fatalf("two modes share the name %q", name)
		}
		seen[name] = true
	}
	// An unknown mode must still be identifiable rather than blank.
	if name := retrievalModeName(retrievalMode(99)); !strings.Contains(name, "99") {
		t.Fatalf("an unknown mode rendered as %q", name)
	}
}

func TestReportSeparatesCodeSearchFromKnowledgeRetrieval(t *testing.T) {
	// These measure different corpora. Code search runs against the semantic
	// index, which grows with the codebase; knowledge retrieval runs against
	// facts, history and docs, which do not. Presenting one number for both
	// would let an empty fact store advertise the latency of a large-codebase
	// search — which is exactly the mistake this benchmark was written to
	// avoid, and did make in its first draft.
	report := scaleBenchReport{
		Queries:          []string{"a", "b"},
		Symbols:          50000,
		KnowledgeRecords: 0,
		Latency: map[string]latencyStats{
			"code_search":       {Samples: 10, P50MS: 1300},
			"knowledge_lexical": {Samples: 10, P50MS: 0.5},
		},
	}
	var sb strings.Builder
	printScaleBenchReport(&sb, report)
	out := sb.String()

	if !strings.Contains(out, "code_search") || !strings.Contains(out, "knowledge_lexical") {
		t.Fatalf("the report does not distinguish the two corpora:\n%s", out)
	}
	// And it has to say what each one searched, or a reader cannot tell that
	// the fast number came from an empty store.
	if !strings.Contains(out, "semantic index") {
		t.Fatalf("the report does not say what code_search runs against:\n%s", out)
	}
	if !strings.Contains(out, "do not grow with the codebase") {
		t.Fatalf("the report does not say the knowledge corpus is a different thing:\n%s", out)
	}
	if !strings.Contains(out, "0 records") {
		t.Fatalf("the report does not disclose the empty knowledge corpus:\n%s", out)
	}
}

func TestReportedRunCountMatchesTheSamplesTaken(t *testing.T) {
	// A header claiming "5 runs each" over a distribution built from fewer is a
	// small lie that makes the numbers look more solid than they are.
	// Deliberately not a count that matches the default --repeats: a test whose
	// expected value equals the constant a bug would hard-code cannot detect
	// that bug, which is what happened on the first pass here.
	report := scaleBenchReport{
		Queries: []string{"a", "b", "c"},
		Latency: map[string]latencyStats{"code_search": {Samples: 21}},
	}
	if got := latencyRuns(report); got != 7 {
		t.Fatalf("runs = %d, want 7 (21 samples / 3 queries)", got)
	}
	// And a partial distribution must report what was actually collected.
	partial := scaleBenchReport{
		Queries: []string{"a", "b"},
		Latency: map[string]latencyStats{"code_search": {Samples: 4}},
	}
	if got := latencyRuns(partial); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
	if got := latencyRuns(scaleBenchReport{}); got != 0 {
		t.Fatalf("runs with no queries = %d, want 0", got)
	}
}

func TestDefaultQueriesAreOrdinaryQuestions(t *testing.T) {
	// A latency measured on terms chosen to hit the index is not a latency.
	// This pins the shape rather than the wording: real questions, more than a
	// couple of them, none of them a bare symbol name.
	queries := defaultScaleQueries()
	if len(queries) < 5 {
		t.Fatalf("only %d default queries; a distribution needs more", len(queries))
	}
	for _, query := range queries {
		if len(strings.Fields(query)) < 2 {
			t.Fatalf("%q is a term, not a question", query)
		}
	}
}

func TestHumanFormattingIsHonestAtBoundaries(t *testing.T) {
	// These numbers go into a published table, so a rounding bug becomes a
	// published wrong number.
	for n, want := range map[int64]string{
		0:          "0 B",
		1023:       "1023 B",
		1024:       "1.0 KB",
		1048576:    "1.0 MB",
		1073741824: "1.00 GB",
	} {
		if got := humanBytes(n); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int64]string{
		999:        "999",
		1000:       "1.0k",
		1000000:    "1.0M",
		745900:     "745.9k",
		7244518:    "7.2M",
		1000000000: "1.0B",
	} {
		if got := humanCount(n); got != want {
			t.Fatalf("humanCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestScaleBenchRequiresARepeatCount(t *testing.T) {
	opts := Options{Version: "test"}
	cmd := newScaleBenchCommand(opts)
	if err := cmd.Flags().Set("repeats", "0"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("--repeats 0 was accepted; the distribution would be empty")
	}
}
