package factstore

// The repository memory store is one flat collection: facts/store.json holds
// every record's full payload keyed by fact id. Branch is metadata on a
// record, never a storage key. Schema history:
//
//	1: records + per-branch views with field overrides (unreleased)
//	2: bodyless marker {storage:"legacy_views"}; payloads lived in
//	   facts/<branch-slug>/facts.ndjson (unreleased)
//	3: this layout
//
// Released clients (v0.2–v0.4) predate every store schema and read/write the
// legacy branch NDJSON files directly — including AFTER the manifest schema
// bump, because their write path appends to the branch file before the
// manifest check fails. Load therefore folds any legacy branch files it finds
// (see repo_fact_compat.go), and Write parks them once their content is
// carried by the flat store.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

const RepositoryPath = "facts/store.json"
const repoFactsVersion = 3

// legacyParkPrefix names the directory migration parks folded legacy branch
// dirs under (facts/.legacy-<unixts>/<dirname>). Verified redundant files are
// removed automatically; unverified bytes remain available for recovery.
const legacyParkPrefix = ".legacy-"

type repoFactStore struct {
	SchemaVersion int                        `json:"schema_version"`
	Records       map[string]json.RawMessage `json:"records"`
}

// readStoreFile reads facts/store.json raw, with the shared forward-version
// gate: a schema newer than this build fails closed.
func readStoreFile(brainDir string) (version int, data []byte, present bool, err error) {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return 0, nil, false, err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, RepositoryPath); err != nil {
		return 0, nil, false, err
	}
	path := filepath.Join(brainDir, filepath.FromSlash(RepositoryPath))
	if info, statErr := os.Lstat(path); statErr == nil && !info.Mode().IsRegular() {
		return 0, nil, true, fmt.Errorf("repository memory store is not a regular file: %s", RepositoryPath)
	}
	data, readErr := os.ReadFile(path)
	if os.IsNotExist(readErr) {
		return 0, nil, false, nil
	}
	if readErr != nil {
		return 0, nil, false, readErr
	}
	// A zero-byte store is a crashed writer's leftover, not a corpus: treat it
	// as absent so readers degrade to the integrity warning instead of dying.
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, nil, false, nil
	}
	version, err = brainstore.CheckedVersionedJSONHeader(data, repoFactsVersion, "repository memory store")
	if err != nil {
		return version, nil, true, err
	}
	return version, data, true, nil
}

// decodeV3 decodes a schema-3 store into records keyed by id, failing closed
// when a body is unparseable or disagrees with its key: a key standing over a
// different fact's body is a substitution — the fact the key names is gone.
func decodeV3(data []byte) (map[string]factmerge.Record, error) {
	var s repoFactStore
	if _, err := brainstore.DecodeStrictVersionedJSON(data, &s, repoFactsVersion, "repository memory store"); err != nil {
		return nil, err
	}
	if s.SchemaVersion != repoFactsVersion || s.Records == nil {
		return nil, fmt.Errorf("invalid repository memory store header")
	}
	out := make(map[string]factmerge.Record, len(s.Records))
	for id, raw := range s.Records {
		var rec factmerge.Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("invalid repository memory record %s: %w", id, err)
		}
		if rec.ID != id {
			return nil, fmt.Errorf("repository memory record %s carries id %q; the record the key names is gone — %s", id, rec.ID, RepairHint())
		}
		out[id] = rec
	}
	return out, nil
}

// Load returns every stored record: the flat store plus, until migration has
// parked them, the legacy sources folded in memory (read-only — Load never
// writes). Records come back in factmerge.Sort order. Corrupt or
// forward-version stores fail closed.
func Load(brainDir string) ([]factmerge.Record, error) {
	combined, err := loadCombined(brainDir)
	if err != nil {
		return nil, err
	}
	return combined.sorted(), nil
}

