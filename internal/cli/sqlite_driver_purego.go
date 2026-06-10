//go:build !brain_cgo

package cli

// Default build: the pure-Go modernc driver, no cgo. This is the Stage 1b
// fallback path — `go build` with no tags produces the same stack 1a shipped
// (modernc + FTS5 + Model2Vec brute-force cosine), so a cgo-less build remains
// a working (degraded-recall) brain. The cgo stack lives behind the brain_cgo
// tag in sqlite_driver_cgo.go.

import (
	_ "modernc.org/sqlite"
)

// sqliteDriverName is the database/sql driver this build registers.
const sqliteDriverName = "sqlite"

// brainCGOBuild reports whether this binary runs the Stage 1b cgo stack.
const brainCGOBuild = false
