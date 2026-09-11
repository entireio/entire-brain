package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// historyLegacyIdentity is a complete strong identity derived from one fully
// read and parsed legacy index. Observed* binds a later best-effort manifest
// update to the exact source generation the caller used.
type historyLegacyIdentity struct {
	ObservedGeneratedAt time.Time
	ObservedIndexPath   string
	ObservedRecords     int
	IndexBytes          int64
	IndexSHA256         string
	RecordsFingerprint  string
}

func historySourceNeedsIdentityUpgrade(source *historySourceManifest) bool {
	return source != nil && (source.IndexBytes == 0 || source.IndexSHA256 == "" || source.RecordsFingerprint == "")
}

// deriveHistoryLegacyIdentity accepts only a complete, internally consistent
// legacy generation. Existing non-empty fields remain authoritative: a partial
// migration with a conflicting field is not repaired opportunistically.
func deriveHistoryLegacyIdentity(source *historySourceManifest, data []byte, index historyIndex) *historyLegacyIdentity {
	if !historySourceNeedsIdentityUpgrade(source) || source.IndexPath == "" || source.GeneratedAt.IsZero() ||
		source.Records < 0 || len(index.Records) != source.Records || !index.GeneratedAt.Equal(source.GeneratedAt) {
		return nil
	}
	identity := &historyLegacyIdentity{
		ObservedGeneratedAt: source.GeneratedAt,
		ObservedIndexPath:   source.IndexPath,
		ObservedRecords:     source.Records,
		IndexBytes:          int64(len(data)),
		IndexSHA256:         historyIndexBytesFingerprint(data),
		RecordsFingerprint:  historyRecordsFingerprint(index.Records),
	}
	if source.IndexBytes != 0 && source.IndexBytes != identity.IndexBytes {
		return nil
	}
	if source.IndexSHA256 != "" && source.IndexSHA256 != identity.IndexSHA256 {
		return nil
	}
	if source.RecordsFingerprint != "" && source.RecordsFingerprint != identity.RecordsFingerprint {
		return nil
	}
	return identity
}

func (identity *historyLegacyIdentity) sourceWithIdentity(source *historySourceManifest) *historySourceManifest {
	if identity == nil || source == nil {
		return source
	}
	upgraded := *source
	upgraded.IndexBytes = identity.IndexBytes
	upgraded.IndexSHA256 = identity.IndexSHA256
	upgraded.RecordsFingerprint = identity.RecordsFingerprint
	return &upgraded
}

// rankHistoryViaLegacyDirectPayload is deliberately scoped to a legacy manifest
// paired with an already-present schema-v2 FTS cache. It validates the cache's
// identity metadata and hydrated row structure against identity derived from
// index truth under the existing cooperative local-cache trust model; it does
// not prove byte-for-byte payload equality. Schema-v1 caches remain ordinary
// misses and are not migrated here. Migration failure is ignored: the result
// remains usable and a later invocation can retry.
func rankHistoryViaLegacyDirectPayload(brainDir string, source *historySourceManifest, identity *historyLegacyIdentity, kind, query string, limit int, cutoff float64) ([]scoredHistoryRecord, bool) {
	if identity == nil {
		return nil, false
	}
	upgraded := identity.sourceWithIdentity(source)
	scored, used, err := rankHistoryViaFreshFTSCutoffDetailed(brainDir, upgraded, kind, query, limit, cutoff)
	if err != nil || !used {
		return nil, false
	}
	_ = persistHistoryLegacyIdentity(brainDir, identity)
	return scored, true
}

