package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type semanticAuditReport struct {
	GeneratedAt           time.Time            `json:"generated_at"`
	Repo                  brainStatusRepo      `json:"repo"`
	BrainPath             string               `json:"brain_path"`
	Provider              string               `json:"provider,omitempty"`
	Version               string               `json:"provider_version,omitempty"`
	Schema                string               `json:"schema_version,omitempty"`
	Snapshot              string               `json:"snapshot_path,omitempty"`
	Store                 string               `json:"store_path,omitempty"`
	Files                 int                  `json:"files"`
	Symbols               int                  `json:"symbols"`
	Relations             int                  `json:"relations"`
	Warnings              int                  `json:"warnings"`
	Failures              int                  `json:"partial_failures"`
	WarningDetails        []semanticWarning    `json:"warning_details,omitempty"`
	PartialFailureDetails []semanticWarning    `json:"partial_failure_details,omitempty"`
	Capabilities          []string             `json:"capabilities,omitempty"`
	FileLanguages         []semanticAuditCount `json:"file_languages,omitempty"`
	Languages             []semanticAuditCount `json:"languages,omitempty"`
	SymbolKinds           []semanticAuditCount `json:"symbol_kinds,omitempty"`
	RelationTypes         []semanticAuditCount `json:"relation_types,omitempty"`
	Freshness             staleReport          `json:"freshness"`
	BlindSpots            []brainBlindSpot     `json:"blind_spots"`
}

type semanticAuditCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

const (
	semanticAuditFailOnNone       = "none"
	semanticAuditFailOnUnsafe     = "unsafe"
	semanticAuditFailOnDegraded   = "degraded"
	semanticAuditFailOnBlindSpots = "blind-spots"
)

var errSemanticAuditGate = errors.New("semantic audit failed configured gate")

type semanticAuditCommandOptions struct {
	json   bool
	failOn string
}

func newSemanticAuditCommand(opts Options) *cobra.Command {
	auditOpts := semanticAuditCommandOptions{failOn: semanticAuditFailOnNone}
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
			return runSemanticAudit(cmd.Context(), cmd, opts, target, auditOpts)
		},
	}
	cmd.Flags().BoolVar(&auditOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&auditOpts.failOn, "fail-on", semanticAuditFailOnNone, "Return nonzero after emitting the report when the selected gate trips: unsafe, degraded, blind-spots, none")
	return cmd
}

func runSemanticAudit(ctx context.Context, cmd *cobra.Command, opts Options, target string, auditOpts semanticAuditCommandOptions) error {
	failOn, err := normalizeSemanticAuditFailOn(auditOpts.failOn)
	if err != nil {
		return err
	}
	report, err := buildSemanticAuditReport(ctx, opts, target)
	if err != nil {
		return err
	}
	if auditOpts.json {
		if err := writeJSON(cmd, report); err != nil {
			return err
		}
	} else {
		renderSemanticAuditReportText(cmd, report)
	}
	if err := semanticAuditFailureForReport(report, failOn); err != nil {
		return renderedCommandError{err: err}
	}
	return nil
}

func renderSemanticAuditReportText(cmd *cobra.Command, report semanticAuditReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "semantic audit: %s\n", report.Freshness.Severity)
	if report.Provider != "" {
		fmt.Fprintf(out, "provider: %s %s (schema %s)\n", report.Provider, report.Version, report.Schema)
	}
	fmt.Fprintf(out, "coverage: %d files, %d symbols, %d relations, %d warnings, %d partial failures\n",
		report.Files, report.Symbols, report.Relations, report.Warnings, report.Failures)
	if len(report.WarningDetails) > 0 {
		fmt.Fprintln(out, "warnings:")
		for _, warning := range report.WarningDetails {
			fmt.Fprintf(out, "  %s\n", semanticAuditWarningSummary(warning))
		}
	}
	if len(report.PartialFailureDetails) > 0 {
		fmt.Fprintln(out, "partial-failures:")
		for _, failure := range report.PartialFailureDetails {
			fmt.Fprintf(out, "  %s\n", semanticAuditWarningSummary(failure))
		}
	}
	if len(report.FileLanguages) > 0 {
		fmt.Fprintf(out, "file-languages: %s\n", semanticAuditCountSummary(report.FileLanguages))
	}
	if len(report.Languages) > 0 {
		fmt.Fprintf(out, "symbol-languages: %s\n", semanticAuditCountSummary(report.Languages))
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
}

