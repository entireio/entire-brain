package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// facts_integrity.go cross-checks the manifest's declared fact source against
// the facts the store can actually produce — both HOW MANY it produces and
// WHICH ones, because a corruption can leave the first right while destroying
// the second (see COUNTS ARE NOT ENOUGH below).
//
// sources.facts in the manifest is a CLAIM: "this brain holds N facts". Every
// health surface printed that claim as though it were an observation, and
// nothing ever compared it to the store. A facts.ndjson that had gone lossy —
// a line overwritten with valid JSON that is not a fact record, a truncated
// file, a botched hand edit — therefore kept reporting N on `status`, `doctor`
// and `facts map` while recall returned fewer, `get fact:<id>` said "not
// found", and `search` volunteered that the answer "may genuinely not be in
// the brain". For a durable-memory product that last line converts silent data
// loss into an affirmative false statement.
//
// The history index already solves this and its wording is the model:
//
//	history index size does not match the manifest; run `entire brain refresh
//	history` to rebuild it
//
// Note WHERE that check lives. It is not inside the per-record reader; it is
// in the function that loads the artifact the manifest declares, so every
// caller of that artifact inherits it and the REBUILD path — which must be
// able to run against the damaged artifact — does not. Facts need the same
// shape, and loadFacts is the wrong seam for three reasons:
//
//  1. loadFacts is per-branch and the manifest records only a total across
//     branches, so loadFacts cannot know what N is for the branch it is
//     reading.
//  2. Its ~30 callers include distill, remember, facts gc/promote/merge and
//     refresh — the very commands that REPAIR a lossy store. A loader that
//     failed closed would wedge the store shut with no way out.
//  3. The manifest's count is produced by loadAllFactBranches (see
//     updateFactSourceManifestLocked). Checking the claim with the same
//     function that produced it makes the check an exact inverse: no
//     branch-name-to-directory ambiguity, and any disagreement is real.
//
// So the check lives here, beside missingFactBranchStores — the existing seam
// for "the manifest names something the store cannot produce" — and every
// surface reads one report. The retrieval note inherits through the single
// emptyResultBlindSpot choke point, which covers recall, search, query and
// retrieve at once.

// COUNTS ARE NOT ENOUGH. The first cut of this check compared Declared against
// Readable and nothing else, which leaves a corruption that PRESERVES the count
// completely invisible: overwrite one line of facts.ndjson with a copy of
// another and the store still yields N records, still parses, still declares N
// — and the fact whose line was taken is gone. `get fact:<id>` says "not
// found", doctor says "facts: ok", and `search` goes back to volunteering that
// the answer "may genuinely not be in the brain". That is the same affirmative
// false statement this file exists to eliminate, reached by a different route.
//
// The obvious repair — compare the declared id SET against the readable one —
// is not available: sources.facts records counts (facts/distilled/authored/
// superseded/by_kind), never ids, and putting ids there would not help. That
// manifest is re-derived from the store itself by summarizeFactSource on every
// remember, refresh, gc, promote and retract, so a declared id set would
// re-baseline onto the damage at the next write exactly as the count does, and
// would cry loss on every legitimate id-changing operation in between.
//
// What IS checkable from the store alone, with no bookkeeping and nothing that
// can go stale, is the invariant the store is written under: WITHIN ONE BRANCH
// AN ID IDENTIFIES EXACTLY ONE FACT. Every writer keys by id before it writes
// (factmerge.Upsert, factmerge.Promote via IndexOf, factmerge.GC), so a branch
// that yields the same id twice was not written by this program. The second
// record did not join the fact set; it stands where a different fact used to
// be. That is loss, and it is loss the count cannot see.
//
// Per BRANCH, emphatically not per brain. A fact id is sha256 of normalized
// text plus sorted paths (factmerge.RecordID) — the branch is not in it — so
// the same fact carried between branches keeps its id, which is precisely what
// `facts promote` does on purpose. A brain-wide id set would report data loss
// on every promote, and a check that cries loss on a legitimate operation is
// worse than the hole it closes.

// factStoreDefectMode names WHICH way the store fails the manifest. They are
// different user situations and must not collapse into one sentence: "not
// built" is a rebuild, "corrupt" is a repair, "lossy" is data that is gone and
// has to be re-distilled, and "duplicated" is data that was overwritten while
// the numbers stayed right.
type factStoreDefectMode string

