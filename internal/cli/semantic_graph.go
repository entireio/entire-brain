package cli

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type semanticGraphSchemaOptions struct {
	json bool
}

type semanticGraphSearchOptions struct {
	limit  int
	offset int
	json   bool
}

type semanticGraphQueryOptions struct {
	limit int
	json  bool
}

type semanticTracePathOptions struct {
	depth int
	json  bool
}

type semanticSnippetOptions struct {
	contextLines int
	json         bool
}

type semanticDeadCodeOptions struct {
	limit int
	json  bool
}

type semanticTraceIngestOptions struct {
	json bool
}

type semanticGraphUIOptions struct {
	limit int
	json  bool
}

type semanticGraphSchemaReport struct {
	Provider      string   `json:"provider,omitempty"`
	SchemaVersion string   `json:"schema_version,omitempty"`
	Profile       string   `json:"profile,omitempty"`
	RelationSet   []string `json:"relation_set,omitempty"`
	Languages     []string `json:"languages,omitempty"`
	// Retrieval-trust diagnostics carried from the semantic source manifest so an
	// agent reading the schema knows how much to trust this index's facts.
	CompletenessLevel string            `json:"completeness_level,omitempty"`
	Trust             string            `json:"trust,omitempty"`
	LanguageTiers     map[string]string `json:"language_tiers,omitempty"`
	Counts            map[string]int    `json:"counts"`
	SymbolKinds       []string          `json:"symbol_kinds"`
	RelationTypes     []string          `json:"relation_types"`
	Metrics           graphMetrics      `json:"metrics"`
}

type graphMetrics struct {
	Hotspots    []graphRank `json:"hotspots"`
	EntryPoints []graphRank `json:"entry_points"`
	Packages    []graphRank `json:"packages"`
	Layers      []graphRank `json:"layers"`
	Clusters    []graphRank `json:"clusters"`
}

type graphRank struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type semanticGraphQueryResult struct {
	Symbols   []semanticRecord `json:"symbols"`
	Relations []semanticRecord `json:"relations"`
	Count     *int             `json:"count,omitempty"`
}

type semanticTracePathResult struct {
	Found     bool             `json:"found"`
	Path      []semanticRecord `json:"path"`
	Relations []semanticRecord `json:"relations"`
}

type semanticSnippetResult struct {
	Symbol  semanticRecord  `json:"symbol"`
	Content semanticContent `json:"content"`
}

type semanticDeadCodeResult struct {
	Symbols []semanticRecord `json:"symbols"`
}

type semanticRuntimeTrace struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type,omitempty"`
}

type semanticTraceIngestReport struct {
	ImportedAt time.Time `json:"imported_at"`
	Path       string    `json:"path"`
	Total      int       `json:"total"`
	Matched    int       `json:"matched_static_edges"`
	Unmatched  int       `json:"unmatched_static_edges"`
}

