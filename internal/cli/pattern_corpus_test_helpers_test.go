package cli

import (
	"database/sql"
	"fmt"
	"os"
)

// openPatternCorpusMutableDB is test-only setup access. Production mutations
// always clone into private staging and publish through a pinned os.Root.
func openPatternCorpusMutableDB(brainDir string) (*sql.DB, error) {
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPath, "pattern corpus")
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("pattern corpus not found: %w", os.ErrNotExist)
	}
	path, err := prepareBrainRelativeSQLiteFile(brainDir, patternCorpusPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	for _, stmt := range patternCorpusSchema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}