const (
	// factStoreDefectNone is a store that can produce what the manifest declares.
	factStoreDefectNone factStoreDefectMode = ""
	// factStoreDefectMissing: a declared branch has no facts.ndjson at all.
	// Already detected by missingFactBranchStores; kept in its own words.
	factStoreDefectMissing factStoreDefectMode = "missing"
	// factStoreDefectUnreadable: a line will not parse. The store is corrupt
	// and nothing on the branch can be read, not just the bad line.
	factStoreDefectUnreadable factStoreDefectMode = "unreadable"
	// factStoreDefectLossy: every line parses, but the store yields fewer
	// facts than the manifest declares. This is the readable-but-short case —
	// the one that used to pass every health surface.
	factStoreDefectLossy factStoreDefectMode = "lossy"
	// factStoreDefectStale: the store yields MORE facts than the manifest
	// declares. Not data loss — the manifest is behind — but still a declared
	// number that was never observed, so it is reported rather than printed.
	factStoreDefectStale factStoreDefectMode = "stale"
	// factStoreDefectDuplicated: every line parses and the counts are not
	// short, but one branch stores the same fact id twice. Deliberately NOT
	// folded into "lossy": lossy is a SHORTFALL — fewer facts than claimed,
	// repaired by re-distilling what is missing — while this is a
	// SUBSTITUTION, where the arithmetic is intact and a specific fact was
	// overwritten by a copy of its neighbour. Telling the two apart is the
	// difference between "some of your memory did not survive" and "this
	// particular record is standing in for one that is gone".
	factStoreDefectDuplicated factStoreDefectMode = "duplicated"
)

// factStoreIntegrity is the cross-check result. The zero value is a healthy
// store, so callers may use it unconditionally.
type factStoreIntegrity struct {
	// Declared is sources.facts.facts — what the manifest claims.
	Declared int `json:"declared"`
	// Readable is how many records the store actually yields that are facts:
	// a record carrying an id. A line that parses as JSON but has no id (the
	// canonical corruption in this defect) is counted in Unusable, never here,
	// because it is unaddressable by `get`, invisible to recall, and dropped
	// by every status/scope filter.
	Readable int `json:"readable"`
	// Unusable counts records read from the store that carry no id. They are
	// reported even when the counts happen to agree, because a manifest
	// re-summarized from an already-damaged store would otherwise hide them.
	Unusable int `json:"unusable,omitempty"`
	// Distinct is how many DIFFERENT fact ids the store yields, summed across
	// branches but counted within each one (a fact id does not include the
	// branch, so `facts promote` legitimately stores one id on two branches —
	// see the file header). In a store this program wrote, Distinct always
	// equals Readable; anything less is a record that overwrote another.
	Distinct int `json:"distinct,omitempty"`
	// DuplicateIDs names the ids a single branch stores more than once, sorted
	// so the report is stable. The full set is kept for JSON and MCP readers;
	// the one-line warning names only the first few.
	DuplicateIDs []string `json:"duplicate_ids,omitempty"`
	// Missing names declared branches whose facts.ndjson is absent.
	Missing []string `json:"missing_branches,omitempty"`
	// ParseError is the first line the store could not parse, in the loader's
	// own words ("parse facts.ndjson line 3: ...").
	ParseError string `json:"parse_error,omitempty"`
	// Unreadable names the declared branches whose facts.ndjson will not parse.
	// loadAllFactBranches stops at the first bad line and does not say WHOSE
	// it was; with several branches declared, that is the difference between a
	// repair the user can aim and one they cannot.
	Unreadable []string `json:"unreadable_branches,omitempty"`
	// Mode is the classification; empty when the store is intact.
	Mode factStoreDefectMode `json:"mode,omitempty"`
}

// OK reports a store that can produce what the manifest declares.
func (r factStoreIntegrity) OK() bool { return r.Mode == factStoreDefectNone }

// Lost is how many declared facts the store cannot produce. Zero unless the
// store is short.
func (r factStoreIntegrity) Lost() int {
	if r.Declared > r.Readable {
		return r.Declared - r.Readable
	}
	return 0
}

// Overwritten is how many stored records lost their place to another record's
// id. Zero unless a branch repeated an id.
//
// Kept separate from Lost() on purpose. Lost() measures the store against the
// manifest's COUNT, and a substitution is invisible to that measurement by
// construction: the count is exactly what the corruption preserved.
func (r factStoreIntegrity) Overwritten() int {
	if r.Readable > r.Distinct {
		return r.Readable - r.Distinct
	}
	return 0
}