type semanticGraphUIReport struct {
	Path      string `json:"path"`
	Provider  string `json:"provider,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Relations int    `json:"relations"`
}

type semanticEnv struct {
	RepoDir  string
	BrainDir string
	Source   *semanticSourceManifest
}

func loadSemanticEnv(cmd *cobra.Command, opts Options) (semanticEnv, error) {
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return semanticEnv{}, err
	}
	storage, err := repoStoragePaths(cmd.Context(), opts.Runner, opts.Env, repoDir)
	if err != nil {
		return semanticEnv{}, err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return semanticEnv{}, err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return semanticEnv{}, errors.New("semantic index missing; run `entire brain index`")
	}
	return semanticEnv{RepoDir: repoDir, BrainDir: storage.BrainDir, Source: manifest.Sources.Semantic}, nil
}

func semanticStorePath(env semanticEnv) (string, error) {
	if env.Source.StorePath == "" {
		return "", errors.New("semantic graph command requires the SQLite semantic store; run `entire brain repair`")
	}
	return validateSemanticDeclaredStore(env.BrainDir, env.Source)
}

func runSemanticGraphSchema(cmd *cobra.Command, opts Options, graphOpts semanticGraphSchemaOptions) error {
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	storePath, err := semanticStorePath(env)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, storePath)
	if err != nil {
		return err
	}
	defer db.Close()
	report := semanticGraphSchemaReport{
		Provider:          env.Source.Provider,
		SchemaVersion:     env.Source.SchemaVersion,
		Profile:           env.Source.Profile,
		RelationSet:       nonNil(env.Source.RelationSet),
		Languages:         nonNil(env.Source.Languages),
		CompletenessLevel: env.Source.CompletenessLevel,
		Trust:             env.Source.Trust,
		LanguageTiers:     env.Source.LanguageTiers,
		Counts:            map[string]int{"files": env.Source.Files, "symbols": env.Source.Symbols, "relations": env.Source.Relations, "externals": env.Source.Externals},
	}
	report.SymbolKinds, err = graphDistinctStrings(db, `SELECT kind FROM symbols WHERE trim(kind) <> '' GROUP BY kind ORDER BY kind`)
	if err != nil {
		return err
	}
	report.RelationTypes, err = graphDistinctStrings(db, `SELECT type FROM relations WHERE trim(type) <> '' GROUP BY type ORDER BY type`)
	if err != nil {
		return err
	}
	report.RelationTypes, err = appendRuntimeTraceRelationType(db, report.RelationTypes)
	if err != nil {
		return err
	}
	report.Metrics, err = semanticGraphMetrics(db)
	if err != nil {
		return err
	}
	if graphOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "provider: %s schema %s profile %s\n", report.Provider, report.SchemaVersion, report.Profile)
	fmt.Fprintf(cmd.OutOrStdout(), "files: %d\nsymbols: %d\nrelations: %d\n", report.Counts["files"], report.Counts["symbols"], report.Counts["relations"])
	fmt.Fprintf(cmd.OutOrStdout(), "relation_types: %s\n", strings.Join(report.RelationTypes, ", "))
	fmt.Fprintf(cmd.OutOrStdout(), "hotspots: %s\n", graphRanksText(report.Metrics.Hotspots))
	fmt.Fprintf(cmd.OutOrStdout(), "entry_points: %s\n", graphRanksText(report.Metrics.EntryPoints))
	return nil
}

func semanticGraphMetrics(db *sql.DB) (graphMetrics, error) {
	if err := ensureSemanticRuntimeTraceTable(db); err != nil {
		return graphMetrics{}, err
	}
	hotspots, err := graphRankRows(db, `
SELECT name, COUNT(*) AS c
FROM (
  SELECT s.id AS id, COALESCE(NULLIF(s.qualified_name,''), s.name, r.to_id) AS name
  FROM relations r
  JOIN symbols s ON s.id = r.to_id
  WHERE r.type NOT IN ('DEFINES','CONTAINS','USES_TYPE','PARAM_TYPE','RETURNS_TYPE')
  UNION ALL
  SELECT s.id AS id, COALESCE(NULLIF(s.qualified_name,''), s.name, rt.to_id) AS name
  FROM runtime_traces rt
  JOIN symbols s ON s.id = rt.to_id OR s.name = rt.to_id OR s.qualified_name = rt.to_id
)
GROUP BY id, name
ORDER BY c DESC, name
LIMIT 10`)
	if err != nil {
		return graphMetrics{}, err
	}
	entryPoints, err := graphRankRows(db, `
SELECT name, COUNT(*) AS c
FROM (
  SELECT s.id AS id, COALESCE(NULLIF(s.qualified_name,''), s.name, r.from_id) AS name
  FROM relations r
  JOIN symbols s ON s.id = r.from_id
  WHERE r.type LIKE 'HANDLES_%' OR r.type IN ('CONFIGURES')
  UNION ALL
  SELECT s.id AS id, COALESCE(NULLIF(s.qualified_name,''), s.name, rt.from_id) AS name
  FROM runtime_traces rt
  JOIN symbols s ON s.id = rt.from_id OR s.name = rt.from_id OR s.qualified_name = rt.from_id
)
GROUP BY id, name
ORDER BY c DESC, name
LIMIT 10`)
	if err != nil {
		return graphMetrics{}, err
	}
	clusters, err := graphRankRows(db, `
SELECT type AS name, COUNT(*) AS c
FROM (
  SELECT type FROM relations
  UNION ALL
  SELECT 'RUNTIME_TRACE' AS type FROM runtime_traces
)
GROUP BY type
ORDER BY c DESC, name
LIMIT 20`)
	if err != nil {
		return graphMetrics{}, err
	}
	packages, layers, err := graphPathMetrics(db)
	if err != nil {
		return graphMetrics{}, err
	}
	return graphMetrics{
		Hotspots:    hotspots,
		EntryPoints: entryPoints,
		Packages:    packages,
		Layers:      layers,
		Clusters:    clusters,
	}, nil
}

func graphRankRows(db *sql.DB, query string, args ...any) ([]graphRank, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ranks []graphRank
	for rows.Next() {
		var rank graphRank
		if err := rows.Scan(&rank.Name, &rank.Count); err != nil {
			return nil, err
		}
		if strings.TrimSpace(rank.Name) != "" {
			ranks = append(ranks, rank)
		}
	}
	if ranks == nil {
		ranks = []graphRank{}
	}
	return ranks, rows.Err()
}

func graphPathMetrics(db *sql.DB) ([]graphRank, []graphRank, error) {
	rows, err := db.Query(`SELECT file_path FROM symbols WHERE trim(file_path) <> ''`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	packages := map[string]int{}
	layers := map[string]int{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, nil, err
		}
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		if dir == "." {
			dir = "<root>"
		}
		packages[dir]++
		layer := strings.Split(dir, "/")[0]
		if layer == "." || layer == "" {
			layer = "<root>"
		}
		layers[layer]++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return graphRankMap(packages, 20), graphRankMap(layers, 20), nil
}

func graphRankMap(counts map[string]int, limit int) []graphRank {
	ranks := make([]graphRank, 0, len(counts))
	for name, count := range counts {
		ranks = append(ranks, graphRank{Name: name, Count: count})
	}
	sort.Slice(ranks, func(i, j int) bool {
		if ranks[i].Count == ranks[j].Count {
			return ranks[i].Name < ranks[j].Name
		}
		return ranks[i].Count > ranks[j].Count
	})
	if len(ranks) > limit {
		ranks = ranks[:limit]
	}
	if ranks == nil {
		return []graphRank{}
	}
	return ranks
}

func graphRanksText(ranks []graphRank) string {
	if len(ranks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ranks))
	for _, rank := range ranks {
		parts = append(parts, fmt.Sprintf("%s:%d", rank.Name, rank.Count))
	}
	return strings.Join(parts, ", ")
}

func graphDistinctStrings(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return nonNil(values), rows.Err()
}

func appendRuntimeTraceRelationType(db *sql.DB, relationTypes []string) ([]string, error) {
	if err := ensureSemanticRuntimeTraceTable(db); err != nil {
		return nil, err
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_traces`).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return relationTypes, nil
	}
	for _, typ := range relationTypes {
		if typ == "RUNTIME_TRACE" {
			return relationTypes, nil
		}
	}
	relationTypes = append(relationTypes, "RUNTIME_TRACE")
	sort.Strings(relationTypes)
	return relationTypes, nil
}

func runSemanticSearchGraph(cmd *cobra.Command, opts Options, graphOpts semanticGraphSearchOptions, query string) error {
	return runSemanticQuery(cmd.Context(), cmd, opts, semanticQueryOptions{limit: graphOpts.limit, offset: graphOpts.offset, json: graphOpts.json}, query)
}