func normalizeSemanticAuditFailOn(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", semanticAuditFailOnNone:
		return semanticAuditFailOnNone, nil
	case semanticAuditFailOnUnsafe:
		return semanticAuditFailOnUnsafe, nil
	case semanticAuditFailOnDegraded:
		return semanticAuditFailOnDegraded, nil
	case semanticAuditFailOnBlindSpots:
		return semanticAuditFailOnBlindSpots, nil
	default:
		return "", fmt.Errorf("--fail-on must be one of: %s, %s, %s, %s", semanticAuditFailOnUnsafe, semanticAuditFailOnDegraded, semanticAuditFailOnBlindSpots, semanticAuditFailOnNone)
	}
}

func semanticAuditFailureForReport(report semanticAuditReport, failOn string) error {
	switch failOn {
	case semanticAuditFailOnNone:
		return nil
	case semanticAuditFailOnUnsafe:
		if report.Freshness.Severity == "unsafe" {
			return fmt.Errorf("%w: freshness is unsafe", errSemanticAuditGate)
		}
	case semanticAuditFailOnDegraded:
		if report.Freshness.Severity == "unsafe" || report.Freshness.Severity == "degraded" {
			return fmt.Errorf("%w: freshness is %s", errSemanticAuditGate, report.Freshness.Severity)
		}
	case semanticAuditFailOnBlindSpots:
		if len(report.BlindSpots) > 0 {
			return fmt.Errorf("%w: %d blind spot(s)", errSemanticAuditGate, len(report.BlindSpots))
		}
	default:
		return fmt.Errorf("--fail-on must be one of: %s, %s, %s, %s", semanticAuditFailOnUnsafe, semanticAuditFailOnDegraded, semanticAuditFailOnBlindSpots, semanticAuditFailOnNone)
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
		report.WarningDetails = semanticWarningDetails(source.Warnings)
		report.PartialFailureDetails = semanticWarningDetails(source.PartialFailures)
		report.Capabilities = sortedStringCopy(source.Capabilities)
		coverage, covErr := semanticAuditStoreCoverage(storage.BrainDir, source, freshness)
		if covErr != nil {
			return semanticAuditReport{}, covErr
		}
		report.FileLanguages = coverage.FileLanguages
		report.Languages = coverage.Languages
		report.SymbolKinds = coverage.SymbolKinds
		report.RelationTypes = coverage.RelationTypes
	}
	return report, nil
}

type semanticAuditCoverage struct {
	FileLanguages []semanticAuditCount
	Languages     []semanticAuditCount
	SymbolKinds   []semanticAuditCount
	RelationTypes []semanticAuditCount
}

func semanticAuditStoreCoverage(brainDir string, source *semanticSourceManifest, freshness staleReport) (semanticAuditCoverage, error) {
	if source == nil || strings.TrimSpace(source.StorePath) == "" {
		return semanticAuditSnapshotCoverage(brainDir, source, freshness)
	}
	if axis, ok := freshness.Axes["store"]; ok && axis.State != "ok" {
		return semanticAuditCoverage{}, nil
	}
	storePath, err := validateSemanticDeclaredStore(brainDir, source)
	if err != nil {
		if axis, ok := freshness.Axes["store"]; ok && axis.State != "ok" {
			return semanticAuditCoverage{}, nil
		}
		return semanticAuditCoverage{}, fmt.Errorf("validate semantic store for audit: %w", err)
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("open semantic store: %w", err)
	}
	defer db.Close()
	fileLanguageQuery := `SELECT CASE
	WHEN path GLOB '*.go' THEN 'go'
	WHEN path GLOB '*.ts' THEN 'typescript'
	WHEN path GLOB '*.tsx' THEN 'typescript'
	WHEN path GLOB '*.js' THEN 'javascript'
	WHEN path GLOB '*.jsx' THEN 'javascript'
	WHEN path GLOB '*.py' THEN 'python'
	WHEN path GLOB '*.rs' THEN 'rust'
	WHEN path GLOB '*.md' THEN 'markdown'
	ELSE 'unknown' END AS coverage_name, COUNT(*) FROM files GROUP BY 1 ORDER BY 1`
	if ok, colErr := semanticSQLiteColumnExists(db, "files", "language"); colErr != nil {
		return semanticAuditCoverage{}, fmt.Errorf("inspect semantic file language column: %w", colErr)
	} else if ok {
		fileLanguageQuery = `SELECT CASE WHEN trim(language) = '' THEN 'unknown' ELSE language END AS coverage_name, COUNT(*) FROM files GROUP BY 1 ORDER BY 1`
	}
	fileLanguages, err := semanticAuditCountQuery(db, fileLanguageQuery)
	if err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("read semantic file languages: %w", err)
	}
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
	return semanticAuditCoverage{FileLanguages: fileLanguages, Languages: languages, SymbolKinds: kinds, RelationTypes: relationTypes}, nil
}

