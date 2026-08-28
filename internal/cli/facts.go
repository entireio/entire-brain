package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

const (
	factsDirName          = "facts"
	factsTaxonomyFileName = "taxonomy.json"
	factsTaxonomyPath     = factsDirName + "/" + factsTaxonomyFileName
	factsFileName         = "facts.ndjson"
	factsMaxPerChunk      = 6 // quality-gate output cap per source chunk

	factOriginDistilled = "distilled"
	factOriginAuthored  = "authored"
)

// factSourceManifest is recorded under sources.facts in the brain manifest,
// parallel to historySourceManifest and the semantic source metadata.
type factSourceManifest struct {
	GeneratedAt       time.Time                 `json:"generated_at"`
	TaxonomyPath      string                    `json:"taxonomy_path"`
	Branches          []string                  `json:"branches,omitempty"`
	Facts             int                       `json:"facts"`
	Distilled         int                       `json:"distilled"`
	Authored          int                       `json:"authored"`
	Superseded        int                       `json:"superseded"`
	Proposals         int                       `json:"proposals"`
	Verified          int                       `json:"verified"`
	Unsigned          int                       `json:"unsigned"`
	ByKind            map[string]int            `json:"by_kind,omitempty"`
	ChunksScanned     int                       `json:"chunks_scanned"`
	ChunksDistilled   int                       `json:"chunks_distilled"`
	CacheHits         int                       `json:"cache_hits,omitempty"`
	FailedChunks      int                       `json:"failed_chunks,omitempty"`
	PreprocessedBytes int64                     `json:"preprocessed_bytes,omitempty"`
	Agent             string                    `json:"agent,omitempty"`
	Model             string                    `json:"model,omitempty"`
	Effort            string                    `json:"effort,omitempty"`
	Branch            string                    `json:"branch,omitempty"`
	Force             bool                      `json:"force,omitempty"`
	Jobs              int                       `json:"jobs,omitempty"`
	ExtractionJobsCap int                       `json:"extraction_jobs_cap,omitempty"`
	MaxChunkBytes     int                       `json:"max_chunk_bytes,omitempty"`
	Confidence        float64                   `json:"confidence_threshold,omitempty"`
	ExtractionCalls   int                       `json:"extraction_agent_calls,omitempty"`
	ReconcileCalls    int                       `json:"reconcile_agent_calls,omitempty"`
	TotalAgentCalls   int                       `json:"total_agent_calls,omitempty"`
	TokenUsage        *distillTokenUsageSummary `json:"token_usage,omitempty"`
	// ExtractionWaitSeconds is the consumer's WAIT on prefetched results, not
	// agent compute: with concurrency > 1, calls completing in the background
	// register ~0s here. Compare total_seconds across --jobs settings for
	// scheduling claims.
	ExtractionWaitSeconds float64  `json:"extraction_wait_seconds,omitempty"`
	ReconcileSeconds      float64  `json:"reconcile_seconds,omitempty"`
	WriteSeconds          float64  `json:"write_seconds,omitempty"`
	TotalSeconds          float64  `json:"total_seconds,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

// factRecord (factmerge.Record) and factAnchor (factmerge.Anchor) are defined
// as type aliases in facts_aliases.go; the durable-fact data model now lives in
// internal/factmerge.

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

// normalizeFactText, factRecordID, validFactPath, and normalizeFactPaths moved
// to internal/factmerge (NormalizeText/RecordID/ValidPath/NormalizePaths); the
// CLI names are forwarding wrappers in facts_aliases.go.

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
// surfaced rather than silently dropping records. The byte-level scan lives in
// factmerge.ParseNDJSON; this keeps only the file/path handling and restores the
// historical "parse <file> line N: ..." message the core cannot know the path
// for.
func parseFactsFile(path string) ([]factRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	records, err := factmerge.ParseNDJSON(f)
	if err != nil {
		var perr *factmerge.ParseError
		if errors.As(err, &perr) {
			return nil, fmt.Errorf("parse %s line %d: %w", filepath.Base(path), perr.Line, perr.Err)
		}
		return nil, err
	}
	return records, nil
}

// missingFactBranchStores returns the branches a fact source manifest declares
// whose facts.ndjson is no longer on disk.
//
// loadFacts treats an absent file as an empty branch (first-run callers must
// not special-case it), so a deleted or never-restored fact store is
// indistinguishable from "this branch has no facts" on every read surface.
// The manifest, however, names exactly which branches the store is supposed to
// hold, and that claim is checkable. Anything it names but cannot produce is a
// source that will silently contribute nothing.
func missingFactBranchStores(brainDir string, source *factSourceManifest) []string {
	if source == nil || brainDir == "" {
		return nil
	}
	var missing []string
	for _, branch := range source.Branches {
		if branch == "" {
			continue
		}
		path := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath(branch)))
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, branch)
		}
	}
	return missing
}

// missingFactStoreWarning renders the caller-facing warning for the branches
// missingFactBranchStores found, or "" when the declared store is intact.
func missingFactStoreWarning(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"facts declared for branch(es) %s but their %s is missing from the brain; those facts cannot be recalled — run `entire brain refresh` and re-distill",
		strings.Join(missing, ", "), factsFileName,
	)
}

// writeFacts persists a branch's facts as newline-delimited JSON, one record
// per line, sorted deterministically so the file is stable across rebuilds and
// diffs cleanly. The branch directory is created if needed. The byte-level
// sort+marshal lives in factmerge.WriteNDJSON; this keeps the atomic,
// symlink-rejecting disk write in the CLI.
func writeFacts(brainDir, branch string, records []factRecord) error {
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, records); err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, factsFileRelPath(branch), buf.Bytes(), 0o600)
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
	return writeBrainRelativeFileAtomic(brainDir, factsTaxonomyPath, data, 0o600)
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

// sortedTaxonomyTopLevels returns the taxonomy's top-level categories sorted
// alphabetically, for stable user-facing messages (e.g. rejecting an explicit
// --path under an unknown category).
func sortedTaxonomyTopLevels(taxonomy factTaxonomy) []string {
	cats := make([]string, 0, len(taxonomy.Categories))
	for category := range taxonomy.Categories {
		cats = append(cats, category)
	}
	sort.Strings(cats)
	return cats
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
		ByKind:          map[string]int{},
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
			// by_kind counts only active facts, matching what recall/tree surface
			// (superseded/retracted facts are not retrievable), so the histogram
			// answers "what kinds can I recall" rather than paralleling Facts.
			if record.Status == factStatusActive {
				source.ByKind[factKindOrInferred(record)]++
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
