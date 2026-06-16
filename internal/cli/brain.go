package cli

import (
	"encoding/json"
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

func loadBrainManifest(outputDir string) (*exportManifest, error) {
	path := filepath.Join(outputDir, exportManifestFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &exportManifest{SchemaVersion: brainManifestSchemaVersion}, nil
		}
		return nil, fmt.Errorf("read brain manifest: %w", err)
	}
	var manifest exportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse brain manifest: %w", err)
	}
	normalizeBrainManifest(&manifest)
	return &manifest, nil
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
