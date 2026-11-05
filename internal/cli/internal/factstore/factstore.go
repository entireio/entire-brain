package factstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

const (
	DirName          = "facts"
	TaxonomyFileName = "taxonomy.json"
	TaxonomyPath     = DirName + "/" + TaxonomyFileName
	FileName         = "facts.ndjson"
	MaxPerChunk      = 6 // quality-gate output cap per source chunk

	OriginDistilled = "distilled"
	OriginAuthored  = "authored"
	OriginImported  = "imported"
)

// factmerge.Record (factmerge.Record) and factmerge.Anchor (factmerge.Anchor) are defined
// as type aliases in facts_aliases.go; the durable-fact data model now lives in
// factmerge.

// Taxonomy is the active taxonomy snapshot. Paths are validated against
// factPathPattern; classification may only invent a new three-level path under
// an existing top-level category.
type Taxonomy struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Categories  map[string]string `json:"categories"` // top-level -> description
	Paths       []PathDef         `json:"paths"`
}

type PathDef struct {
	Path        string   `json:"path"`
	Description string   `json:"description"`
	Examples    []string `json:"examples,omitempty"`
}

// normalizeFactText, factRecordID, validFactPath, and normalizeFactPaths moved
// to factmerge (NormalizeText/RecordID/ValidPath/NormalizePaths); the
// CLI names are forwarding wrappers in facts_aliases.go.

// TopLevel returns the top-level category of a taxonomy path (the segment
// before the first dot), or "" if the path is empty.
func TopLevel(path string) string {
	if i := strings.IndexByte(path, '.'); i >= 0 {
		return path[:i]
	}
	return path
}

// --- on-disk layout -------------------------------------------------------

// The flat store (facts/store.json) and its legacy-layout folding live in
// repo_fact_store.go and repo_fact_compat.go. Load and Write are defined
// there; branch is metadata on each record, never a storage key.

