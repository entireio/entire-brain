package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/factmerge"
	"os"
	"sort"
	"strconv"
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
	if err := json.Unmarshal(data, &direct); err != nil || direct == nil {
		// Unmarshalling into a struct SUCCEEDS for any JSON object, leaving
		// Results nil — so decoding alone cannot tell "an export with no
		// memories" from "not an export at all". The key has to be looked for
		// explicitly, or {"nope":true} imports zero records and reports
		// success, which is the silent no-op this error exists to prevent.
		var envelope struct {
			Results *[]mem0Memory `json:"results"`
		}
		if envErr := json.Unmarshal(data, &envelope); envErr != nil || envelope.Results == nil {
			// Distinguish "the shape is wrong" from "a field had the wrong
			// type", and never discard the decoder's own message.
			//
			// Both errors were previously thrown away and replaced with "not a
			// mem0 export", which is FALSE for a real export carrying one bad
			// field: `{"results":[{"id":1}]}` is unmistakably a mem0 export,
			// and the decoder already knew it was `id` that was numeric. The
			// user was told their file was the wrong kind of file and given no
			// way to find the field.
			if typeErr := firstJSONTypeError(err, envErr); typeErr != nil {
				return nil, 0, fmt.Errorf("mem0 export has an unexpected type: %w", typeErr)
			}
			return nil, 0, fmt.Errorf("not a mem0 export: expected a JSON array of memories, or an object with a \"results\" array")
		}
		direct = *envelope.Results
	}
	out := make([]importedMemory, 0, len(direct))
	skipped := 0
	for _, m := range direct {
		// Foreign text is UNTRUSTED CONTENT, not just untrusted size.
		//
		// This file already treats the input as hostile by bounding how much
		// of it it will read. It did not bound what is IN it: m.Memory was
		// stored verbatim, and printFactLine (facts_read_cmd.go) and the
		// compact brief both print f.Text unescaped. So ANSI escapes, bidi
		// overrides and zero-width runes from another tool's export reach the
		// terminal and, worse, the agent's prompt.
		//
		// sanitizeDistilledFactText exists for exactly this and was already
		// applied to agent-produced text in distill.go. Text arriving from a
		// third-party tool has strictly weaker provenance than text this
		// brain's own agent produced, so it cannot be held to a weaker
		// standard. Stripping is counted and reported rather than silent: a
		// fact whose text changed on the way in is something the owner should
		// be able to see.
		text, stripped := sanitizeDistilledFactText(strings.TrimSpace(m.Memory))
		text = strings.TrimSpace(text)
		if text == "" {
			// A memory with no text is not a fact. Skipping is right; skipping
			// silently is not, so it is counted and reported.
			skipped++
			continue
		}
		createdAt, createdBad := parseImportTime(m.CreatedAt)
		updatedAt, updatedBad := parseImportTime(m.UpdatedAt)
		// The foreign IDS are untrusted text too, not just the memory body.
		//
		// m.Memory goes through sanitizeDistilledFactText above because
		// unescaped foreign text reaches the terminal. The ids take the same
		// path and were left raw: ForeignID is embedded in the anchor as
		// "<tool>:<id>", and `facts show` prints anchor.SessionID straight
		// through valueOrUnset with %s (facts_read_cmd.go:540), which neither
		// quotes nor strips. An export with ANSI escapes, bidi overrides or
		// zero-width runes in `id` reached the terminal unsanitised --
		// contradicting this file's own stated threat model for foreign text.
		//
		// BOTH ids are sanitised, and that is not optional: ReplacedBy is
		// matched against ForeignID through byForeignID, so cleaning one and
		// not the other would silently break every supersession whose id
		// happened to contain a stripped character.
		foreignID, foreignIDStripped := sanitizeDistilledFactText(strings.TrimSpace(m.ID))
		replacedBy, replacedByStripped := sanitizeDistilledFactText(strings.TrimSpace(m.ReplacedBy))
		mem := importedMemory{
			ForeignID:  foreignID,
			Text:       text,
			Categories: m.Categories,
			CreatedAt:  createdAt,
			UpdatedAt:  updatedAt,
			ReplacedBy: replacedBy,
		}
		if foreignIDStripped || replacedByStripped {
			mem.Unsupported = append(mem.Unsupported,
				"control, bidi or zero-width characters in a source id (stripped on import; the id is printed by `facts show` and embedded in the fact's anchor)")
		}
		if createdBad || updatedBad {
			// The caller substitutes `now` for a zero time, so an unparseable
			// timestamp re-dates the fact to the import date. Say so: recency
			// ordering is what these fields exist for, and silently stamping
			// every fact with today is worse than carrying no date at all,
			// because it looks like real information.
			mem.Unsupported = append(mem.Unsupported, fmt.Sprintf(
				"a timestamp this import could not read (%s); the fact is dated at import time instead, so its position in recency order is not the source's",
				unreadableTimestampDetail(m, createdBad, updatedBad)))
		}
		if stripped {
			// Reuse the channel this file already has for "the source said
			// something we changed or could not keep", so a fact whose text
			// was altered on the way in is visible in the report rather than
			// silently different from the export.
			mem.Unsupported = append(mem.Unsupported,
				"control, bidi or zero-width characters in the memory text (stripped on import; they render in terminals and in agent prompts)")
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

// parseImportTime returns the parsed time, and whether the value was PRESENT
// but could not be parsed.
//
// Returning only a zero time made those two cases indistinguishable, and the
// caller substitutes `now` for a zero. So an export whose timestamps are epoch
// seconds, or "2024-07-01 12:00:00" with a space instead of a T, had every
// fact silently re-dated to the import date while the report said "imported
// 400 of 400". Recency ordering is exactly what these fields are for, and
// every imported fact claiming today destroys it invisibly.
//
// The layouts below cover the shapes actually seen in mem0, Letta and
// sqlite-backed exports. Anything else is reported rather than guessed.
func parseImportTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999", // no zone (sqlite, python isoformat)
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999Z07:00",
		"2006-01-02 15:04:05.999999", // space separator
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), false
		}
	}
	// Epoch seconds or milliseconds. Requiring 10+ digits keeps a bare year or
	// a short numeric id from being read as a date; 10 digits is 2001-09-09
	// onward, and no export predates that.
	if len(value) >= 10 && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			if len(value) >= 13 {
				return time.UnixMilli(n).UTC(), false
			}
			return time.Unix(n, 0).UTC(), false
		}
	}
	return time.Time{}, true
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
func importedFactPaths(prefix string, categories []string) ([]string, []string) {
	// Use the capacity the identity model allows instead of keeping one
	// category and dropping the rest in silence.
	//
	// importTaxonomyPath takes the FIRST usable category and breaks, so a
	// memory tagged ["deployment","security"] arrived under
	// project.imported.deployment with "security" gone and nothing said. That
	// is the same silent-drop class this file refuses everywhere else, and
	// factmerge.MaxPaths is 2 -- the room for a second path was already there.
	//
	// Categories beyond the cap are returned so the caller can report them.
	// They are genuinely dropped; the cap is an identity invariant, not a
	// formatting choice.
	usable := make([]string, 0, len(categories))
	seen := map[string]bool{}
	for _, category := range categories {
		cleaned := strings.ToLower(sanitizeImportSegment(category))
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		usable = append(usable, cleaned)
	}

	var dropped []string
	if len(usable) > factmerge.MaxPaths {
		dropped = append(dropped, usable[factmerge.MaxPaths:]...)
		usable = usable[:factmerge.MaxPaths]
	}

	candidates := make([]string, 0, len(usable))
	for _, segment := range usable {
		candidates = append(candidates, importTaxonomyPathForSegment(prefix, segment))
	}
	if len(candidates) == 0 {
		candidates = append(candidates, importTaxonomyPath(prefix, nil))
	}

	paths := normalizeFactPaths(candidates)
	if len(paths) == 0 {
		// Unreachable while the prefix is validated up front and the sanitiser
		// emits only taxonomy-legal characters — both of which this file now
		// guarantees, so there is no test that reaches this line. It stays
		// because an unpathed fact is unfindable, and a future change to either
		// of those two guarantees should not be able to produce one silently.
		paths = normalizeFactPaths([]string{importTaxonomyPath("", nil)})
	}
	return paths, dropped
}

