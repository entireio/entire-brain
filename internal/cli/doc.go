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
	docDirName        = "docs"
	docIndexFileName  = "index.json"
	docIndexPath      = docDirName + "/" + docIndexFileName
	maxDocChunkBytes  = 3000 // ~750 tokens, qmd-scale chunks
	docSummaryMaxSize = 700
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
	ID      string `json:"id"`
	Path    string `json:"path"`    // brain-relative markdown path
	Heading string `json:"heading"` // nearest section heading, for context
	Line    int    `json:"line"`    // 1-based start line in the file
	Text    string `json:"text"`
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
func loadDocRecordsFromSeed(brainDir string) ([]docRecord, int, error) {
	seedDir := filepath.Join(brainDir, seedDirName)
	if _, err := os.Stat(seedDir); err != nil {
		return nil, 0, nil // no seed yet; not an error
	}
	var records []docRecord
	files := 0
	err := filepath.WalkDir(seedDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".md") {
			return nil
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(brainDir, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		files++
		for _, c := range chunkTranscript(string(data), maxDocChunkBytes) {
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			records = append(records, docRecord{
				ID:      docRecordID(rel, c.StartLine, text),
				Path:    rel,
				Heading: firstHeading(text),
				Line:    c.StartLine,
				Text:    text,
			})
		}
		return nil
	})
	return records, files, err
}

// writeDocIndexAndSource builds and persists the doc index + manifest source from
// the seed markdown. It mirrors writeBrainHistoryIndexAndSource.
func writeDocIndexAndSource(brainDir string, now time.Time) (*docSourceManifest, error) {
	records, files, err := loadDocRecordsFromSeed(brainDir)
	if err != nil {
		return nil, err
	}
	index := docIndex{GeneratedAt: now, Records: records}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(brainDir, filepath.FromSlash(docIndexPath)), data, 0o600); err != nil {
		return nil, fmt.Errorf("write doc index: %w", err)
	}
	source := &docSourceManifest{GeneratedAt: now, IndexPath: docIndexPath, Records: len(records), Files: files}
	// Build the derived BM25 index alongside its truth (best-effort).
	if db, ftsErr := openDocFTS(brainDir, index); ftsErr == nil {
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
	dir := filepath.Join(brainDir, docDirName, embedStoreDirName)
	return &embedStore{path: filepath.Join(dir, embedStoreFileName), modelID: modelID, dim: dim}
}

func loadDocIndex(brainDir string) (docIndex, error) {
	var index docIndex
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(docIndexPath)))
	if err != nil {
		return index, err
	}
	return index, json.Unmarshal(data, &index)
}
