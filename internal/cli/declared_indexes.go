package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/docstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// declared_indexes.go cross-checks the retrieval indexes the manifest DECLARES
// against the artifacts on disk.
//
// It is the same defect facts_integrity.go was written for, one layer over:
// sources.docs and sources.history are CLAIMS ("this brain holds a doc index at
// docs/index.json"), every health surface printed the claim as an observation,
// and nothing compared the two. `rm -rf` the brain's docs/ and history/ and
// status went on printing
//
//	instant  + sessions  + seed  + docs  x semantic  + history  ...
//
// with `+ docs` sitting directly above its own warning line saying docs/ was
// absent. Only `semantic` was caught, and only because a freshness probe
// happens to open the store.
//
// The consequences then diverged, which is the part that made it dangerous.
// `search` exited 1 on a raw `lstat …/history: no such file or directory`.
// `vsearch` exited 0 with "no results" and a note blaming missing FACTS — over
// a brain that had answered the same query with six doc hits minutes earlier.
//
// Shape, deliberately copied from factstore.Inspect rather than invented again:
//
//   - The check lives beside the artifact's own reader, not inside it, so the
//     REBUILD path can still run against the damaged brain.
//   - A source the manifest does not declare is healthy, full stop. That is
//     what keeps a brand-new brain reading as "not built" instead of "corrupt",
//     and it is the distinction every surface here is load-bearing on.
//   - One report, read by every surface: the status instant line and the
//     retrieval error path ask the same function and get the same sentence.

// declaredIndexOrder is build order, matching the instant line's.
var declaredIndexOrder = []brainstore.DeclaredIndexSource{brainstore.DeclaredIndexSeed, brainstore.DeclaredIndexDocs, brainstore.DeclaredIndexHistory}

// declaredIndexPath returns the artifact a source declares, or "" when the
// source is not declared at all. A declared source with no path is treated as
// undeclared: there is nothing to check it against, and inventing a default
// would be this file asserting a claim the manifest never made.
func declaredIndexPath(manifest *exportManifest, source brainstore.DeclaredIndexSource) string {
	if manifest == nil || manifest.Sources == nil {
		return ""
	}
	switch source {
	case brainstore.DeclaredIndexSeed:
		if manifest.Sources.Seed == nil {
			return ""
		}
		return manifest.Sources.Seed.SummaryPath
	case brainstore.DeclaredIndexHistory:
		if manifest.Sources.History == nil {
			return ""
		}
		return manifest.Sources.History.IndexPath
	case brainstore.DeclaredIndexDocs:
		if manifest.Sources.Docs == nil {
			return ""
		}
		// The docs source records index_path, but every reader of the doc index
		// opens the constant (see docstore.OpenDeclaredIndex). Report the path that is
		// actually read, so the remedy names the file that is actually missing.
		return docstore.IndexPath
	}
	return ""
}

// inspectDeclaredIndex cross-checks ONE declared source against the store.
//
// An undeclared source returns a healthy result: "this brain has no doc index"
// is not index loss, and reporting it as such would turn every brand-new brain
// into a corrupt one.
func inspectDeclaredIndex(brainDir string, manifest *exportManifest, source brainstore.DeclaredIndexSource) brainstore.DeclaredIndexDefect {
	rel := declaredIndexPath(manifest, source)
	if brainDir == "" || rel == "" {
		return brainstore.DeclaredIndexDefect{}
	}
	var err error
	if source == brainstore.DeclaredIndexHistory {
		// The history index is generation-addressed and has its own path
		// grammar; validateHistoryIndexPath is the reader's own gate, so a path
		// this brain would refuse to READ is not reported as a missing file.
		err = verifyDeclaredHistoryIndex(brainDir, rel)
	} else {
		err = verifyDeclaredBrainFile(brainDir, rel)
	}
	if err == nil {
		return brainstore.DeclaredIndexDefect{}
	}
	return brainstore.DeclaredIndexDefect{
		Source: source,
		Path:   rel,
		Absent: errors.Is(err, fs.ErrNotExist),
		Err:    err,
	}
}

// inspectDeclaredIndexes runs the cross-check over every declared retrieval
// index, in build order. Empty slice when the brain is whole.
func inspectDeclaredIndexes(brainDir string, manifest *exportManifest) []brainstore.DeclaredIndexDefect {
	var defects []brainstore.DeclaredIndexDefect
	for _, source := range declaredIndexOrder {
		if defect := inspectDeclaredIndex(brainDir, manifest, source); !defect.OK() {
			defects = append(defects, defect)
		}
	}
	return defects
}

// inspectBrainDeclaredIndex is inspectDeclaredIndex for callers holding only a
// brain directory. A manifest that will not load is reported as healthy: the
// manifest's own health is a separate check that owns that message, and a read
// path must not answer "your doc index is gone" when what it actually found was
// an unparseable manifest.
func inspectBrainDeclaredIndex(brainDir string, source brainstore.DeclaredIndexSource) brainstore.DeclaredIndexDefect {
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return brainstore.DeclaredIndexDefect{}
	}
	return inspectDeclaredIndex(brainDir, manifest, source)
}

// verifyDeclaredBrainFile reports whether a brain-relative artifact is present
// as a regular file, through the same containment guard every other reader in
// this package uses. It opens nothing: presence is the axis this check is
// about, and the readers themselves own content integrity (digests, bounded
// reads, open-alias rejection).
func verifyDeclaredBrainFile(brainDir, rel string) error {
	clean := filepath.Clean(filepath.FromSlash(rel))
	// rejectSymlinkPathComponents performs the containment and symlink checks
	// and lstats every component, so a missing directory ANYWHERE on the path
	// surfaces here as fs.ErrNotExist — which is exactly the condition being
	// reported, and is why `rm -rf history/` is caught and not only a deleted
	// index.json.
	if err := base.RejectSymlinkPathComponents(brainDir, clean); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(brainDir, clean))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel)
	}
	return nil
}

// verifyDeclaredHistoryIndex is verifyDeclaredBrainFile behind the history
// index's own path grammar.
func verifyDeclaredHistoryIndex(brainDir, indexPath string) error {
	clean, err := historystore.ValidateIndexPath(indexPath)
	if err != nil {
		return err
	}
	return verifyDeclaredBrainFile(brainDir, clean)
}
