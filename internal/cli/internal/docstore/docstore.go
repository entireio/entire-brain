// Package docstore is the read layer over the brain's doc index: the `doc`
// retrieval source's persisted chunks. The index is a derived, rebuildable
// artifact, like the history index; building it stays with the cli seed
// pipeline, this package only validates and loads what a build declared.
package docstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

const (
	DirName       = "docs"
	IndexFileName = "index.json"
	IndexPath     = DirName + "/" + IndexFileName
)

// A doc index aggregates multiple seed documents, so use the general 64 MiB
// ceiling rather than the smaller manifest limit. Variable for boundary tests.
var MaxIndexBytes int64 = base.DefaultMaxReadBytes

type Index struct {
	GeneratedAt time.Time `json:"generated_at"`
	Records     []Record  `json:"records"`
}

type Record struct {
	ID         string `json:"id"`
	Path       string `json:"path"`    // brain-relative markdown path
	Heading    string `json:"heading"` // first markdown heading within the chunk, if any (empty when the chunk starts mid-section)
	Line       int    `json:"line"`    // 1-based start line in the file
	Text       string `json:"text"`
	Historical bool   `json:"historical,omitempty"` // explicitly superseded reference material; current docs rank first
}

// ScoredRecord pairs a doc record with its query score for ranking.
type ScoredRecord struct {
	Record Record
	Score  int
}

// VerifyDeclaredIndex reports why the docs index a manifest declares cannot
// be read. It probes exactly the file Load opens, so a "docs: ok"
// status is a statement about the bytes retrieval will actually rank, not about
// a manifest field. Retrieval treats a missing index as an empty docs layer
// (os.IsNotExist is skipped, not raised), so without this check a deleted or
// unreadable index is indistinguishable from "nothing matched".
func VerifyDeclaredIndex(brainDir string) error {
	f, err := OpenDeclaredIndex(brainDir)
	if err != nil {
		return err
	}
	return f.Close()
}

// Both diagnostics and queries must validate the descriptor they actually use.
func OpenDeclaredIndex(brainDir string) (*os.File, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(IndexPath))
	if err := base.RejectExistingSymlinkPathComponents(brainDir, IndexPath); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", IndexPath)
	}
	// Nonblocking/no-follow open also covers a swap after the path checks.
	f, err := os.OpenFile(path, os.O_RDONLY|base.FileLockOpenFlags()|base.MemoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		f.Close()
		return nil, fmt.Errorf("%s changed while opening", IndexPath)
	}
	if opened.Size() > MaxIndexBytes {
		f.Close()
		return nil, &base.ReadBoundExceededError{Source: IndexPath, Max: MaxIndexBytes}
	}
	if err := base.RejectOpenFileAlias(path, f, IndexPath); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func Load(brainDir string) (Index, error) {
	var index Index
	f, err := OpenDeclaredIndex(brainDir)
	if err != nil {
		return index, err
	}
	defer f.Close()
	data, err := base.SafeReadAll(f, MaxIndexBytes, IndexPath)
	if err != nil {
		return index, err
	}
	return index, json.Unmarshal(data, &index)
}
