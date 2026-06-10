//go:build brain_cgo

package cli

// Stage 1b cgo stack (alignment plan): the brain's SQLite engine converges on
// the qmd/Librarian-local stack — mattn/go-sqlite3 with the sqlite-vec
// extension compiled in — so the two local tools can share one driver, one
// schema, and (eventually) one store. Selected with
//
//	go build -tags "brain_cgo sqlite_fts5"
//
// brain_cgo implies cgo (mattn does not build without it) and MUST be paired
// with mattn's sqlite_fts5 feature tag: the history/facts/doc indexes are FTS5
// virtual tables, and mattn compiles FTS5 in only under that tag (modernc
// ships it unconditionally). TestBrainCGOFTS5Compiled guards against a
// misconfigured build. The default (no tags) build keeps the pure-Go modernc
// driver — the cgo-less fallback the Stage 1b exit gate requires.

import (
	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

// sqliteDriverName is the database/sql driver this build registers.
const sqliteDriverName = "sqlite3"

// brainCGOBuild reports whether this binary runs the Stage 1b cgo stack.
const brainCGOBuild = true

func init() {
	// Register sqlite-vec on every future connection (sqlite3_auto_extension),
	// so vec0 virtual tables are available wherever the driver is opened.
	sqlitevec.Auto()
}
