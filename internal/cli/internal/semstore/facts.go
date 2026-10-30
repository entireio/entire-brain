package semstore

import (
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// NeighborCandidateIDs returns the relation endpoint ids that are not already in
// symbols — the only ids neighbor materialization needs to load, so resolution
// stays proportional to the fan-out rather than the whole symbol table. Ordered
// (relations order) and deduped.
func NeighborCandidateIDs(symbols, relations []Record) []string {
	inSymbols := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		inSymbols[symbol.ID] = struct{}{}
	}
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := inSymbols[id]; ok {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, relation := range relations {
		add(relation.FromID)
		add(relation.ToID)
	}
	return ids
}

// ResolveContextNeighbors materializes the relation endpoints that are not
// already in symbols into full records, so an agent gets "who calls this / what
// this calls" without a follow-up query per bare from_id/to_id. symbolsByID need
// only contain the candidate endpoints (see NeighborCandidateIDs); ordering
// follows the relations and the result is capped at limit to bound fan-out on a
// hot symbol.
func ResolveContextNeighbors(symbols, relations []Record, symbolsByID map[string]Record, limit int) []Record {
	if limit <= 0 || len(relations) == 0 || len(symbolsByID) == 0 {
		return nil
	}
	inSymbols := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		inSymbols[symbol.ID] = struct{}{}
	}
	seen := map[string]struct{}{}
	var neighbors []Record
	// add materializes one endpoint; it returns true once limit is reached so the
	// caller can stop. Handling FromID/ToID directly avoids a per-relation slice
	// allocation on this hot path.
	add := func(id string) bool {
		if id == "" {
			return false
		}
		if _, ok := inSymbols[id]; ok {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		record, ok := symbolsByID[id]
		if !ok {
			return false
		}
		seen[id] = struct{}{}
		neighbors = append(neighbors, record)
		return len(neighbors) >= limit
	}
	for _, relation := range relations {
		if add(relation.FromID) || add(relation.ToID) {
			return neighbors
		}
	}
	return neighbors
}

func ContextFacts(brainDir string, source *bmanifest.SemanticSource, query string, limit, offset int) ([]Record, []Record, []Record, error) {
	storePath, err := ValidateDeclaredStore(brainDir, source)
	if err != nil {
		return nil, nil, nil, err
	}
	symbols, err := FindSymbolsInSQLite(storePath, query, limit, offset)
	if err != nil {
		return nil, nil, nil, err
	}
	relations, err := FindRelationsForSymbolsInSQLite(storePath, symbols, limit*4)
	if err != nil {
		return nil, nil, nil, err
	}
	var neighbors []Record
	if ids := NeighborCandidateIDs(symbols, relations); len(ids) > 0 {
		symbolsByID, err := LoadSymbolsByIDsSQLite(storePath, ids)
		if err != nil {
			return nil, nil, nil, err
		}
		neighbors = ResolveContextNeighbors(symbols, relations, symbolsByID, limit)
	}
	return symbols, relations, neighbors, nil
}

func ImpactFacts(brainDir string, source *bmanifest.SemanticSource, query string, depth, limit int) ([]Record, []Record, []Record, error) {
	storePath, err := ValidateDeclaredStore(brainDir, source)
	if err != nil {
		return nil, nil, nil, err
	}
	roots, err := FindSymbolsInSQLite(storePath, query, limit, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	symbols, relations, err := traverseImpactSQLite(storePath, roots, depth, limit)
	return roots, symbols, relations, err
}

func SymbolsForFiles(brainDir string, source *bmanifest.SemanticSource, files []string, limit int) ([]Record, error) {
	storePath, err := ValidateDeclaredStore(brainDir, source)
	if err != nil {
		return nil, err
	}
	return findSymbolsForFilesSQLite(storePath, files, limit)
}

func GraphFacts(brainDir string, source *bmanifest.SemanticSource, query string, depth, limit int) ([]Record, []Record, []Record, map[string]Record, error) {
	storePath, err := ValidateDeclaredStore(brainDir, source)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	roots, err := FindSymbolsInSQLite(storePath, query, limit, 0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	symbols, relations, err := traverseImpactSQLite(storePath, roots, depth, limit)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	symbolsByID, err := loadSymbolsByIDSQLite(storePath)
	return roots, symbols, relations, symbolsByID, err
}

func LoadBoundaryRecords(brainDir string, source *bmanifest.SemanticSource, relationTypes []string) (map[string]Record, []Record, error) {
	storePath, err := ValidateDeclaredStore(brainDir, source)
	if err != nil {
		return nil, nil, err
	}
	symbolsByID, err := loadSymbolsByIDSQLite(storePath)
	if err != nil {
		return nil, nil, err
	}
	relations, err := findRelationsByTypesSQLite(storePath, relationTypes)
	return symbolsByID, relations, err
}
