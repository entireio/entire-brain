package cli

import (
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// testLegacyFactsRel is the pre-flat branch-store layout, kept as a FIXTURE
// shape: the legacy fold and the migration read any facts/<dir>/facts.ndjson,
// so tests that simulate an old binary's writes use this path.
func testLegacyFactsRel(branch string) string {
	dir := strings.ReplaceAll(branch, "/", "-") + "-legacy00"
	return filepath.ToSlash(filepath.Join(factstore.DirName, dir, "facts.ndjson"))
}

// testFlatStorePath is where the current code persists facts.
func testFlatStorePath(brainDir string) string {
	return filepath.Join(brainDir, factstore.DirName, "store.json")
}
