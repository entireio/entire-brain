package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
)

type semanticBenchOptions struct {
	json              bool
	semBinary         string
	profile           string
	timeout           time.Duration
	inactivityTimeout time.Duration
	keep              bool
	progress          bool
}

type semanticBenchReport struct {
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

func newBenchmarkCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run local benchmark helpers",
	}
	cmd.AddCommand(newSemanticBenchCommand(opts))
	return cmd
}

func newSemanticBenchCommand(opts Options) *cobra.Command {
	benchOpts := semanticBenchOptions{semBinary: "entire"}
	cmd := &cobra.Command{
		Use:   "semantic [path]",
		Short: "Benchmark end-to-end semantic Brain indexing in an isolated store",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSemanticBench(cmd.Context(), cmd, opts, benchOpts, target)
		},
	}
	cmd.Flags().BoolVar(&benchOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&benchOpts.semBinary, "sem-binary", "entire", "Entire CLI binary that exposes `sem` provider commands")
	cmd.Flags().StringVar(&benchOpts.profile, "profile", "syntax-only", "Semantic provider snapshot profile")
	cmd.Flags().DurationVar(&benchOpts.timeout, "sem-timeout", 0, "Overall deadline for the semantic provider snapshot (0 uses the default)")
	cmd.Flags().DurationVar(&benchOpts.inactivityTimeout, "sem-inactivity-timeout", 0, "Abort the snapshot if the provider emits no records for this long (0 uses the default)")
	cmd.Flags().BoolVar(&benchOpts.keep, "keep", false, "Keep the isolated benchmark store after the run")
	cmd.Flags().BoolVar(&benchOpts.progress, "progress", false, "Print Brain indexing phase progress to stderr")
	return cmd
}

func runSemanticBench(ctx context.Context, cmd *cobra.Command, opts Options, benchOpts semanticBenchOptions, target string) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("semantic benchmark requires a local repository path: %s", target)
	}
	tempRoot, err := os.MkdirTemp("", "entire-brain-sem-bench-*")
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
	benchOpts.progressPhase("preparing isolated store")
	benchRunOpts := opts
	benchRunOpts.Env = benchEnv

	var after runtime.MemStats
	runtime.GC()
	start := time.Now()
	indexOpts := semanticIndexOptions{
		force:             true,
		semBinary:         benchOpts.semBinary,
		profile:           benchOpts.profile,
		timeout:           benchOpts.timeout,
		inactivityTimeout: benchOpts.inactivityTimeout,
		progress:          benchOpts.progressPhase,
	}
	indexCmd := &cobra.Command{Use: "bench semantic"}
	indexCmd.SetOut(io.Discard)
	indexCmd.SetErr(io.Discard)
	if err := runSemanticIndex(ctx, indexCmd, benchRunOpts, indexOpts, repoDir); err != nil {
		return err
	}
	wall := time.Since(start)
	runtime.ReadMemStats(&after)

	storage, err := repoStoragePaths(ctx, opts.Runner, benchEnv, repoDir)
	if err != nil {
		return err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return fmt.Errorf("semantic benchmark did not produce a semantic source")
	}
	src := manifest.Sources.Semantic
	report := semanticBenchReport{
		GeneratedAt:     opts.Now().UTC(),
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
		WallMS:          roundMillis(wall),
		MaxRSSBytes:     processMaxRSSBytes(),
		HeapAllocBytes:  after.HeapAlloc,
		TempRoot:        tempRoot,
		Kept:            benchOpts.keep,
	}
	if !benchOpts.json {
		fmt.Fprintf(cmd.OutOrStdout(), "semantic bench for %s\n", report.RepoRoot)
		fmt.Fprintf(cmd.OutOrStdout(), "profile: %s\n", report.Profile)
		fmt.Fprintf(cmd.OutOrStdout(), "files: %d\nsymbols: %d\nrelations: %d\n", report.Files, report.Symbols, report.Relations)
		fmt.Fprintf(cmd.OutOrStdout(), "wall_ms: %.2f\nmax_rss_bytes: %d\nheap_alloc_bytes: %d\n", report.WallMS, report.MaxRSSBytes, report.HeapAllocBytes)
		if benchOpts.keep {
			fmt.Fprintf(cmd.OutOrStdout(), "brain_dir: %s\n", report.BrainDir)
		}
		return nil
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func (o semanticBenchOptions) progressPhase(phase string) {
	if o.progress {
		fmt.Fprintf(os.Stderr, "semantic bench: %s\n", phase)
	}
}

func roundMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
