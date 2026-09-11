package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const brainManifestSchemaVersion = 3

const (
	brainLockDirName      = "locks"
	brainWriteLockName    = "write.lock"
	brainManifestLockName = "manifest.lock"
	brainPrivacyLockName  = "privacy-side-effect.lock"
	brainWriteLockTimeout = 10 * time.Second
)

type brainSources struct {
	Seed     *seedSourceManifest     `json:"seed,omitempty"`
	Sessions *sessionSourceManifest  `json:"sessions,omitempty"`
	Semantic *semanticSourceManifest `json:"semantic,omitempty"`
	History  *historySourceManifest  `json:"history,omitempty"`
	Facts    *factSourceManifest     `json:"facts,omitempty"`
	Docs     *docSourceManifest      `json:"docs,omitempty"`
	Patterns *patternSourceManifest  `json:"patterns,omitempty"`
}

type sessionSourceManifest struct {
	GeneratedAt        time.Time       `json:"generated_at"`
	EntireCLIVersion   string          `json:"entire_cli_version,omitempty"`
	DefaultBranch      string          `json:"default_branch,omitempty"`
	TranscriptMode     string          `json:"transcript_mode"`
	Scope              string          `json:"scope"`
	CheckpointLimit    int             `json:"checkpoint_limit"`
	CheckpointsScanned int             `json:"checkpoints_scanned"`
	OldestSessionAt    *time.Time      `json:"oldest_session_at,omitempty"`
	LatestCheckpointID string          `json:"latest_checkpoint_id,omitempty"`
	Branches           []exportBranch  `json:"branches,omitempty"`
	Sessions           []exportSession `json:"sessions"`
	Warnings           []string        `json:"warnings,omitempty"`
}

func brainWriteLockPath(brainDir string) string {
	return filepath.Join(brainDir, brainLockDirName, brainWriteLockName)
}

func acquireBrainWriteLock(brainDir string) (func(), error) {
	return acquireBrainWriteLockTimeout(brainDir, brainWriteLockTimeout)
}

