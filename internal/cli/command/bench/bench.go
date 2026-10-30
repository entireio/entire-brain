package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/tui"
	"github.com/spf13/cobra"
)

// Config supplies the shared command options and the assembly package's
// storage and indexing operations. Dependencies are per invocation, never
// global hooks.
type Config struct {
	base.Options
	Deps Deps
}

// Deps keeps the benchmark commands independent of command assembly, repo
// identity resolution, and the indexing engine.
type Deps struct {
	RepoStoragePaths func(context.Context, base.CommandRunner, base.EntireEnv, string) (base.RepoStorage, error)
	// RunSemanticIndex runs one full code-index build for the target
	// repository under the supplied options' environment.
	RunSemanticIndex func(ctx context.Context, cmd *cobra.Command, opts base.Options, req IndexRequest, target string) error
	// ProcessMaxRSSBytes reports the process's peak resident set size, or 0
	// where the platform cannot say.
	ProcessMaxRSSBytes func() uint64
	// ScaleCommand constructs the `bench scale` subcommand, which still lives
	// with the retrieval engine it measures.
	ScaleCommand func() *cobra.Command
}

// IndexRequest is the slice of the index build's options the benchmark sets.
type IndexRequest struct {
	Force             bool
	GraphBinary       string
	Profile           string
	Timeout           time.Duration
	InactivityTimeout time.Duration
	Progress          func(phase string)
}

type IndexBenchOptions struct {
	JSON              bool
	GraphBinary       string
	Profile           string
	Timeout           time.Duration
	InactivityTimeout time.Duration
	Keep              bool
	Progress          bool
}

