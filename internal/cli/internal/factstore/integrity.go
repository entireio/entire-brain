package factstore

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// integrity.go cross-checks the manifest's declared fact source against
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
// shape, and Load is the wrong seam for three reasons:
//
//  1. Load is per-branch and the manifest records only a total across
//     branches, so Load cannot know what N is for the branch it is
//     reading.
//  2. Its ~30 callers include distill, remember, facts gc/promote/merge and
//     refresh — the very commands that REPAIR a lossy store. A loader that
//     failed closed would wedge the store shut with no way out.
//  3. The manifest's count is produced by LoadAllBranches (see
//     updateFactSourceManifestLocked). Checking the claim with the same
//     function that produced it makes the check an exact inverse: no
//     branch-name-to-directory ambiguity, and any disagreement is real.
//
// So the check lives here, beside MissingBranchStores — the existing seam
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
// manifest is re-derived from the store itself by SummarizeSource on every
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

// DefectMode names WHICH way the store fails the manifest. They are
// different user situations and must not collapse into one sentence: "not
// built" is a rebuild, "corrupt" is a repair, "lossy" is data that is gone and
// has to be re-distilled, and "duplicated" is data that was overwritten while
// the numbers stayed right.
type DefectMode string

const (
	// DefectNone is a store that can produce what the manifest declares.
	DefectNone DefectMode = ""
	// DefectMissing: a declared branch has no facts.ndjson at all.
	// Already detected by MissingBranchStores; kept in its own words.
	DefectMissing DefectMode = "missing"
	// DefectUnreadable: a line will not parse. The store is corrupt
	// and nothing on the branch can be read, not just the bad line.
	DefectUnreadable DefectMode = "unreadable"
	// DefectLossy: every line parses, but the store yields fewer
	// facts than the manifest declares. This is the readable-but-short case —
	// the one that used to pass every health surface.
	DefectLossy DefectMode = "lossy"
	// DefectStale: the store yields MORE facts than the manifest
	// declares. Not data loss — the manifest is behind — but still a declared
	// number that was never observed, so it is reported rather than printed.
	DefectStale DefectMode = "stale"
	// DefectDuplicated: every line parses and the counts are not
	// short, but one branch stores the same fact id twice. Deliberately NOT
	// folded into "lossy": lossy is a SHORTFALL — fewer facts than claimed,
	// repaired by re-distilling what is missing — while this is a
	// SUBSTITUTION, where the arithmetic is intact and a specific fact was
	// overwritten by a copy of its neighbour. Telling the two apart is the
	// difference between "some of your memory did not survive" and "this
	// particular record is standing in for one that is gone".
	DefectDuplicated DefectMode = "duplicated"
)

// Integrity is the cross-check result. The zero value is a healthy
// store, so callers may use it unconditionally.
type Integrity struct {
	// Declared is sources.facts.facts — what the manifest claims.
	Declared int `json:"declared"`
	// Readable is how many records the store actually yields that are facts:
	// a record carrying an id. A line that parses as JSON but has no id (the
	// canonical corruption in this defect) is counted in Unusable, never here,
	// because it is unaddressable by `get`, invisible to recall, and dropped
	// by every status/scope filter.
	Readable int `json:"readable"`
	// Pending is sources.facts.pending_reconcile — staged candidate records
	// the manifest deliberately declares OUTSIDE Facts (SummarizeSource
	// books a factmerge.StatusCandidate under its own counter until the next
	// distill reconciles it). The store yields those records, so every
	// Readable-vs-Declared comparison must add Pending back or a brain with
	// one staged candidate reports "the manifest is behind the store"
	// forever, through the very refresh the warning recommends.
	Pending int `json:"pending,omitempty"`
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
	// so the report is stable. The full set is kept for JSON readers;
	// the one-line warning names only the first few.
	DuplicateIDs []string `json:"duplicate_ids,omitempty"`
	// Missing names declared branches whose facts.ndjson is absent.
	Missing []string `json:"missing_branches,omitempty"`
	// ParseError is the first line the store could not parse, in the loader's
	// own words ("parse facts.ndjson line 3: ...").
	ParseError string `json:"parse_error,omitempty"`
	// Unreadable names the declared branches whose facts.ndjson will not parse.
	// LoadAllBranches stops at the first bad line and does not say WHOSE
	// it was; with several branches declared, that is the difference between a
	// repair the user can aim and one they cannot.
	Unreadable []string `json:"unreadable_branches,omitempty"`
	// Mode is the classification; empty when the store is intact.
	Mode DefectMode `json:"mode,omitempty"`
}

