// Package historystore holds the history INDEX/RECORD read layer: the index
// and record types, the hardened checked loaders for the generation-addressed
// history index, and the path grammar/validation those loaders share with the
// projection build pipeline that stays in package cli.
package historystore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

const (
	IndexFileName  = "index.json"
	IndexPath      = brainstore.HistoryDirName + "/" + IndexFileName
	GenerationsDir = brainstore.HistoryDirName + "/generations"
	StagingDir     = brainstore.HistoryDirName + "/staging"
)

type Index struct {
	GeneratedAt time.Time `json:"generated_at"`
	Records     []Record  `json:"records"`
	Warnings    []string  `json:"warnings,omitempty"`
	// StorageIdentity is the manifest-addressed immutable generation path when
	// this index was loaded from one. It is not serialized into the index; local
	// accelerators use it to distinguish equal-time/equal-count generations.
	StorageIdentity string `json:"-"`
	// ContentIdentity hashes the bounded bytes read from disk. It detects local
	// corruption/tampering within an otherwise immutable generation path.
	ContentIdentity string `json:"-"`
	// RecordsFingerprint is populated after build/load validation so the FTS
	// freshness check stays O(1). Any caller that replaces Records must clear it.
	RecordsFingerprint string `json:"-"`
}

type Record struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Branch  string   `json:"branch,omitempty"`
	Path    string   `json:"path"`
	Line    int      `json:"line"`
	Summary string   `json:"summary"`
	Terms   []string `json:"terms,omitempty"`

	// Experimental conversation-exchange extension (kind "exchange", see
	// conversation.go). All fields are additive and omitted for classic
	// records, so the established history JSON is byte-identical for them.
	// Summary holds the bounded deterministic search projection; the only
	// conversation body stored in the index; full text lives only in the
	// exported transcript and is re-parsed by get.
	SessionID   string `json:"session_id,omitempty"`
	Agent       string `json:"agent,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"` // RFC3339 session time
	EndLine     int    `json:"end_line,omitempty"`   // inclusive 1-based range end
	TurnOrdinal int    `json:"turn_ordinal,omitempty"`
	// RangeIncomplete is set when the exact source range could not be proven
	// (range_complete=false in the contract; inverted so omitempty works).
	RangeIncomplete     bool     `json:"range_incomplete,omitempty"`
	ContentRole         string   `json:"content_role,omitempty"` // "historical_evidence"
	ProjectionTruncated bool     `json:"projection_truncated,omitempty"`
	SourceDigest        string   `json:"source_digest,omitempty"` // transcript digest at extraction
	RequestDigest       string   `json:"request_digest,omitempty"`
	IdentityDegraded    bool     `json:"identity_degraded,omitempty"`
	ToolNames           []string `json:"tool_names,omitempty"`
}

