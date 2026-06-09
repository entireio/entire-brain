package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type semanticAuditReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Repo        brainStatusRepo  `json:"repo"`
	BrainPath   string           `json:"brain_path"`
	Provider    string           `json:"provider,omitempty"`
	Version     string           `json:"provider_version,omitempty"`
	Schema      string           `json:"schema_version,omitempty"`
	Snapshot    string           `json:"snapshot_path,omitempty"`
	Store       string           `json:"store_path,omitempty"`
	Files       int              `json:"files"`
	Symbols     int              `json:"symbols"`
	Relations   int              `json:"relations"`
	Warnings    int              `json:"warnings"`
	Failures    int              `json:"partial_failures"`
	Freshness   staleReport      `json:"freshness"`
	BlindSpots  []brainBlindSpot `json:"blind_spots"`
}

func newSemanticAuditCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "semantic-audit [path]",
		Short: "Audit semantic index coverage, freshness, and blind spots",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSemanticAudit(cmd.Context(), cmd, opts, target, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runSemanticAudit(ctx context.Context, cmd *cobra.Command, opts Options, target string, jsonOut bool) error {
	report, err := buildSemanticAuditReport(ctx, opts, target)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(cmd, report)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "semantic audit: %s\n", report.Freshness.Severity)
	if report.Provider != "" {
		fmt.Fprintf(out, "provider: %s %s (schema %s)\n", report.Provider, report.Version, report.Schema)
	}
	fmt.Fprintf(out, "coverage: %d files, %d symbols, %d relations, %d warnings, %d partial failures\n",
		report.Files, report.Symbols, report.Relations, report.Warnings, report.Failures)
	keys := make([]string, 0, len(report.Freshness.Axes))
	for key := range report.Freshness.Axes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		axis := report.Freshness.Axes[key]
		line := key + ": " + axis.State
		if axis.Detail != "" {
			line += " (" + axis.Detail + ")"
		}
		fmt.Fprintln(out, line)
	}
	if len(report.BlindSpots) == 0 {
		fmt.Fprintln(out, "blind-spots: none")
	} else {
		fmt.Fprintf(out, "blind-spots: %d\n", len(report.BlindSpots))
		for _, spot := range report.BlindSpots {
			fmt.Fprintf(out, "  %s", valueOrUnset(spot.Path))
			if spot.Code != "" {
				fmt.Fprintf(out, " [%s]", spot.Code)
			}
			if strings.TrimSpace(spot.Detail) != "" {
				fmt.Fprintf(out, " %s", spot.Detail)
			}
			fmt.Fprintln(out)
		}
	}
	return nil
}

func buildSemanticAuditReport(ctx context.Context, opts Options, target string) (semanticAuditReport, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return semanticAuditReport{}, err
	}
	if !local {
		return semanticAuditReport{}, fmt.Errorf("semantic-audit requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return semanticAuditReport{}, err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return semanticAuditReport{}, err
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return semanticAuditReport{}, err
	}
	spots, err := brainBlindSpotsForRepo(ctx, opts, repoDir)
	if err != nil {
		return semanticAuditReport{}, err
	}
	report := semanticAuditReport{
		GeneratedAt: opts.Now().UTC(),
		Repo:        brainStatusRepo{Root: repoDir, Key: storage.Key},
		BrainPath:   storage.BrainDir,
		Freshness:   freshness,
		BlindSpots:  nonNilBlindSpots(spots),
	}
	if manifest.Sources != nil && manifest.Sources.Semantic != nil {
		source := manifest.Sources.Semantic
		report.Provider = source.Provider
		report.Version = source.ProviderVersion
		report.Schema = source.SchemaVersion
		report.Snapshot = source.SnapshotPath
		report.Store = source.StorePath
		report.Files = source.Files
		report.Symbols = source.Symbols
		report.Relations = source.Relations
		report.Warnings = len(source.Warnings)
		report.Failures = len(source.PartialFailures)
	}
	return report, nil
}

func nonNilBlindSpots(spots []brainBlindSpot) []brainBlindSpot {
	if spots == nil {
		return []brainBlindSpot{}
	}
	return spots
}