func runSemanticQueryGraph(cmd *cobra.Command, opts Options, graphOpts semanticGraphQueryOptions, query string) error {
	if graphOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	storePath, err := semanticStorePath(env)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, storePath)
	if err != nil {
		return err
	}
	defer db.Close()
	filters := parseGraphQuery(query)
	if filters.Count {
		count, err := countGraphRelations(db, filters)
		if err != nil {
			return err
		}
		result := semanticGraphQueryResult{Symbols: []semanticRecord{}, Relations: []semanticRecord{}, Count: &count}
		if graphOpts.json {
			return writeJSON(cmd, result)
		}
		fmt.Fprintln(cmd.OutOrStdout(), count)
		return nil
	}
	relations, err := queryGraphRelations(db, filters, graphOpts.limit)
	if err != nil {
		return err
	}
	ids := map[string]struct{}{}
	for _, relation := range relations {
		ids[relation.FromID] = struct{}{}
		ids[relation.ToID] = struct{}{}
	}
	symbols, err := loadGraphNodes(db, sortedIDSet(ids))
	if err != nil {
		return err
	}
	result := semanticGraphQueryResult{Symbols: nonNil(symbols), Relations: nonNil(relations)}
	if graphOpts.json {
		return writeJSON(cmd, result)
	}
	for _, relation := range result.Relations {
		fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s %s\n", relation.FromID, relation.ToID, relation.Type)
	}
	return nil
}

type graphQueryFilters struct {
	Type             string
	From             string
	To               string
	Text             string
	Limit            int
	FromKind         string
	ToKind           string
	FromName         string
	ToName           string
	FromNameContains string
	ToNameContains   string
	Count            bool
}

func parseGraphQuery(query string) graphQueryFilters {
	if filters, ok := parseCypherGraphQuery(query); ok {
		return filters
	}
	var filters graphQueryFilters
	for _, field := range strings.Fields(query) {
		key, value, ok := strings.Cut(field, ":")
		if !ok {
			if filters.Text == "" {
				filters.Text = field
			} else {
				filters.Text += " " + field
			}
			continue
		}
		switch strings.ToLower(key) {
		case "type", "relation":
			filters.Type = strings.ToUpper(value)
		case "from":
			filters.From = value
		case "to":
			filters.To = value
		default:
			if filters.Text == "" {
				filters.Text = field
			} else {
				filters.Text += " " + field
			}
		}
	}
	return filters
}

var (
	cypherRelationRe    = regexp.MustCompile(`(?is)\bMATCH\s*\(\s*[A-Za-z_][A-Za-z0-9_]*\s*\)\s*(<-|-)\s*\[\s*[A-Za-z_][A-Za-z0-9_]*\s*(?::\s*([A-Za-z0-9_]+))?\s*\]\s*(->|-)\s*\(\s*[A-Za-z_][A-Za-z0-9_]*\s*\)`)
	cypherLimitRe       = regexp.MustCompile(`(?is)\bLIMIT\s+([0-9]+)\b`)
	cypherReturnCountRe = regexp.MustCompile(`(?is)\bRETURN\s+count\s*\(\s*(?:\*|[A-Za-z_][A-Za-z0-9_]*)\s*\)`)
	cypherWhereRe       = regexp.MustCompile(`(?is)\bWHERE\s+(.+?)(?:\bRETURN\b|\bLIMIT\b|$)`)
	cypherPredRe        = regexp.MustCompile(`(?is)\b([abr])\.(name|qualified_name|kind|file_path|language|type|reason)\s*(=|CONTAINS)\s*['"]([^'"]+)['"]`)
)

func parseCypherGraphQuery(query string) (graphQueryFilters, bool) {
	if !strings.Contains(strings.ToUpper(query), "MATCH") {
		return graphQueryFilters{}, false
	}
	match := cypherRelationRe.FindStringSubmatch(query)
	if match == nil {
		return graphQueryFilters{}, false
	}
	reverse, ok := cypherRelationDirection(match[1], match[3])
	if !ok {
		return graphQueryFilters{}, false
	}
	filters := graphQueryFilters{}
	if len(match) > 2 && strings.TrimSpace(match[2]) != "" {
		filters.Type = strings.ToUpper(strings.TrimSpace(match[2]))
	}
	filters.Count = cypherReturnCountRe.MatchString(query)
	if limit := cypherLimitRe.FindStringSubmatch(query); len(limit) == 2 {
		if n, err := strconv.Atoi(limit[1]); err == nil && n > 0 {
			filters.Limit = n
		}
	}
	if where := cypherWhereRe.FindStringSubmatch(query); len(where) == 2 {
		for _, pred := range cypherPredRe.FindAllStringSubmatch(where[1], -1) {
			if len(pred) != 5 {
				continue
			}
			alias, field, op, value := strings.ToLower(pred[1]), strings.ToLower(pred[2]), strings.ToUpper(pred[3]), strings.TrimSpace(pred[4])
			applyCypherPredicate(&filters, alias, field, op, value)
		}
	}
	if reverse {
		filters = reverseCypherEndpointFilters(filters)
	}
	return filters, true
}

func cypherRelationDirection(left, right string) (bool, bool) {
	switch {
	case left == "-" && right == "->":
		return false, true
	case left == "<-" && right == "-":
		return true, true
	default:
		return false, false
	}
}

func reverseCypherEndpointFilters(filters graphQueryFilters) graphQueryFilters {
	filters.From, filters.To = filters.To, filters.From
	filters.FromKind, filters.ToKind = filters.ToKind, filters.FromKind
	filters.FromName, filters.ToName = filters.ToName, filters.FromName
	filters.FromNameContains, filters.ToNameContains = filters.ToNameContains, filters.FromNameContains
	return filters
}

func applyCypherPredicate(filters *graphQueryFilters, alias, field, op, value string) {
	switch alias + "." + field + "." + op {
	case "a.name.=":
		filters.FromName = value
	case "a.qualified_name.=":
		filters.FromName = value
	case "a.kind.=":
		filters.FromKind = value
	case "a.name.CONTAINS", "a.qualified_name.CONTAINS", "a.file_path.CONTAINS", "a.language.CONTAINS":
		filters.FromNameContains = value
	case "b.name.=":
		filters.ToName = value
	case "b.qualified_name.=":
		filters.ToName = value
	case "b.kind.=":
		filters.ToKind = value
	case "b.name.CONTAINS", "b.qualified_name.CONTAINS", "b.file_path.CONTAINS", "b.language.CONTAINS":
		filters.ToNameContains = value
	case "r.type.=":
		filters.Type = strings.ToUpper(value)
	case "r.reason.CONTAINS":
		if filters.Text == "" {
			filters.Text = value
		} else {
			filters.Text += " " + value
		}
	}
}

