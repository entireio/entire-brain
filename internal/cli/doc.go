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
)

// doc.go is the `doc` source: it indexes the brain's own markdown — the
// agent-synthesized seed summaries plus the copied repo docs under seed/docs/ —
// into retrievable chunks. Facts/history cover session-derived work; docs cover
// the written/designed knowledge sessions don't, and give the brain a way to
// retrieve its own documentation (qmd's core use case). The index is a derived,
// rebuildable artifact, like the history index.

const (
	docDirName       = "docs"
	docIndexFileName = "index.json"
	docIndexPath     = docDirName + "/" + docIndexFileName
	maxDocChunkBytes = 3000 // ~750 tokens, qmd-scale chunks
)

type docSourceManifest struct {
	GeneratedAt time.Time `json:"generated_at"`
	IndexPath   string    `json:"index_path"`
	Records     int       `json:"records"`
	Files       int       `json:"files"`
	Warnings    []string  `json:"warnings,omitempty"`
}

type docIndex struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Records     []docRecord `json:"records"`
}

type docRecord struct {
	ID         string `json:"id"`
	Path       string `json:"path"`    // brain-relative markdown path
	Heading    string `json:"heading"` // first markdown heading within the chunk, if any (empty when the chunk starts mid-section)
	Line       int    `json:"line"`    // 1-based start line in the file
	Text       string `json:"text"`
	Historical bool   `json:"historical,omitempty"` // explicitly superseded reference material; current docs rank first
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
		if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
			warnings = append(warnings, fmt.Sprintf("skip %s: %v", rel, err))
			return nil
		}
		data, rErr := safeReadFile(path, maxSeedDocBytes)
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
	err := withBrainWriteLock(brainDir, func() error {
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
	if err := writeBrainRelativeFileAtomic(brainDir, docIndexPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("write doc index: %w", err)
	}
	source := &docSourceManifest{GeneratedAt: now, IndexPath: docIndexPath, Records: len(records), Files: files, Warnings: warnings}
	// Build the derived BM25 index alongside its truth (best-effort).
	if db, ftsErr := openDocFTSLocked(brainDir, index); ftsErr == nil {
		_ = db.Close()
	}
	manifest, err := loadBrainManifest(brainDir)
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

// newDocEmbedStore is the disk-backed vector cache for doc chunks, mirroring the
// fact embed store: keyed by doc id, headered with the embedder model+dim so a
// model switch triggers a clean rebuild. This is the pure-Go stand-in for
// sqlite-vec — brute-force cosine over a cached set, fast at the brain's scale.
func newDocEmbedStore(brainDir, modelID string, dim int) *embedStore {
	rel := filepath.ToSlash(filepath.Join(docDirName, embedStoreDirName, embedStoreFileName))
	return &embedStore{path: filepath.Join(brainDir, filepath.FromSlash(rel)), brainDir: brainDir, relPath: rel, modelID: modelID, dim: dim}
}

func loadDocIndex(brainDir string) (docIndex, error) {
	var index docIndex
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(docIndexPath)))
	if err != nil {
		return index, err
	}
	return index, json.Unmarshal(data, &index)
}
