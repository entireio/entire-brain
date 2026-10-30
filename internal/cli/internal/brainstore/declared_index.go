package brainstore

import (
	"fmt"
	"os"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// DeclaredIndexSource names which declared artifact is being checked. The ids
// are the component ids `status` already prints on its instant line, so a
// source reported as broken here and a source reported as failed there are
// visibly the same thing.
type DeclaredIndexSource string

const (
	DeclaredIndexSeed    DeclaredIndexSource = "seed"
	DeclaredIndexHistory DeclaredIndexSource = "history"
	DeclaredIndexDocs    DeclaredIndexSource = "docs"
)

// Label names the source the way the setup lines say it; the cli status
// surface derives its matching entries from here so the vocabulary has one
// home.
func (s DeclaredIndexSource) Label() string {
	switch s {
	case DeclaredIndexSeed:
		return "repository context"
	case DeclaredIndexHistory:
		return "history index"
	case DeclaredIndexDocs:
		return "doc index"
	}
	return string(s)
}

// DeclaredIndexRepairHint is the repair. Rebuilding is the whole answer here —
// unlike a lossy fact store, nothing has to be re-distilled, because every one
// of these artifacts is derived deterministically from sources the brain still
// holds.
func DeclaredIndexRepairHint() string {
	return "run `" + base.BrainInvocation(os.LookupEnv, "refresh") + "`"
}

// DeclaredIndexDefect is one declared artifact the store cannot produce. The
// zero value is a healthy source, so callers may use it unconditionally.
type DeclaredIndexDefect struct {
	Source DeclaredIndexSource `json:"source"`
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
func (d DeclaredIndexDefect) OK() bool { return d.Source == "" }

// Warning renders the single caller-facing line, or "" when nothing is wrong.
// It is written to work as a status detail AND as an error message, because a
// reader hitting this from `status` and a reader hitting it from `vsearch` need
// the identical sentence.
func (d DeclaredIndexDefect) Warning() string {
	if d.OK() {
		return ""
	}
	// Label is the one vocabulary: a source named here and the
	// same source named on the instant line must read as the same thing.
	label := d.Source.Label()
	if d.Absent {
		return fmt.Sprintf("%s is declared in the manifest but %s is not on disk — %s",
			label, d.Path, DeclaredIndexRepairHint())
	}
	return fmt.Sprintf("%s declared in the manifest cannot be read: %v — %s",
		label, d.Err, DeclaredIndexRepairHint())
}

// Error lets a defect be returned straight from a read path.
func (d DeclaredIndexDefect) Error() string { return d.Warning() }