// RecordsFingerprint is the content identity shared by history/index.json
// and its derived indexes. It covers every record field in stable record order,
// while deliberately excluding generation time and warnings: neither changes
// retrieval content. Empty and nil term slices have the same JSON representation
// in index.json (omitempty), so canonicalize them to the same identity here too.
func RecordsFingerprint(records []Record) string {
	h := sha256.New()
	_, _ = io.WriteString(h, "entire-brain/history-records/v1\x00")
	_, _ = io.WriteString(h, strconv.Itoa(len(records))+"\x00")
	for _, record := range records {
		if len(record.Terms) == 0 {
			record.Terms = nil
		}
		data, err := json.Marshal(record)
		if err != nil {
			return ""
		}
		_, _ = io.WriteString(h, strconv.Itoa(len(data))+":")
		_, _ = h.Write(data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func IndexBytesFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ValidSHA256(value string) bool {
	raw, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return strings.HasPrefix(value, "sha256:") && err == nil && len(raw) == 32
}

func LoadIndex(brainDir string, source *bmanifest.HistorySource) (Index, error) {
	index, err := loadIndexOnce(brainDir, source)
	if err == nil || source == nil || !ValidGenerationArtifactPath(source.IndexPath, IndexFileName) {
		return index, err
	}

	// A reader may have loaded the previous manifest immediately before a
	// writer committed and pruned that generation. Resolve the manifest again
	// and retry once only when it names a different index. A corrupt current
	// generation is never hidden by this race recovery path.
	manifest, manifestErr := bmanifest.Load(brainDir)
	if manifestErr != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		return Index{}, err
	}
	current := manifest.Sources.History
	if current.IndexPath == source.IndexPath {
		return Index{}, err
	}
	retried, retryErr := loadIndexOnce(brainDir, current)
	if retryErr != nil {
		return Index{}, fmt.Errorf("history generation changed while reading; current generation is also unreadable: %w", retryErr)
	}
	return retried, nil
}

// readIndexBytes performs the hardened read: canonical path, no
// symlinked components, regular file, no open-alias, bounded read, and (for a
// generation-addressed source) the manifest content digest. It returns the
// exact bytes that were verified so hashing and parsing can never straddle two
// generations.
func readIndexBytes(brainDir string, source *bmanifest.HistorySource) ([]byte, string, bool, error) {
	if source == nil || source.IndexPath == "" {
		return nil, "", false, errors.New("history index missing; run `" + clibase.BrainInvocation(os.LookupEnv, "refresh") + "`")
	}
	generationAddressed := ValidGenerationArtifactPath(source.IndexPath, IndexFileName)
	if generationAddressed && source.IndexDigest == "" {
		return nil, "", false, fmt.Errorf("%s: generation-addressed history index is missing its manifest content digest; run `"+clibase.BrainInvocation(os.LookupEnv, "refresh")+"`", brainstore.MigrationRequiredCode)
	}
	clean, err := ValidateIndexPath(source.IndexPath)
	if err != nil {
		return nil, "", false, err
	}
	if err := clibase.RejectSymlinkPathComponents(brainDir, clean); err != nil {
		return nil, "", false, IndexReadError(source, err)
	}
	path := filepath.Join(brainDir, clean)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", false, IndexReadError(source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", false, fmt.Errorf("%s: history index must be a regular file and must not be a symlink: %s", brainstore.StateCorruptCode, source.IndexPath)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|clibase.FileLockOpenFlags()|clibase.MemoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, "", false, err
	}
	defer f.Close()
	if err := clibase.RejectOpenFileAlias(path, f, "history index"); err != nil {
		return nil, "", false, fmt.Errorf("%s: %w", brainstore.StateCorruptCode, HardlinkRepairHint(err))
	}
	data, err := clibase.SafeReadAll(f, clibase.SemanticSnapshotMaxBytes(), path)
	if err != nil {
		return nil, "", false, err
	}
	contentSum := sha256.Sum256(data)
	contentDigest := "sha256:" + hex.EncodeToString(contentSum[:])
	if source.IndexDigest != "" {
		if !brainstore.ValidSHA256Identity(source.IndexDigest) {
			return nil, "", false, fmt.Errorf("%s: history index digest in manifest is invalid", brainstore.StateCorruptCode)
		}
		if source.IndexDigest != contentDigest {
			return nil, "", false, fmt.Errorf("%s: history index digest does not match the manifest commit", brainstore.StateCorruptCode)
		}
	}
	return data, contentDigest, generationAddressed, nil
}

// IndexReadError names the one condition the hardened read path could
// otherwise only express as a raw filesystem error.
//
// The guard lstats every path component, so deleting the brain's history/
// directory surfaced as `load history index: lstat <brainDir>/history: no such
// file or directory` — an internal path, no condition, no remedy. The nil-source
// case three lines above already says "history index missing; run `entire brain
// refresh`"; a DECLARED index that is not on disk is the same sentence with the
// artifact named. Every other failure keeps its own words.
func IndexReadError(source *bmanifest.HistorySource, err error) error {
	if errors.Is(err, clibase.ErrFileHardlinked) {
		return HardlinkRepairHint(err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return brainstore.DeclaredIndexDefect{Source: brainstore.DeclaredIndexHistory, Path: source.IndexPath, Absent: true, Err: err}
}

// HardlinkRepairHint names the remedy for an externally hardlinked
// history artifact. Backup and bench tooling hardlink-copies state trees, the
// nlink>1 guard then refuses every read, and that silently drops history
// context from every brief, recall, and query. The canonical sessions are
// untouched, so one deterministic rebuild is always the fix.
func HardlinkRepairHint(err error) error {
	if err == nil || !errors.Is(err, clibase.ErrFileHardlinked) {
		return err
	}
	return fmt.Errorf("%w — history context is dropped from briefs and recall until the projection is rebuilt; run `%s`", err, clibase.BrainInvocation(os.LookupEnv, "memory repair"))
}

func loadIndexOnce(brainDir string, source *bmanifest.HistorySource) (Index, error) {
	data, contentDigest, generationAddressed, err := readIndexBytes(brainDir, source)
	if err != nil {
		return Index{}, err
	}
	index, err := DecodeIndex(data, source)
	if err != nil {
		return Index{}, err
	}
	index.ContentIdentity = contentDigest
	if generationAddressed {
		index.StorageIdentity = source.IndexPath
	}
	return index, nil
}

// LoadIndexWithLegacyIdentity returns the same verified index as
// LoadIndex plus an optional identity upgrade derived from the
// exact byte slice that was parsed. Older manifests predate the strong source
// identity fields required by direct FTS hydration. Keeping hashing and parsing
// on one immutable slice prevents a path replacement from producing a hash of
// generation A and parsed records from generation B.
func LoadIndexWithLegacyIdentity(brainDir string, source *bmanifest.HistorySource) (Index, *LegacyIdentity, error) {
	data, contentDigest, generationAddressed, err := readIndexBytes(brainDir, source)
	if err != nil {
		return Index{}, nil, err
	}
	index, err := DecodeIndex(data, source)
	if err != nil {
		return Index{}, nil, err
	}
	index.ContentIdentity = contentDigest
	if generationAddressed {
		index.StorageIdentity = source.IndexPath
	}
	legacyIdentity := DeriveLegacyIdentity(source, data, index)
	if legacyIdentity != nil {
		// The direct/index-backed FTS freshness check consumes this private
		// identity. It is derived from the parsed truth, not trusted from a
		// legacy manifest, and avoids recomputing it later in this same request.
		index.RecordsFingerprint = legacyIdentity.RecordsFingerprint
	}
	return index, legacyIdentity, nil
}

func DecodeIndex(data []byte, source *bmanifest.HistorySource) (Index, error) {
	if source.IndexBytes > 0 && int64(len(data)) != source.IndexBytes {
		return Index{}, fmt.Errorf("history index size mismatch: got %d bytes, manifest declares %d; run `"+clibase.BrainInvocation(os.LookupEnv, "refresh history")+"` to rebuild it", len(data), source.IndexBytes)
	}
	if source.IndexSHA256 != "" && IndexBytesFingerprint(data) != source.IndexSHA256 {
		return Index{}, errors.New("history index checksum does not match manifest")
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		return Index{}, fmt.Errorf("%s: decode history index: %w", brainstore.StateCorruptCode, err)
	}
	if source.RecordsFingerprint != "" {
		if !ValidSHA256(source.RecordsFingerprint) {
			return Index{}, errors.New("history records fingerprint is invalid")
		}
		if len(index.Records) != source.Records {
			return Index{}, fmt.Errorf("history record count mismatch: got %d, manifest declares %d", len(index.Records), source.Records)
		}
		if !source.GeneratedAt.IsZero() && !index.GeneratedAt.Equal(source.GeneratedAt) {
			return Index{}, errors.New("history index generation does not match manifest")
		}
		// IndexSHA256 binds the exact serialized record set to this manifest.
		// Recomputing the canonical per-record hash here would marshal hundreds
		// of thousands of records on every semantic/get path; the exact byte
		// checksum above already provides the integrity check we need.
		index.RecordsFingerprint = source.RecordsFingerprint
	}
	return index, nil
}

func ValidateIndexPath(indexPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(indexPath))
	cleanSlash := filepath.ToSlash(clean)
	if cleanSlash != indexPath {
		return "", fmt.Errorf("history index_path must be canonical: %s", indexPath)
	}
	if strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) ||
		(cleanSlash != IndexPath && !ValidGenerationArtifactPath(cleanSlash, IndexFileName)) {
		return "", fmt.Errorf("history index_path is unsafe: %s", indexPath)
	}
	return clean, nil
}

func ValidGenerationArtifactPath(rel, fileName string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != brainstore.HistoryDirName || parts[1] != "generations" || parts[3] != fileName || len(parts[2]) != 40 {
		return false
	}
	_, err := hex.DecodeString(parts[2])
	return err == nil
}

func ValidStagingArtifactPath(rel, fileName string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != brainstore.HistoryDirName || parts[1] != "staging" || parts[3] != fileName || len(parts[2]) != 40 {
		return false
	}
	_, err := hex.DecodeString(parts[2])
	return err == nil
}

func GenerationArtifactPath(generation, name string) string {
	return filepath.ToSlash(filepath.Join(GenerationsDir, generation, name))
}
