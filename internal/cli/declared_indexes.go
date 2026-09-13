package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// Shape, deliberately copied from inspectFactStore rather than invented again:
//
//   - The check lives beside the artifact's own reader, not inside it, so the
//     REBUILD path can still run against the damaged brain.
//   - A source the manifest does not declare is healthy, full stop. That is
//     what keeps a brand-new brain reading as "not built" instead of "corrupt",
//     and it is the distinction every surface here is load-bearing on.
//   - One report, read by every surface: the status instant line and the
//     retrieval error path ask the same function and get the same sentence.

// declaredIndexSource names which declared artifact is being checked. The ids
// are the component ids `status` already prints on its instant line, so a
// source reported as broken here and a source reported as failed there are
// visibly the same thing.
type declaredIndexSource string

const (
	declaredIndexSeed    declaredIndexSource = brainComponentSeed
	declaredIndexHistory declaredIndexSource = brainComponentHistory
	declaredIndexDocs    declaredIndexSource = brainComponentDocs
)

// declaredIndexOrder is build order, matching the instant line's.
var declaredIndexOrder = []declaredIndexSource{declaredIndexSeed, declaredIndexDocs, declaredIndexHistory}

// declaredIndexRepairHint is the repair. Rebuilding is the whole answer here —
// unlike a lossy fact store, nothing has to be re-distilled, because every one
// of these artifacts is derived deterministically from sources the brain still
// holds.
const declaredIndexRepairHint = "run `entire brain refresh`"

// declaredIndexDefect is one declared artifact the store cannot produce. The
// zero value is a healthy source, so callers may use it unconditionally.
type declaredIndexDefect struct {
	Source declaredIndexSource `json:"source"`
	// Path is the brain-relative artifact the manifest names. Naming it is the
	// difference between a repair the reader can aim and a raw internal lstat.
	Path string `json:"path,omitempty"`
	// Absent separates the two situations a reader reacts to differently: the
	// file is gone (something deleted it) versus the file is there and will not
	// open (permissions, a symlink swap, a directory where a file belongs).
	Absent bool `json:"absent,omitempty"`
	// Err is the underlying failure, kept for anyone debugging a real fault.
	Err error `json:"-"`
}

// OK reports a source that can produce what the manifest declares.
func (d declaredIndexDefect) OK() bool { return d.Source == "" }

// Warning renders the single caller-facing line, or "" when nothing is wrong.
// It is written to work as a status detail AND as an error message, because a
// reader hitting this from `status` and a reader hitting it from `vsearch` need
// the identical sentence.
func (d declaredIndexDefect) Warning() string {
	if d.OK() {
		return ""
	}
	// setupComponentLabels is the one vocabulary: a source named here and the
	// same source named on the instant line must read as the same thing.
	label := setupComponentLabel(string(d.Source))
	if d.Absent {
		return fmt.Sprintf("%s is declared in the manifest but %s is not on disk — %s",
			label, d.Path, declaredIndexRepairHint)
	}
	return fmt.Sprintf("%s declared in the manifest cannot be read: %v — %s",
		label, d.Err, declaredIndexRepairHint)
}

// Error lets a defect be returned straight from a read path.
func (d declaredIndexDefect) Error() string { return d.Warning() }

// declaredIndexPath returns the artifact a source declares, or "" when the
// source is not declared at all. A declared source with no path is treated as
// undeclared: there is nothing to check it against, and inventing a default
// would be this file asserting a claim the manifest never made.
func declaredIndexPath(manifest *exportManifest, source declaredIndexSource) string {
	if manifest == nil || manifest.Sources == nil {
		return ""
	}
	switch source {
	case declaredIndexSeed:
		if manifest.Sources.Seed == nil {
			return ""
		}
		return manifest.Sources.Seed.SummaryPath
	case declaredIndexHistory:
		if manifest.Sources.History == nil {
			return ""
		}
		return manifest.Sources.History.IndexPath
	case declaredIndexDocs:
		if manifest.Sources.Docs == nil {
			return ""
		}
		// The docs source records index_path, but every reader of the doc index
		// opens the constant (see openDeclaredDocIndex). Report the path that is
		// actually read, so the remedy names the file that is actually missing.
		return docIndexPath
	}
	return ""
}

// inspectDeclaredIndex cross-checks ONE declared source against the store.
//
// An undeclared source returns a healthy result: "this brain has no doc index"
// is not index loss, and reporting it as such would turn every brand-new brain
// into a corrupt one.
func inspectDeclaredIndex(brainDir string, manifest *exportManifest, source declaredIndexSource) declaredIndexDefect {
	rel := declaredIndexPath(manifest, source)
	if brainDir == "" || rel == "" {
		return declaredIndexDefect{}
	}
	var err error
	if source == declaredIndexHistory {
		// The history index is generation-addressed and has its own path
		// grammar; validateHistoryIndexPath is the reader's own gate, so a path
		// this brain would refuse to READ is not reported as a missing file.
		err = verifyDeclaredHistoryIndex(brainDir, rel)
	} else {
		err = verifyDeclaredBrainFile(brainDir, rel)
	}
	if err == nil {
		return declaredIndexDefect{}
	}
	return declaredIndexDefect{
		Source: source,
		Path:   rel,
		Absent: errors.Is(err, fs.ErrNotExist),
		Err:    err,
	}
}

// inspectDeclaredIndexes runs the cross-check over every declared retrieval
// index, in build order. Empty slice when the brain is whole.
func inspectDeclaredIndexes(brainDir string, manifest *exportManifest) []declaredIndexDefect {
	var defects []declaredIndexDefect
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
func inspectBrainDeclaredIndex(brainDir string, source declaredIndexSource) declaredIndexDefect {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return declaredIndexDefect{}
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
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
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
	clean, err := validateHistoryIndexPath(indexPath)
	if err != nil {
		return err
	}
	return verifyDeclaredBrainFile(brainDir, clean)
}
