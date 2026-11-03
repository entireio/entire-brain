package factstore

// Legacy-layout folding. This file knows every pre-flat layout so the rest of
// the package doesn't:
//
//   - schema-1 store.json: records plus per-branch views whose entries may
//     override individual record fields;
//   - legacy branch NDJSON files (facts/<branch-slug>/facts.ndjson): the
//     schema-2 world, every released client (v0.2–v0.4), and the stragglers
//     those clients keep writing after the manifest bump, because their write
//     path appends to the branch file before their manifest check fails.
//
// Keep it until released clients that write branch files are no longer
// supported; then delete the file wholesale.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// --- in-memory fold ---------------------------------------------------------

type combinedRecords struct {
	byID map[string]factmerge.Record
	// cleanupEvidence binds strictly readable legacy input to this migration.
	cleanupEvidence map[string]migrationCleanupEvidence
	// unusable counts stored lines that parse but carry no fact id. They are
	// unaddressable and never folded; the parked originals retain the bytes.
	unusable int
	// storeVersion is the on-disk store.json schema (0 when absent).
	storeVersion int
	storePresent bool
	// legacyFiles counts the facts.ndjson files folded from legacy branch dirs.
	legacyFiles int
	// collidedIDs are fact ids one legacy FILE stored more than once. A repeat
	// across files is the ordinary hosted-pull mirror; a repeat within one file
	// means a record stands where another used to be (COR-2220's damage shape)
	// and integrity must name it.
	collidedIDs map[string]struct{}
	// legacyDirs are the facts/ subdirectories migration must park.
	legacyDirs []string
	// legacyIDs are the fact ids legacy sources (schema-1 views, branch files)
	// contributed — the migration summary's fold count.
	legacyIDs map[string]struct{}
	// crossRepeats counts legacy-source records whose id another source already
	// yielded (a promote or hosted mirror stored one fact in two branch files).
	// Pre-migration manifests counted those per file, so the integrity check
	// adds them back to Readable or every legacy brain reads as lossy.
	crossRepeats int
}

func (c combinedRecords) sorted() []factmerge.Record {
	out := make([]factmerge.Record, 0, len(c.byID))
	for _, rec := range c.byID {
		out = append(out, rec)
	}
	factmerge.Sort(out)
	return out
}

func (c combinedRecords) ids() map[string]struct{} {
	out := make(map[string]struct{}, len(c.byID))
	for id := range c.byID {
		out[id] = struct{}{}
	}
	return out
}

// loadCombined reads the flat store and folds every legacy source in memory.
// Read-only: it never writes, parks, or migrates.
func loadCombined(brainDir string) (combinedRecords, error) {
	return loadCombinedWithEvidence(brainDir, false)
}

func loadCombinedWithEvidence(brainDir string, collectEvidence bool) (combinedRecords, error) {
	combined := combinedRecords{byID: map[string]factmerge.Record{}}
	version, data, present, err := readStoreFile(brainDir)
	if err != nil {
		return combined, err
	}
	combined.storeVersion = version
	combined.storePresent = present
	var v1ByDir map[string][]factmerge.Record
	switch {
	case !present:
	case version == repoFactsVersion:
		records, err := decodeV3(data)
		if err != nil {
			return combined, err
		}
		for _, rec := range records {
			combined.fold(rec)
		}
	case version == 2:
		// Bodyless marker; payloads live in the legacy branch files below.
		if err := checkLegacyMarker(data); err != nil {
			return combined, err
		}
	case version == 1:
		// Folded AFTER the branch files below: a view is a snapshot of its
		// dir's file, so the live file decides which of its entries survive.
		v1ByDir, err = decodeV1(data)
		if err != nil {
			return combined, err
		}
	default:
		return combined, fmt.Errorf("invalid repository memory store header")
	}

	combined.legacyDirs, err = legacyBranchDirs(brainDir)
	if err != nil {
		return combined, err
	}
	// fileIDs records, per dir with a live facts.ndjson, the ids that file
	// holds — including a present-but-empty file, which is an explicit purge.
	fileIDs := map[string]map[string]struct{}{}
	for _, dir := range combined.legacyDirs {
		rel := filepath.Join(DirName, dir, FileName)
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		info, statErr := os.Lstat(path)
		if statErr != nil {
			continue // a dir without a facts.ndjson holds nothing to fold
		}
		// A facts.ndjson that EXISTS but is a symlink or otherwise non-regular
		// is tampering or damage, never an empty store: refuse rather than
		// silently omit its records (the pre-flat loader's contract) — and
		// never read through a link that may point outside the brain.
		if !info.Mode().IsRegular() {
			return combined, fmt.Errorf("legacy fact store is not a regular file: %s", filepath.ToSlash(rel))
		}
		var records []factmerge.Record
		if collectEvidence {
			var digest string
			records, digest, err = readMigrationRecords(path)
			if digest != "" {
				if combined.cleanupEvidence == nil {
					combined.cleanupEvidence = map[string]migrationCleanupEvidence{}
				}
				evidence := migrationCleanupEvidence{digest: digest}
				for _, rec := range records {
					rec.Provenance = nil // The fold unions anchors; retain only small payload evidence.
					evidence.records = append(evidence.records, rec)
				}
				combined.cleanupEvidence[dir] = evidence
			}
		} else {
			records, err = parseFactsFile(path)
		}
		if err != nil {
			return combined, err
		}
		combined.legacyFiles++
		seenInFile := make(map[string]struct{}, len(records))
		for _, rec := range records {
			if id := strings.TrimSpace(rec.ID); id != "" {
				if _, repeated := seenInFile[id]; repeated {
					if combined.collidedIDs == nil {
						combined.collidedIDs = map[string]struct{}{}
					}
					combined.collidedIDs[id] = struct{}{}
				} else if _, known := combined.byID[id]; known {
					combined.crossRepeats++
				}
				seenInFile[id] = struct{}{}
			}
			combined.foldLegacy(rec)
		}
		fileIDs[dir] = seenInFile
	}
	// Schema-1 views. A view whose dir has a live branch file is the file's
	// stale snapshot: entries the file no longer carries were deleted there
	// (an old `privacy purge` edits only the file) and must not resurrect.
	// A view with no live file is the only copy and folds whole.
	for dir, records := range v1ByDir {
		live, hasFile := fileIDs[dir]
		for _, rec := range records {
			if hasFile {
				if _, ok := live[rec.ID]; !ok {
					continue
				}
			}
			if _, known := combined.byID[strings.TrimSpace(rec.ID)]; known {
				combined.crossRepeats++
			}
			combined.foldLegacy(rec)
		}
	}
	return combined, nil
}