// OK reports a store that can produce what the manifest declares.
func (r Integrity) OK() bool { return r.Mode == DefectNone }

// pendingClause names the staged-candidate share of the expected count, so a
// mismatch message whose arithmetic includes Pending still adds up on the page.
func (r Integrity) pendingClause() string {
	if r.Pending == 0 {
		return ""
	}
	return fmt.Sprintf(" (+%d staged candidate(s))", r.Pending)
}

// expected is every record the manifest accounts for: published facts plus
// staged candidates awaiting reconciliation. It is what the store must yield
// for the cross-check to call it intact.
func (r Integrity) expected() int { return r.Declared + r.Pending }

// Lost is how many declared facts the store cannot produce. Zero unless the
// store is short.
func (r Integrity) Lost() int {
	if r.expected() > r.Readable {
		return r.expected() - r.Readable
	}
	return 0
}

// Overwritten is how many stored records lost their place to another record's
// id. Zero unless a branch repeated an id.
//
// Kept separate from Lost() on purpose. Lost() measures the store against the
// manifest's COUNT, and a substitution is invisible to that measurement by
// construction: the count is exactly what the corruption preserved.
func (r Integrity) Overwritten() int {
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

// RepairHint is the repair for a store that lost content. Rebuilding the
// manifest alone would only re-baseline the loss, so the fact commands' own
// wording — refresh, then re-distill — is what gets named.
func RepairHint() string {
	return "run `" + base.BrainInvocation(os.LookupEnv, "refresh") + "` and re-distill"
}

// Warning renders the single caller-facing line, or "" when the store is
// intact. Each mode keeps its own words.
func (r Integrity) Warning() string {
	switch r.Mode {
	case DefectMissing:
		return MissingStoreWarning(r.Missing)
	case DefectUnreadable:
		where := "memory store"
		if len(r.Unreadable) > 0 {
			where = "memory store for " + strings.Join(r.Unreadable, ", ")
		}
		return fmt.Sprintf(
			"%s is corrupt: %s; the %d memory(ies) the manifest declares cannot be recalled — %s",
			where, r.ParseError, r.Declared, RepairHint(),
		)
	case DefectLossy:
		detail := fmt.Sprintf(
			"memory count does not match the manifest: %d declared%s, %d readable",
			r.Declared, r.pendingClause(), r.Readable,
		)
		if lost := r.Lost(); lost > 0 {
			detail += fmt.Sprintf(" (%d lost)", lost)
		}
		if r.Unusable > 0 {
			detail += fmt.Sprintf("; %d stored line(s) carry no memory id", r.Unusable)
		}
		// A store can be BOTH short and collided. Reporting only the shortfall
		// would understate the damage — the overwritten fact is gone too, and
		// it is not one of the ones the arithmetic accounted for.
		if overwritten := r.Overwritten(); overwritten > 0 {
			detail += fmt.Sprintf("; %d more record(s) repeat a memory id (%s)", overwritten, factStoreIDList(r.DuplicateIDs))
		}
		return detail + " — " + RepairHint()
	case DefectDuplicated:
		return fmt.Sprintf(
			"memory ids are not unique: %d stored record(s) repeat an id the branch already holds (%s); an id identifies exactly one memory, so each repeat stands where another memory used to be and that memory can no longer be recalled; the manifest's %d were counted, not read — %s",
			r.Overwritten(), factStoreIDList(r.DuplicateIDs), r.Declared, RepairHint(),
		)
	case DefectStale:
		return fmt.Sprintf(
			"memory count does not match the manifest: %d declared%s, %d readable; the manifest is behind the store — run `"+base.BrainInvocation(os.LookupEnv, "refresh")+"`",
			r.Declared, r.pendingClause(), r.Readable,
		)
	default:
		return ""
	}
}

// Inspect cross-checks a declared fact source against the store on
// disk. A nil source is a brain with no facts declared: nothing is claimed, so
// nothing can be lost, and the result is healthy. That is what keeps a
// genuinely empty brain from reporting loss.
func Inspect(brainDir string, source *bmanifest.FactSource) Integrity {
	if brainDir == "" || source == nil {
		return Integrity{}
	}
	report := Integrity{Declared: source.Facts, Pending: source.PendingReconcile}

	// Mode 1 — not built. A declared branch with no facts.ndjson is the
	// already-handled case; keep its more specific message rather than folding
	// it into the count mismatch it would also produce.
	if missing := MissingBranchStores(brainDir, source); len(missing) > 0 {
		report.Missing = missing
		report.Mode = DefectMissing
		return report
	}

	// Mode 2 — corrupt. LoadAllBranches is the same function
	// updateFactSourceManifestLocked used to PRODUCE source.Facts, so a
	// disagreement below is a real one and not a re-derivation artifact.
	// Validate declared paths through the same guarded reader used by recall
	// before the aggregate loader opens any of those stores.
	if broken := UnreadableBranchStores(brainDir, source); len(broken) > 0 {
		report.ParseError = broken[0].Err.Error()
		for _, store := range broken {
			report.Unreadable = append(report.Unreadable, store.Branch)
		}
		report.Mode = DefectUnreadable
		return report
	}
	byFile, err := LoadStoreFiles(brainDir)
	if err != nil {
		report.ParseError = err.Error()
		for _, broken := range UnreadableBranchStores(brainDir, source) {
			report.Unreadable = append(report.Unreadable, broken.Branch)
		}
		report.Mode = DefectUnreadable
		return report
	}
	duplicated := map[string]struct{}{}
	for _, records := range byFile {
		// One `seen` per store FILE, never one for the whole brain: a file is
		// the set `Load` hands to recall, so a repeat within one file is
		// the only shape that shadows a memory. The same id across files is
		// ordinary: `facts promote` copies a record into another branch's
		// store, and the hosted pull mirrors the repo feed into the current
		// branch's store. Bucketing by the branch recorded on each record
		// conflated those mirrors into one bucket and reported them as
		// corruption (COR-2220).
		seen := make(map[string]struct{}, len(records))
		for _, record := range records {
			if !IsFactRecord(record) {
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
	case report.Readable < report.expected(), report.Unusable > 0:
		report.Mode = DefectLossy
	// Mode 4 — readable, and the arithmetic is not short, but a branch holds
	// one id twice. This runs BEFORE the stale test on purpose: a duplicate
	// that was appended rather than written over also makes the store longer
	// than the manifest, and "the manifest is behind the store — run refresh"
	// is the one piece of advice that must not be given here. Refreshing
	// re-derives the count from the collided store, declares it, and the
	// brain goes quiet about a fact it can no longer produce.
	case len(report.DuplicateIDs) > 0:
		report.Mode = DefectDuplicated
	case report.Readable > report.expected():
		report.Mode = DefectStale
	}
	return report
}

// InspectBrain is Inspect for callers that hold only a brain
// directory. A manifest that will not load, or one declaring no fact source,
// is reported as healthy: "this brain has no facts" is not fact loss, and the
// manifest's own health is a separate check that owns that message.
func InspectBrain(brainDir string) Integrity {
	if brainDir == "" {
		return Integrity{}
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil || manifest == nil || manifest.Sources == nil {
		return Integrity{}
	}
	return Inspect(brainDir, manifest.Sources.Facts)
}

// IsFactRecord reports whether a parsed line is actually a durable fact. The
// id is the fact's identity — content-derived at authoring time and the only
// handle `get`, recall, dedup and supersede have — so a record without one is
// not a fact, however well-formed its JSON. The manifest producer
// (SummarizeSource) and this checker share this predicate so the declared
// number and the checked number can never disagree by definition.
func IsFactRecord(record factmerge.Record) bool {
	return strings.TrimSpace(record.ID) != ""
}
