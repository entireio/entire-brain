package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The command's headline promise is that it "neither reads nor disturbs the
// brain for this repository". --skip-index cannot keep that promise: measuring
// an index that already exists means measuring the brain that holds it. The
// promise and the exception have to travel together, or the one mode that
// touches the real brain is the one nobody was warned about.
func TestSkipIndexDeclaresThatItTouchesTheRealBrain(t *testing.T) {
	cmd := newScaleBenchCommand(Options{Version: "test"})

	flag := cmd.Flags().Lookup("skip-index")
	if flag == nil {
		t.Fatal("--skip-index is gone")
	}
	if !strings.Contains(strings.ToLower(flag.Usage), "real brain") {
		t.Fatalf("--skip-index help does not say it measures the real brain: %q", flag.Usage)
	}

	// The long help makes the isolation claim, so it is where the exception
	// has to be stated: a reader who stops at the promise is the one misled.
	if !strings.Contains(cmd.Long, "neither reads nor disturbs") {
		t.Fatal("the isolation claim is gone; this test guards its exception")
	}
	if !strings.Contains(cmd.Long, "--skip-index") {
		t.Fatalf("the long help promises isolation without naming the exception:\n%s", cmd.Long)
	}
}

// The benchmark indexes the repository it was given and then times queries
// against it. The query path resolves its repo from the environment, not from
// an argument — runSemanticQuery calls exportRepoDir, which returns
// env.RepoRoot or falls back to os.Getwd. So an isolated environment that
// overrides only the plugin directories leaves `bench scale <path>`, the
// documented form, indexing one repo and timing another.
func TestBenchEnvironmentTargetsTheRepositoryBeingMeasured(t *testing.T) {
	ambient := t.TempDir()
	target := t.TempDir()

	benchEnv := benchIsolatedEnv(EntireEnv{RepoRoot: ambient}, t.TempDir(), target)

	resolved, err := exportRepoDir(benchEnv)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != target {
		t.Fatalf("queries would resolve against %q, but the index was built for %q", resolved, target)
	}
	// The plugin directories must still be the isolated ones, or the benchmark
	// would be reading the brain somebody actually uses.
	if strings.HasPrefix(benchEnv.PluginDataDir, ambient) || benchEnv.PluginDataDir == "" {
		t.Fatalf("the isolated store was lost: %q", benchEnv.PluginDataDir)
	}
}

// --skip-index measures an existing index, which lives in the real plugin
// store — but the repository being measured is still the one that was asked
// for. Assigning the caller's options back wholesale carried the ambient
// RepoRoot with them, so the numbers came from whichever repo the shell was in
// while the report was labelled with the one on the command line.
func TestSkipIndexKeepsTheRealStoreButTheRequestedRepository(t *testing.T) {
	ambient := t.TempDir()
	target := t.TempDir()
	realStore := t.TempDir()

	env := benchExistingBrainEnv(EntireEnv{RepoRoot: ambient, PluginDataDir: realStore}, target)

	resolved, err := exportRepoDir(env)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != target {
		t.Fatalf("queries would resolve against %q, but the report names %q", resolved, target)
	}
	// The point of --skip-index is the real store; it must survive.
	if env.PluginDataDir != realStore {
		t.Fatalf("--skip-index lost the real plugin store: %q", env.PluginDataDir)
	}
}

// --skip-index measures an index that already exists. Pointed at a repository
// with none, measureBrainSize walks a directory that is not there and reports
// 0 bytes rather than failing, so the report came back "index_bytes: 0,
// symbols: 0" — which reads as a measurement of a very small brain rather than
// the absence of one. That is the confusion this command exists to avoid.
func TestSkipIndexRefusesARepositoryWithNoIndex(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable: %v %s", args, err, out)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(repoDir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-qm", "seed")

	opts := Options{Version: "test", Env: EntireEnv{
		RepoRoot:        repoDir,
		PluginConfigDir: filepath.Join(t.TempDir(), "config"),
		PluginDataDir:   filepath.Join(t.TempDir(), "data"),
		PluginStateDir:  filepath.Join(t.TempDir(), "state"),
		PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
	}, Runner: ExecRunner{}, Now: time.Now}

	cmd := newScaleBenchCommand(opts)
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Flags().Set("skip-index", "true"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, []string{repoDir})
	if err == nil {
		t.Fatal("a repository with no index was measured and reported as zeros")
	}
	if !strings.Contains(err.Error(), "no semantic index") {
		t.Fatalf("the error does not name the cause: %v", err)
	}
	// And it must say what to do about it.
	if !strings.Contains(err.Error(), "entire brain index") {
		t.Fatalf("the error does not name the remedy: %v", err)
	}
}