// fold merges one record into the set by content id: the lifecycle winner is
// resolved first (see incomingLifecycleWins), then factmerge.Upsert unions
// provenance and metadata onto the winner. Order-independent: folding A then B
// lands on the same record as B then A.
func (c *combinedRecords) fold(incoming factmerge.Record) {
	if strings.TrimSpace(incoming.ID) == "" {
		c.unusable++
		return
	}
	existing, ok := c.byID[incoming.ID]
	if !ok {
		c.byID[incoming.ID] = incoming
		return
	}
	winner, loser := existing, incoming
	if incomingLifecycleWins(existing, incoming) {
		winner, loser = incoming, existing
	}
	c.byID[incoming.ID] = factmerge.Upsert([]factmerge.Record{winner}, loser)[0]
}

// foldLegacy is fold for records read out of legacy sources, tracked so the
// migration summary can say how many records those sources contributed.
func (c *combinedRecords) foldLegacy(incoming factmerge.Record) {
	if strings.TrimSpace(incoming.ID) != "" {
		if c.legacyIDs == nil {
			c.legacyIDs = map[string]struct{}{}
		}
		c.legacyIDs[incoming.ID] = struct{}{}
	}
	c.fold(incoming)
}

// incomingLifecycleWins decides which copy's lifecycle survives a same-id
// merge. Ranks: retirement (retracted/superseded) > active > candidate —
// except that a strictly NEWER active copy revives a retirement (a later
// explicit remember is a real decision; an active replay of a stale
// projection is not). Rank ties go to the newer UpdatedAt; full ties keep the
// already-stored side.
func incomingLifecycleWins(existing, incoming factmerge.Record) bool {
	re, ri := lifecycleRank(existing), lifecycleRank(incoming)
	if re == 2 && ri == 1 {
		return incoming.UpdatedAt.After(existing.UpdatedAt)
	}
	if ri == 2 && re == 1 {
		return !existing.UpdatedAt.After(incoming.UpdatedAt)
	}
	if ri != re {
		return ri > re
	}
	return incoming.UpdatedAt.After(existing.UpdatedAt)
}

func lifecycleRank(r factmerge.Record) int {
	switch r.Status {
	case factmerge.StatusCandidate:
		return 0
	case factmerge.StatusActive:
		return 1
	default: // retracted, superseded
		return 2
	}
}

// --- legacy store.json decoding ----------------------------------------------

type legacyV1Store struct {
	SchemaVersion int                        `json:"schema_version"`
	Records       map[string]json.RawMessage `json:"records"`
	Views         map[string]legacyV1View    `json:"views"`
}

type legacyV1View struct {
	LegacyDigest string          `json:"legacy_digest,omitempty"`
	Entries      []legacyV1Entry `json:"entries"`
}

type legacyV1Entry struct {
	ID       string                     `json:"id"`
	Override map[string]json.RawMessage `json:"override,omitempty"`
}

