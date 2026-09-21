package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Importing memory from another tool.
//
// Brain could export — `bundle` — and could not import. That asymmetry is
// exactly the friction someone hits when they try to switch to us: everything
// they have already recorded stays where it is, and Brain starts empty. cognee
// ships importers for mem0, Letta, Zep and Graphiti for this reason.
//
// Two honesty constraints shape the whole file:
//
//  1. An imported memory has no provenance in THIS repository. It was recorded
//     somewhere else, against something else. Brain's facts carry anchors —
//     commits, sessions, checkpoints — and inventing one here would launder a
//     foreign assertion into evidence. Imported facts therefore carry an
//     explicit "imported" origin and an anchor naming the source tool and the
//     foreign id, and `verify` will report them as unverifiable-here, which is
//     the truth.
//
//  2. The mem0 mapping below is written from mem0's published API reference,
//     not from a real export we ran. Field names come from their documented
//     memory object. Where their model has no counterpart in ours — TTL via
//     expiration_date is the clear one — the importer says so rather than
//     silently dropping it.
const factOriginImported = "imported"

// importedMemory is the normalised shape every adapter produces. Keeping the
// adapters thin and the fact construction single means a new source is a field
// mapping, not another copy of the provenance rules.
type importedMemory struct {
	ForeignID  string
	Text       string
	Categories []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ReplacedBy string
	// Unsupported records what the source expressed and Brain has no way to
	// represent, so the import report can name it instead of losing it quietly.
	Unsupported []string
}

