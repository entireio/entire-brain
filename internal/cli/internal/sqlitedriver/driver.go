// Package sqlitedriver registers the SQLite backend shared by CLI stores and tests.
package sqlitedriver

import (
	_ "modernc.org/sqlite"
)

// The brain's SQLite driver is the pure-Go modernc build, which ships FTS5
// unconditionally — every index (history, facts, docs) runs on it. The
// brain_cgo variant (mattn/go-sqlite3 + sqlite-vec) existed for the Stage 1b
// vector store and the planned qmd/Librarian shared-store convergence; both
// died with the semantic-retrieval removal (trail #278), so the tag and the C
// toolchain requirement were retired with it. If a shared mattn stack returns,
// reintroduce the tag here with the FTS5 pairing guard it used to carry.

// Name is the database/sql driver this build registers.
const Name = "sqlite"

// BrainCGO is retained for the capabilities report (build.brain_cgo),
// which committed agent-guides parse; it is permanently false.
const BrainCGO = false