func acquireBrainWriteLockTimeout(brainDir string, timeout time.Duration) (func(), error) {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, brainLockDirName); err != nil {
		return nil, err
	}
	lock, err := acquireFileLock(brainWriteLockPath(brainDir), "brain_locked", timeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

func withBrainWriteLock(brainDir string, fn func() error) error {
	unlock, err := acquireBrainWriteLock(brainDir)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// acquireBrainPrivacySideEffectLock serializes irreversible provider egress
// with privacy-policy mutations without blocking ordinary projections,
// retrievals, or cancellation requests for the duration of a provider call.
// Any operation needing both locks must acquire this lock before write.lock.
func acquireBrainPrivacySideEffectLock(brainDir string) (func(), error) {
	return acquireBrainPrivacySideEffectLockTimeout(brainDir, brainWriteLockTimeout)
}

func acquireBrainPrivacySideEffectLockTimeout(brainDir string, timeout time.Duration) (func(), error) {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, brainLockDirName); err != nil {
		return nil, err
	}
	lock, err := acquireFileLock(filepath.Join(brainDir, brainLockDirName, brainPrivacyLockName), memoryErrPrivacyBusy, timeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

func withBrainPrivacySideEffectLock(brainDir string, fn func() error) error {
	unlock, err := acquireBrainPrivacySideEffectLock(brainDir)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func withBrainManifestWriteLock(brainDir string, fn func() error) error {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, brainLockDirName); err != nil {
		return err
	}
	lock, err := acquireFileLock(filepath.Join(brainDir, brainLockDirName, brainManifestLockName), "brain_manifest_locked", brainWriteLockTimeout)
	if err != nil {
		return err
	}
	defer lock.Close()
	return fn()
}

// loadBrainManifest reads the manifest for the callers that only consume it —
// status, doctor, the MCP surface, the memory worker, distillation, search.
//
// It tolerates fields it does not declare. Manifest fields are added and retired
// without a schema bump (history_coverage.checkpointed_unexported_commits was
// retired at version 3), so every manifest written before such a change carries
// a field a later build has never heard of. Rejecting those bytes reported the
// whole brain as memory_state_corrupt: all sources off, no facts, and the memory
// worker failing on its retry loop indefinitely, for state that was merely
// written by a different build and is entirely readable.
//
// Writers must not be this forgiving — see loadBrainManifestForReplace.
func loadBrainManifest(outputDir string) (*exportManifest, error) {
	manifest, _, err := readBrainManifest(outputDir, true)
	return manifest, err
}

// loadBrainManifestForReplace reads the manifest on behalf of a writer about to
// overwrite it, and refuses fields it cannot round-trip: re-encoding from a
// struct that never saw them erases what another build recorded. Availability is
// the reader's concern; not destroying state is this one's.
func loadBrainManifestForReplace(outputDir string) (*exportManifest, error) {
	manifest, _, err := readBrainManifest(outputDir, false)
	return manifest, err
}

// readBrainManifest also reports whether it dropped fields written by another
// build, so status and doctor can recommend a refresh instead of staying silent.
func readBrainManifest(outputDir string, tolerateUnknownFields bool) (*exportManifest, bool, error) {
	data, present, err := readMemoryStateFile(outputDir, exportManifestFileName, "brain manifest", maxManifestBytes)
	if err != nil {
		return nil, false, fmt.Errorf("read brain manifest: %w", err)
	}
	if !present {
		return &exportManifest{SchemaVersion: brainManifestSchemaVersion}, false, nil
	}
	var manifest exportManifest
	version, err := checkedVersionedJSONHeader(data, brainManifestSchemaVersion, "brain manifest")
	if err != nil {
		return nil, false, fmt.Errorf("parse brain manifest: %w", err)
	}
	droppedUnknownFields, err := decodeVersionedJSONBody(data, &manifest, tolerateUnknownFields)
	if err != nil {
		if errors.Is(err, errTrailingJSONData) {
			return nil, false, fmt.Errorf("parse brain manifest: %s: brain manifest contains trailing JSON data", memoryErrStateCorrupt)
		}
		return nil, false, fmt.Errorf("parse brain manifest: %s: brain manifest cannot be parsed: %w", memoryErrStateCorrupt, err)
	}
	// Versionless legacy exports predate the top-level schema field and adapt as
	// v1. Explicit v1-v3 documents are supported; no writer may down-convert a
	// vNext manifest or silently discard additive fields.
	if version < 0 || version > brainManifestSchemaVersion {
		return nil, false, fmt.Errorf("%s: unsupported brain manifest schema version %d", memoryErrUnsupportedVersion, version)
	}
	normalizeBrainManifest(&manifest)
	return &manifest, droppedUnknownFields, nil
}

// errManifestRoundTrips reports a manifest this build can already rewrite, so
// there is nothing for a forced refresh to discard.
var errManifestRoundTrips = errors.New("manifest round-trips")

// discardManifestThisBuildCannotRewrite removes a manifest whose only defect is
// fields this build cannot name, so an explicit forced refresh can rebuild it.
//
// Every writer refuses such a manifest deliberately: re-encoding from a struct
// that never saw a field erases it. That leaves a brain readable but frozen,
// and until now the only way out was for a person to delete the file by hand.
// An operator asking for a forced refresh is asking for exactly that
// replacement, rebuilt from canonical sources -- but only for this one cause:
//
//   - a newer schema stays. Down-converting a vNext manifest is the loss the
//     version check exists to prevent, and --force does not overrule it.
//   - genuine corruption stays. That is a diagnosis for the operator, not bytes
//     to delete on a guess.
//
// The removal runs under the manifest write lock and through the checked
// remover, so it inherits the same symlink and file-identity guarantees as
// every other cooperative manifest mutation.
func discardManifestThisBuildCannotRewrite(brainDir string) (bool, error) {
	unrewritable := false
	err := withBrainManifestWriteLock(brainDir, func() error {
		return removeCheckedMemoryStateFile(brainDir, exportManifestFileName, "brain manifest", maxManifestBytes, func(data []byte) error {
			version, err := checkedVersionedJSONHeader(data, brainManifestSchemaVersion, "brain manifest")
			if err != nil {
				return err
			}
			// The same range the reader enforces. checkedVersionedJSONHeader only
			// rejects versions newer than this build, so without this a negative
			// or otherwise out-of-range version reaches the delete -- state the
			// reader already calls unsupported, which is exactly the "genuine
			// corruption stays" case above.
			if version < 0 || version > brainManifestSchemaVersion {
				return fmt.Errorf("%s: unsupported brain manifest schema version %d", memoryErrUnsupportedVersion, version)
			}
			var manifest exportManifest
			droppedUnknownFields, decodeErr := decodeVersionedJSONBody(data, &manifest, true)
			if decodeErr != nil {
				return fmt.Errorf("%s: brain manifest cannot be parsed: %w", memoryErrStateCorrupt, decodeErr)
			}
			if !droppedUnknownFields {
				return errManifestRoundTrips
			}
			unrewritable = true
			return nil
		})
	})
	if errors.Is(err, errManifestRoundTrips) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return unrewritable, nil
}

func normalizeBrainManifest(manifest *exportManifest) {
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = 1
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	if manifest.Sources.Sessions == nil && (len(manifest.Sessions) > 0 || manifest.TranscriptMode != "" || manifest.CheckpointsScanned > 0) {
		manifest.Sources.Sessions = sessionSourceFromExport(*manifest)
	}
}

func sessionSourceFromExport(manifest exportManifest) *sessionSourceManifest {
	return &sessionSourceManifest{
		GeneratedAt:        manifest.GeneratedAt,
		EntireCLIVersion:   manifest.EntireCLIVersion,
		DefaultBranch:      manifest.DefaultBranch,
		TranscriptMode:     manifest.TranscriptMode,
		Scope:              manifest.Scope,
		CheckpointLimit:    manifest.CheckpointLimit,
		CheckpointsScanned: manifest.CheckpointsScanned,
		OldestSessionAt:    oldestSessionAt(manifest.Sessions),
		LatestCheckpointID: latestCheckpointID(manifest.Sessions),
		Branches:           append([]exportBranch(nil), manifest.Branches...),
		Sessions:           append([]exportSession(nil), manifest.Sessions...),
		Warnings:           append([]string(nil), manifest.Warnings...),
	}
}

func oldestSessionAt(sessions []exportSession) *time.Time {
	var oldest time.Time
	for _, session := range sessions {
		if session.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || session.CreatedAt.Before(oldest) {
			oldest = session.CreatedAt.UTC()
		}
	}
	if oldest.IsZero() {
		return nil
	}
	return &oldest
}

func latestCheckpointID(sessions []exportSession) string {
	var latest exportSession
	for _, session := range sessions {
		if latest.SessionID == "" || shouldReplaceExportSession(latest, session) {
			latest = session
		}
	}
	return latest.LatestCheckpoint
}

func shouldReplaceExportSession(current, candidate exportSession) bool {
	if candidate.CreatedAt.After(current.CreatedAt) {
		return true
	}
	if current.CreatedAt.Equal(candidate.CreatedAt) {
		return candidate.LatestCheckpoint > current.LatestCheckpoint
	}
	return false
}

func writeBrainSessionSource(outputDir, repoKey string, sessionManifest exportManifest) error {
	return withBrainWriteLock(outputDir, func() error {
		return writeBrainSessionSourceLocked(outputDir, repoKey, sessionManifest)
	})
}

func writeBrainSessionSourceLocked(outputDir, repoKey string, sessionManifest exportManifest) error {
	existing, err := loadBrainManifest(outputDir)
	if err != nil {
		return err
	}
	if existing.Sources == nil {
		existing.Sources = &brainSources{}
	}
	sessionManifest.SchemaVersion = brainManifestSchemaVersion
	sessionManifest.RepoKey = repoKey
	sessionManifest.Sources = existing.Sources
	sessionManifest.Sources.Sessions = sessionSourceFromExport(sessionManifest)
	if existing.Sources.Seed != nil {
		sessionManifest.Sources.Seed = existing.Sources.Seed
	}
	if existing.Sources.Semantic != nil {
		sessionManifest.Sources.Semantic = existing.Sources.Semantic
	}
	return writeBrainManifestAndReadme(outputDir, sessionManifest)
}

func writeBrainSeedSource(outputDir, repoKey string, seed *seedSourceManifest) error {
	return withBrainWriteLock(outputDir, func() error {
		return writeBrainSeedSourceLocked(outputDir, repoKey, seed)
	})
}

func writeBrainSeedSourceLocked(outputDir, repoKey string, seed *seedSourceManifest) error {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.SchemaVersion = brainManifestSchemaVersion
	manifest.RepoKey = repoKey
	manifest.GeneratedAt = seed.GeneratedAt
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = time.Now().UTC()
	}
	manifest.Sources.Seed = seed
	applySessionSourceAliases(manifest)
	return writeBrainManifestAndReadme(outputDir, *manifest)
}

func applySessionSourceAliases(manifest *exportManifest) {
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return
	}
	sessions := manifest.Sources.Sessions
	manifest.EntireCLIVersion = sessions.EntireCLIVersion
	manifest.DefaultBranch = sessions.DefaultBranch
	manifest.TranscriptMode = sessions.TranscriptMode
	manifest.Scope = sessions.Scope
	manifest.CheckpointLimit = sessions.CheckpointLimit
	manifest.CheckpointsScanned = sessions.CheckpointsScanned
	manifest.Branches = append([]exportBranch(nil), sessions.Branches...)
	manifest.Sessions = append([]exportSession(nil), sessions.Sessions...)
	manifest.Warnings = append([]string(nil), sessions.Warnings...)
}

func writeBrainManifestAndReadme(outputDir string, manifest exportManifest) error {
	return withBrainManifestWriteLock(outputDir, func() error {
		// All production manifest replacements share this leaf-specific lock and
		// classify the exact present bytes while holding it. This does not claim
		// to exclude hostile non-cooperating filesystem mutation, but no current
		// writer can down-convert a vNext or erase unrecognized current bytes it
		// observed.
		if _, err := loadBrainManifestForReplace(outputDir); err != nil {
			return err
		}
		normalizeBrainManifest(&manifest)
		manifest.SchemaVersion = brainManifestSchemaVersion
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", exportManifestFileName, err)
		}
		data = append(data, '\n')
		if err := writeBrainRelativeFileAtomic(outputDir, exportManifestFileName, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", exportManifestFileName, err)
		}
		if err := writeBrainRelativeFileAtomic(outputDir, exportReadmeFileName, []byte(renderBrainReadme(manifest)), 0o600); err != nil {
			return fmt.Errorf("write brain readme: %w", err)
		}
		return nil
	})
}