// The header claims "N runs each", which is a statement about every mode. It
// was read from whichever key Go's randomised map iteration reached first, so
// with modes that disagree the same report could print different numbers on
// successive runs — and name a count belonging to a mode the reader was not
// looking at.
func TestLatencyRunsOnlyReportsACountEveryModeShares(t *testing.T) {
	agreeing := scaleBenchReport{
		Queries: []string{"a", "b"},
		Latency: map[string]latencyStats{
			"code_search":       {Samples: 6},
			"knowledge_lexical": {Samples: 6},
			"knowledge_hybrid":  {Samples: 6},
		},
	}
	if runs := latencyRuns(agreeing); runs != 3 {
		t.Fatalf("agreeing modes reported %d runs per query, want 3", runs)
	}

	// One mode short: some queries failed there and not elsewhere.
	disagreeing := scaleBenchReport{
		Queries: []string{"a", "b"},
		Latency: map[string]latencyStats{
			"code_search":       {Samples: 6},
			"knowledge_lexical": {Samples: 4},
			"knowledge_hybrid":  {Samples: 6},
		},
	}
	// Run it repeatedly: a map-order-dependent answer passes once and fails later.
	for i := 0; i < 64; i++ {
		if runs := latencyRuns(disagreeing); runs != 0 {
			t.Fatalf("disagreeing modes reported %d runs each on attempt %d; the claim is not true of every mode", runs, i)
		}
	}
}

// countFileLines used to read whole files to count newlines in them, so a large
// tracked file was held entirely in memory. It now streams in a fixed buffer.
// The counts must be byte-for-byte what the read-whole version produced --
// especially across the buffer boundary, where a naive chunked count drops or
// doubles the line a newline straddles.
func TestStreamedLineCountMatchesReadingWhole(t *testing.T) {
	readWhole := func(t *testing.T, path string) (int64, int64) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			return 0, 0
		}
		var lines int64
		for _, b := range data {
			if b == '\n' {
				lines++
			}
		}
		if data[len(data)-1] != '\n' {
			lines++
		}
		return lines, int64(len(data))
	}

	cases := map[string]string{
		"empty":                  "",
		"no trailing newline":    "alpha\nbeta",
		"trailing newline":       "alpha\nbeta\n",
		"only newlines":          "\n\n\n",
		"single line no newline": "x",
		// Straddles the 64 KiB buffer: the newline lands just past the boundary.
		"across the buffer":   strings.Repeat("a", (64<<10)-1) + "\n" + strings.Repeat("b", 10) + "\n",
		"newline on boundary": strings.Repeat("a", (64<<10)-1) + "\nb",
		"larger than buffer":  strings.Repeat("line\n", 40000),
		// Exactly one buffer, so the last read that returns data fills it
		// completely and the read after it returns only EOF. An implementation
		// that tracks the final byte on partial reads alone misses the last
		// line here.
		"exactly one buffer, no trailing newline":  strings.Repeat("a", (64<<10)-1) + "x",
		"exactly one buffer, trailing newline":     strings.Repeat("a", (64<<10)-1) + "\n",
		"exactly two buffers, no trailing newline": strings.Repeat("a", (128<<10)-1) + "x",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			wantLines, wantBytes := readWhole(t, path)
			gotLines, gotBytes, err := countFileLines(path)
			if err != nil {
				t.Fatal(err)
			}
			if gotLines != wantLines || gotBytes != wantBytes {
				t.Fatalf("streamed %d lines/%d bytes, reading whole gives %d/%d",
					gotLines, gotBytes, wantLines, wantBytes)
			}
		})
	}
}

