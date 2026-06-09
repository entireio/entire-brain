package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type semanticAuditReport struct {
	GeneratedAt   time.Time            `json:"generated_at"`
	Repo          brainStatusRepo      `json:"repo"`
	BrainPath     string               `json:"brain_path"`
	Provider      string               `json:"provider,omitempty"`
	Version       string               `json:"provider_version,omitempty"`
	Schema        string               `json:"schema_version,omitempty"`
	Snapshot      string               `json:"snapshot_path,omitempty"`
	Store         string               `json:"store_path,omitempty"`
	Files         int                  `json:"files"`
	Symbols       int                  `json:"symbols"`
	Relations     int                  `json:"relations"`
	Warnings      int                  `json:"warnings"`
	Failures      int                  `json:"partial_failures"`
	Capabilities  []string             `json:"capabilities,omitempty"`
	Languages     []semanticAuditCount `json:"languages,omitempty"`
	SymbolKinds   []semanticAuditCount `json:"symbol_kinds,omitempty"`
	RelationTypes []semanticAuditCount `json:"relation_types,omitempty"`
	Freshness     staleReport          `json:"freshness"`
	BlindSpots    []brainBlindSpot     `json:"blind_spots"`
}

type semanticAuditCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
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
	if len(report.Languages) > 0 {
		fmt.Fprintf(out, "languages: %s\n", semanticAuditCountSummary(report.Languages))
	}
	if len(report.RelationTypes) > 0 {
		fmt.Fprintf(out, "relation-types: %s\n", semanticAuditCountSummary(report.RelationTypes))
	}
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
		report.Capabilities = sortedStringCopy(source.Capabilities)
		coverage, covErr := semanticAuditStoreCoverage(storage.BrainDir, source)
		if covErr != nil {
			return semanticAuditReport{}, covErr
		}
		report.Languages = coverage.Languages
		report.SymbolKinds = coverage.SymbolKinds
		report.RelationTypes = coverage.RelationTypes
	}
	return report, nil
}

type semanticAuditCoverage struct {
	Languages     []semanticAuditCount
	SymbolKinds   []semanticAuditCount
	RelationTypes []semanticAuditCount
}

func semanticAuditStoreCoverage(brainDir string, source *semanticSourceManifest) (semanticAuditCoverage, error) {
	if source == nil || strings.TrimSpace(source.StorePath) == "" {
		return semanticAuditCoverage{}, nil
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(source.StorePath))
	if _, err := os.Stat(storePath); err != nil {
		if os.IsNotExist(err) {
			return semanticAuditCoverage{}, nil
		}
		return semanticAuditCoverage{}, fmt.Errorf("stat semantic store: %w", err)
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("open semantic store: %w", err)
	}
	defer db.Close()
	languages, err := semanticAuditCountQuery(db, `SELECT CASE WHEN trim(language) = '' THEN 'unknown' ELSE language END AS coverage_name, COUNT(*) FROM symbols GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("read semantic languages: %w", err)
	}
	kinds, err := semanticAuditCountQuery(db, `SELECT CASE WHEN trim(kind) = '' THEN 'unknown' ELSE kind END AS coverage_name, COUNT(*) FROM symbols GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("read semantic symbol kinds: %w", err)
	}
	relationTypes, err := semanticAuditCountQuery(db, `SELECT CASE WHEN trim(type) = '' THEN 'unknown' ELSE type END AS coverage_name, COUNT(*) FROM relations GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("read semantic relation types: %w", err)
	}
	return semanticAuditCoverage{Languages: languages, SymbolKinds: kinds, RelationTypes: relationTypes}, nil
}

func semanticAuditCountQuery(db *sql.DB, query string) ([]semanticAuditCount, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []semanticAuditCount
	for rows.Next() {
		var item semanticAuditCount
		if err := rows.Scan(&item.Name, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func semanticAuditCountSummary(items []semanticAuditCount) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprintf("%s=%d", item.Name, item.Count))
	}
	return strings.Join(parts, ", ")
}

func sortedStringCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func nonNilBlindSpots(spots []brainBlindSpot) []brainBlindSpot {
	if spots == nil {
		return []brainBlindSpot{}
	}
	return spots
}
