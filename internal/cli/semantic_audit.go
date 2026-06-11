package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// semantic_audit.go is the machinery behind the Semantic section of `status`
// (coverage breakdowns and the --fail-on gate). The former standalone
// `semantic-audit` and `stale` commands were merged into `status`.

type semanticAuditCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

const (
	semanticAuditFailOnNone       = "none"
	semanticAuditFailOnUnsafe     = "unsafe"
	semanticAuditFailOnDegraded   = "degraded"
	semanticAuditFailOnBlindSpots = "blind-spots"
	semanticAuditFailOnRelease    = "release"
)

var errSemanticAuditGate = errors.New("semantic audit failed configured gate")

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
	case semanticAuditFailOnRelease:
		return semanticAuditFailOnRelease, nil
	default:
		return "", fmt.Errorf("--fail-on must be one of: %s", semanticAuditFailOnValues())
	}
}

// semanticAuditFailureForReport evaluates the --fail-on gate against the
// semantic freshness severity and blind-spot count from a status report. A
// missing semantic source yields severity "" which the release gate treats as
// not-ok — an unindexed brain must not pass a release health check silently.
func semanticAuditFailureForReport(severity string, blindSpots int, failOn string) error {
	switch failOn {
	case semanticAuditFailOnNone:
		return nil
	case semanticAuditFailOnUnsafe:
		if severity == "unsafe" {
			return fmt.Errorf("%w: freshness is unsafe", errSemanticAuditGate)
		}
	case semanticAuditFailOnDegraded:
		if severity == "unsafe" || severity == "degraded" {
			return fmt.Errorf("%w: freshness is %s", errSemanticAuditGate, severity)
		}
	case semanticAuditFailOnBlindSpots:
		if blindSpots > 0 {
			return fmt.Errorf("%w: %d blind spot(s)", errSemanticAuditGate, blindSpots)
		}
	case semanticAuditFailOnRelease:
		var failures []string
		if severity != "ok" {
			failures = append(failures, "freshness is "+valueOrUnset(severity))
		}
		if blindSpots > 0 {
			failures = append(failures, fmt.Sprintf("%d blind spot(s)", blindSpots))
		}
		if len(failures) > 0 {
			return fmt.Errorf("%w: %s", errSemanticAuditGate, strings.Join(failures, "; "))
		}
	default:
		return fmt.Errorf("--fail-on must be one of: %s", semanticAuditFailOnValues())
	}
	return nil
}

func semanticAuditFailOnValues() string {
	return strings.Join([]string{
		semanticAuditFailOnRelease,
		semanticAuditFailOnUnsafe,
		semanticAuditFailOnDegraded,
		semanticAuditFailOnBlindSpots,
		semanticAuditFailOnNone,
	}, ", ")
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
		// The store axis was already confirmed "ok" above, so a validation
		// error here is a real inconsistency, not expected staleness.
		return semanticAuditCoverage{}, fmt.Errorf("validate semantic store for audit: %w", err)
	}
	db, err := sql.Open(sqliteDriverName, storePath)
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
