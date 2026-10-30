package semstore

import (
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// findSymbolsTokenizedSQLite ranks symbols by the inverse-document-
// frequency-weighted sum of query tokens appearing in their full-text body. It
// is the fallback for multi-word queries that never appear verbatim in any one
// symbol; weighting by rarity keeps a specific term (matched by few symbols)
// from being tied with a generic filler term (matched by many).
func findSymbolsTokenizedSQLite(db *sql.DB, tokens []string, limit, offset int) ([]Record, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM symbols`).Scan(&total); err != nil {
		return nil, err
	}
	type weightedToken struct {
		value  string
		weight int
		order  int
	}
	weighted := make([]weightedToken, 0, len(tokens))
	for i, token := range tokens {
		var df int
		if err := db.QueryRow(`SELECT count(*) FROM symbol_fts WHERE instr(lower(text), ?) > 0`, token).Scan(&df); err != nil {
			return nil, err
		}
		if df == 0 {
			continue
		}
		weighted = append(weighted, weightedToken{value: token, weight: TokenIDFWeight(total, df), order: i})
	}
	// Keep the task's leading identifiers before filling the fixed search budget
	// with rare terms. Pure IDF selection lets incidental rare words at the end
	// of a long agent prompt displace its task ID and primary domain nouns.
	selected := append([]weightedToken(nil), weighted[:min(semanticQueryLeadingTokenReserve, len(weighted))]...)
	sort.SliceStable(weighted, func(i, j int) bool {
		if weighted[i].weight != weighted[j].weight {
			return weighted[i].weight > weighted[j].weight
		}
		return weighted[i].order < weighted[j].order
	})
	for _, candidate := range weighted {
		if len(selected) >= semanticQueryTokenSearchLimit {
			break
		}
		if slices.ContainsFunc(selected, func(existing weightedToken) bool { return existing.order == candidate.order }) {
			continue
		}
		selected = append(selected, candidate)
	}
	if len(selected) == 0 {
		return nil, nil
	}
	tokens = tokens[:0]
	weights := make([]int, 0, len(selected))
	for _, candidate := range selected {
		tokens = append(tokens, candidate.value)
		weights = append(weights, candidate.weight)
	}
	var conds, score strings.Builder
	args := make([]any, 0, len(tokens)*2+2)
	for i := range tokens {
		if i > 0 {
			conds.WriteString(" OR ")
			score.WriteString(" + ")
		}
		conds.WriteString("instr(lower(f.text), ?) > 0")
		// Weights are computed integers, safe to inline as SQL literals.
		fmt.Fprintf(&score, "(CASE WHEN instr(lower(f.text), ?) > 0 THEN %d ELSE 0 END)", weights[i])
	}
	// Token args are consumed once for the score expression and once for the
	// WHERE clause, in that order.
	for _, token := range tokens {
		args = append(args, token)
	}
	for _, token := range tokens {
		args = append(args, token)
	}
	args = append(args, limit, offset)
	q := `SELECT s.id, s.kind, s.name, s.qualified_name, s.file_path, s.start_line, s.end_line, s.signature, s.language, s.stable_id_version, (` + score.String() + `) AS hits
FROM symbols s
JOIN symbol_fts f ON f.id = s.id
WHERE ` + conds.String() + `
ORDER BY hits DESC, s.qualified_name
LIMIT ? OFFSET ?`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []Record
	for rows.Next() {
		var record Record
		record.RecordType = "symbol"
		var hits int
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion, &hits); err != nil {
			return nil, err
		}
		record.Score = hits
		results = append(results, record)
	}
	return results, rows.Err()
}

func FindSymbolsInSQLite(storePath, query string, limit, offset int) ([]Record, error) {
	db, err := OpenReadDB(storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	literal := strings.ToLower(strings.TrimSpace(query))
	// Match the full-text body OR an exact id/qualified-name. The id branch lets
	// an agent paste a record id straight from `search`/`overview` results into
	// `context`/`impact` and get that exact symbol, instead of an empty result
	// because the colon-delimited id never appears as a substring of the body.
	rows, err := db.Query(`SELECT s.id, s.kind, s.name, s.qualified_name, s.file_path, s.start_line, s.end_line, s.signature, s.language, s.stable_id_version
FROM symbols s
JOIN symbol_fts f ON f.id = s.id
WHERE instr(lower(f.text), ?) > 0 OR lower(s.id) = ? OR lower(s.qualified_name) = ?
ORDER BY CASE WHEN lower(s.id) = ? THEN 0 WHEN lower(s.name) = lower(?) THEN 1 WHEN lower(s.qualified_name) = lower(?) THEN 2 ELSE 3 END, s.qualified_name
LIMIT ? OFFSET ?`, literal, literal, literal, literal, query, query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []Record
	for rows.Next() {
		var record Record
		record.RecordType = "symbol"
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
			return nil, err
		}
		results = append(results, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fall back to token-overlap ranking only when the verbatim query matched
	// nothing and the query is multi-word. This leaves every single-symbol and
	// substring lookup untouched while making natural-language queries (and the
	// task strings `brief` passes here) match on their content words.
	if len(results) == 0 && !QueryLooksLikePath(query) {
		tokens := QueryTokens(query)
		if len(tokens) > 1 {
			return findSymbolsTokenizedSQLite(db, tokens, limit, offset)
		}
	}
	return results, nil
}

func FindRelationsForSymbolsInSQLite(storePath string, symbols []Record, limit int) ([]Record, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	db, err := OpenReadDB(storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// Chunk the IN-clause. Each id is bound twice (from_id and to_id), so a single
	// query over the whole set blows past SQLITE_MAX_VARIABLE_NUMBER (~32766) once
	// the symbol set exceeds ~16k — which the viz graph reaches at scale. Query in
	// batches and merge by the relation rowid so the global "ORDER BY id LIMIT"
	// still holds: each batch returns its own first `limit` rows by id, and any row
	// a batch drops has a larger id than `limit` rows it kept, so it can never
	// belong in the global top-N. A set that fits one batch behaves exactly like
	// the old single query. (LoadSymbolsByIDsSQLite chunks the same way.)
	const chunk = 900
	type keyedRel struct {
		id  int64
		rec Record
	}
	var merged []keyedRel
	seen := make(map[int64]bool)
	// trimMerged keeps peak memory O(limit) instead of O(chunks x limit): once
	// merged holds well past `limit` rows, only the smallest `limit` rowids can
	// still make the final cut, so the rest can be dropped early. A trimmed row
	// re-arriving from a later batch is deduped away again by the final trim.
	trimMerged := func() {
		if limit <= 0 || len(merged) <= limit*2 {
			return
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].id < merged[j].id })
		merged = merged[:limit]
		seen = make(map[int64]bool, limit)
		for i := range merged {
			seen[merged[i].id] = true
		}
	}
	for start := 0; start < len(symbols); start += chunk {
		end := start + chunk
		if end > len(symbols) {
			end = len(symbols)
		}
		batch := symbols[start:end]
		placeholders := make([]string, len(batch))
		for i := range batch {
			placeholders[i] = "?"
		}
		args := make([]any, 0, len(batch)*2+1)
		for _, symbol := range batch {
			args = append(args, symbol.ID)
		}
		for _, symbol := range batch {
			args = append(args, symbol.ID)
		}
		args = append(args, limit)
		inClause := strings.Join(placeholders, ",")
		if err := func() error {
			rows, err := db.Query(`SELECT id, from_id, to_id, type, confidence, reason FROM relations
WHERE from_id IN (`+inClause+`) OR to_id IN (`+inClause+`)
ORDER BY id LIMIT ?`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var record Record
				record.RecordType = "relation"
				if err := rows.Scan(&id, &record.FromID, &record.ToID, &record.Type, &record.Confidence, &record.Reason); err != nil {
					return err
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				merged = append(merged, keyedRel{id: id, rec: record})
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
		trimMerged()
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].id < merged[j].id })
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	relations := make([]Record, len(merged))
	for i := range merged {
		relations[i] = merged[i].rec
	}
	return relations, nil
}

func findRelationsByTypesSQLite(storePath string, relationTypes []string) ([]Record, error) {
	if len(relationTypes) == 0 {
		return nil, nil
	}
	db, err := OpenReadDB(storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	args := make([]any, 0, len(relationTypes))
	placeholders := make([]string, 0, len(relationTypes))
	for _, relationType := range relationTypes {
		args = append(args, relationType)
		placeholders = append(placeholders, "?")
	}
	rows, err := db.Query(`SELECT from_id, to_id, type, confidence, reason FROM relations WHERE type IN (`+strings.Join(placeholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var relations []Record
	for rows.Next() {
		var record Record
		record.RecordType = "relation"
		if err := rows.Scan(&record.FromID, &record.ToID, &record.Type, &record.Confidence, &record.Reason); err != nil {
			return nil, err
		}
		relations = append(relations, record)
	}
	return relations, rows.Err()
}

func traverseImpactSQLite(storePath string, roots []Record, depth, limit int) ([]Record, []Record, error) {
	if len(roots) == 0 {
		return nil, nil, nil
	}
	symbolsByID, err := loadSymbolsByIDSQLite(storePath)
	if err != nil {
		return nil, nil, err
	}
	frontier := map[string]struct{}{}
	seen := map[string]struct{}{}
	var symbols []Record
	for _, root := range roots {
		frontier[root.ID] = struct{}{}
		if _, ok := seen[root.ID]; !ok {
			seen[root.ID] = struct{}{}
			symbols = append(symbols, root)
		}
	}
	var relations []Record
	for level := 0; level < depth && len(frontier) > 0; level++ {
		current := make([]Record, 0, len(frontier))
		for id := range frontier {
			current = append(current, Record{ID: id})
		}
		edges, err := FindRelationsForSymbolsInSQLite(storePath, current, limit*4)
		if err != nil {
			return nil, nil, err
		}
		next := map[string]struct{}{}
		for _, edge := range edges {
			relations = append(relations, edge)
			for _, id := range []string{edge.FromID, edge.ToID} {
				if _, ok := seen[id]; ok {
					continue
				}
				if symbol, ok := symbolsByID[id]; ok && len(symbols) < limit {
					seen[id] = struct{}{}
					next[id] = struct{}{}
					symbols = append(symbols, symbol)
					if len(symbols) >= limit {
						break
					}
				}
			}
		}
		frontier = next
	}
	return symbols, relations, nil
}

func loadSymbolsByIDSQLite(storePath string) (map[string]Record, error) {
	db, err := OpenReadDB(storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version FROM symbols`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	symbols := map[string]Record{}
	for rows.Next() {
		var record Record
		record.RecordType = "symbol"
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
			return nil, err
		}
		symbols[record.ID] = record
	}
	return symbols, rows.Err()
}

// LoadSymbolsByIDsSQLite loads only the requested symbol ids via an
// indexed `id IN (...)` lookup (chunked to stay under SQLite's parameter cap),
// so neighbor materialization is O(fan-out), not O(total symbols).
func LoadSymbolsByIDsSQLite(storePath string, ids []string) (map[string]Record, error) {
	out := map[string]Record{}
	if len(ids) == 0 {
		return out, nil
	}
	db, err := OpenReadDB(storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	const chunk = 900
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		if err := func() error {
			rows, err := db.Query(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version FROM symbols WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var record Record
				record.RecordType = "symbol"
				if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
					return err
				}
				out[record.ID] = record
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func findSymbolsForFilesSQLite(storePath string, files []string, limit int) ([]Record, error) {
	if len(files) == 0 {
		return nil, nil
	}
	db, err := OpenReadDB(storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	args := make([]any, 0, len(files)+1)
	placeholders := make([]string, 0, len(files))
	for _, file := range files {
		args = append(args, file)
		placeholders = append(placeholders, "?")
	}
	args = append(args, limit)
	rows, err := db.Query(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version
FROM symbols WHERE file_path IN (`+strings.Join(placeholders, ",")+`) ORDER BY file_path, start_line LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var symbols []Record
	for rows.Next() {
		var record Record
		record.RecordType = "symbol"
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
			return nil, err
		}
		symbols = append(symbols, record)
	}
	return symbols, rows.Err()
}

// LoadSymbols reads the symbol records from Graph's live store. No
// source at all is the ordinary unbuilt case and stays silently empty; a
// DECLARED source that is not the live graph handle is unreadable and must
// error, so the Sources row flips to "unreadable" instead of rendering a
// healthy empty Code tab.
func LoadSymbols(brainDir string, source *bmanifest.SemanticSource, limit int) ([]Record, error) {
	if source == nil {
		return nil, nil
	}
	storePath, err := ValidateDeclaredStore(brainDir, source)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = -1
	}
	return FindSymbolsInSQLite(storePath, "", limit, 0)
}
