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
	"sort"
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

type semanticGraphSchemaReport struct {
	Provider      string         `json:"provider,omitempty"`
	SchemaVersion string         `json:"schema_version,omitempty"`
	Profile       string         `json:"profile,omitempty"`
	RelationSet   []string       `json:"relation_set,omitempty"`
	Languages     []string       `json:"languages,omitempty"`
	Counts        map[string]int `json:"counts"`
	SymbolKinds   []string       `json:"symbol_kinds"`
	RelationTypes []string       `json:"relation_types"`
}

type semanticGraphQueryResult struct {
	Symbols   []semanticRecord `json:"symbols"`
	Relations []semanticRecord `json:"relations"`
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
		Provider:      env.Source.Provider,
		SchemaVersion: env.Source.SchemaVersion,
		Profile:       env.Source.Profile,
		RelationSet:   nonNilStrings(env.Source.RelationSet),
		Languages:     nonNilStrings(env.Source.Languages),
		Counts:        map[string]int{"files": env.Source.Files, "symbols": env.Source.Symbols, "relations": env.Source.Relations, "externals": env.Source.Externals},
	}
	report.SymbolKinds, err = graphDistinctStrings(db, `SELECT kind FROM symbols WHERE trim(kind) <> '' GROUP BY kind ORDER BY kind`)
	if err != nil {
		return err
	}
	report.RelationTypes, err = graphDistinctStrings(db, `SELECT type FROM relations WHERE trim(type) <> '' GROUP BY type ORDER BY type`)
	if err != nil {
		return err
	}
	if graphOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "provider: %s schema %s profile %s\n", report.Provider, report.SchemaVersion, report.Profile)
	fmt.Fprintf(cmd.OutOrStdout(), "files: %d\nsymbols: %d\nrelations: %d\n", report.Counts["files"], report.Counts["symbols"], report.Counts["relations"])
	fmt.Fprintf(cmd.OutOrStdout(), "relation_types: %s\n", strings.Join(report.RelationTypes, ", "))
	return nil
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
	return nonNilStrings(values), rows.Err()
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
	relations, err := queryGraphRelations(db, filters, graphOpts.limit)
	if err != nil {
		return err
	}
	ids := map[string]struct{}{}
	for _, relation := range relations {
		ids[relation.FromID] = struct{}{}
		ids[relation.ToID] = struct{}{}
	}
	symbols, err := loadGraphSymbols(db, sortedIDSet(ids))
	if err != nil {
		return err
	}
	result := semanticGraphQueryResult{Symbols: nonNilRecords(symbols), Relations: nonNilRecords(relations)}
	if graphOpts.json {
		return writeJSON(cmd, result)
	}
	for _, relation := range result.Relations {
		fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s %s\n", relation.FromID, relation.ToID, relation.Type)
	}
	return nil
}

type graphQueryFilters struct {
	Type string
	From string
	To   string
	Text string
}

func parseGraphQuery(query string) graphQueryFilters {
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

func queryGraphRelations(db *sql.DB, filters graphQueryFilters, limit int) ([]semanticRecord, error) {
	query := `SELECT from_id, to_id, type, confidence, reason, warning_codes FROM relations WHERE 1=1`
	var args []any
	if filters.Type != "" {
		query += ` AND type = ?`
		args = append(args, filters.Type)
	}
	if filters.From != "" {
		query += ` AND from_id IN (SELECT id FROM symbols WHERE id = ? OR name = ? OR qualified_name = ?)`
		args = append(args, filters.From, filters.From, filters.From)
	}
	if filters.To != "" {
		query += ` AND to_id IN (SELECT id FROM symbols WHERE id = ? OR name = ? OR qualified_name = ?)`
		args = append(args, filters.To, filters.To, filters.To)
	}
	if filters.Text != "" {
		query += ` AND (from_id LIKE ? OR to_id LIKE ? OR reason LIKE ?)`
		like := "%" + filters.Text + "%"
		args = append(args, like, like, like)
	}
	query += ` ORDER BY type, from_id, to_id LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphRelations(rows)
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
	return scanGraphRelations(rows)
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
	result := semanticDeadCodeResult{Symbols: nonNilRecords(symbols)}
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
	matched := 0
	for _, trace := range traces {
		ok, err := runtimeTraceMatchesStaticEdge(db, trace)
		if err != nil {
			return err
		}
		if ok {
			matched++
		}
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
	if ingestOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ingested runtime traces: %d\nmatched_static_edges: %d\nartifact: %s\n", report.Total, report.Matched, report.Path)
	return nil
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
	return nonNilRecords(relations), rows.Err()
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
	return scanGraphSymbols(rows)
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
	return nonNilRecords(symbols), rows.Err()
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

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