func TestPercentileUsesCeilingForNearestRank(t *testing.T) {
	values := make([]float64, 31)
	for i := range values {
		values[i] = float64(i + 1)
	}
	if got := percentile(values, .95); got != 30 {
		t.Fatalf("p95 of 31 samples = %v, want rank 30", got)
	}
}

func TestBrainSizeRejectsMissingStore(t *testing.T) {
	if _, _, err := measureBrainSize(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing store reported as a zero-byte measurement")
	}
}

func TestLatencyRunsRejectsPartialAndEmptyModes(t *testing.T) {
	for _, samples := range []int{0, 5, 7} {
		r := scaleBenchReport{
			Queries: []string{"a", "b"},
			Latency: map[string]latencyStats{
				"code_search":       {Samples: 6},
				"knowledge_lexical": {Samples: samples},
			},
		}
		if got := latencyRuns(r); got != 0 {
			t.Fatalf("6 and %d samples reported %d runs each", samples, got)
		}
	}
}

func TestScaleKnowledgeCountIncludesHistory(t *testing.T) {
	index := ftsTestIndex()
	brainDir, _ := writeDirectHistoryFTSFixture(t, index)
	got, err := countScaleKnowledgeRecords(brainDir, "main")
	if err != nil || got != len(index.Records) {
		t.Fatalf("history-only corpus: count=%d err=%v, want %d", got, err, len(index.Records))
	}
}

func TestScaleCorpusPreservesWhitespaceInTrackedPaths(t *testing.T) {
	dir := t.TempDir()
	name := " leading.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "--", name}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	got, warnings, err := measureCorpus(context.Background(), Options{Runner: ExecRunner{}}, dir)
	if err != nil || len(warnings) != 0 || got.files != 1 || got.lines != 1 {
		t.Fatalf("corpus=%+v warnings=%v err=%v", got, warnings, err)
	}
}

func TestScaleReportShowsSamplesAndSuppressesRunsOnFailure(t *testing.T) {
	r := scaleBenchReport{Queries: []string{"a", "b"},
		Latency:  map[string]latencyStats{"code_search": {Samples: 6, Failures: 2}},
		Warnings: []string{"code_search: 2 search(es) failed and are not in the distribution"}}
	var out strings.Builder
	printScaleBenchReport(&out, r)
	if strings.Contains(out.String(), "runs each") || !strings.Contains(out.String(), "samples") {
		t.Fatalf("failed queries misrepresented: %s", out.String())
	}
}

func TestScaleReportRetainsRunCountWithInformationalWarnings(t *testing.T) {
	r := scaleBenchReport{Queries: []string{"a", "b"},
		Latency:  map[string]latencyStats{"code_search": {Samples: 6}},
		Warnings: []string{"the knowledge corpus is empty"}}
	var out strings.Builder
	printScaleBenchReport(&out, r)
	if !strings.Contains(out.String(), "3 runs each") {
		t.Fatalf("informational warning hid successful run count: %s", out.String())
	}
}

func TestScaleLatencyFailuresAreStructured(t *testing.T) {
	queries := []string{"first query", "second query"}
	stats, warnings := measureCodeSearchLatency(context.Background(), Options{
		Env: EntireEnv{RepoRoot: t.TempDir()}, Runner: ExecRunner{},
	}, queries, 3)
	if stats.Samples != 0 || stats.Failures != len(queries) || len(warnings) == 0 {
		t.Fatalf("failed code warm-ups: stats=%+v warnings=%v", stats, warnings)
	}
	brainDir, source := writeDirectHistoryFTSFixture(t, ftsTestIndex())
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(source.IndexPath))); err != nil {
		t.Fatal(err)
	}
	modes, records, warnings := measureKnowledgeLatency(t.TempDir(), brainDir, "main", queries, 3)
	if records != -1 || len(warnings) == 0 {
		t.Fatalf("unreadable knowledge corpus: records=%d warnings=%v", records, warnings)
	}
	for mode, stats := range modes {
		if stats.Samples != 0 || stats.Failures != len(queries) {
			t.Fatalf("%s failed warm-ups: %+v", mode, stats)
		}
	}
}