type legacyV2Marker struct {
	SchemaVersion int    `json:"schema_version"`
	Storage       string `json:"storage"`
}

func checkLegacyMarker(data []byte) error {
	var marker legacyV2Marker
	if _, err := brainstore.DecodeStrictVersionedJSON(data, &marker, 2, "repository memory store"); err != nil {
		return err
	}
	if marker.Storage != "legacy_views" {
		return fmt.Errorf("invalid repository memory storage mode")
	}
	return nil
}

// decodeV1 flattens a schema-1 store: every view entry, overrides applied,
// becomes a plain record, grouped by the view's dir so the caller can weigh
// each view against its dir's live branch file.
func decodeV1(data []byte) (map[string][]factmerge.Record, error) {
	s := legacyV1Store{}
	if _, err := brainstore.DecodeStrictVersionedJSON(data, &s, 1, "repository memory store"); err != nil {
		return nil, err
	}
	if s.SchemaVersion != 1 || s.Records == nil || s.Views == nil {
		return nil, fmt.Errorf("invalid repository memory store header")
	}
	out := map[string][]factmerge.Record{}
	for dir, view := range s.Views {
		for _, entry := range view.Entries {
			rec, err := decodeLegacyV1Entry(s, entry)
			if err != nil {
				return nil, fmt.Errorf("memory view %s: %w", dir, err)
			}
			out[dir] = append(out[dir], rec)
		}
	}
	return out, nil
}

func decodeLegacyV1Entry(s legacyV1Store, entry legacyV1Entry) (factmerge.Record, error) {
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

// --- migration ---------------------------------------------------------------

// MigrationSummary reports what a MigrateRepository pass changed. The zero
// value is a no-op pass.
type MigrationSummary struct {
	// Folded is how many records legacy sources contributed to the fold.
	Folded int
	// StragglerFiles counts legacy facts.ndjson files found while the store was
	// already flat: a released (pre-flat) entire-brain wrote memories after
	// migration. The caller should tell the user to update that machine.
	StragglerFiles int
	// MovedDirs are the brain-relative legacy dirs parked under
	// facts/.legacy-<ts>/ after the fold was verified.
	MovedDirs []string
	// Cleanup reports best-effort removal separately from migration publication.
	Cleanup CleanupStatus
}

// Changed reports whether the pass wrote anything.
func (s MigrationSummary) Changed() bool {
	return s.Folded > 0 || s.StragglerFiles > 0 || len(s.MovedDirs) > 0
}

// MigrateRepository is the standing, idempotent reconcile of legacy layouts
// into the flat store: build (fold every source in memory), verify (re-read
// and compare complete records), then park legacy dirs for verified cleanup.
// Already-flat stores also receive a bounded cleanup pass. Callers hold the brain write lock; refresh calls this on every run,
// which is what folds the files released clients keep writing.
func MigrateRepository(brainDir string) (MigrationSummary, error) {
	var summary MigrationSummary
	err := withRepoFactStoreLock(brainDir, func() error {
		var err error
		summary, err = migrateRepositoryLocked(brainDir)
		if err == nil {
			summary.Cleanup = cleanupLegacyLocked(brainDir)
		}
		return err
	})
	return summary, err
}

func migrateRepositoryLocked(brainDir string) (MigrationSummary, error) {
	var summary MigrationSummary
	combined, err := loadCombinedWithEvidence(brainDir, true)
	if err != nil {
		return summary, err
	}
	flatAlready := combined.storeVersion == repoFactsVersion || !combined.storePresent
	if flatAlready && len(combined.legacyDirs) == 0 {
		return summary, nil
	}
	if combined.storeVersion == repoFactsVersion {
		summary.StragglerFiles = combined.legacyFiles
	}
	summary.Folded = len(combined.legacyIDs)
	expected := combined.sorted()
	if err := writeFlatStore(brainDir, expected); err != nil {
		return summary, err
	}
	if err := verifyFlatStore(brainDir, expected); err != nil {
		return summary, err
	}
	proofs := map[string]cleanupReceipt{}
	if len(combined.cleanupEvidence) > 0 {
		facts, openErr := os.OpenRoot(filepath.Join(brainDir, DirName))
		if openErr != nil {
			return summary, openErr
		}
		hash, _, hashErr := hashRootFile(facts, "store.json")
		facts.Close()
		if hashErr != nil {
			return summary, hashErr
		}
		for dir, evidence := range combined.cleanupEvidence {
			if evidence.containedBy(combined.byID) {
				proofs[dir] = cleanupReceipt{SchemaVersion: 1, SourceSHA256: evidence.digest, StoreSHA256: hash}
			}
		}
	}
	moved, err := parkLegacyDirsVerified(brainDir, proofs)
	summary.MovedDirs = moved
	return summary, err
}