// mem0Memory mirrors mem0's documented memory object. Only the fields that
// survive the crossing are declared; the rest are deliberately ignored and
// reported rather than half-mapped.
type mem0Memory struct {
	ID             string            `json:"id"`
	Memory         string            `json:"memory"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	Categories     []string          `json:"categories"`
	Metadata       map[string]any    `json:"metadata"`
	ReplacedBy     string            `json:"replaced_by"`
	ExpirationDate string            `json:"expiration_date"`
	UserID         string            `json:"user_id"`
	AgentID        string            `json:"agent_id"`
	Structured     map[string]any    `json:"structured_attributes"`
	Synthesized    bool              `json:"synthesized"`
	Extra          map[string]string `json:"-"`
}

// parseMem0 accepts either a bare array of memories or the paginated envelope
// {"results": [...]}, because both shapes appear depending on which endpoint an
// export came from. A file that is neither is an error naming both, not a
// silent zero-record import.
func parseMem0(data []byte) ([]importedMemory, int, error) {
	var direct []mem0Memory
	if err := json.Unmarshal(data, &direct); err != nil {
		// Unmarshalling into a struct SUCCEEDS for any JSON object, leaving
		// Results nil — so decoding alone cannot tell "an export with no
		// memories" from "not an export at all". The key has to be looked for
		// explicitly, or {"nope":true} imports zero records and reports
		// success, which is the silent no-op this error exists to prevent.
		var envelope struct {
			Results *[]mem0Memory `json:"results"`
		}
		if envErr := json.Unmarshal(data, &envelope); envErr != nil || envelope.Results == nil {
			return nil, 0, fmt.Errorf("not a mem0 export: expected a JSON array of memories, or an object with a \"results\" array")
		}
		direct = *envelope.Results
	}
	out := make([]importedMemory, 0, len(direct))
	skipped := 0
	for _, m := range direct {
		text := strings.TrimSpace(m.Memory)
		if text == "" {
			// A memory with no text is not a fact. Skipping is right; skipping
			// silently is not, so it is counted and reported.
			skipped++
			continue
		}
		mem := importedMemory{
			ForeignID:  strings.TrimSpace(m.ID),
			Text:       text,
			Categories: m.Categories,
			CreatedAt:  parseImportTime(m.CreatedAt),
			UpdatedAt:  parseImportTime(m.UpdatedAt),
			ReplacedBy: strings.TrimSpace(m.ReplacedBy),
		}
		// Brain has no TTL. Dropping an expiry without saying so would turn a
		// memory its owner scheduled to disappear into one that never does.
		if len(m.Metadata) > 0 {
			// Named, not dropped. A mem0 export can carry meaningful content
			// in metadata, and this file's own contract is that anything the
			// source expresses and Brain cannot represent is reported rather
			// than lost quietly — which is the whole reason somebody would
			// trust an import enough to switch.
			mem.Unsupported = append(mem.Unsupported,
				"metadata (Brain facts carry taxonomy paths and provenance, not arbitrary key/value pairs)")
		}
		if len(m.Structured) > 0 {
			mem.Unsupported = append(mem.Unsupported,
				"structured_attributes (Brain has no structured-attribute model; the fact text is imported without them)")
		}
		if strings.TrimSpace(m.ExpirationDate) != "" {
			mem.Unsupported = append(mem.Unsupported, "expiration_date (Brain has no TTL; the fact is imported without one)")
		}
		// mem0 scopes by user and agent. Brain scopes by repository and branch,
		// so the scoping does not survive and is preserved as text rather than
		// pretended away.
		if strings.TrimSpace(m.UserID) != "" || strings.TrimSpace(m.AgentID) != "" {
			mem.Unsupported = append(mem.Unsupported, "user_id/agent_id (Brain scopes by repository and branch, not by user)")
		}
		out = append(out, mem)
	}
	return out, skipped, nil
}

func parseImportTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// importTaxonomyPath maps a foreign category onto a Brain taxonomy path. A
// source's categories are free text, so they cannot be trusted to land under a
// known top level; everything is filed under the caller's prefix, which keeps
// imported knowledge visibly separate from what this repository established
// itself until someone reclassifies it.
func importTaxonomyPath(prefix string, categories []string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "project.imported"
	}
	segment := "general"
	for _, category := range categories {
		if cleaned := sanitizeImportSegment(category); cleaned != "" {
			segment = strings.ToLower(cleaned)
			break
		}
	}
	return prefix + "." + segment
}

// importedFactPaths normalises the taxonomy path the way every other
// fact-construction path does.
//
// A path that does not equal its own normalisation violates the identity
// invariant VerifyIdentity enforces and `facts sync` relies on, so a record
// built from a mixed-case --path-prefix would be rejected downstream by the
// very check meant to protect it. Normalising here — before the id is derived
// from the paths — means the id matches the path that is actually stored.
func importedFactPaths(prefix string, categories []string) []string {
	paths := normalizeFactPaths([]string{importTaxonomyPath(prefix, categories)})
	if len(paths) == 0 {
		// Unreachable while the prefix is validated up front and the sanitiser
		// emits only taxonomy-legal characters — both of which this file now
		// guarantees, so there is no test that reaches this line. It stays
		// because an unpathed fact is unfindable, and a future change to either
		// of those two guarantees should not be able to produce one silently.
		paths = normalizeFactPaths([]string{importTaxonomyPath("", nil)})
	}
	return paths
}

// validateImportPrefix rejects a --path-prefix that cannot survive
// normalisation, up front and by name.
//
// Failing here beats importing a thousand memories under a path the rest of the
// system will not accept, and beats silently rewriting what the user typed into
// something they did not ask for.
func validateImportPrefix(prefix string) error {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil // the default is used instead
	}
	normalized := normalizeFactPaths([]string{prefix + ".general"})
	if len(normalized) != 1 {
		return fmt.Errorf("--path-prefix %q is not a usable taxonomy prefix (expected category.subcategory)", prefix)
	}
	// Compared against what the user typed, not a lowercased copy of it:
	// lowercasing first would accept "Mixed.Case" and then quietly store
	// something else, which is the substitution this check exists to refuse.
	if normalized[0] != prefix+".general" {
		return fmt.Errorf("--path-prefix %q is not in normal form; use %q",
			prefix, strings.TrimSuffix(normalized[0], ".general"))
	}
	return nil
}

// sanitizeImportSegment keeps a foreign category to characters usable in a
// taxonomy segment. Categories are free text from another tool, so they are
// normalised rather than trusted.
// sanitizeImportSegment reduces a foreign category to one taxonomy segment.
//
// The output has to satisfy the taxonomy pattern, which admits only [a-z0-9_]
// — a hyphen is not allowed, so a category like "work-notes" produced a path
// that normalisation silently dropped, leaving the record with no paths at all.
func sanitizeImportSegment(segment string) string {
	segment = strings.ToLower(strings.TrimSpace(segment))
	var b strings.Builder
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

type factImportReport struct {
	Source      string   `json:"source"`
	File        string   `json:"file"`
	Read        int      `json:"read"`
	Imported    int      `json:"imported"`
	SkippedText int      `json:"skipped_empty_text"`
	Superseded  int      `json:"marked_superseded"`
	Unsupported []string `json:"unsupported,omitempty"`
	DryRun      bool     `json:"dry_run"`
}

// memoriesToFacts converts normalised memories into fact records. now supplies
// the timestamp for any memory whose source carried none, so an undated import
// is dated at import time rather than at the zero value.
func memoriesToFacts(memories []importedMemory, skipped int, source, prefix, branch string, now time.Time) ([]factRecord, factImportReport) {
	// Read is what the file contained, not what survived parsing: "3 of 3"
	// when the file held four is exactly the silent drop this reports against.
	report := factImportReport{Source: source, Read: len(memories) + skipped, SkippedText: skipped}
	unsupported := map[string]bool{}
	facts := make([]factRecord, 0, len(memories))
	// Supersession is resolved after the loop, because a memory can be
	// replaced by one that appears later in the export and every fact needs
	// its id before any reference to it can be written.
	byForeignID := make(map[string]int, len(memories))
	replaces := make([]string, 0, len(memories))
	for _, mem := range memories {
		paths := importedFactPaths(prefix, mem.Categories)
		created, updated := mem.CreatedAt, mem.UpdatedAt
		if created.IsZero() {
			created = now
		}
		if updated.IsZero() {
			updated = created
		}
		status := factStatusActive
		if mem.ReplacedBy != "" {
			// The source already superseded this memory. Importing it as
			// active would resurrect something its owner retired.
			status = factStatusSuperseded
			report.Superseded++
		}
		facts = append(facts, factRecord{
			ID:     factRecordID(mem.Text, paths),
			Paths:  paths,
			Text:   mem.Text,
			Branch: branch,
			Origin: factOriginImported,
			Status: status,
			// The anchor names where this came from. It is not evidence from
			// this repository and must not read like it: verify will report
			// these as unverifiable-here, correctly.
			Provenance: []factAnchor{{SessionID: source + ":" + mem.ForeignID}},
			CreatedAt:  created,
			UpdatedAt:  updated,
		})
		byForeignID[mem.ForeignID] = len(facts) - 1
		replaces = append(replaces, mem.ReplacedBy)
		for _, note := range mem.Unsupported {
			unsupported[note] = true
		}
	}
	// SupersededBy has to name a fact in THIS store. Writing the foreign id
	// would leave a reference that reconciliation and display look up and never
	// find — a dangling pointer dressed as lineage, which is worse than no
	// lineage because it reads as though it resolves. Where the replacement was
	// imported too, the reference is real; where it was not, the link cannot be
	// represented and is reported rather than invented.
	danglingSupersessions := 0
	for i, foreignReplacement := range replaces {
		if foreignReplacement == "" {
			continue
		}
		target, ok := byForeignID[foreignReplacement]
		if !ok {
			danglingSupersessions++
			continue
		}
		facts[i].SupersededBy = facts[target].ID
	}
	if danglingSupersessions > 0 {
		unsupported[fmt.Sprintf(
			"supersession target outside this export (%d fact(s) are marked superseded with no successor in the store)",
			danglingSupersessions)] = true
	}

	for note := range unsupported {
		report.Unsupported = append(report.Unsupported, note)
	}
	sort.Strings(report.Unsupported)
	report.Imported = len(facts)
	return facts, report
}

func newFactsImportCommand(opts Options) *cobra.Command {
	var (
		source  string
		file    string
		prefix  string
		branch  string
		dryRun  bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import durable facts from another memory tool's export",
		Long: "Import memories exported from another tool as durable facts.\n\n" +
			"Imported facts carry origin \"imported\" and an anchor naming the source tool and\n" +
			"the foreign id. They have no provenance in this repository, so `verify` reports\n" +
			"them as unverifiable-here — that is accurate, not a failure.\n\n" +
			"The mem0 mapping follows mem0's published API reference. Anything the source\n" +
			"expresses that Brain cannot represent is named in the report rather than dropped\n" +
			"silently.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(file) == "" {
				return fmt.Errorf("--file is required")
			}
			data, err := readImportFile(file)
			if err != nil {
				return err
			}
			var memories []importedMemory
			var skipped int
			switch strings.ToLower(strings.TrimSpace(source)) {
			case "mem0":
				memories, skipped, err = parseMem0(data)
			default:
				return fmt.Errorf("unknown --source %q (supported: mem0)", source)
			}
			if err != nil {
				return err
			}
			if err := validateImportPrefix(prefix); err != nil {
				return err
			}
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			facts, report := memoriesToFacts(memories, skipped, strings.ToLower(source), prefix, resolvedBranch, opts.Now().UTC())
			report.File = file
			report.DryRun = dryRun

			if !dryRun {
				if err := withBrainWriteLock(brainDir, func() error {
					existing, loadErr := loadFacts(brainDir, resolvedBranch)
					if loadErr != nil {
						return loadErr
					}
					for _, fact := range facts {
						existing = upsertFact(existing, fact)
					}
					if err := writeFacts(brainDir, resolvedBranch, existing); err != nil {
						return err
					}
					// Every other fact mutator refreshes the source manifest
					// under the same lock, and import must too: `facts status`,
					// the integrity checks and the freshness report all read
					// their counts and generation time from it, so an import
					// that skipped this left them describing the brain as it
					// was before the import — stale, and silently so.
					return updateFactSourceManifestLocked(brainDir, opts.Now().UTC())
				}); err != nil {
					return err
				}
			}
			if jsonOut {
				return writeIndentedJSON(cmd.OutOrStdout(), report)
			}
			verb := "imported"
			if dryRun {
				verb = "would import"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %d of %d memories from %s\n", verb, report.Imported, report.Read, report.Source)
			if report.SkippedText > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  %d skipped: no memory text\n", report.SkippedText)
			}
			if report.Superseded > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  %d already superseded at the source; imported as superseded\n", report.Superseded)
			}
			for _, note := range report.Unsupported {
				fmt.Fprintf(cmd.OutOrStdout(), "  not carried over: %s\n", note)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "mem0", "Source tool the export came from (mem0)")
	cmd.Flags().StringVar(&file, "file", "", "Path to the export file, or - for stdin")
	cmd.Flags().StringVar(&prefix, "path-prefix", "project.imported", "Taxonomy prefix imported facts are filed under")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to import onto (default: current)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be imported without writing")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the report as JSON")
	return cmd
}

// readImportFile reads an export, bounded.
//
// The file is named by the operator, but that is not a reason to read it
// unboundedly: a 16 GiB export is a memory-exhaustion problem whether it was
// chosen deliberately or by mistake, and a FIFO or device file at that path
// blocks forever rather than failing. safeReadFile and safeReadAll are what
// every other untrusted-input surface here uses, for exactly these two
// reasons.
func readImportFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "-" {
		return safeReadAll(os.Stdin, maxManifestBytes, "import file from stdin")
	}
	data, err := safeReadFile(path, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read export: %w", err)
	}
	return data, nil
}