func semanticAuditSnapshotCoverage(brainDir string, source *semanticSourceManifest, freshness staleReport) (semanticAuditCoverage, error) {
	if source == nil || strings.TrimSpace(source.SnapshotPath) == "" {
		return semanticAuditCoverage{}, nil
	}
	if axis, ok := freshness.Axes["snapshot"]; ok && axis.State != "ok" {
		return semanticAuditCoverage{}, nil
	}
	snapshotRel, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return semanticAuditCoverage{}, err
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotRel); err != nil {
		return semanticAuditCoverage{}, err
	}
	f, err := os.Open(filepath.Join(brainDir, snapshotRel))
	if err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("open semantic snapshot for audit: %w", err)
	}
	defer f.Close()
	files := map[string]string{}
	languages := map[string]int{}
	kinds := map[string]int{}
	relationTypes := map[string]int{}
	scanner := newSemanticScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		if line == 1 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return semanticAuditCoverage{}, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		switch record.RecordType {
		case "file":
			if path := record.semanticPath(); path != "" {
				files[path] = auditCoverageName(record.Language)
			}
		case "symbol":
			languages[auditCoverageName(record.Language)]++
			kinds[auditCoverageName(record.Kind)]++
			if path := record.semanticPath(); path != "" {
				if files[path] == "" || files[path] == "unknown" {
					files[path] = auditCoverageName(record.Language)
				}
			}
		case "relation":
			relationTypes[auditCoverageName(record.Type)]++
		}
	}
	if err := scanner.Err(); err != nil {
		return semanticAuditCoverage{}, fmt.Errorf("scan semantic snapshot for audit: %w", err)
	}
	fileLanguages := map[string]int{}
	for _, language := range files {
		fileLanguages[auditCoverageName(language)]++
	}
	return semanticAuditCoverage{
		FileLanguages: semanticAuditCountsFromMap(fileLanguages),
		Languages:     semanticAuditCountsFromMap(languages),
		SymbolKinds:   semanticAuditCountsFromMap(kinds),
		RelationTypes: semanticAuditCountsFromMap(relationTypes),
	}, nil
}

func auditCoverageName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func semanticAuditCountsFromMap(counts map[string]int) []semanticAuditCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]semanticAuditCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, semanticAuditCount{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func semanticSQLiteColumnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
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

func semanticAuditWarningSummary(warning semanticWarning) string {
	parts := []string{valueOrUnset(warning.Code)}
	if warning.Severity != "" {
		parts = append(parts, warning.Severity)
	}
	if warning.Path != "" {
		parts = append(parts, warning.Path)
	}
	if warning.Effect != "" {
		parts = append(parts, warning.Effect)
	}
	if warning.Detail != "" {
		parts = append(parts, warning.Detail)
	}
	return strings.Join(parts, " | ")
}

func semanticWarningDetails(warnings []semanticWarning) []semanticWarning {
	if len(warnings) == 0 {
		return nil
	}
	return append([]semanticWarning(nil), warnings...)
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