// importTaxonomyPathForSegment builds one taxonomy path from an
// already-sanitised segment, so importedFactPaths can build several without
// re-running the first-category-wins selection in importTaxonomyPath.
func importTaxonomyPathForSegment(prefix, segment string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "project.imported"
	}
	if segment == "" {
		segment = "general"
	}
	return prefix + "." + segment
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

// validateImportPrefixAgainstTaxonomy rejects a --path-prefix whose top-level
// category the taxonomy does not know.
//
// `facts add --path` already refuses this, and says why: "otherwise --path can
// mint immediately-orphaned facts the rest of the system treats as invalid"
// (facts_write_cmd.go). Import had only the shape check above, so
// `--path-prefix notes.inbox` could land a thousand facts that factmerge.GC
// reports as orphans while the command printed "imported 1000 of 1000".
//
// An explicit prefix is deliberate, exactly like an explicit --path, so an
// unknown category is a hard error rather than a silent drop that would hide
// a typo across the whole import. This runs after the brain is resolved,
// because the taxonomy lives there; the shape check stays up front so a
// malformed prefix still fails before any file is read.
func validateImportPrefixAgainstTaxonomy(brainDir, prefix string, now time.Time) error {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil // the default prefix is taxonomy-valid by construction
	}
	taxonomy, err := loadFactTaxonomy(brainDir, now)
	if err != nil {
		// A taxonomy that cannot be read is not evidence the prefix is wrong.
		// Refusing the import here would turn a brain-state problem into a
		// rejected import, so the shape check above stands alone.
		return nil
	}
	candidate := prefix + ".general"
	var warnings []string
	if kept := filterFactPathsByTaxonomy([]string{candidate}, taxonomy, &warnings); len(kept) == 1 {
		return nil
	}
	return fmt.Errorf("--path-prefix %q is not under a known taxonomy category (%s); importing under it would mint facts the rest of the system treats as orphaned",
		prefix, strings.Join(sortedTaxonomyTopLevels(taxonomy), ", "))
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
	Source string `json:"source"`
	File   string `json:"file"`
	// Branch the facts landed on. Facts are branch-scoped, so an import that
	// resolved to a different branch than the user had in mind is invisible
	// without this -- `facts recall` on another branch then reports an empty
	// corpus, which reads as a failed import rather than a misplaced one.
	Branch      string   `json:"branch,omitempty"`
	Read        int      `json:"read"`
	Imported    int      `json:"imported"`
	SkippedText int      `json:"skipped_empty_text"`
	Superseded  int      `json:"marked_superseded"`
	Unsupported []string `json:"unsupported,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	DryRun      bool     `json:"dry_run"`
}

// memoriesToFacts converts normalised memories into fact records. now supplies
// the timestamp for any memory whose source carried none, so an undated import
// is dated at import time rather than at the zero value.
func memoriesToFacts(memories []importedMemory, skipped int, source, prefix, branch string, now time.Time) ([]factRecord, factImportReport) {
	// Read is what the file contained, not what survived parsing: "3 of 3"
	// when the file held four is exactly the silent drop this reports against.
	report := factImportReport{Source: source, Branch: branch, Read: len(memories) + skipped, SkippedText: skipped}
	unsupported := map[string]bool{}
	facts := make([]factRecord, 0, len(memories))
	// Supersession is resolved after the loop, because a memory can be
	// replaced by one that appears later in the export and every fact needs
	// its id before any reference to it can be written.
	byForeignID := make(map[string]int, len(memories))
	duplicateForeignIDs := map[string]bool{}
	replaces := make([]string, 0, len(memories))
	for _, mem := range memories {
		paths, droppedCategories := importedFactPaths(prefix, mem.Categories)
		if len(droppedCategories) > 0 {
			// A fact may live under at most factmerge.MaxPaths taxonomy paths,
			// so categories past that are genuinely lost. Saying which ones
			// beats discovering later that a tag the source had cannot be
			// found anywhere in this brain.
			report.Unsupported = append(report.Unsupported, fmt.Sprintf(
				"categories beyond the %d taxonomy paths a fact may hold: %s (the fact is filed under the first %d)",
				factmerge.MaxPaths, strings.Join(droppedCategories, ", "), factmerge.MaxPaths))
		}
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
		if mem.ForeignID != "" {
			if _, seen := byForeignID[mem.ForeignID]; seen {
				// A source id that appears twice cannot identify anything. Left
				// alone, the last occurrence wins and any replaced_by naming
				// that id resolves to whichever memory happened to be last in
				// the file — a supersession pointed at an arbitrary fact, which
				// is worse than no supersession because it looks deliberate.
				duplicateForeignIDs[mem.ForeignID] = true
			}
			byForeignID[mem.ForeignID] = len(facts) - 1
		}
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
	ambiguousSupersessions := 0
	selfSupersessions := 0
	for i, foreignReplacement := range replaces {
		if foreignReplacement == "" {
			continue
		}
		if duplicateForeignIDs[foreignReplacement] {
			// Ambiguous: the id names more than one memory, so there is no
			// right answer and picking one would invent a relationship.
			ambiguousSupersessions++
			continue
		}
		target, ok := byForeignID[foreignReplacement]
		if !ok {
			danglingSupersessions++
			continue
		}
		if facts[target].ID == facts[i].ID {
			// A memory naming itself as its own replacement, either directly
			// or through two memories that normalize to one fact. Writing it
			// would mark the fact superseded by itself: a lineage that can
			// never be followed and reads as retired with nowhere to go.
			selfSupersessions++
			continue
		}
		facts[i].SupersededBy = facts[target].ID
	}
	if len(duplicateForeignIDs) > 0 {
		unsupported[fmt.Sprintf(
			"duplicate source ids (%d id(s) name more than one memory, so any supersession referring to them is ambiguous)",
			len(duplicateForeignIDs))] = true
	}
	if selfSupersessions > 0 {
		unsupported[fmt.Sprintf(
			"supersession target is the memory itself (%d fact(s) name their own id as the replacement and are left with no successor)",
			selfSupersessions)] = true
	}
	if ambiguousSupersessions > 0 {
		unsupported[fmt.Sprintf(
			"supersession target is ambiguous (%d fact(s) name a duplicated source id and are left with no successor)",
			ambiguousSupersessions)] = true
	}
	if danglingSupersessions > 0 {
		unsupported[fmt.Sprintf(
			"supersession target outside this export (%d fact(s) are marked superseded with no successor in the store)",
			danglingSupersessions)] = true
	}

	// Two memories whose text and category normalise to the same thing are the
	// same fact under a content-addressed id, and upsert correctly folds them
	// into one. What was wrong was the count: report.Imported said two, so the
	// numbers disagreed with the store and nothing explained why.
	// Duplicates that disagree on status are the dangerous half of the merge.
	// Upsert keeps the first copy's Status/SupersededBy, so which one won
	// depended on export order: a memory the source had retired could come
	// second and have its retirement silently dropped, leaving it Active here.
	// Losing a retirement is the worse direction, so the retirement is applied
	// to every copy before the merge and the conflict is reported.
	retirement := map[string]string{}
	conflictingRetirement := map[string]bool{}
	for _, fact := range facts {
		if fact.SupersededBy == "" {
			continue
		}
		switch existing := retirement[fact.ID]; {
		case existing == "":
			retirement[fact.ID] = fact.SupersededBy
		case existing != fact.SupersededBy:
			// Two copies of one fact naming different successors. There is no
			// right answer and taking the first would invent a relationship
			// out of export order, which is what the duplicate-source-id case
			// already refuses to do.
			conflictingRetirement[fact.ID] = true
		}
	}
	statusConflicts := 0
	for i := range facts {
		if conflictingRetirement[facts[i].ID] {
			facts[i].SupersededBy = ""
			continue
		}
		successor := retirement[facts[i].ID]
		if successor == "" || facts[i].SupersededBy == successor {
			continue
		}
		if facts[i].SupersededBy == "" {
			statusConflicts++
		}
		facts[i].SupersededBy = successor
		facts[i].Status = factStatusSuperseded
	}
	if len(conflictingRetirement) > 0 {
		unsupported[fmt.Sprintf(
			"supersession target disagrees between duplicates (%d fact(s) are named with more than one replacement and are left with no successor)",
			len(conflictingRetirement))] = true
	}
	if statusConflicts > 0 {
		unsupported[fmt.Sprintf(
			"%d memor(ies) duplicate a retired memory without its replacement; the retirement was kept rather than dropped on export order",
			statusConflicts)] = true
	}

	distinct := map[string]bool{}
	for _, fact := range facts {
		distinct[fact.ID] = true
	}
	if collapsed := len(facts) - len(distinct); collapsed > 0 {
		unsupported[fmt.Sprintf(
			"%d memor(ies) are identical to another under the same category and were merged into one fact",
			collapsed)] = true
	}
	for note := range unsupported {
		report.Unsupported = append(report.Unsupported, note)
	}
	sort.Strings(report.Unsupported)
	if report.Superseded > 0 {
		report.Warnings = append(report.Warnings,
			"Superseded facts keep source timestamps; facts gc may prune them immediately if they are older than its retention window. Preview facts gc without --force and adjust --retain before deleting imported history.")
	}
	report.Imported = len(distinct)
	// Count superseded in the SAME UNIT as Imported.
	//
	// It was incremented once per MEMORY while Imported counts distinct FACTS,
	// so two memories that dedupe into one fact -- both superseded at the
	// source -- reported "2 already superseded at the source" out of "1
	// imported". Two numbers in different units, presented as if they were
	// comparable, is the defect this report exists to avoid.
	report.Superseded = countSupersededFacts(facts)
	return facts, report
}

// countSupersededFacts counts superseded facts by DISTINCT ID, so the number is
// comparable with Imported, which is also a count of distinct ids. Two memories
// that collapse into one fact are one fact here too.
func countSupersededFacts(facts []factRecord) int {
	seen := map[string]bool{}
	for _, fact := range facts {
		if fact.Status == factStatusSuperseded {
			seen[fact.ID] = true
		}
	}
	return len(seen)
}

// countKeptActiveFacts reports how many imported-superseded facts are still
// active in this brain, which is the divergence the import note describes.
//
// Counted per distinct fact id rather than per memory. Duplicates collapse into
// one content-addressed fact, so a fact whose duplicates are both superseded
// would otherwise be counted once per copy and the note would claim more
// diverging facts than the brain contains.
func countKeptActiveFacts(imported, existing []factRecord) int {
	counted := map[string]bool{}
	kept := 0
	for _, fact := range imported {
		if fact.Status != factStatusSuperseded || counted[fact.ID] {
			continue
		}
		counted[fact.ID] = true
		if i := indexOfFact(existing, fact.ID); i >= 0 && existing[i].Status == factStatusActive {
			kept++
		}
	}
	return kept
}

// countImportsOntoInactiveFacts counts incoming ACTIVE facts whose id already
// exists locally with a non-active status.
//
// This is the mirror of countKeptActiveFacts, and it was missing. Upsert unions
// anchors and never moves Status or Text, which is right for a re-distill: a
// fact retracted here should not spring back to life because it was distilled
// again. The consequence for an import is that re-importing text identical to
// a locally RETRACTED fact attaches an anchor to the retracted record, lands
// nothing retrievable, and still counts toward "imported N of N".
//
// The local status is kept, for the same reason the other direction keeps it --
// a decision made in this repository is not reversed by a foreign export -- so
// the only thing missing was saying so.
func countImportsOntoInactiveFacts(imported, existing []factRecord) int {
	counted := map[string]bool{}
	blocked := 0
	for _, fact := range imported {
		if fact.Status != factStatusActive || counted[fact.ID] {
			continue
		}
		counted[fact.ID] = true
		if i := indexOfFact(existing, fact.ID); i >= 0 && existing[i].Status != factStatusActive {
			blocked++
		}
	}
	return blocked
}

// factImportSources is the one list of tools `facts import` accepts, shared so
// that verify's foreign-anchor detection cannot drift from what import
// actually writes. An anchor minted by this file is "<source>:<foreign id>"
// where source is one of these.
var factImportSources = []string{"mem0"}

// anchorNamesAForeignSource reports whether an anchor names a session in
// another tool rather than one this repository exported.
//
// `facts import` writes exactly one anchor, carrying ONLY SessionID
// "<source>:<foreign id>" -- no commit, checkpoint, transcript, turn, line or
// verified flag. Both conditions are required: the prefix alone would misread
// a local session id that happened to contain a colon, and the shape alone
// would misread a sparse local anchor.
func anchorNamesAForeignSource(anchor factAnchor) bool {
	if strings.TrimSpace(anchor.Commit) != "" || strings.TrimSpace(anchor.CheckpointID) != "" ||
		strings.TrimSpace(anchor.Transcript) != "" || strings.TrimSpace(anchor.TurnID) != "" ||
		anchor.Line > 0 || anchor.Verified {
		return false
	}
	session := strings.TrimSpace(anchor.SessionID)
	for _, source := range factImportSources {
		if strings.HasPrefix(session, source+":") && len(session) > len(source)+1 {
			return true
		}
	}
	return false
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
				return fmt.Errorf("unknown --source %q (supported: %s)", source, strings.Join(factImportSources, ", "))
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
			} // The shape of the prefix was checked before any file was read;
			// whether the taxonomy knows it can only be checked once the brain
			// is resolved.
			if err := validateImportPrefixAgainstTaxonomy(brainDir, prefix, opts.Now().UTC()); err != nil {
				return err
			}

			facts, report := memoriesToFacts(memories, skipped, strings.ToLower(source), prefix, resolvedBranch, opts.Now().UTC())
			report.File = file
			report.DryRun = dryRun

			keptActive := 0
			ontoInactive := 0
			if dryRun {
				// A dry run that cannot report this says less than the real run
				// it is supposed to preview, which is the one thing a preview
				// must not do. Reading the current facts needs no write lock.
				existing, loadErr := loadFacts(brainDir, resolvedBranch)
				if loadErr != nil {
					return loadErr
				}
				keptActive = countKeptActiveFacts(facts, existing)
				ontoInactive = countImportsOntoInactiveFacts(facts, existing)
			}
			// BEFORE the write lock: a command that is going to fail must not
			// mutate the brain first.
			//
			// This check used to sit after the write block, so a doomed import
			// took the lock, rewrote the facts file with an empty set and
			// bumped the manifest's generation and freshness fields -- making
			// the brain look freshly imported -- and only then returned the
			// error saying nothing was imported. It also discarded the
			// manifest-refresh warning collected inside that lock, because
			// returning an error here skips the report entirely, which is
			// exactly the loss the warning was added to prevent.
			//
			// Nothing in the condition depends on the write: every field comes
			// from memoriesToFacts above.
			//
			// Some records skipped is different and stays a warning: a real
			// export can carry an empty memory.
			if report.Read > 0 && report.Imported == 0 && report.SkippedText == report.Read {
				return fmt.Errorf("no record in %s has a %q field: %d record(s) read, all skipped — this is probably an export from a different tool, or a different endpoint's shape",
					report.Source, "memory", report.Read)
			}
			// Set inside the write lock, read after it: a refresh failure is
			// reported alongside a successful import rather than replacing it.
			var manifestErr error
			if !dryRun {
				if err := withBrainWriteLock(brainDir, func() error {
					existing, loadErr := loadFacts(brainDir, resolvedBranch)
					if loadErr != nil {
						return loadErr
					}
					// upsertFact unions provenance and never moves a status,
					// which is right for a re-distill and wrong here: a memory
					// the source retired, already present locally as active,
					// would be imported and stay active with the report saying
					// it was superseded.
					//
					// The local status is KEPT rather than overwritten — a fact
					// asserted in this repository is not retired by a foreign
					// export — and the divergence is counted, so it is visible
					// instead of silently on either side.
					keptActive = countKeptActiveFacts(facts, existing)
					ontoInactive = countImportsOntoInactiveFacts(facts, existing)
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
					// A manifest refresh that fails AFTER the facts are on disk
					// must not present as a failed import.
					//
					// writeFacts above already succeeded, so the facts are in
					// the store. Returning this error aborted before any report
					// was printed, so the user was told the import failed while
					// their brain held every imported fact -- and would then
					// reasonably re-run it. The import DID happen; what is
					// wrong is that `facts status` and the freshness report
					// still describe the brain as it was before, which is a
					// different problem with a different remedy.
					manifestErr = updateFactSourceManifestLocked(brainDir, opts.Now().UTC())
					return nil
				}); err != nil {
					return err
				}
				if manifestErr != nil {
					report.Warnings = append(report.Warnings, fmt.Sprintf(
						"the facts were imported, but the source manifest could not be refreshed (%v); `facts status` and freshness will describe the brain as it was before this import until `entire brain refresh` runs",
						manifestErr))
					sort.Strings(report.Warnings)
				}
			}
			// Outside the write branch: a dry run computes this too, and a
			// preview that reports less than the run it previews is the one
			// thing a preview must not do.
			if ontoInactive > 0 {
				report.Unsupported = append(report.Unsupported, fmt.Sprintf(
					"%d imported fact(s) match text already present here with a non-active status (retracted or superseded); "+
						"the local status is kept, so they are not retrievable -- un-retract them if the source is right",
					ontoInactive))
				sort.Strings(report.Unsupported)
			}
			if keptActive > 0 {
				report.Unsupported = append(report.Unsupported, fmt.Sprintf(
					"supersession not applied to %d fact(s) already active in this brain "+
						"(a local assertion is not retired by an export; retract them explicitly if the source is right)",
					keptActive))
				sort.Strings(report.Unsupported)
			}
			// EVERY record lacking memory text is a format mismatch, not an
			// import.
			//
			// A bare JSON array parses as mem0, so an export from another tool
			// (Letta, Zep) whose records carry no `memory` key reached the
			// report as "imported 0 of N / N skipped: no memory text" and
			// exited 0. This file's own doctrine is that a file which is not an
			// export "is an error naming both, not a silent zero-record
			// import" -- and a file where not one record has text is that same
			// case, discovered one layer later.
			//
			if jsonOut {
				return writeIndentedJSON(cmd.OutOrStdout(), report)
			}
			verb := "imported"
			if dryRun {
				verb = "would import"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %d of %d memories from %s\n", verb, report.Imported, report.Read, report.Source)
			if report.Branch != "" {
				// Facts are branch-scoped, so which branch they landed on is
				// not a detail: recall on any other branch will not see them.
				fmt.Fprintf(cmd.OutOrStdout(), "  on branch %s\n", report.Branch)
			}
			if report.SkippedText > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  %d skipped: no memory text\n", report.SkippedText)
			}
			if report.Superseded > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  %d already superseded at the source; imported as superseded\n", report.Superseded)
			}
			for _, warning := range report.Warnings {
				fmt.Fprintf(cmd.OutOrStdout(), "  warning: %s\n", warning)
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

// firstJSONTypeError returns the first error that names a FIELD rather than a
// shape, so a real export with one bad field is not reported as the wrong kind
// of file.
//
// A *json.UnmarshalTypeError carries the field and the offending Go type,
// which is the only part of this the user can act on. A syntax error is a
// different problem and is left to the shape message, because a truncated or
// non-JSON file genuinely is not an export.
func firstJSONTypeError(errs ...error) error {
	for _, err := range errs {
		if err == nil {
			continue
		}
		var typeErr *json.UnmarshalTypeError
		// Field is the whole point. Trying the bare-array shape first produces
		// its own UnmarshalTypeError for any object ("cannot unmarshal object
		// into []mem0Memory"), which is a SHAPE complaint naming nothing the
		// user can look for. Only an error that names a field improves on the
		// format message.
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return typeErr
		}
	}
	return nil
}

// unreadableTimestampDetail names which field could not be read and quotes the
// value, so the report points at the export rather than merely admitting
// defeat.
func unreadableTimestampDetail(m mem0Memory, createdBad, updatedBad bool) string {
	switch {
	case createdBad && updatedBad:
		return fmt.Sprintf("created_at %q and updated_at %q", m.CreatedAt, m.UpdatedAt)
	case createdBad:
		return fmt.Sprintf("created_at %q", m.CreatedAt)
	default:
		return fmt.Sprintf("updated_at %q", m.UpdatedAt)
	}
}
