package cli

import (
	"encoding/json"
	"fmt"
	"io"
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

// sanitizeImportSegment keeps a foreign category to characters usable in a
// taxonomy segment. Categories are free text from another tool, so they are
// normalised rather than trusted.
func sanitizeImportSegment(segment string) string {
	segment = strings.TrimSpace(segment)
	var b strings.Builder
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
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
	for _, mem := range memories {
		paths := []string{importTaxonomyPath(prefix, mem.Categories)}
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
			status = "superseded"
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
		for _, note := range mem.Unsupported {
			unsupported[note] = true
		}
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
					return writeFacts(brainDir, resolvedBranch, existing)
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

func readImportFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "-" {
		return io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read export: %w", err)
	}
	return data, nil
}