// parseFactsFile reads a facts.ndjson file at an absolute path. A missing file
// yields an empty slice; a malformed line is a hard error so a corrupt store is
// surfaced rather than silently dropping records. The byte-level scan lives in
// factmerge.ParseNDJSON; this keeps only the file/path handling and restores the
// historical "parse <file> line N: ..." message the core cannot know the path
// for.
func parseFactsFile(path string) ([]factmerge.Record, error) {
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

// --- taxonomy -------------------------------------------------------------

// DefaultTaxonomy is the shipped baseline taxonomy. The top-level
// categories here are fixed in Phase A; distillation may invent a new
// three-level path only under one of these categories. The brain regenerates
// taxonomy.json from this default on refresh.
func DefaultTaxonomy(now time.Time) Taxonomy {
	return Taxonomy{
		GeneratedAt: now,
		Categories: map[string]string{
			"preferences":  "How the user wants to work and how code should be written.",
			"architecture": "Resolved design, boundary, and data-flow decisions and their rationale.",
			"project":      "Stack, tooling, CI/CD, ownership, and standing project facts.",
			"workflow":     "Branching, testing, review, and release process rules.",
			"constraints":  "Non-obvious invariants, gotchas, and hard constraints.",
		},
		Paths: []PathDef{
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

// LoadTaxonomy reads the active taxonomy snapshot, falling back to the
// shipped default when the file is absent so callers always get a usable
// taxonomy. A present-but-malformed file is a hard error.
func LoadTaxonomy(brainDir string, now time.Time) (Taxonomy, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(TaxonomyPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultTaxonomy(now), nil
		}
		return Taxonomy{}, err
	}
	var taxonomy Taxonomy
	if err := json.Unmarshal(data, &taxonomy); err != nil {
		return Taxonomy{}, fmt.Errorf("parse %s: %w", TaxonomyPath, err)
	}
	return taxonomy, nil
}

// WriteTaxonomy persists the active taxonomy snapshot.
func WriteTaxonomy(brainDir string, taxonomy Taxonomy) error {
	data, err := json.MarshalIndent(taxonomy, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return brainstore.WriteBrainRelativeFileAtomic(brainDir, TaxonomyPath, data, 0o600)
}

// TaxonomyTopLevels returns the set of top-level categories the taxonomy
// recognizes. A fact whose path's top-level is absent from this set is an
// orphan (reported, never auto-deleted).
func TaxonomyTopLevels(taxonomy Taxonomy) map[string]struct{} {
	tops := make(map[string]struct{}, len(taxonomy.Categories))
	for category := range taxonomy.Categories {
		tops[category] = struct{}{}
	}
	return tops
}

// SortedTaxonomyTopLevels returns the taxonomy's top-level categories sorted
// alphabetically, for stable user-facing messages (e.g. rejecting an explicit
// --path under an unknown category).
func SortedTaxonomyTopLevels(taxonomy Taxonomy) []string {
	cats := make([]string, 0, len(taxonomy.Categories))
	for category := range taxonomy.Categories {
		cats = append(cats, category)
	}
	sort.Strings(cats)
	return cats
}

// PathKnownTopLevel reports whether path's top-level category exists in the
// taxonomy. This is the conservative drift check: a fact keeps its assigned
// paths literally, and only its top-level category must still exist for it to
// remain valid.
func PathKnownTopLevel(taxonomy Taxonomy, path string) bool {
	_, ok := TaxonomyTopLevels(taxonomy)[TopLevel(path)]
	return ok
}

// TaxonomyBlock renders the taxonomy as the prompt section the agent uses to
// classify facts: the fixed top-level categories and the known three-level
// paths with descriptions. Output is deterministic (sorted) so the rendered
// prompt is stable across runs.
func TaxonomyBlock(taxonomy Taxonomy) string {
	var b strings.Builder
	b.WriteString("## Taxonomy\n\n")
	b.WriteString("Top-level categories are fixed. You may invent a new three-level path only under one of these categories:\n\n")

	categories := make([]string, 0, len(taxonomy.Categories))
	for category := range taxonomy.Categories {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for _, category := range categories {
		fmt.Fprintf(&b, "- `%s` — %s\n", category, taxonomy.Categories[category])
	}

	if len(taxonomy.Paths) > 0 {
		b.WriteString("\nKnown paths (prefer these):\n\n")
		paths := append([]PathDef(nil), taxonomy.Paths...)
		sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
		for _, def := range paths {
			if def.Description != "" {
				fmt.Fprintf(&b, "- `%s` — %s\n", def.Path, def.Description)
			} else {
				fmt.Fprintf(&b, "- `%s`\n", def.Path)
			}
		}
	}
	return b.String()
}

// FilterPathsByTaxonomy keeps only paths whose top-level category exists in
// the taxonomy, recording a warning for each dropped path. Paths are already
// syntactically valid (factmerge.NormalizePaths ran first).
func FilterPathsByTaxonomy(paths []string, taxonomy Taxonomy, warnings *[]string) []string {
	tops := TaxonomyTopLevels(taxonomy)
	kept := paths[:0:0]
	for _, path := range paths {
		if _, ok := tops[TopLevel(path)]; ok {
			kept = append(kept, path)
			continue
		}
		*warnings = append(*warnings, fmt.Sprintf("dropped path under unknown category: %s", path))
	}
	return kept
}

// ValidateKindFlag rejects an unrecognized --kind value (empty = no filter).
func ValidateKindFlag(kind string) error {
	if strings.TrimSpace(kind) == "" || factmerge.ValidFactKind(kind) {
		return nil
	}
	return fmt.Errorf("--kind must be one of decision|invariant|gotcha|preference|convention")
}

// ValidateScopeFlag rejects an unrecognized --scope value.
func ValidateScopeFlag(scope string) error {
	switch scope {
	case "", ScopeLocal, ScopeCrossCutting:
		return nil
	default:
		return fmt.Errorf("--scope must be %q or %q", ScopeLocal, ScopeCrossCutting)
	}
}

// --- manifest summary -----------------------------------------------------

// SummarizeSource folds the flat fact store into the source manifest
// recorded under sources.facts in the brain manifest. Branches is left empty:
// memories are repo-wide, and the field survives only so older manifests
// still parse.
func SummarizeSource(now time.Time, records []factmerge.Record, chunksScanned, chunksDistilled, proposals int, warnings []string) *bmanifest.FactSource {
	source := &bmanifest.FactSource{
		GeneratedAt:     now,
		TaxonomyPath:    TaxonomyPath,
		Proposals:       proposals,
		ChunksScanned:   chunksScanned,
		ChunksDistilled: chunksDistilled,
		Warnings:        append([]string(nil), warnings...),
		ByKind:          map[string]int{},
	}
	for _, record := range records {
		// A stored line carrying no id is not a fact (see IsFactRecord): it
		// cannot be fetched, recalled, verified or superseded. Counting it
		// here is how a damaged store re-declared its own damage as healthy
		// on the next `remember` or `refresh`, and it is what the integrity
		// cross-check compares against.
		if !IsFactRecord(record) {
			continue
		}
		if record.Status == factmerge.StatusCandidate {
			// A staged candidate is not a published memory yet: counting it
			// into Facts would inflate every surface after a crashed run,
			// while hiding it would make the staged work invisible. Its own
			// counter keeps both honest until the next distill reconciles it.
			source.PendingReconcile++
			continue
		}
		source.Facts++
		switch record.Origin {
		case OriginDistilled:
			source.Distilled++
		case OriginAuthored:
			source.Authored++
		case OriginImported:
			// Without this bucket an imported fact raises the total and no
			// line item, so the origins visibly do not sum to the count and
			// nothing explains the difference.
			source.Imported++
		}
		if record.Status == factmerge.StatusSuperseded {
			source.Superseded++
		}
		// by_kind counts only active facts, matching what recall/tree surface
		// (superseded/retracted facts are not retrievable), so the histogram
		// answers "what kinds can I recall" rather than paralleling Facts.
		if record.Status == factmerge.StatusActive {
			source.ByKind[KindOrInferred(record)]++
		}
		for _, anchor := range record.Provenance {
			if anchor.Verified {
				source.Verified++
			} else {
				source.Unsigned++
			}
		}
	}
	return source
}
