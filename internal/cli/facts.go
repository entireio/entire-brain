package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	factsDirName          = "facts"
	factsTaxonomyFileName = "taxonomy.json"
	factsTaxonomyPath     = factsDirName + "/" + factsTaxonomyFileName
	factsFileName         = "facts.ndjson"
	factsMaxLineBytes     = 64 * 1024
	factsMaxPerChunk      = 6 // quality-gate output cap per source chunk
	factsMaxPaths         = 2 // a fact may live under at most two taxonomy paths

	factOriginDistilled = "distilled"
	factOriginAuthored  = "authored"

	factStatusActive     = "active"
	factStatusSuperseded = "superseded"
	factStatusRetracted  = "retracted"
)

// factPathPattern matches a three-level taxonomy path
// (category.subcategory.type), each segment lowercase letters, digits, and
// underscores, the first segment starting with a letter. e.g.
// preferences.coding.style or architecture.boundaries.rationale.
var factPathPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+){2}$`)

// factSourceManifest is recorded under sources.facts in the brain manifest,
// parallel to historySourceManifest and the semantic source metadata.
type factSourceManifest struct {
	GeneratedAt     time.Time `json:"generated_at"`
	TaxonomyPath    string    `json:"taxonomy_path"`
	Branches        []string  `json:"branches,omitempty"`
	Facts           int       `json:"facts"`
	Distilled       int       `json:"distilled"`
	Authored        int       `json:"authored"`
	Superseded      int       `json:"superseded"`
	Proposals       int       `json:"proposals"`
	Verified        int       `json:"verified"`
	Unsigned        int       `json:"unsigned"`
	ChunksScanned   int       `json:"chunks_scanned"`
	ChunksDistilled int       `json:"chunks_distilled"`
	Warnings        []string  `json:"warnings,omitempty"`
}

// factRecord is one durable, self-contained statement. The id is content
// derived (sha256 of normalized text + sorted paths) so re-distilling a turn
// is idempotent and dedupe is a map lookup.
type factRecord struct {
	ID           string       `json:"id"`
	Paths        []string     `json:"paths"` // 1-2 taxonomy paths
	Text         string       `json:"text"`  // third person about the user
	Branch       string       `json:"branch"`
	Origin       string       `json:"origin"` // "distilled" | "authored"
	Status       string       `json:"status"` // "active" | "superseded" | "retracted"
	Confidence   string       `json:"confidence,omitempty"`
	Provenance   []factAnchor `json:"provenance"` // >=1; signed source turns
	RelatedIDs   []string     `json:"related_ids,omitempty"`
	SupersededBy string       `json:"superseded_by,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// factAnchor cites the source a fact was derived from or authored against. In
// Phase A only the checkpoint-level fields are populated. TurnID is filled in
// Phase B once Entire CLI exposes turn-level signed anchors; Verified is set by
// `verify` against the checkpoint signature.
type factAnchor struct {
	SessionID    string `json:"session_id"`
	Commit       string `json:"commit,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`    // Phase B
	Transcript   string `json:"transcript,omitempty"` // brain-relative path
	Line         int    `json:"line,omitempty"`       // turn offset in transcript
	Verified     bool   `json:"verified,omitempty"`   // set by `verify`
}

// factTaxonomy is the active taxonomy snapshot. Paths are validated against
// factPathPattern; classification may only invent a new three-level path under
// an existing top-level category.
type factTaxonomy struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Categories  map[string]string `json:"categories"` // top-level -> description
	Paths       []factPathDef     `json:"paths"`
}

type factPathDef struct {
	Path        string   `json:"path"`
	Description string   `json:"description"`
	Examples    []string `json:"examples,omitempty"`
}

// normalizeFactText collapses a fact statement to a stable form for content
// hashing: lowercased, with internal whitespace runs reduced to single spaces.
// Two statements that differ only in casing or spacing share an id.
func normalizeFactText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// factRecordID is sha256(normalize(text) + "\x00" + join(sortedPaths, ",")),
// hex-truncated like the other brain ids. Paths must already be normalized
// (see normalizeFactPaths) so the same statement under the same paths always
// hashes identically regardless of the order the agent emitted them.
func factRecordID(text string, sortedPaths []string) string {
	sum := sha256.Sum256([]byte(normalizeFactText(text) + "\x00" + strings.Join(sortedPaths, ",")))
	return "fact:" + hex.EncodeToString(sum[:12])
}

// validFactPath reports whether path is a syntactically valid three-level
// taxonomy path. It does not check the path against the active taxonomy.
func validFactPath(path string) bool {
	return factPathPattern.MatchString(path)
}