type IndexBenchReport struct {
	GeneratedAt     time.Time `json:"generated_at"`
	RepoRoot        string    `json:"repo_root"`
	BrainDir        string    `json:"brain_dir"`
	Profile         string    `json:"profile,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	ProviderVersion string    `json:"provider_version,omitempty"`
	SchemaVersion   string    `json:"schema_version,omitempty"`
	Files           int       `json:"files"`
	Symbols         int       `json:"symbols"`
	Relations       int       `json:"relations"`
	Externals       int       `json:"externals"`
	Warnings        int       `json:"warnings"`
	PartialFailures int       `json:"partial_failures"`
	WallMS          float64   `json:"wall_ms"`
	MaxRSSBytes     uint64    `json:"max_rss_bytes"`
	HeapAllocBytes  uint64    `json:"heap_alloc_bytes"`
	TempRoot        string    `json:"temp_root"`
	Kept            bool      `json:"kept"`
}

func New(cfg Config) *cobra.Command {
	cmd := &cobra.Command{
		Hidden: true,
		Use:    "bench",
		Short:  "Benchmark code indexing performance",
	}
	cmd.AddCommand(newIndexCommand(cfg))
	cmd.AddCommand(cfg.Deps.ScaleCommand())
	return cmd
}

func newIndexCommand(cfg Config) *cobra.Command {
	benchOpts := IndexBenchOptions{GraphBinary: "entire"}
	cmd := &cobra.Command{
		Use:   "index [path]",
		Short: "Benchmark end-to-end code indexing in an isolated store",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if cfg.Env.RepoRoot != "" {
				target = cfg.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return RunIndexBench(cmd.Context(), cmd, cfg, benchOpts, target)
		},
	}
	cmd.Flags().BoolVar(&benchOpts.JSON, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&benchOpts.GraphBinary, "graph-binary", "entire", "Entire CLI binary that exposes `graph` provider commands")
	cmd.Flags().StringVar(&benchOpts.Profile, "profile", "syntax-only", "Graph provider snapshot profile")
	cmd.Flags().DurationVar(&benchOpts.Timeout, "graph-timeout", 0, "Overall deadline for the graph provider snapshot (0 uses the default)")
	cmd.Flags().DurationVar(&benchOpts.InactivityTimeout, "graph-inactivity-timeout", 0, "Abort the snapshot if the provider emits no records for this long (0 uses the default)")
	cmd.Flags().BoolVar(&benchOpts.Keep, "keep", false, "Keep the isolated benchmark store after the run")
	cmd.Flags().BoolVar(&benchOpts.Progress, "progress", false, "Print Brain indexing phase progress to stderr")
	return cmd
}

func RunIndexBench(ctx context.Context, cmd *cobra.Command, cfg Config, benchOpts IndexBenchOptions, target string) error {
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, cfg.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("index benchmark requires a local repository path: %s", target)
	}
	tempRoot, err := os.MkdirTemp("", "entire-brain-graph-bench-*")
	if err != nil {
		return err
	}
	if !benchOpts.Keep {
		defer os.RemoveAll(tempRoot)
	}
	benchEnv := cfg.Env
	benchEnv.PluginConfigDir = filepath.Join(tempRoot, "config")
	benchEnv.PluginDataDir = filepath.Join(tempRoot, "data")
	benchEnv.PluginStateDir = filepath.Join(tempRoot, "state")
	benchEnv.PluginCacheDir = filepath.Join(tempRoot, "cache")
	benchOpts.progressPhase("preparing isolated store")
	benchRunOpts := cfg.Options
	benchRunOpts.Env = benchEnv

	var after runtime.MemStats
	runtime.GC()
	start := time.Now()
	indexOpts := IndexRequest{
		Force:             true,
		GraphBinary:       benchOpts.GraphBinary,
		Profile:           benchOpts.Profile,
		Timeout:           benchOpts.Timeout,
		InactivityTimeout: benchOpts.InactivityTimeout,
		Progress:          benchOpts.progressPhase,
	}
	indexCmd := &cobra.Command{Use: "bench index"}
	indexCmd.SetOut(io.Discard)
	indexCmd.SetErr(io.Discard)
	if err := cfg.Deps.RunSemanticIndex(ctx, indexCmd, benchRunOpts, indexOpts, repoDir); err != nil {
		return err
	}
	wall := time.Since(start)
	runtime.ReadMemStats(&after)

	storage, err := cfg.Deps.RepoStoragePaths(ctx, cfg.Runner, benchEnv, repoDir)
	if err != nil {
		return err
	}
	manifest, err := bmanifest.Load(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return fmt.Errorf("index benchmark did not produce a code-index source")
	}
	src := manifest.Sources.Semantic
	report := IndexBenchReport{
		GeneratedAt:     cfg.Now().UTC(),
		RepoRoot:        repoDir,
		BrainDir:        storage.BrainDir,
		Profile:         src.Profile,
		Provider:        src.Provider,
		ProviderVersion: src.ProviderVersion,
		SchemaVersion:   src.SchemaVersion,
		Files:           src.Files,
		Symbols:         src.Symbols,
		Relations:       src.Relations,
		Externals:       src.Externals,
		Warnings:        len(src.Warnings),
		PartialFailures: len(src.PartialFailures),
		WallMS:          RoundMillis(wall),
		MaxRSSBytes:     cfg.Deps.ProcessMaxRSSBytes(),
		HeapAllocBytes:  after.HeapAlloc,
		TempRoot:        tempRoot,
		Kept:            benchOpts.Keep,
	}
	if !benchOpts.JSON {
		fmt.Fprintf(cmd.OutOrStdout(), "index bench for %s\n", report.RepoRoot)
		fmt.Fprintf(cmd.OutOrStdout(), "profile: %s\n", report.Profile)
		fmt.Fprintf(cmd.OutOrStdout(), "files: %d\nsymbols: %d\nrelations: %d\n", report.Files, report.Symbols, report.Relations)
		fmt.Fprintf(cmd.OutOrStdout(), "wall_ms: %.2f\nmax_rss_bytes: %d\nheap_alloc_bytes: %d\n", report.WallMS, report.MaxRSSBytes, report.HeapAllocBytes)
		if benchOpts.Keep {
			fmt.Fprintf(cmd.OutOrStdout(), "brain_dir: %s\n", report.BrainDir)
		}
		return nil
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func (o IndexBenchOptions) progressPhase(phase string) {
	if o.Progress {
		r := tui.NewRenderer(os.Stderr)
		fmt.Fprintln(os.Stderr, r.Dim(fmt.Sprintf("index bench: %s", phase)))
	}
}

func RoundMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