func cleanBrainRelativePath(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside brain: %s", rel)
	}
	return clean, nil
}

func writeBrainRelativeFileAtomic(brainDir, rel string, data []byte, perm os.FileMode) error {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	clean, err := cleanBrainRelativePath(rel)
	if err != nil {
		return err
	}
	dir := filepath.Dir(clean)
	if dir != "." {
		if err := rejectExistingSymlinkPathComponents(brainDir, dir); err != nil {
			return err
		}
	}
	abs := filepath.Join(brainDir, clean)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return err
	}
	return writeFileAtomic(abs, data, perm)
}

func prepareBrainRelativeSQLiteFile(brainDir, rel string) (string, error) {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return "", err
	}
	clean, err := cleanBrainRelativePath(rel)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(clean)
	if dir != "." {
		if err := rejectExistingSymlinkPathComponents(brainDir, dir); err != nil {
			return "", err
		}
	}
	abs := filepath.Join(brainDir, clean)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return "", err
	}
	if err := rejectUnsafeExistingRegularFile(abs, "sqlite cache"); err != nil {
		return "", err
	}
	return abs, nil
}

func removeBrainRelativeFile(brainDir, rel string) error {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	clean, err := cleanBrainRelativePath(rel)
	if err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(brainDir, clean))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// removeBrainRelativeFileExpected removes a file only when it is still the
// exact inode/file identity observed by a descriptor-bound inventory. Privacy
// cleanup uses this after validating content so a raced replacement is never
// unlinked under the authority of the original file.
func removeBrainRelativeFileExpected(brainDir, rel string, expected os.FileInfo) error {
	if expected == nil {
		return fmt.Errorf("memory_state_unsafe: missing expected file identity for %s", rel)
	}
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	clean, err := cleanBrainRelativePath(rel)
	if err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return err
	}
	path := filepath.Join(brainDir, clean)
	current, err := memoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("memory_state_unsafe: expected file disappeared before removal: %s", filepath.ToSlash(clean))
		}
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(expected, current) {
		return fmt.Errorf("memory_state_unsafe: file changed before removal: %s", filepath.ToSlash(clean))
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFileAtomic(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func renderBrainReadme(manifest exportManifest) string {
	if manifest.Sources == nil || (manifest.Sources.Seed == nil && manifest.Sources.Semantic == nil && manifest.Sources.History == nil) {
		return renderExportReadme(manifest)
	}
	return renderCombinedBrainReadme(manifest)
}