// normalizeFactPaths trims, lowercases, drops syntactically invalid paths,
// deduplicates, sorts, and caps the result at factsMaxPaths. The returned slice
// is the canonical path set used both for the record id and on disk.
func normalizeFactPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	cleaned := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.ToLower(strings.TrimSpace(path))
		if path == "" || !validFactPath(path) {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		cleaned = append(cleaned, path)
	}
	sort.Strings(cleaned)
	if len(cleaned) > factsMaxPaths {
		cleaned = cleaned[:factsMaxPaths]
	}
	return cleaned
}

// factTopLevel returns the top-level category of a taxonomy path (the segment
// before the first dot), or "" if the path is empty.
func factTopLevel(path string) string {
	if i := strings.IndexByte(path, '.'); i >= 0 {
		return path[:i]
	}
	return path
}

// --- on-disk layout -------------------------------------------------------

// factsBranchRelDir returns the brain-relative directory holding a branch's
// facts. The readable slug alone is not collision-free — distinct branches can
// sanitize to the same slug (e.g. "evis/search" and "evis-search", or "Main"
// and "main") — and two branches sharing a facts file would clobber each other
// and break branch isolation. A short hash of the full branch name is appended
// so each branch maps to a stable, unique directory while staying greppable.
func factsBranchRelDir(branch string) string {
	slug := safePathComponent(branch, "branch", 80)
	sum := sha256.Sum256([]byte(branch))
	return filepath.ToSlash(filepath.Join(factsDirName, slug+"-"+hex.EncodeToString(sum[:4])))
}

func factsFileRelPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), factsFileName))
}

// loadFacts reads a branch's facts.ndjson. A missing file is not an error: it
// yields an empty slice so first-run callers do not special-case it. Blank
// lines are skipped; a malformed line is a hard error so a corrupt store is
// surfaced rather than silently dropping records.
func loadFacts(brainDir, branch string) ([]factRecord, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath(branch)))
	return parseFactsFile(path)
}