// factStoreDuplicateIDsShown caps how many colliding ids the single-line
// warning names. The complete list stays in DuplicateIDs; a warning that
// printed a hundred ids would push the repair off the end of the line.
const factStoreDuplicateIDsShown = 3

// factStoreIDList renders at most factStoreDuplicateIDsShown ids and says how
// many it held back, so the line stays one line without implying the damage
// stopped where the list did.
func factStoreIDList(ids []string) string {
	if len(ids) <= factStoreDuplicateIDsShown {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(ids[:factStoreDuplicateIDsShown], ", "),
		len(ids)-factStoreDuplicateIDsShown,
	)
}

// factsRepairHint is the repair for a store that lost content. Rebuilding the
// manifest alone would only re-baseline the loss, so the fact commands' own
// wording — refresh, then re-distill — is what gets named.
const factsRepairHint = "run `entire brain refresh` and re-distill"

// Warning renders the single caller-facing line, or "" when the store is
// intact. Each mode keeps its own words.
func (r factStoreIntegrity) Warning() string {
	switch r.Mode {
	case factStoreDefectMissing:
		return missingFactStoreWarning(r.Missing)
	case factStoreDefectUnreadable:
		where := "fact store"
		if len(r.Unreadable) > 0 {
			where = "fact store for " + strings.Join(r.Unreadable, ", ")
		}
		return fmt.Sprintf(
			"%s is corrupt: %s; the %d fact(s) the manifest declares cannot be recalled — %s",
			where, r.ParseError, r.Declared, factsRepairHint,
		)
	case factStoreDefectLossy:
		detail := fmt.Sprintf(
			"fact count does not match the manifest: %d declared, %d readable",
			r.Declared, r.Readable,
		)
		if lost := r.Lost(); lost > 0 {
			detail += fmt.Sprintf(" (%d lost)", lost)
		}
		if r.Unusable > 0 {
			detail += fmt.Sprintf("; %d stored line(s) carry no fact id", r.Unusable)
		}
		// A store can be BOTH short and collided. Reporting only the shortfall
		// would understate the damage — the overwritten fact is gone too, and
		// it is not one of the ones the arithmetic accounted for.
		if overwritten := r.Overwritten(); overwritten > 0 {
			detail += fmt.Sprintf("; %d more record(s) repeat a fact id (%s)", overwritten, factStoreIDList(r.DuplicateIDs))
		}
		return detail + " — " + factsRepairHint
	case factStoreDefectDuplicated:
		return fmt.Sprintf(
			"fact ids are not unique: %d stored record(s) repeat an id the branch already holds (%s); an id identifies exactly one fact, so each repeat stands where another fact used to be and that fact can no longer be recalled; the manifest's %d were counted, not read — %s",
			r.Overwritten(), factStoreIDList(r.DuplicateIDs), r.Declared, factsRepairHint,
		)
	case factStoreDefectStale:
		return fmt.Sprintf(
			"fact count does not match the manifest: %d declared, %d readable; the manifest is behind the store — run `entire brain refresh`",
			r.Declared, r.Readable,
		)
	default:
		return ""
	}
}