func queryGraphRelations(db *sql.DB, filters graphQueryFilters, limit int) ([]semanticRecord, error) {
	if filters.Limit > 0 && filters.Limit < limit {
		limit = filters.Limit
	}
	where, args := graphRelationWhereSQL(filters)
	query := `SELECT from_id, to_id, type, confidence, reason, warning_codes FROM relations` + where
	query += ` ORDER BY type, from_id, to_id LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relations, err := scanGraphRelations(rows)
	if err != nil {
		return nil, err
	}
	if filters.Type == "" || filters.Type == "RUNTIME_TRACE" {
		traces, err := queryRuntimeTraceRelations(db, filters, limit-len(relations))
		if err != nil {
			return nil, err
		}
		relations = append(relations, traces...)
	}
	if len(relations) > limit {
		relations = relations[:limit]
	}
	return nonNil(relations), nil
}

func countGraphRelations(db *sql.DB, filters graphQueryFilters) (int, error) {
	where, args := graphRelationWhereSQL(filters)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM relations`+where, args...).Scan(&count); err != nil {
		return 0, err
	}
	if filters.Type == "" || filters.Type == "RUNTIME_TRACE" {
		traceCount, err := countRuntimeTraceRelations(db, filters)
		if err != nil {
			return 0, err
		}
		count += traceCount
	}
	return count, nil
}