// Write replaces the flat store with exactly these records. Callers hold the
// brain write lock over their load-mutate-write; the store lock additionally
// serializes snapshot commits.
//
// Legacy branch dirs still on disk are parked afterwards: under the brain
// write lock the caller's Load already folded their content (legacy writers
// take the same lock), so the written set carries everything they held — and
// an id the caller removed on purpose (gc, the mirror sweep) must not
// resurrect from a file nothing maintains anymore.
func Write(brainDir string, records []factmerge.Record) error {
	return withRepoFactStoreLock(brainDir, func() error {
		// Establish the pre-mutation migration before an intentional delete or
		// retraction. Never merge legacy records into the caller's replacement set.
		dirs, err := legacyBranchDirs(brainDir)
		if err != nil {
			return err
		}
		if len(dirs) > 0 {
			if _, err := migrateRepositoryLocked(brainDir); err != nil {
				return err
			}
		} else if _, _, _, err := readStoreFile(brainDir); err != nil {
			// Preserve the forward-version gate without decoding the entire
			// existing corpus again on every already-migrated write.
			return err
		}
		_ = cleanupLegacyLocked(brainDir)
		return writeFlatStore(brainDir, records)
	})
}

func writeFlatStore(brainDir string, records []factmerge.Record) error {
	s := repoFactStore{SchemaVersion: repoFactsVersion, Records: make(map[string]json.RawMessage, len(records))}
	for _, rec := range records {
		if strings.TrimSpace(rec.ID) == "" {
			return fmt.Errorf("refusing to store a memory without an id: %q", rec.Text)
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		s.Records[rec.ID] = raw
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, RepositoryPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return cleanupRepoFactTemps(brainDir)
}

// legacyBranchDirs lists the facts/ subdirectories legacy layouts and released
// clients write branch NDJSON files into. Parked dirs (.legacy-*) and any
// other dotted name are never legacy sources.
func legacyBranchDirs(brainDir string) ([]string, error) {
	if err := base.RejectExistingSymlinkPathComponents(brainDir, DirName); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(brainDir, DirName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirs = append(dirs, entry.Name())
	}
	sort.Strings(dirs)
	return dirs, nil
}

// parkLegacyDirs moves every legacy branch dir to facts/.legacy-<unixts>/.
// Returns the brain-relative paths it moved. Callers hold the store lock.
func parkLegacyDirs(brainDir string) ([]string, error) { return parkLegacyDirsVerified(brainDir, nil) }

func parkLegacyDirsVerified(brainDir string, proofs map[string]cleanupReceipt) ([]string, error) {
	dirs, err := legacyBranchDirs(brainDir)
	if err != nil || len(dirs) == 0 {
		return nil, err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, DirName); err != nil {
		return nil, err
	}
	// MkdirTemp keeps repeated parks distinct: a released client can re-create
	// a legacy dir and be parked again within the same second, and renaming
	// onto an earlier park's entry would fail.
	parkAbs, err := os.MkdirTemp(filepath.Join(brainDir, DirName), fmt.Sprintf("%s%d-", legacyParkPrefix, time.Now().Unix()))
	if err != nil {
		return nil, err
	}
	moved := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		rel := filepath.Join(DirName, dir)
		if err := base.RejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
			return moved, err
		}
		if err := os.Rename(filepath.Join(brainDir, rel), filepath.Join(parkAbs, dir)); err != nil {
			return moved, err
		}
		// A receipt ties the input consumed by the verified migration to its
		// committed result. Later deliberate deletions must not strand this copy.
		if proof, ok := proofs[dir]; ok {
			branch, openErr := os.OpenRoot(filepath.Join(parkAbs, dir))
			if openErr == nil {
				encoded, _ := json.Marshal(proof)
				// A failed receipt only defers cleanup; the committed migration remains
				// valid and the original recovery bytes remain parked.
				_ = writeCleanupReceipt(branch, append(encoded, '\n'))
				branch.Close()
			}
		}
		moved = append(moved, filepath.ToSlash(rel))
	}
	return moved, nil
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

// A process can die while the atomic writer is filling its temporary file.
// These are never exportable data or backups; writes remove them even when the
// live snapshot no longer references the memory. Production callers hold the
// store lock, so no live writer is swept.
func cleanupRepoFactTemps(brainDir string) error {
	if err := base.RejectExistingSymlinkPathComponents(brainDir, DirName); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(brainDir, DirName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".store.json.tmp-") {
			continue
		}
		rel := filepath.Join(DirName, entry.Name())
		if err := base.RejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular memory store temporary file: %s", rel)
		}
		if err := os.Remove(filepath.Join(brainDir, rel)); err != nil {
			return err
		}
	}
	return nil
}
