package factstore

// The repository store owns memory bodies once. Views are selections plus
// field overrides, not independent collections. Overrides preserve legacy
// lifecycle/provenance differences without promoting a branch's assertions to
// every other branch. The compatibility adapter lives in repo_fact_compat.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

const RepositoryPath = "facts/store.json"
const repoFactsVersion = 1

type repoFactStore struct {
	SchemaVersion int                        `json:"schema_version"`
	Records       map[string]json.RawMessage `json:"records"`
	Views         map[string]repoFactView    `json:"views"`
}

type repoFactView struct {
	// LegacyDigest is a reconciliation checkpoint, never a memory identity.
	LegacyDigest string          `json:"legacy_digest,omitempty"`
	Entries      []repoFactEntry `json:"entries"`
}

type repoFactEntry struct {
	ID       string                     `json:"id"`
	Override map[string]json.RawMessage `json:"override,omitempty"`
}

func emptyRepoFactStore() repoFactStore {
	return repoFactStore{SchemaVersion: repoFactsVersion, Records: map[string]json.RawMessage{}, Views: map[string]repoFactView{}}
}

func readRepoFactStore(brainDir string) (repoFactStore, bool, error) {
	s := emptyRepoFactStore()
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return s, false, err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, RepositoryPath); err != nil {
		return s, false, err
	}
	path := filepath.Join(brainDir, filepath.FromSlash(RepositoryPath))
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return s, true, fmt.Errorf("repository memory store is not a regular file: %s", RepositoryPath)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	// Decode into zero state: omitted fields must not inherit the defaults
	// used for a genuinely absent store.
	s = repoFactStore{}
	if _, err := brainstore.DecodeStrictVersionedJSON(data, &s, repoFactsVersion, "repository memory store"); err != nil {
		return s, true, err
	}
	if s.SchemaVersion != repoFactsVersion || s.Records == nil || s.Views == nil {
		return s, true, fmt.Errorf("invalid repository memory store header")
	}
	// Validate each body once, even when hundreds of views reference it.
	for id := range s.Records {
		if _, err := s.decodeEntry(repoFactEntry{ID: id}); err != nil {
			return s, true, err
		}
	}
	for dir, view := range s.Views {
		if err := validRepoFactView(dir); err != nil {
			return s, true, err
		}
		for _, entry := range view.Entries {
			if _, exists := s.Records[entry.ID]; !exists {
				return s, true, fmt.Errorf("memory view %s references missing record %s", dir, entry.ID)
			}
			if len(entry.Override) != 0 {
				if _, err := s.decodeEntry(entry); err != nil {
					return s, true, err
				}
			}
		}
	}
	return s, true, nil
}

func validRepoFactView(dir string) error {
	if dir == "" || dir == "." || dir == ".." || filepath.Base(dir) != dir || bytes.ContainsAny([]byte(dir), "/\\") {
		return fmt.Errorf("invalid memory view %q", dir)
	}
	return nil
}

func (s repoFactStore) view(dir string) ([]factmerge.Record, error) {
	v, ok := s.Views[dir]
	if !ok {
		return nil, nil
	}
	out := make([]factmerge.Record, 0, len(v.Entries))
	for _, entry := range v.Entries {
		rec, err := s.decodeEntry(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s repoFactStore) decodeEntry(entry repoFactEntry) (factmerge.Record, error) {
	var rec factmerge.Record
	raw, ok := s.Records[entry.ID]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return rec, fmt.Errorf("missing or invalid repository memory record %s", entry.ID)
	}
	if len(entry.Override) != 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return rec, fmt.Errorf("invalid repository memory record %s", entry.ID)
		}
		for key, value := range entry.Override {
			if value == nil { // absent field, distinct from a JSON null
				delete(fields, key)
			} else {
				fields[key] = value
			}
		}
		var err error
		raw, err = json.Marshal(fields)
		if err != nil {
			return rec, err
		}
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, err
	}
	if rec.ID != entry.ID {
		return rec, fmt.Errorf("memory view changes record identity %s", entry.ID)
	}
	return rec, nil
}

// replaceView changes only this selection. Other views keep their existing
// values, including retractions and source anchors. A body's representative is
// stable until it is no longer referenced; no timestamp arbitrates truth.
func (s *repoFactStore) replaceView(dir string, records []factmerge.Record, digest string) error {
	if err := validRepoFactView(dir); err != nil {
		return err
	}
	v := repoFactView{LegacyDigest: digest, Entries: make([]repoFactEntry, 0, len(records))}
	for _, rec := range records {
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		canonical, ok := s.Records[rec.ID]
		if !ok {
			s.Records[rec.ID] = raw
			canonical = raw
		}
		entry := repoFactEntry{ID: rec.ID}
		if !bytes.Equal(raw, canonical) {
			var a, b map[string]json.RawMessage
			if err := json.Unmarshal(canonical, &a); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &b); err != nil {
				return err
			}
			entry.Override = map[string]json.RawMessage{}
			for key, val := range b {
				if !bytes.Equal(val, a[key]) {
					entry.Override[key] = val
				}
			}
			for key := range a {
				if _, exists := b[key]; !exists {
					entry.Override[key] = nil
				}
			}
		}
		v.Entries = append(v.Entries, entry)
	}
	s.Views[dir] = v
	return nil
}

// compact rebuilds representatives from current views. This both collects
// unreferenced bodies and removes obsolete fields/anchors from representatives
// after privacy deletion or a lifecycle update. It is deterministic.
func (s *repoFactStore) compact() error {
	next := emptyRepoFactStore()
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
		if err := next.replaceView(dir, records, s.Views[dir].LegacyDigest); err != nil {
			return err
		}
	}
	*s = next
	return nil
}

func writeRepoFactStore(brainDir string, s *repoFactStore) error {
	if err := s.compact(); err != nil {
		return err
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

// A process can die while the atomic writer is filling its temporary file.
// These are never exportable data or backups, and a privacy purge must remove
// them even when the live snapshot no longer references the purged memory.
// Production callers hold the store lock, so no live native writer is swept.
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