// parseFactsFile reads a facts.ndjson file at an absolute path. A missing file
// yields an empty slice; a malformed line is a hard error so a corrupt store is
// surfaced rather than silently dropping records.
func parseFactsFile(path string) ([]factRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 8*1024), factsMaxLineBytes)
	var records []factRecord
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var record factRecord
		if err := json.Unmarshal([]byte(text), &record); err != nil {
			return nil, fmt.Errorf("parse %s line %d: %w", filepath.Base(path), line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// writeFacts persists a branch's facts as newline-delimited JSON, one record
// per line, sorted deterministically so the file is stable across rebuilds and
// diffs cleanly. The branch directory is created if needed.
func writeFacts(brainDir, branch string, records []factRecord) error {
	sortFactRecords(records)
	var buf strings.Builder
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	dir := filepath.Join(brainDir, filepath.FromSlash(factsBranchRelDir(branch)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create facts directory: %w", err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath(branch)))
	return writeFileAtomic(path, []byte(buf.String()), 0o600)
}

// sortFactRecords orders records by path, then text, then id so the on-disk
// ndjson is deterministic regardless of insertion order.
func sortFactRecords(records []factRecord) {
	sort.Slice(records, func(i, j int) bool {
		li, lj := strings.Join(records[i].Paths, ","), strings.Join(records[j].Paths, ",")
		if li != lj {
			return li < lj
		}
		if records[i].Text != records[j].Text {
			return records[i].Text < records[j].Text
		}
		return records[i].ID < records[j].ID
	})
}

// upsertFact inserts incoming into the set keyed by id, or — when a fact with
// the same id already exists — unions the new provenance anchors into the
// existing fact and advances its UpdatedAt. Exact duplicates (same normalized
// text and paths) therefore collapse for free, which is what makes re-distilling
// a turn idempotent. It returns the updated set; the input slice may be reused.
//
// upsertFact deliberately does not implement merge/supersede across *different*
// ids: that is the agent's judgment during distillation, handled separately.
func upsertFact(records []factRecord, incoming factRecord) []factRecord {
	for i := range records {
		if records[i].ID != incoming.ID {
			continue
		}
		records[i].Provenance = unionFactAnchors(records[i].Provenance, incoming.Provenance)
		if incoming.UpdatedAt.After(records[i].UpdatedAt) {
			records[i].UpdatedAt = incoming.UpdatedAt
		}
		return records
	}
	return append(records, incoming)
}

// unionFactAnchors appends anchors from b that are not already present in a,
// preserving order. Anchors are compared on their identifying fields so the
// same source turn is never recorded twice.
func unionFactAnchors(a, b []factAnchor) []factAnchor {
	seen := make(map[string]struct{}, len(a)+len(b))
	key := func(anchor factAnchor) string {
		return strings.Join([]string{anchor.SessionID, anchor.Commit, anchor.CheckpointID, anchor.TurnID, anchor.Transcript, fmt.Sprint(anchor.Line)}, "\x00")
	}
	out := make([]factAnchor, 0, len(a)+len(b))
	for _, anchor := range a {
		k := key(anchor)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, anchor)
	}
	for _, anchor := range b {
		k := key(anchor)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, anchor)
	}
	return out
}

// --- taxonomy -------------------------------------------------------------

// defaultFactTaxonomy is the shipped baseline taxonomy. The top-level
// categories here are fixed in Phase A; distillation may invent a new
// three-level path only under one of these categories. The brain regenerates
// taxonomy.json from this default on refresh.
func defaultFactTaxonomy(now time.Time) factTaxonomy {
	return factTaxonomy{
		GeneratedAt: now,
		Categories: map[string]string{
			"preferences":  "How the user wants to work and how code should be written.",
			"architecture": "Resolved design, boundary, and data-flow decisions and their rationale.",
			"project":      "Stack, tooling, CI/CD, ownership, and standing project facts.",
			"workflow":     "Branching, testing, review, and release process rules.",
			"constraints":  "Non-obvious invariants, gotchas, and hard constraints.",
		},
		Paths: []factPathDef{
			{Path: "preferences.coding.style", Description: "Stated code-style preferences.", Examples: []string{"prefers table-driven tests"}},
			{Path: "preferences.workflow.general", Description: "How the user prefers to work day to day."},
			{Path: "architecture.boundaries.rationale", Description: "Why a module or package boundary exists."},
			{Path: "architecture.data.flow", Description: "How data moves through the system."},
			{Path: "project.tooling.stack", Description: "Languages, frameworks, and tools the project uses."},
			{Path: "project.ci_cd.pipeline", Description: "Build, test, and deploy pipeline facts."},
			{Path: "workflow.branching.rules", Description: "Branching and merge conventions."},
			{Path: "workflow.testing.rules", Description: "How and when tests must be run."},
			{Path: "constraints.invariants.general", Description: "Invariants that must hold across changes."},
		},
	}
}

// loadFactTaxonomy reads the active taxonomy snapshot, falling back to the
// shipped default when the file is absent so callers always get a usable
// taxonomy. A present-but-malformed file is a hard error.
func loadFactTaxonomy(brainDir string, now time.Time) (factTaxonomy, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(factsTaxonomyPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultFactTaxonomy(now), nil
		}
		return factTaxonomy{}, err
	}
	var taxonomy factTaxonomy
	if err := json.Unmarshal(data, &taxonomy); err != nil {
		return factTaxonomy{}, fmt.Errorf("parse %s: %w", factsTaxonomyPath, err)
	}
	return taxonomy, nil
}

// writeFactTaxonomy persists the active taxonomy snapshot.
func writeFactTaxonomy(brainDir string, taxonomy factTaxonomy) error {
	data, err := json.MarshalIndent(taxonomy, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Join(brainDir, factsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create facts directory: %w", err)
	}
	return writeFileAtomic(filepath.Join(brainDir, filepath.FromSlash(factsTaxonomyPath)), data, 0o600)
}

// factTaxonomyTopLevels returns the set of top-level categories the taxonomy
// recognizes. A fact whose path's top-level is absent from this set is an
// orphan (reported, never auto-deleted).
func factTaxonomyTopLevels(taxonomy factTaxonomy) map[string]struct{} {
	tops := make(map[string]struct{}, len(taxonomy.Categories))
	for category := range taxonomy.Categories {
		tops[category] = struct{}{}
	}
	return tops
}

// factPathKnownTopLevel reports whether path's top-level category exists in the
// taxonomy. This is the conservative drift check: a fact keeps its assigned
// paths literally, and only its top-level category must still exist for it to
// remain valid.
func factPathKnownTopLevel(taxonomy factTaxonomy, path string) bool {
	_, ok := factTaxonomyTopLevels(taxonomy)[factTopLevel(path)]
	return ok
}

// --- manifest summary -----------------------------------------------------

// summarizeFactSource folds a per-branch view of the fact store into the
// source manifest recorded under sources.facts in the brain manifest.
func summarizeFactSource(now time.Time, byBranch map[string][]factRecord, chunksScanned, chunksDistilled, proposals int, warnings []string) *factSourceManifest {
	source := &factSourceManifest{
		GeneratedAt:     now,
		TaxonomyPath:    factsTaxonomyPath,
		Proposals:       proposals,
		ChunksScanned:   chunksScanned,
		ChunksDistilled: chunksDistilled,
		Warnings:        append([]string(nil), warnings...),
	}
	branches := make([]string, 0, len(byBranch))
	for branch := range byBranch {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	source.Branches = branches
	for _, branch := range branches {
		for _, record := range byBranch[branch] {
			source.Facts++
			switch record.Origin {
			case factOriginDistilled:
				source.Distilled++
			case factOriginAuthored:
				source.Authored++
			}
			if record.Status == factStatusSuperseded {
				source.Superseded++
			}
			for _, anchor := range record.Provenance {
				if anchor.Verified {
					source.Verified++
				} else {
					source.Unsigned++
				}
			}
		}
	}
	return source
}
