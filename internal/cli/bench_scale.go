package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// How big does a brain get, and how fast does it answer?
//
// Both of Brain's cells in the "scales to large codebases" comparison read
// Unknown, and they read Unknown because nothing measured them. The existing
// `bench semantic` times indexing and reports symbol counts, which answers a
// different question: the claims competitors publish are about the size of the
// index on disk and the latency of a query — "100M lines in 250MB", "sub-200ms
// search", "sub-50ms over ~2GB". We could not answer either at any scale.
//
// This measures exactly those two things, against a repository's real size in
// lines, so the result can be put next to those numbers and compared. It is
// deliberately a measurement tool and not a claim: it reports what the machine
// it ran on did, with the inputs stated, and the numbers we publish come from
// running it rather than from estimating.
//
// Nothing here extrapolates. A curve measured to a few million lines says
// nothing certain about a hundred million, and the honest output of this
// command is the size actually tested — which is still strictly more than
// Unknown.

type scaleBenchOptions struct {
	json        bool
	keep        bool
	queries     []string
	repeats     int
	graphBinary string
	profile     string
	skipIndex   bool
}

type latencyStats struct {
	Samples int     `json:"samples"`
	MinMS   float64 `json:"min_ms"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	MaxMS   float64 `json:"max_ms"`
	MeanMS  float64 `json:"mean_ms"`
}

type scaleBenchReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	RepoRoot    string    `json:"repo_root"`
	Commit      string    `json:"commit,omitempty"`

	// Corpus: the size of what was indexed, in the unit the comparison uses.
	TrackedFiles int   `json:"tracked_files"`
	SourceFiles  int   `json:"source_files"`
	SourceLines  int64 `json:"source_lines"`
	SourceBytes  int64 `json:"source_bytes"`

	// What the provider actually modelled. Symbols can be far below files when
	// a repository is mostly languages the provider has no parser for, and a
	// size-per-line number is misleading without it.
	IndexedFiles int `json:"indexed_files"`
	Symbols      int `json:"symbols"`
	Relations    int `json:"relations"`

	// Index cost on disk. BytesPerKLine is the comparable figure.
	IndexBytes    int64   `json:"index_bytes"`
	BytesPerKLine float64 `json:"bytes_per_kloc"`
	IndexRatio    float64 `json:"index_bytes_per_source_byte"`

	// Build cost.
	IndexWallMS   float64          `json:"index_wall_ms,omitempty"`
	LinesPerSec   float64          `json:"lines_per_sec,omitempty"`
	MaxRSSBytes   uint64           `json:"max_rss_bytes,omitempty"`
	IndexSkipped  bool             `json:"index_skipped,omitempty"`
	ComponentSize []scaleComponent `json:"components,omitempty"`

	// Query cost, per retrieval mode. The mode matters: lexical and hybrid are
	// different products, and reporting one number would flatter whichever is
	// faster.
	Latency          map[string]latencyStats `json:"latency_ms"`
	KnowledgeRecords int                     `json:"knowledge_records"`
	Queries          []string                `json:"queries"`

	Warnings []string `json:"warnings,omitempty"`
}

type scaleComponent struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// defaultScaleQueries are ordinary questions rather than terms picked to hit
// the index. A latency figure measured on queries chosen to be fast is not a
// latency figure.
func defaultScaleQueries() []string {
	return []string{
		"how are retries handled",
		"where is the configuration loaded",
		"authentication and token refresh",
		"database migration",
		"error handling for network failures",
		"how is the cache invalidated",
		"logging and metrics",
		"what runs on startup",
	}
}

func newScaleBenchCommand(opts Options) *cobra.Command {
	benchOpts := scaleBenchOptions{graphBinary: "entire", profile: "syntax-only", repeats: 5}
	cmd := &cobra.Command{
		Use:   "scale [path]",
		Short: "Measure index size on disk and query latency against a repository's size",
		Long: strings.TrimSpace(`
Reports how large a brain gets and how quickly it answers, against the size of
the repository in lines — the terms large-codebase claims are usually stated in.

It builds the index in an isolated store, so it neither reads nor disturbs the
brain for this repository, and reports bytes per thousand lines alongside query
latency per retrieval mode. --skip-index is the exception: measuring an index
that already exists means measuring the real brain, and a run that does is
labelled as such in its own output.

The result describes the machine it ran on and the repository it was given.
Nothing is extrapolated: a number measured at one size is not a claim about a
larger one.`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runScaleBench(cmd.Context(), cmd, opts, benchOpts, target)
		},
	}
	cmd.Flags().BoolVar(&benchOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&benchOpts.keep, "keep", false, "Keep the isolated store so it can be inspected")
	cmd.Flags().StringArrayVar(&benchOpts.queries, "query", nil, "Query to time (repeatable; defaults to a fixed set)")
	cmd.Flags().IntVar(&benchOpts.repeats, "repeats", 5, "Timed runs per query")
	cmd.Flags().StringVar(&benchOpts.graphBinary, "graph-binary", "entire", "Entire CLI binary that exposes `graph` provider commands")
	cmd.Flags().StringVar(&benchOpts.profile, "profile", "syntax-only", "Semantic provider snapshot profile")
	cmd.Flags().BoolVar(&benchOpts.skipIndex, "skip-index", false,
		"Measure this repository's real brain instead of building an isolated one (the only mode that touches it)")
	return cmd
}

func runScaleBench(ctx context.Context, cmd *cobra.Command, opts Options, benchOpts scaleBenchOptions, target string) error {
	if benchOpts.repeats < 1 {
		return fmt.Errorf("--repeats must be at least 1")
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("the scale benchmark requires a local repository path: %s", target)
	}

	report := scaleBenchReport{
		GeneratedAt: opts.Now().UTC(),
		RepoRoot:    repoDir,
		Queries:     benchOpts.queries,
		Latency:     map[string]latencyStats{},
	}
	if len(report.Queries) == 0 {
		report.Queries = defaultScaleQueries()
	}
	if commit, cerr := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD"); cerr == nil {
		report.Commit = strings.TrimSpace(commit)
	}

	// Corpus size first: without it the index size is a number with no
	// denominator, and "250MB" means nothing without "of what".
	corpus, corpusWarnings, err := measureCorpus(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	report.TrackedFiles = corpus.tracked
	report.SourceFiles = corpus.files
	report.SourceLines = corpus.lines
	report.SourceBytes = corpus.bytes
	report.Warnings = append(report.Warnings, corpusWarnings...)

	// An isolated store, so the benchmark neither reads nor disturbs the brain
	// somebody actually uses for this repository. --skip-index is the one
	// exception and cannot avoid being one: it exists to measure an index that
	// already exists, which only the real brain has. That run is announced in
	// the report rather than left to this comment.
	tempRoot, err := os.MkdirTemp("", "entire-brain-scale-bench-*")
	if err != nil {
		return err
	}
	if !benchOpts.keep {
		defer os.RemoveAll(tempRoot)
	}
	benchEnv := opts.Env
	benchEnv.PluginConfigDir = filepath.Join(tempRoot, "config")
	benchEnv.PluginDataDir = filepath.Join(tempRoot, "data")
	benchEnv.PluginStateDir = filepath.Join(tempRoot, "state")
	benchEnv.PluginCacheDir = filepath.Join(tempRoot, "cache")
	benchRun := opts
	benchRun.Env = benchEnv

	if benchOpts.skipIndex {
		report.IndexSkipped = true
		// Measuring an existing index means measuring the brain that holds it.
		// A report whose numbers silently came from somewhere other than the
		// isolated store would be the wrong kind of surprise, so it says so.
		benchRun = opts
		report.Warnings = append(report.Warnings,
			"--skip-index measured the real brain for this repository, not an isolated store: "+
				"queries read it, and retrieval may write its caches")
	} else {
		indexCmd := &cobra.Command{Use: "bench scale"}
		indexCmd.SetOut(io.Discard)
		indexCmd.SetErr(io.Discard)
		start := time.Now()
		if err := runSemanticIndex(ctx, indexCmd, benchRun, semanticIndexOptions{
			force:       true,
			graphBinary: benchOpts.graphBinary,
			profile:     benchOpts.profile,
		}, repoDir); err != nil {
			return err
		}
		report.IndexWallMS = roundMillis(time.Since(start))
		report.MaxRSSBytes = processMaxRSSBytes()
		if report.IndexWallMS > 0 {
			report.LinesPerSec = float64(report.SourceLines) / (report.IndexWallMS / 1000)
		}
	}

	storage, err := repoStoragePaths(ctx, benchRun.Runner, benchRun.Env, repoDir)
	if err != nil {
		return err
	}
	brainDir := storage.BrainDir
	if manifest, merr := loadBrainManifest(brainDir); merr == nil && manifest != nil && manifest.Sources != nil && manifest.Sources.Semantic != nil {
		report.IndexedFiles = manifest.Sources.Semantic.Files
		report.Symbols = manifest.Sources.Semantic.Symbols
		report.Relations = manifest.Sources.Semantic.Relations
	}

	// Size on disk, which is the figure the comparison is actually about.
	size, components, err := measureBrainSize(brainDir)
	if err != nil {
		return err
	}
	report.IndexBytes = size
	report.ComponentSize = components
	if report.SourceLines > 0 {
		report.BytesPerKLine = float64(size) / (float64(report.SourceLines) / 1000)
	}
	if report.SourceBytes > 0 {
		report.IndexRatio = float64(size) / float64(report.SourceBytes)
	}

	branch := distillDefaultBranch
	if current, berr := gitScalar(ctx, benchRun.Runner, repoDir, "branch", "--show-current"); berr == nil && strings.TrimSpace(current) != "" {
		branch = strings.TrimSpace(current)
	}
	// Code search is the measurement the large-codebase comparison is about: it
	// runs against the semantic index, which is the thing that grows with the
	// repository. It is reported first and separately for that reason.
	codeStats, codeWarnings := measureCodeSearchLatency(ctx, benchRun, report.Queries, benchOpts.repeats)
	report.Latency["code_search"] = codeStats
	report.Warnings = append(report.Warnings, codeWarnings...)

	// Knowledge retrieval searches facts, history and docs — a different corpus
	// that does NOT grow with the codebase. Its latency is reported because it
	// is what `recall` costs, and flagged when the corpus is empty, because a
	// sub-millisecond number over nothing is not a latency for anything.
	knowledge, knowledgeRecords, knowledgeWarnings := measureKnowledgeLatency(repoDir, brainDir, branch, report.Queries, benchOpts.repeats)
	report.KnowledgeRecords = knowledgeRecords
	for mode, stats := range knowledge {
		report.Latency[mode] = stats
	}
	report.Warnings = append(report.Warnings, knowledgeWarnings...)
	if knowledgeRecords == 0 {
		report.Warnings = append(report.Warnings,
			"the knowledge corpus (facts, history, docs) is empty in this store, so knowledge_* latencies measure an empty search and are not comparable to a populated brain")
	}

	if benchOpts.json {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printScaleBenchReport(cmd.OutOrStdout(), report)
	return nil
}

type corpusSize struct {
	tracked int
	files   int
	lines   int64
	bytes   int64
}

// measureCorpus counts the repository's source in lines.
//
// It counts git-tracked files, not everything on disk: build output, vendored
// dependencies and node_modules are not the codebase, and including them would
// inflate the denominator and flatter every ratio computed from it.
func measureCorpus(ctx context.Context, opts Options, repoDir string) (corpusSize, []string, error) {
	var size corpusSize
	var warnings []string

	out, err := gitScalar(ctx, opts.Runner, repoDir, "ls-files", "-z")
	if err != nil {
		return size, nil, fmt.Errorf("list tracked files (the scale benchmark needs a git repository): %w", err)
	}
	paths := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	skipped := 0
	for _, rel := range paths {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		size.tracked++
		if !isCountableSourcePath(rel) {
			continue
		}
		lines, bytes, err := countFileLines(filepath.Join(repoDir, filepath.FromSlash(rel)))
		if err != nil {
			skipped++
			continue
		}
		size.files++
		size.lines += lines
		size.bytes += bytes
	}
	if skipped > 0 {
		// Name it rather than silently shrinking the denominator.
		warnings = append(warnings, fmt.Sprintf("%d tracked file(s) could not be read and are not counted", skipped))
	}
	if size.lines == 0 {
		warnings = append(warnings, "no source lines were counted; ratios per line are omitted")
	}
	return size, warnings, nil
}

// isCountableSourcePath excludes what is in the tree but is not the codebase.
func isCountableSourcePath(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, prefix := range []string{"vendor/", "node_modules/", "third_party/", "testdata/"} {
		if strings.HasPrefix(rel, prefix) || strings.Contains(rel, "/"+prefix) {
			return false
		}
	}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".tar",
		".mp4", ".mov", ".woff", ".woff2", ".ttf", ".otf", ".bin", ".wasm", ".so", ".dylib", ".a":
		return false
	}
	return true
}

func countFileLines(path string) (lines, bytes int64, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	// A symlink's target is counted under its own path if it is tracked;
	// following it here would count the same content twice.
	if !info.Mode().IsRegular() {
		return 0, 0, fmt.Errorf("not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	bytes = int64(len(data))
	if bytes == 0 {
		return 0, 0, nil
	}
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	// A final line without a trailing newline is still a line.
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines, bytes, nil
}

// measureBrainSize sums the brain directory and breaks it down by top-level
// component, so a surprising total can be attributed rather than just reported.
func measureBrainSize(brainDir string) (int64, []scaleComponent, error) {
	var total int64
	byComponent := map[string]int64{}
	err := filepath.WalkDir(brainDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // an unreadable entry must not fail the measurement
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		rel, relErr := filepath.Rel(brainDir, path)
		if relErr != nil {
			return nil
		}
		component := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		byComponent[component] += info.Size()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	components := make([]scaleComponent, 0, len(byComponent))
	for name, bytes := range byComponent {
		components = append(components, scaleComponent{Name: name, Bytes: bytes})
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].Bytes != components[j].Bytes {
			return components[i].Bytes > components[j].Bytes
		}
		return components[i].Name < components[j].Name
	})
	return total, components, nil
}

// measureCodeSearchLatency times search over the semantic index — the corpus
// that grows with the codebase, and therefore the only one a large-codebase
// claim is about.
//
// One untimed warm-up per query runs first: the first search pays for opening
// the store, and counting it would report a cold-start cost as the latency of a
// query.
func measureCodeSearchLatency(ctx context.Context, opts Options, queries []string, repeats int) (latencyStats, []string) {
	var (
		samples  []float64
		warnings []string
		failures int
	)
	run := func(query string) error {
		// Output is discarded: this measures retrieval, and rendering results to
		// a terminal is not part of what a caller pays for.
		cmd := &cobra.Command{Use: "bench scale"}
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetContext(ctx)
		return runSemanticQuery(ctx, cmd, opts, semanticQueryOptions{limit: 20, json: true}, query)
	}
	for _, query := range queries {
		if err := run(query); err != nil {
			failures++
			continue
		}
		for i := 0; i < repeats; i++ {
			start := time.Now()
			err := run(query)
			elapsed := time.Since(start)
			if err != nil {
				failures++
				continue
			}
			samples = append(samples, float64(elapsed.Nanoseconds())/1e6)
		}
	}
	if failures > 0 {
		// A latency over the queries that happened to succeed is not a latency
		// for the workload, so the count is reported beside it.
		warnings = append(warnings, fmt.Sprintf("code_search: %d search(es) failed and are not in the distribution", failures))
	}
	return summarizeLatency(samples), warnings
}

// retrievalModeName exists because retrievalMode is an int. string(mode) on it
// yields a rune, which produced blank column headings — a label that silently
// vanishes is worse than no label, because the number underneath still looks
// authoritative.
func retrievalModeName(mode retrievalMode) string {
	switch mode {
	case modeLexical:
		return "knowledge_lexical"
	case modeVector:
		return "knowledge_vector"
	case modeHybrid:
		return "knowledge_hybrid"
	default:
		return fmt.Sprintf("knowledge_mode_%d", int(mode))
	}
}

// measureKnowledgeLatency times retrieval over facts, history and docs, and
// returns how many records that corpus actually holds — without which a fast
// number cannot be distinguished from an empty one.
func measureKnowledgeLatency(repoDir, brainDir, branch string, queries []string, repeats int) (map[string]latencyStats, int, []string) {
	stats := map[string]latencyStats{}
	var warnings []string

	records := 0
	if facts, err := loadFacts(brainDir, branch); err == nil {
		records += len(facts)
	}
	if index, err := loadDocIndex(brainDir); err == nil {
		records += len(index.Records)
	}

	for _, mode := range []retrievalMode{modeLexical, modeHybrid} {
		var samples []float64
		failures := 0
		for _, query := range queries {
			if _, err := retrieveUnified(repoDir, brainDir, branch, query, 20, mode); err != nil {
				failures++
				continue
			}
			for i := 0; i < repeats; i++ {
				start := time.Now()
				_, err := retrieveUnified(repoDir, brainDir, branch, query, 20, mode)
				elapsed := time.Since(start)
				if err != nil {
					failures++
					continue
				}
				samples = append(samples, float64(elapsed.Nanoseconds())/1e6)
			}
		}
		name := retrievalModeName(mode)
		if failures > 0 {
			warnings = append(warnings, fmt.Sprintf("%s: %d retrieval(s) failed and are not in the distribution", name, failures))
		}
		stats[name] = summarizeLatency(samples)
	}
	return stats, records, warnings
}

func summarizeLatency(samples []float64) latencyStats {
	if len(samples) == 0 {
		return latencyStats{}
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return latencyStats{
		Samples: len(sorted),
		MinMS:   sorted[0],
		P50MS:   percentile(sorted, 0.50),
		P95MS:   percentile(sorted, 0.95),
		P99MS:   percentile(sorted, 0.99),
		MaxMS:   sorted[len(sorted)-1],
		MeanMS:  sum / float64(len(sorted)),
	}
}

// percentile uses nearest-rank on the sorted sample. With the sample sizes here
// (tens to hundreds) interpolation would imply a precision the measurement does
// not have.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func printScaleBenchReport(out io.Writer, r scaleBenchReport) {
	fmt.Fprintf(out, "scale bench for %s\n", r.RepoRoot)
	if r.Commit != "" {
		fmt.Fprintf(out, "commit: %s\n", r.Commit)
	}
	fmt.Fprintf(out, "\ncorpus\n")
	fmt.Fprintf(out, "  source lines:   %s\n", humanCount(r.SourceLines))
	fmt.Fprintf(out, "  source files:   %s of %s tracked\n", humanCount(int64(r.SourceFiles)), humanCount(int64(r.TrackedFiles)))
	fmt.Fprintf(out, "  source bytes:   %s\n", humanBytes(r.SourceBytes))

	fmt.Fprintf(out, "\nindex\n")
	fmt.Fprintf(out, "  on disk:        %s\n", humanBytes(r.IndexBytes))
	if r.BytesPerKLine > 0 {
		fmt.Fprintf(out, "  per 1k lines:   %s\n", humanBytes(int64(r.BytesPerKLine)))
	}
	if r.IndexRatio > 0 {
		fmt.Fprintf(out, "  of source size: %.1f%%\n", r.IndexRatio*100)
	}
	fmt.Fprintf(out, "  modelled:       %s symbols, %s relations, %s files\n",
		humanCount(int64(r.Symbols)), humanCount(int64(r.Relations)), humanCount(int64(r.IndexedFiles)))
	if !r.IndexSkipped {
		fmt.Fprintf(out, "  build:          %.1fs", r.IndexWallMS/1000)
		if r.LinesPerSec > 0 {
			fmt.Fprintf(out, " (%s lines/sec)", humanCount(int64(r.LinesPerSec)))
		}
		fmt.Fprintln(out)
		if r.MaxRSSBytes > 0 {
			fmt.Fprintf(out, "  peak rss:       %s\n", humanBytes(int64(r.MaxRSSBytes)))
		}
	}
	for _, component := range r.ComponentSize {
		if component.Bytes*20 < r.IndexBytes {
			continue // anything under 5% is noise in this breakdown
		}
		fmt.Fprintf(out, "    %-14s %s\n", component.Name, humanBytes(component.Bytes))
	}

	fmt.Fprintf(out, "\nquery latency (%d queries, %d runs each)\n", len(r.Queries), latencyRuns(r))
	fmt.Fprintf(out, "  code_search runs against the semantic index (%s symbols).\n", humanCount(int64(r.Symbols)))
	fmt.Fprintf(out, "  knowledge_* run against facts/history/docs (%s records), which do not grow with the codebase.\n", humanCount(int64(r.KnowledgeRecords)))
	modes := make([]string, 0, len(r.Latency))
	for mode := range r.Latency {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	fmt.Fprintf(out, "  %-10s %8s %8s %8s %8s\n", "mode", "p50", "p95", "p99", "max")
	for _, mode := range modes {
		s := r.Latency[mode]
		if s.Samples == 0 {
			fmt.Fprintf(out, "  %-10s %8s\n", mode, "n/a")
			continue
		}
		fmt.Fprintf(out, "  %-10s %7.1fms %7.1fms %7.1fms %7.1fms\n", mode, s.P50MS, s.P95MS, s.P99MS, s.MaxMS)
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(out, "\nwarning: %s\n", warning)
	}
}

// latencyRuns reports the repeats per query, derived from the samples actually
// collected so the header cannot claim a number of runs that did not happen.
func latencyRuns(r scaleBenchReport) int {
	if len(r.Queries) == 0 {
		return 0
	}
	for _, stats := range r.Latency {
		if stats.Samples > 0 {
			return stats.Samples / len(r.Queries)
		}
	}
	return 0
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
