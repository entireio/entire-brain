package factstore

// This adapter is the only place the repository store knows about legacy
// branch files. Keep it until pre-store binaries are no longer supported.
//
// The old file is written BEFORE the snapshot. It is a durable redo source:
// after interruption, its digest differs from the snapshot's checkpoint and
// the next read imports it. A successful write has published both copies.
// Legacy writers already use the brain write lock; callers retain that lock
// around read/modify/write. The store lock additionally serializes snapshot
// commits (including direct internal writers), never the other way around.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func repoFactViewDir(branch string) string { return filepath.Base(BranchRelDir(branch)) }

func legacyFactViewRel(dir string) string {
	return filepath.ToSlash(filepath.Join(DirName, dir, FileName))
}

// NativeProjection renders an old-format export from current native state.
// Exporters must not rely on a projection still existing on disk.
func NativeProjection(brainDir, dir string) ([]byte, bool, error) {
	s, native, err := readRepoFactStore(brainDir)
	if err != nil || !native {
		return nil, false, err
	}
	if err := s.reconcileView(brainDir, dir); err != nil {
		return nil, false, err
	}
	if _, exists := s.Views[dir]; !exists {
		return nil, false, nil
	}
	records, err := s.view(dir)
	if err != nil {
		return nil, false, err
	}
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, records); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

func readLegacyFactView(brainDir, dir string) ([]byte, bool, error) {
	if err := validRepoFactView(dir); err != nil {
		return nil, false, err
	}
	rel := legacyFactViewRel(dir)
	if err := base.RejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
		return nil, false, err
	}
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("fact store is not a regular file: %s", rel)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (s *repoFactStore) reconcileView(brainDir, dir string) error {
	data, present, err := readLegacyFactView(brainDir, dir)
	if err != nil || !present {
		// Removing a projection is not a canonical deletion. An explicit
		// empty view (GC/purge) is a mutation and DOES reconcile below.
		return err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if v, ok := s.Views[dir]; ok && v.LegacyDigest == digest {
		return nil
	}
	records, err := factmerge.ParseNDJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse %s: %w; %s", legacyFactViewRel(dir), err, RepairHint())
	}
	previous, err := s.view(dir)
	if err != nil {
		return err
	}
	byID := make(map[string]factmerge.Record, len(previous))
	for _, rec := range previous {
		byID[rec.ID] = rec
	}
	for i, rec := range records {
		old, ok := byID[rec.ID]
		if ok && (old.Status == factmerge.StatusRetracted || old.Status == factmerge.StatusSuperseded) &&
			rec.Status == factmerge.StatusActive && !rec.UpdatedAt.After(old.UpdatedAt) {
			// A replay of a stale projection cannot revive a retirement. A
			// later explicit remember remains supported by both CLI versions.
			records[i].Status = old.Status
			records[i].SupersededBy = old.SupersededBy
			records[i].UpdatedAt = old.UpdatedAt
		}
	}
	return s.replaceView(dir, records, digest)
}

func reconcileRepoFactStore(brainDir string) (repoFactStore, error) {
	s, _, err := readRepoFactStore(brainDir)
	if err != nil {
		return s, err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, DirName); err != nil {
		return s, err
	}
	entries, err := os.ReadDir(filepath.Join(brainDir, DirName))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := s.reconcileView(brainDir, entry.Name()); err != nil {
			return s, err
		}
	}
	return s, nil
}

func loadRepoFactView(brainDir, branch string) ([]factmerge.Record, error) {
	s, _, err := readRepoFactStore(brainDir)
	if err != nil {
		return nil, err
	}
	dir := repoFactViewDir(branch)
	if err := s.reconcileView(brainDir, dir); err != nil {
		return nil, err
	}
	return s.view(dir)
}

func loadRepoFactViews(brainDir string) (map[string][]factmerge.Record, error) {
	s, err := reconcileRepoFactStore(brainDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]factmerge.Record, len(s.Views))
	for dir := range s.Views {
		out[dir], err = s.view(dir)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func withRepoFactStoreLock(brainDir string, fn func() error) error {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	rel := filepath.Join(brainstore.BrainLockDirName, "memory-store.lock")
	if err := base.RejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
		return err
	}
	lock, err := brainstore.AcquireFileLock(filepath.Join(brainDir, rel), "memory_store_locked", brainstore.BrainWriteLockTimeout)
	if err != nil {
		return err
	}
	defer lock.Close()
	return fn()
}

func writeRepoFactView(brainDir, dir string, records []factmerge.Record) error {
	return commitRepoFactView(brainDir, dir, records, writeRepoFactStore)
}

// persist is an operation-local fault-injection seam; tests do not replace
// process globals while concurrent writers are running.
func commitRepoFactView(brainDir, dir string, records []factmerge.Record, persist func(string, *repoFactStore) error) error {
	if err := validRepoFactView(dir); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, records); err != nil {
		return err
	}
	return withRepoFactStoreLock(brainDir, func() error {
		s, _, err := readRepoFactStore(brainDir)
		if err != nil {
			return err
		}
		// A branch write must not depend on unrelated legacy projections
		// being readable. Full reconciliation belongs to inventory and
		// migration; retain other native views exactly as stored here.
		if err := s.reconcileView(brainDir, dir); err != nil {
			return err
		}
		sum := sha256.Sum256(buf.Bytes())
		if err := s.replaceView(dir, records, hex.EncodeToString(sum[:])); err != nil {
			return err
		}
		if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, legacyFactViewRel(dir), buf.Bytes(), 0o600); err != nil {
			return err
		}
		return persist(brainDir, &s)
	})
}

// MigrateRepository is idempotent, including after a failed snapshot write.
// Original branch files remain readable by old releases and form the rollback
// representation; there is no backup containing stale private memories.
func MigrateRepository(brainDir string) error {
	return withRepoFactStoreLock(brainDir, func() error {
		s, err := reconcileRepoFactStore(brainDir)
		if err != nil {
			return err
		}
		return writeRepoFactStore(brainDir, &s)
	})
}

// RollbackRepository materializes CURRENT views, including post-upgrade
// writes. Keep the snapshot until every projection has been published. The
// operation is rerunnable after any failure; callers hold the brain write lock.
func RollbackRepository(brainDir string) error {
	return withRepoFactStoreLock(brainDir, func() error {
		s, err := reconcileRepoFactStore(brainDir)
		if err != nil {
			return err
		}
		dirs := make([]string, 0, len(s.Views))
		for dir := range s.Views {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			records, err := s.view(dir)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if err := factmerge.WriteNDJSON(&buf, records); err != nil {
				return err
			}
			if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, legacyFactViewRel(dir), buf.Bytes(), 0o600); err != nil {
				return err
			}
		}
		if err := cleanupRepoFactTemps(brainDir); err != nil {
			return err
		}
		err = os.Remove(filepath.Join(brainDir, filepath.FromSlash(RepositoryPath)))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	})
}
