package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/docstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// doc.go is the `doc` source: it indexes the brain's own markdown — the
// agent-synthesized seed summaries plus the copied repo docs under seed/docs/ —
// into retrievable chunks. Facts/history cover session-derived work; docs cover
// the written/designed knowledge sessions don't, and give the brain a way to
// retrieve its own documentation (qmd's core use case). The index is a derived,
// rebuildable artifact, like the history index. The read layer (index schema,
// validated open, load) lives in docstore; this file is the build/write side.

const maxDocChunkBytes = 3000 // ~750 tokens, qmd-scale chunks

// Historical documents are untrusted context, not forbidden context. Apply a
// bounded relevance discount so equally relevant current material wins while a
// much stronger historical match remains discoverable and carries its explicit
// verification caveat.
func docTrustAdjustedScore(score float64, historical bool) float64 {
	if historical {
		if score < 0 {
			return score / 0.85
		}
		return score * 0.85
	}
	return score
}

func docRecordID(path string, line int, text string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", path, line, text)))
	return hex.EncodeToString(sum[:])[:12]
}

// firstHeading returns the first markdown heading in a chunk (without the leading
// #s), so a retrieved chunk can be labeled with its section.
func firstHeading(text string) string {
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			return strings.TrimSpace(strings.TrimLeft(t, "#"))
		}
	}
	return ""
}

// loadDocRecordsFromSeed scans every markdown file under brain/seed and chunks it
// into doc records. Size-based chunking (qmd-style) keeps it simple and robust.
// Per-file walk/read failures are collected as warnings (not hard errors, so one
// unreadable file doesn't lose the whole index) and surfaced via the manifest.
func loadDocRecordsFromSeed(brainDir string) (records []docRecord, files int, warnings []string, err error) {
	seedDir := filepath.Join(brainDir, seedDirName)
	info, statErr := os.Lstat(seedDir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, 0, nil, nil // no seed yet; not an error
		}
		return nil, 0, nil, statErr // a real stat failure (e.g. permissions) must not look like an empty index
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, []string{"skip seed: path component must not be a symlink: seed"}, nil
	}
	relTo := func(path string) string {
		if rel, relErr := filepath.Rel(brainDir, path); relErr == nil {
			return filepath.ToSlash(rel)
		}
		return path
	}
	err = filepath.WalkDir(seedDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			warnings = append(warnings, fmt.Sprintf("walk %s: %v", relTo(path), walkErr))
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".md") {
			return nil
		}
		rel := relTo(path)
		clean := filepath.Clean(filepath.FromSlash(rel))
		if err := base.RejectSymlinkPathComponents(brainDir, clean); err != nil {
			warnings = append(warnings, fmt.Sprintf("skip %s: %v", rel, err))
			return nil
		}
		data, rErr := base.SafeReadFile(path, base.MaxSeedDocBytes)
		if rErr != nil {
			warnings = append(warnings, fmt.Sprintf("read %s: %v", rel, rErr))
			return nil
		}
		files++
		historical := docFileHistorical(rel, string(data))
		for _, c := range chunkLines(string(data), maxDocChunkBytes, false) {
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
			// Trim only the trailing newline the chunker appends — keep leading
			// indentation so an indented code block / nested list that a chunk starts
			// inside isn't corrupted (and the hashed id stays faithful to the source).
			text := strings.TrimRight(c.Text, "\n")
			records = append(records, docRecord{
				ID:         docRecordID(rel, c.StartLine, text),
				Path:       rel,
				Heading:    firstHeading(text),
				Line:       c.StartLine,
				Text:       text,
				Historical: historical,
			})
		}
		return nil
	})
	return records, files, warnings, err
}

func docFileHistorical(path, text string) bool {
	lowerPath := "/" + strings.ToLower(filepath.ToSlash(path)) + "/"
	for _, marker := range []string{"/archive/", "/historical/", "/outdated/", "/deprecated/"} {
		if strings.Contains(lowerPath, marker) {
			return true
		}
	}
	markerText := strings.ToLower(text)
	if len(markerText) > 2000 {
		markerText = markerText[:2000]
	}
	return strings.Contains(markerText, "<!-- entire-brain-status: historical -->") ||
		strings.Contains(markerText, "<!-- entire-brain-status: superseded -->")
}

// writeDocIndexAndSource builds and persists the doc index + manifest source from
// the seed markdown. It mirrors writeBrainHistoryIndexAndSource.
func writeDocIndexAndSource(brainDir string, now time.Time) (*docSourceManifest, error) {
	var source *docSourceManifest
	err := brainstore.WithBrainWriteLock(brainDir, func() error {
		var runErr error
		source, runErr = writeDocIndexAndSourceLocked(brainDir, now)
		return runErr
	})
	return source, err
}

func writeDocIndexAndSourceLocked(brainDir string, now time.Time) (*docSourceManifest, error) {
	records, files, warnings, err := loadDocRecordsFromSeed(brainDir)
	if err != nil {
		return nil, err
	}
	index := docIndex{GeneratedAt: now, Records: records}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if int64(len(data)) > docstore.MaxIndexBytes {
		return nil, &readBoundExceededError{Source: docstore.IndexPath, Max: docstore.MaxIndexBytes}
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, docstore.IndexPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("write doc index: %w", err)
	}
	source := &docSourceManifest{GeneratedAt: now, IndexPath: docstore.IndexPath, Records: len(records), Files: files, Warnings: warnings}
	// Build the derived BM25 index alongside its truth (best-effort).
	if db, ftsErr := openDocFTSLocked(brainDir, index); ftsErr == nil {
		_ = db.Close()
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.Docs = source
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = now
	}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		return nil, err
	}
	return source, nil
}