func graphRelationWhereSQL(filters graphQueryFilters) (string, []any) {
	query := ` WHERE 1=1`
	var args []any
	if filters.Type != "" {
		query += ` AND type = ?`
		args = append(args, filters.Type)
	}
	if filters.From != "" {
		clause, clauseArgs := graphEndpointIDOrNameSQL("from_id", filters.From)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if filters.To != "" {
		clause, clauseArgs := graphEndpointIDOrNameSQL("to_id", filters.To)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if filters.Text != "" {
		query += ` AND (from_id LIKE ? OR to_id LIKE ? OR reason LIKE ?)`
		like := "%" + filters.Text + "%"
		args = append(args, like, like, like)
	}
	if filters.FromName != "" {
		clause, clauseArgs := graphEndpointNameSQL("from_id", filters.FromName)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if filters.ToName != "" {
		clause, clauseArgs := graphEndpointNameSQL("to_id", filters.ToName)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if filters.FromKind != "" {
		clause, clauseArgs := graphEndpointKindSQL("from_id", filters.FromKind)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if filters.ToKind != "" {
		clause, clauseArgs := graphEndpointKindSQL("to_id", filters.ToKind)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if filters.FromNameContains != "" {
		clause, clauseArgs := graphEndpointContainsSQL("from_id", filters.FromNameContains)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if filters.ToNameContains != "" {
		clause, clauseArgs := graphEndpointContainsSQL("to_id", filters.ToNameContains)
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	return query, args
}

func graphEndpointIDOrNameSQL(column, value string) (string, []any) {
	return `(` + column + ` IN (SELECT id FROM symbols WHERE id = ? OR name = ? OR qualified_name = ?) OR EXISTS (SELECT 1 FROM files f WHERE ` + column + ` LIKE '%:file:' || f.path AND (f.path = ? OR f.path LIKE '%/' || ?)))`,
		[]any{value, value, value, value, value}
}

func graphEndpointNameSQL(column, value string) (string, []any) {
	return `(` + column + ` IN (SELECT id FROM symbols WHERE name = ? OR qualified_name = ?) OR EXISTS (SELECT 1 FROM files f WHERE ` + column + ` LIKE '%:file:' || f.path AND (f.path = ? OR f.path LIKE '%/' || ?)))`,
		[]any{value, value, value, value}
}

func graphEndpointKindSQL(column, value string) (string, []any) {
	if strings.EqualFold(value, "file") {
		return `(EXISTS (SELECT 1 FROM files f WHERE ` + column + ` LIKE '%:file:' || f.path))`, nil
	}
	return `(` + column + ` IN (SELECT id FROM symbols WHERE kind = ?))`, []any{value}
}

func graphEndpointContainsSQL(column, value string) (string, []any) {
	like := "%" + value + "%"
	return `(` + column + ` IN (SELECT id FROM symbols WHERE name LIKE ? OR qualified_name LIKE ? OR file_path LIKE ? OR language LIKE ?) OR EXISTS (SELECT 1 FROM files f WHERE ` + column + ` LIKE '%:file:' || f.path AND (f.path LIKE ? OR f.language LIKE ?)))`,
		[]any{like, like, like, like, like, like}
}

func runSemanticTracePath(cmd *cobra.Command, opts Options, traceOpts semanticTracePathOptions, from, to string) error {
	if traceOpts.depth <= 0 {
		return errors.New("--depth must be greater than zero")
	}
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	storePath, err := semanticStorePath(env)
	if err != nil {
		return err
	}
	start, err := findSemanticRecordByIDOrNameSQLite(storePath, from)
	if err != nil {
		return err
	}
	end, err := findSemanticRecordByIDOrNameSQLite(storePath, to)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, storePath)
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := traceGraphPath(db, start.ID, end.ID, traceOpts.depth)
	if err != nil {
		return err
	}
	if traceOpts.json {
		return writeJSON(cmd, result)
	}
	if !result.Found {
		fmt.Fprintln(cmd.OutOrStdout(), "no path found")
		return nil
	}
	for _, record := range result.Path {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", displaySymbolName(record))
	}
	return nil
}

func traceGraphPath(db *sql.DB, startID, endID string, maxDepth int) (semanticTracePathResult, error) {
	type node struct {
		ID    string
		Path  []string
		Edges []semanticRecord
	}
	queue := []node{{ID: startID, Path: []string{startID}}}
	seen := map[string]struct{}{startID: {}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if len(current.Path)-1 >= maxDepth {
			continue
		}
		edges, err := relationsFrom(db, current.ID)
		if err != nil {
			return semanticTracePathResult{}, err
		}
		for _, edge := range edges {
			next := edge.ToID
			if _, ok := seen[next]; ok {
				continue
			}
			path := append(append([]string{}, current.Path...), next)
			edgePath := append(append([]semanticRecord{}, current.Edges...), edge)
			if next == endID {
				symbols, err := loadGraphSymbols(db, path)
				if err != nil {
					return semanticTracePathResult{}, err
				}
				return semanticTracePathResult{Found: true, Path: symbols, Relations: edgePath}, nil
			}
			seen[next] = struct{}{}
			queue = append(queue, node{ID: next, Path: path, Edges: edgePath})
		}
	}
	return semanticTracePathResult{Found: false, Path: []semanticRecord{}, Relations: []semanticRecord{}}, nil
}

func relationsFrom(db *sql.DB, id string) ([]semanticRecord, error) {
	rows, err := db.Query(`SELECT from_id, to_id, type, confidence, reason, warning_codes FROM relations WHERE from_id = ? ORDER BY type, to_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relations, err := scanGraphRelations(rows)
	if err != nil {
		return nil, err
	}
	traceRelations, err := runtimeTraceRelationsFrom(db, id)
	if err != nil {
		return nil, err
	}
	relations = append(relations, traceRelations...)
	sort.Slice(relations, func(i, j int) bool {
		if relations[i].Type == relations[j].Type {
			return relations[i].ToID < relations[j].ToID
		}
		return relations[i].Type < relations[j].Type
	})
	return nonNil(relations), nil
}

func runtimeTraceRelationsFrom(db *sql.DB, id string) ([]semanticRecord, error) {
	if err := ensureSemanticRuntimeTraceTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT rt.from_id, COALESCE(to_sym.id, rt.to_id) AS to_id, rt.observed_type, rt.matched_static_edge, rt.source_path
FROM runtime_traces rt
LEFT JOIN symbols from_sym ON from_sym.id = rt.from_id OR from_sym.name = rt.from_id OR from_sym.qualified_name = rt.from_id
LEFT JOIN symbols to_sym ON to_sym.id = rt.to_id OR to_sym.name = rt.to_id OR to_sym.qualified_name = rt.to_id
WHERE rt.from_id = ? OR from_sym.id = ?
ORDER BY rt.imported_at, rt.id`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []semanticRecord
	for rows.Next() {
		var record semanticRecord
		var observedType, sourcePath string
		var matched int
		if err := rows.Scan(&record.FromID, &record.ToID, &observedType, &matched, &sourcePath); err != nil {
			return nil, err
		}
		record.RecordType = "runtime_trace"
		record.FromID = id
		record.Type = "RUNTIME_TRACE"
		record.Confidence = 1
		record.Reason = strings.TrimSpace("runtime trace observed " + observedType)
		if record.Reason == "runtime trace observed" {
			record.Reason = "runtime trace observed edge"
		}
		record.WarningCodes = []string{}
		if matched == 0 {
			record.Confidence = 0.5
			record.WarningCodes = []string{"UNMATCHED_STATIC_EDGE"}
		}
		record.Evidence = []semanticEvidence{{Kind: "runtime_trace_import", FilePath: sourcePath, Detail: observedType}}
		records = append(records, record)
	}
	return nonNil(records), rows.Err()
}

func runSemanticSnippet(cmd *cobra.Command, opts Options, snippetOpts semanticSnippetOptions, idOrName string) error {
	if snippetOpts.contextLines < 0 {
		return errors.New("--context-lines must be zero or greater")
	}
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	record, err := findSemanticRecordByIDOrName(env.BrainDir, env.Source, idOrName)
	if err != nil {
		return err
	}
	start := record.StartLine - snippetOpts.contextLines
	if start < 1 {
		start = 1
	}
	end := record.EndLine + snippetOpts.contextLines
	content := semanticContextContent(env.RepoDir, []semanticRecord{{
		FilePath:  record.FilePath,
		StartLine: start,
		EndLine:   end,
	}})
	if len(content) == 0 {
		return fmt.Errorf("source snippet unavailable for %s", idOrName)
	}
	result := semanticSnippetResult{Symbol: record, Content: content[0]}
	if snippetOpts.json {
		return writeJSON(cmd, result)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s:%d-%d\n%s\n", result.Content.Path, result.Content.StartLine, result.Content.EndLine, result.Content.Text)
	return nil
}

func runSemanticDeadCode(cmd *cobra.Command, opts Options, deadOpts semanticDeadCodeOptions) error {
	if deadOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	storePath, err := semanticStorePath(env)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, storePath)
	if err != nil {
		return err
	}
	defer db.Close()
	symbols, err := queryDeadCodeSymbols(db, deadOpts.limit)
	if err != nil {
		return err
	}
	result := semanticDeadCodeResult{Symbols: nonNil(symbols)}
	if deadOpts.json {
		return writeJSON(cmd, result)
	}
	for _, symbol := range result.Symbols {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s:%d-%d\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
	}
	return nil
}

func queryDeadCodeSymbols(db *sql.DB, limit int) ([]semanticRecord, error) {
	rows, err := db.Query(`
SELECT s.id, s.kind, s.name, s.qualified_name, s.file_path, s.start_line, s.end_line, s.signature, s.language, s.stable_id_version
FROM symbols s
WHERE s.kind IN ('function','method')
  AND NOT EXISTS (
    SELECT 1 FROM relations r
    WHERE r.to_id = s.id
      AND r.type NOT IN ('DEFINES','CONTAINS','USES_TYPE','PARAM_TYPE','RETURNS_TYPE','READS_FIELD','WRITES_FIELD','ACCESSES')
  )
  AND NOT EXISTS (
    SELECT 1 FROM relations r
    WHERE r.from_id = s.id
      AND r.type LIKE 'HANDLES_%'
  )
ORDER BY s.file_path, s.start_line, s.qualified_name
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphSymbols(rows)
}

func runSemanticIngestTraces(cmd *cobra.Command, opts Options, ingestOpts semanticTraceIngestOptions, path string) error {
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	storePath, err := semanticStorePath(env)
	if err != nil {
		return err
	}
	traces, err := readRuntimeTraces(path)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, storePath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureSemanticRuntimeTraceTable(db); err != nil {
		return err
	}
	matched := 0
	traceMatches := make([]bool, len(traces))
	for i, trace := range traces {
		ok, err := runtimeTraceMatchesStaticEdge(db, trace)
		if err != nil {
			return err
		}
		if ok {
			matched++
		}
		traceMatches[i] = ok
	}
	report := semanticTraceIngestReport{
		ImportedAt: time.Now().UTC(),
		Path:       filepath.ToSlash(path),
		Total:      len(traces),
		Matched:    matched,
		Unmatched:  len(traces) - matched,
	}
	rel, err := writeRuntimeTraceImport(env.BrainDir, report, traces)
	if err != nil {
		return err
	}
	report.Path = rel
	if err := insertRuntimeTraceFacts(db, report, traces, traceMatches); err != nil {
		return err
	}
	if ingestOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ingested runtime traces: %d\nmatched_static_edges: %d\nartifact: %s\n", report.Total, report.Matched, report.Path)
	return nil
}

func runSemanticGraphUI(cmd *cobra.Command, opts Options, uiOpts semanticGraphUIOptions, outputPath string) error {
	if uiOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	env, err := loadSemanticEnv(cmd, opts)
	if err != nil {
		return err
	}
	storePath, err := semanticStorePath(env)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, storePath)
	if err != nil {
		return err
	}
	defer db.Close()
	relations, err := graphUIRelations(db, uiOpts.limit)
	if err != nil {
		return err
	}
	metrics, err := semanticGraphMetrics(db)
	if err != nil {
		return err
	}
	data := struct {
		Provider string           `json:"provider"`
		Profile  string           `json:"profile"`
		Counts   map[string]int   `json:"counts"`
		Metrics  graphMetrics     `json:"metrics"`
		Edges    []semanticRecord `json:"edges"`
	}{
		Provider: env.Source.Provider,
		Profile:  env.Source.Profile,
		Counts:   map[string]int{"files": env.Source.Files, "symbols": env.Source.Symbols, "relations": env.Source.Relations, "externals": env.Source.Externals},
		Metrics:  metrics,
		Edges:    relations,
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(outputPath) == "" {
		outputPath = "semantic-graph.html"
	}
	abs, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(abs, []byte(renderSemanticGraphHTML(payload)), 0o600); err != nil {
		return err
	}
	report := semanticGraphUIReport{Path: abs, Provider: env.Source.Provider, Profile: env.Source.Profile, Relations: len(relations)}
	if uiOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote graph UI: %s\n", abs)
	return nil
}

func graphUIRelations(db *sql.DB, limit int) ([]semanticRecord, error) {
	rows, err := db.Query(`SELECT from_id, to_id, type, confidence, reason, warning_codes FROM relations ORDER BY type, from_id, to_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphRelations(rows)
}

func renderSemanticGraphHTML(payload []byte) string {
	escaped := strings.ReplaceAll(string(payload), "</", "<\\/")
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Semantic Graph</title>
<style>
:root{color-scheme:light dark;font-family:Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
body{margin:0;background:#f7f7f4;color:#1d2228}
main{max-width:1180px;margin:0 auto;padding:28px}
header{display:flex;align-items:flex-end;justify-content:space-between;gap:20px;border-bottom:1px solid #d7d8d2;padding-bottom:16px}
h1{font-size:26px;line-height:1.2;margin:0;font-weight:700}
h2{font-size:15px;margin:0 0 10px;color:#38414a}
.meta{font-size:13px;color:#53606b}
.grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin:18px 0}
.stat,.panel{background:#fff;border:1px solid #dfe1dc;border-radius:8px;padding:14px}
.stat strong{display:block;font-size:24px}
.panels{display:grid;grid-template-columns:1fr 1fr;gap:14px}
.bar{display:grid;grid-template-columns:minmax(120px,1fr) minmax(100px,2fr) 44px;align-items:center;gap:8px;font-size:13px;margin:7px 0}
.fill{height:9px;background:#477a7b;border-radius:999px}
table{width:100%;border-collapse:collapse;font-size:13px;background:#fff;border:1px solid #dfe1dc;border-radius:8px;overflow:hidden}
th,td{text-align:left;border-bottom:1px solid #ecede8;padding:8px;vertical-align:top}
th{background:#eceee8;font-size:12px;text-transform:uppercase;color:#4c5963}
.edges{margin-top:14px}
input{width:100%;box-sizing:border-box;border:1px solid #cfd3ca;border-radius:6px;padding:9px 10px;font:inherit;margin-bottom:10px}
@media(max-width:760px){main{padding:18px}.grid,.panels{grid-template-columns:1fr}header{display:block}.bar{grid-template-columns:1fr}}
@media(prefers-color-scheme:dark){body{background:#171b1e;color:#eef1ed}.stat,.panel,table{background:#20262a;border-color:#384147}header,th,td{border-color:#384147}th{background:#252d31;color:#c7d0d6}.meta{color:#aeb9bf}.fill{background:#74a8a2}input{background:#171b1e;color:#eef1ed;border-color:#455057}}
</style>
</head>
<body>
<main>
<header><div><h1>Semantic Graph</h1><div class="meta" id="meta"></div></div></header>
<section class="grid" id="stats"></section>
<section class="panels">
<div class="panel"><h2>Hotspots</h2><div id="hotspots"></div></div>
<div class="panel"><h2>Entry Points</h2><div id="entrypoints"></div></div>
<div class="panel"><h2>Packages</h2><div id="packages"></div></div>
<div class="panel"><h2>Relation Clusters</h2><div id="clusters"></div></div>
</section>
<section class="edges">
<input id="filter" placeholder="Filter edges by symbol, relation, or reason">
<table><thead><tr><th>Type</th><th>From</th><th>To</th><th>Confidence</th><th>Reason</th></tr></thead><tbody id="edges"></tbody></table>
</section>
</main>
<script type="application/json" id="graph-data">` + escaped + `</script>
<script>
const data=JSON.parse(document.getElementById('graph-data').textContent);
document.getElementById('meta').textContent=[data.provider,data.profile].filter(Boolean).join(' / ');
const stats=document.getElementById('stats');
for (const [k,v] of Object.entries(data.counts||{})){const d=document.createElement('div');d.className='stat';d.innerHTML='<span>'+k+'</span><strong>'+v+'</strong>';stats.appendChild(d);}
function bars(id, rows){const el=document.getElementById(id);const max=Math.max(1,...(rows||[]).map(r=>r.count));for(const r of rows||[]){const b=document.createElement('div');b.className='bar';b.innerHTML='<span>'+r.name+'</span><span class="fill" style="width:'+Math.max(4,Math.round(r.count/max*100))+'%"></span><span>'+r.count+'</span>';el.appendChild(b);}}
bars('hotspots',data.metrics.hotspots);bars('entrypoints',data.metrics.entry_points);bars('packages',data.metrics.packages);bars('clusters',data.metrics.clusters);
const tbody=document.getElementById('edges');function renderEdges(q=''){tbody.textContent='';q=q.toLowerCase();for(const e of data.edges||[]){const text=[e.type,e.from_id,e.to_id,e.reason].join(' ').toLowerCase();if(q&&!text.includes(q))continue;const tr=document.createElement('tr');tr.innerHTML='<td>'+e.type+'</td><td>'+e.from_id+'</td><td>'+e.to_id+'</td><td>'+Number(e.confidence||0).toFixed(2)+'</td><td>'+(e.reason||'')+'</td>';tbody.appendChild(tr);}}
renderEdges();document.getElementById('filter').addEventListener('input',e=>renderEdges(e.target.value));
</script>
</body>
</html>
`
}

func readRuntimeTraces(path string) ([]semanticRuntimeTrace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var traces []semanticRuntimeTrace
	dec := json.NewDecoder(f)
	if err := dec.Decode(&traces); err == nil {
		return traces, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	// Cap the per-line buffer so an oversized line in an untrusted trace file fails
	// loudly via scanner.Err() rather than relying on bufio's silent 64 KiB default.
	scanner.Buffer(make([]byte, 0, 64*1024), semanticRecordMaxBytes())
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var trace semanticRuntimeTrace
		if err := json.Unmarshal([]byte(line), &trace); err != nil {
			return nil, err
		}
		traces = append(traces, trace)
	}
	return traces, scanner.Err()
}

func runtimeTraceMatchesStaticEdge(db *sql.DB, trace semanticRuntimeTrace) (bool, error) {
	relationType := strings.TrimSpace(trace.Type)
	query := `SELECT COUNT(*) FROM relations WHERE from_id IN (SELECT id FROM symbols WHERE id = ? OR name = ? OR qualified_name = ?) AND to_id IN (SELECT id FROM symbols WHERE id = ? OR name = ? OR qualified_name = ?)`
	args := []any{trace.From, trace.From, trace.From, trace.To, trace.To, trace.To}
	if relationType != "" {
		query += ` AND type = ?`
		args = append(args, relationType)
	}
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func ensureSemanticRuntimeTraceTable(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS runtime_traces (id INTEGER PRIMARY KEY AUTOINCREMENT, imported_at TEXT NOT NULL, source_path TEXT NOT NULL, from_id TEXT NOT NULL, to_id TEXT NOT NULL, observed_type TEXT, matched_static_edge INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_traces_from ON runtime_traces(from_id)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_traces_to ON runtime_traces(to_id)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func insertRuntimeTraceFacts(db *sql.DB, report semanticTraceIngestReport, traces []semanticRuntimeTrace, matched []bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for i, trace := range traces {
		isMatched := 0
		if i < len(matched) && matched[i] {
			isMatched = 1
		}
		if _, err := tx.Exec(`INSERT INTO runtime_traces(imported_at, source_path, from_id, to_id, observed_type, matched_static_edge) VALUES (?, ?, ?, ?, ?, ?)`,
			report.ImportedAt.Format(time.RFC3339Nano), report.Path, trace.From, trace.To, strings.TrimSpace(trace.Type), isMatched); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func queryRuntimeTraceRelations(db *sql.DB, filters graphQueryFilters, limit int) ([]semanticRecord, error) {
	if limit <= 0 {
		return []semanticRecord{}, nil
	}
	if err := ensureSemanticRuntimeTraceTable(db); err != nil {
		return nil, err
	}
	where, args := runtimeTraceWhereSQL(filters)
	query := `SELECT from_id, to_id, observed_type, matched_static_edge, source_path FROM runtime_traces` + where
	query += ` ORDER BY imported_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []semanticRecord
	for rows.Next() {
		var record semanticRecord
		var observedType, sourcePath string
		var matched int
		if err := rows.Scan(&record.FromID, &record.ToID, &observedType, &matched, &sourcePath); err != nil {
			return nil, err
		}
		record.RecordType = "runtime_trace"
		record.Type = "RUNTIME_TRACE"
		record.Confidence = 1
		record.Reason = strings.TrimSpace("runtime trace observed " + observedType)
		if record.Reason == "runtime trace observed" {
			record.Reason = "runtime trace observed edge"
		}
		record.WarningCodes = []string{}
		if matched == 0 {
			record.Confidence = 0.5
			record.WarningCodes = []string{"UNMATCHED_STATIC_EDGE"}
		}
		record.Evidence = []semanticEvidence{{Kind: "runtime_trace_import", FilePath: sourcePath, Detail: observedType}}
		records = append(records, record)
	}
	return nonNil(records), rows.Err()
}

func countRuntimeTraceRelations(db *sql.DB, filters graphQueryFilters) (int, error) {
	if err := ensureSemanticRuntimeTraceTable(db); err != nil {
		return 0, err
	}
	where, args := runtimeTraceWhereSQL(filters)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_traces`+where, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func runtimeTraceWhereSQL(filters graphQueryFilters) (string, []any) {
	query := ` WHERE 1=1`
	var args []any
	if filters.From != "" {
		query += ` AND from_id = ?`
		args = append(args, filters.From)
	}
	if filters.To != "" {
		query += ` AND to_id = ?`
		args = append(args, filters.To)
	}
	if filters.Text != "" {
		query += ` AND (from_id LIKE ? OR to_id LIKE ? OR observed_type LIKE ? OR source_path LIKE ?)`
		like := "%" + filters.Text + "%"
		args = append(args, like, like, like, like)
	}
	if filters.FromName != "" {
		query += ` AND from_id IN (SELECT id FROM symbols WHERE name = ? OR qualified_name = ?)`
		args = append(args, filters.FromName, filters.FromName)
	}
	if filters.ToName != "" {
		query += ` AND to_id IN (SELECT id FROM symbols WHERE name = ? OR qualified_name = ?)`
		args = append(args, filters.ToName, filters.ToName)
	}
	if filters.FromKind != "" {
		query += ` AND from_id IN (SELECT id FROM symbols WHERE kind = ?)`
		args = append(args, filters.FromKind)
	}
	if filters.ToKind != "" {
		query += ` AND to_id IN (SELECT id FROM symbols WHERE kind = ?)`
		args = append(args, filters.ToKind)
	}
	if filters.FromNameContains != "" {
		query += ` AND from_id IN (SELECT id FROM symbols WHERE name LIKE ? OR qualified_name LIKE ? OR file_path LIKE ? OR language LIKE ?)`
		like := "%" + filters.FromNameContains + "%"
		args = append(args, like, like, like, like)
	}
	if filters.ToNameContains != "" {
		query += ` AND to_id IN (SELECT id FROM symbols WHERE name LIKE ? OR qualified_name LIKE ? OR file_path LIKE ? OR language LIKE ?)`
		like := "%" + filters.ToNameContains + "%"
		args = append(args, like, like, like, like)
	}
	return query, args
}

func writeRuntimeTraceImport(brainDir string, report semanticTraceIngestReport, traces []semanticRuntimeTrace) (string, error) {
	rel := filepath.ToSlash(filepath.Join(semanticDirName, "traces", "import-"+report.ImportedAt.Format("20060102T150405Z")+".json"))
	if err := rejectExistingSymlinkPathComponents(brainDir, filepath.FromSlash(rel)); err != nil {
		return "", err
	}
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	payload := struct {
		Report semanticTraceIngestReport `json:"report"`
		Traces []semanticRuntimeTrace    `json:"traces"`
	}{Report: report, Traces: traces}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(full, data, 0o600); err != nil {
		return "", err
	}
	return rel, nil
}

func scanGraphRelations(rows *sql.Rows) ([]semanticRecord, error) {
	var relations []semanticRecord
	for rows.Next() {
		var record semanticRecord
		var warningCodes string
		record.RecordType = "relation"
		if err := rows.Scan(&record.FromID, &record.ToID, &record.Type, &record.Confidence, &record.Reason, &warningCodes); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(warningCodes), &record.WarningCodes)
		relations = append(relations, record)
	}
	return nonNil(relations), rows.Err()
}

func loadGraphNodes(db *sql.DB, ids []string) ([]semanticRecord, error) {
	if len(ids) == 0 {
		return []semanticRecord{}, nil
	}
	symbols, err := loadGraphSymbols(db, ids)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		seen[symbol.ID] = struct{}{}
	}
	fileIDsByPath := map[string][]string{}
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		if path, ok := graphFilePathFromID(id); ok {
			fileIDsByPath[path] = append(fileIDsByPath[path], id)
		}
	}
	files, err := loadGraphFiles(db, fileIDsByPath)
	if err != nil {
		return nil, err
	}
	nodesByID := make(map[string]semanticRecord, len(symbols)+len(files))
	for _, symbol := range symbols {
		nodesByID[symbol.ID] = symbol
	}
	for _, file := range files {
		nodesByID[file.ID] = file
	}
	ordered := make([]semanticRecord, 0, len(nodesByID))
	for _, id := range ids {
		if node, ok := nodesByID[id]; ok {
			ordered = append(ordered, node)
		}
	}
	return nonNil(ordered), nil
}

func loadGraphSymbols(db *sql.DB, ids []string) ([]semanticRecord, error) {
	if len(ids) == 0 {
		return []semanticRecord{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Query(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version FROM symbols WHERE id IN (`+placeholders+`) ORDER BY file_path, start_line, qualified_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	symbols, err := scanGraphSymbols(rows)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]semanticRecord, len(symbols))
	for _, symbol := range symbols {
		byID[symbol.ID] = symbol
	}
	ordered := make([]semanticRecord, 0, len(ids))
	for _, id := range ids {
		if symbol, ok := byID[id]; ok {
			ordered = append(ordered, symbol)
		}
	}
	return nonNil(ordered), nil
}

func loadGraphFiles(db *sql.DB, idsByPath map[string][]string) ([]semanticRecord, error) {
	if len(idsByPath) == 0 {
		return []semanticRecord{}, nil
	}
	paths := make([]string, 0, len(idsByPath))
	for path := range idsByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(paths)), ",")
	args := make([]any, len(paths))
	for i, path := range paths {
		args[i] = path
	}
	rows, err := db.Query(`SELECT path, blob, content_hash, language FROM files WHERE path IN (`+placeholders+`) ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []semanticRecord
	for rows.Next() {
		var path, blob, contentHash, language string
		if err := rows.Scan(&path, &blob, &contentHash, &language); err != nil {
			return nil, err
		}
		for _, id := range idsByPath[path] {
			files = append(files, semanticRecord{
				RecordType:    "file",
				ID:            id,
				Kind:          "file",
				Name:          filepath.Base(path),
				QualifiedName: path,
				FilePath:      path,
				Path:          path,
				Language:      language,
				Blob:          blob,
				Signature:     contentHash,
			})
		}
	}
	return nonNil(files), rows.Err()
}

func graphFilePathFromID(id string) (string, bool) {
	_, path, ok := strings.Cut(id, ":file:")
	if !ok || strings.TrimSpace(path) == "" {
		return "", false
	}
	return filepath.ToSlash(path), true
}

func scanGraphSymbols(rows *sql.Rows) ([]semanticRecord, error) {
	var symbols []semanticRecord
	for rows.Next() {
		var record semanticRecord
		record.RecordType = "symbol"
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
			return nil, err
		}
		symbols = append(symbols, record)
	}
	return nonNil(symbols), rows.Err()
}

func sortedIDSet(ids map[string]struct{}) []string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		if strings.HasPrefix(id, "external:") {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