// inspectFactStore cross-checks a declared fact source against the store on
// disk. A nil source is a brain with no facts declared: nothing is claimed, so
// nothing can be lost, and the result is healthy. That is what keeps a
// genuinely empty brain from reporting loss.
func inspectFactStore(brainDir string, source *factSourceManifest) factStoreIntegrity {
	if brainDir == "" || source == nil {
		return factStoreIntegrity{}
	}
	report := factStoreIntegrity{Declared: source.Facts}

	// Mode 1 — not built. A declared branch with no facts.ndjson is the
	// already-handled case; keep its more specific message rather than folding
	// it into the count mismatch it would also produce.
	if missing := missingFactBranchStores(brainDir, source); len(missing) > 0 {
		report.Missing = missing
		report.Mode = factStoreDefectMissing
		return report
	}

	// Mode 2 — corrupt. loadAllFactBranches is the same function
	// updateFactSourceManifestLocked used to PRODUCE source.Facts, so a
	// disagreement below is a real one and not a re-derivation artifact.
	// Validate declared paths through the same guarded reader used by recall
	// before the aggregate loader opens any of those stores.
	if broken := unreadableFactBranchStores(brainDir, source); len(broken) > 0 {
		report.ParseError = broken[0].Err.Error()
		for _, store := range broken {
			report.Unreadable = append(report.Unreadable, store.Branch)
		}
		report.Mode = factStoreDefectUnreadable
		return report
	}
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		report.ParseError = err.Error()
		for _, broken := range unreadableFactBranchStores(brainDir, source) {
			report.Unreadable = append(report.Unreadable, broken.Branch)
		}
		report.Mode = factStoreDefectUnreadable
		return report
	}
	duplicated := map[string]struct{}{}
	for _, records := range byBranch {
		// One `seen` per branch, never one for the whole brain: see the file
		// header on why a brain-wide id set would fire on every `facts
		// promote`. byBranch is keyed by the branch recorded ON each record,
		// which is what every writer sets to the store it writes into, so this
		// is the same set `loadFacts` hands to recall for that branch.
		seen := make(map[string]struct{}, len(records))
		for _, record := range records {
			if !isFactRecord(record) {
				report.Unusable++
				continue
			}
			report.Readable++
			if _, repeat := seen[record.ID]; repeat {
				duplicated[record.ID] = struct{}{}
				continue
			}
			seen[record.ID] = struct{}{}
			report.Distinct++
		}
	}
	if len(duplicated) > 0 {
		report.DuplicateIDs = make([]string, 0, len(duplicated))
		for id := range duplicated {
			report.DuplicateIDs = append(report.DuplicateIDs, id)
		}
		sort.Strings(report.DuplicateIDs)
	}

	// Mode 3 — readable but short (or long).
	switch {
	case report.Readable < report.Declared, report.Unusable > 0:
		report.Mode = factStoreDefectLossy
	// Mode 4 — readable, and the arithmetic is not short, but a branch holds
	// one id twice. This runs BEFORE the stale test on purpose: a duplicate
	// that was appended rather than written over also makes the store longer
	// than the manifest, and "the manifest is behind the store — run refresh"
	// is the one piece of advice that must not be given here. Refreshing
	// re-derives the count from the collided store, declares it, and the
	// brain goes quiet about a fact it can no longer produce.
	case len(report.DuplicateIDs) > 0:
		report.Mode = factStoreDefectDuplicated
	case report.Readable > report.Declared:
		report.Mode = factStoreDefectStale
	}
	return report
}

// inspectBrainFactStore is inspectFactStore for callers that hold only a brain
// directory. A manifest that will not load, or one declaring no fact source,
// is reported as healthy: "this brain has no facts" is not fact loss, and the
// manifest's own health is a separate check that owns that message.
func inspectBrainFactStore(brainDir string) factStoreIntegrity {
	if brainDir == "" {
		return factStoreIntegrity{}
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest == nil || manifest.Sources == nil {
		return factStoreIntegrity{}
	}
	return inspectFactStore(brainDir, manifest.Sources.Facts)
}

// errFactStoreIncomplete is the exit verdict for a read surface that rendered
// a fact view over a store the manifest says is short. The view itself is
// printed — hiding the survivors helps nobody — but the command exits nonzero
// so a script cannot read the output as complete, matching how a corrupt
// history index already behaves.
var errFactStoreIncomplete = errors.New("fact store cannot produce every fact the manifest declares")

// reportFactStoreIntegrity prints the cross-check verdict for a fact read
// surface and returns the error that should end the command, or nil when the
// store is intact.
//
// In text mode the warning goes to stdout, ahead of the view it qualifies, so
// a reader who pipes stdout sees it beside the numbers it corrects. In JSON
// mode stdout belongs to the document and the warning goes to stderr instead;
// the nonzero exit is what a JSON consumer must key on.
func reportFactStoreIntegrity(cmd *cobra.Command, brainDir string, jsonOut bool) error {
	integrity := inspectBrainFactStore(brainDir)
	if integrity.OK() {
		return nil
	}
	out := cmd.OutOrStdout()
	if jsonOut {
		out = cmd.ErrOrStderr()
	}
	fmt.Fprintf(out, "warning: %s\n", integrity.Warning())
	return renderedCommandError{err: errFactStoreIncomplete}
}

// isFactRecord reports whether a parsed line is actually a durable fact. The
// id is the fact's identity — content-derived at authoring time and the only
// handle `get`, recall, dedup and supersede have — so a record without one is
// not a fact, however well-formed its JSON. The manifest producer
// (summarizeFactSource) and this checker share this predicate so the declared
// number and the checked number can never disagree by definition.
func isFactRecord(record factRecord) bool {
	return strings.TrimSpace(record.ID) != ""
}