// persistHistoryLegacyIdentity fills only absent identity fields. It takes the
// existing writer lock without waiting, reloads the manifest to preserve any
// concurrent changes, and re-reads the authoritative file under that lock. The
// second byte identity check closes the read-to-lock replacement window.
func persistHistoryLegacyIdentity(brainDir string, identity *historyLegacyIdentity) error {
	if identity == nil {
		return nil
	}
	unlock, err := acquireBrainWriteLockTimeout(brainDir, 0)
	if err != nil {
		return err
	}
	defer unlock()

	// Strict: this migration rewrites the manifest from this struct further
	// down, so a field this build cannot name must stop it rather than be
	// dropped on the way through.
	manifest, err := loadBrainManifestForReplace(brainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		return errors.New("history source changed before identity migration")
	}
	current := manifest.Sources.History
	if current.IndexPath != identity.ObservedIndexPath || current.Records != identity.ObservedRecords ||
		!current.GeneratedAt.Equal(identity.ObservedGeneratedAt) {
		return errors.New("history source generation changed before identity migration")
	}
	if err := historyIdentityFieldsCompatible(current, identity); err != nil {
		return err
	}
	fingerprint, err := historyIndexFingerprintForIdentityRecheck(brainDir, current.IndexPath, identity.IndexBytes)
	if err != nil {
		return err
	}
	if fingerprint != identity.IndexSHA256 {
		return errors.New("history index changed before identity migration")
	}

	changed := false
	if current.IndexBytes == 0 {
		current.IndexBytes = identity.IndexBytes
		changed = true
	}
	if current.IndexSHA256 == "" {
		current.IndexSHA256 = identity.IndexSHA256
		changed = true
	}
	if current.RecordsFingerprint == "" {
		current.RecordsFingerprint = identity.RecordsFingerprint
		changed = true
	}
	if !changed {
		return nil
	}
	return writeHistoryIdentityManifestAtomic(brainDir, *manifest)
}

func historyIdentityFieldsCompatible(source *historySourceManifest, identity *historyLegacyIdentity) error {
	if source.IndexBytes != 0 && source.IndexBytes != identity.IndexBytes {
		return errors.New("history index size identity changed before migration")
	}
	if source.IndexSHA256 != "" && source.IndexSHA256 != identity.IndexSHA256 {
		return errors.New("history index checksum identity changed before migration")
	}
	if source.RecordsFingerprint != "" && source.RecordsFingerprint != identity.RecordsFingerprint {
		return errors.New("history records identity changed before migration")
	}
	return nil
}

func historyIndexFingerprintForIdentityRecheck(brainDir, indexPath string, expectedBytes int64) (string, error) {
	if expectedBytes <= 0 {
		return "", errors.New("history index identity has invalid size")
	}
	clean, err := validateHistoryIndexPath(indexPath)
	if err != nil {
		return "", err
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return "", err
	}
	path := filepath.Join(brainDir, clean)
	if err := rejectUnsafeExistingRegularFile(path, "history index"); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|fileLockOpenFlags(), 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := rejectOpenFileAlias(path, f, "history index"); err != nil {
		return "", err
	}
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	if before.Size() != expectedBytes {
		return "", fmt.Errorf("history index size changed before identity migration: got %d, want %d", before.Size(), expectedBytes)
	}

	h := sha256.New()
	n, err := io.CopyN(h, f, expectedBytes)
	if err != nil {
		return "", fmt.Errorf("read history index identity: copied %d of %d bytes: %w", n, expectedBytes, err)
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); err != io.EOF {
		if err != nil {
			return "", fmt.Errorf("read history index identity tail: %w", err)
		}
		if n > 0 {
			return "", errors.New("history index grew before identity migration")
		}
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", errors.New("history index changed while verifying identity")
	}
	if err := rejectOpenFileAlias(path, f, "history index"); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// Identity migration does not alter the human README, generation time, or any
// other source. The manifest replacement itself is atomic and private.
func writeHistoryIdentityManifestAtomic(brainDir string, manifest exportManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", exportManifestFileName, err)
	}
	data = append(data, '\n')
	if err := writeBrainRelativeFileAtomic(brainDir, exportManifestFileName, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", exportManifestFileName, err)
	}
	return nil
}
