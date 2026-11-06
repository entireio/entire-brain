package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"

	"github.com/spf13/cobra"
)

type factsStatusReport struct {
	SchemaVersion int               `json:"schema_version"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Repo          string            `json:"repo"`
	BrainPath     string            `json:"brain_path"`
	RepoHead      string            `json:"repo_head,omitempty"`
	BrainManifest string            `json:"brain_manifest_sha256,omitempty"`
	Branch        string            `json:"branch,omitempty"`
	AllBranches   bool              `json:"all_branches,omitempty"`
	FactsArmReady bool              `json:"facts_arm_ready"`
	Totals        factsStatusCounts `json:"totals"`
	// Vitality summarizes the branch's serve receipts (vitality Phase 1);
	// omitted in --all-branches mode, where each branch carries its own.
	Vitality      *factsVitalitySummary `json:"vitality,omitempty"`
	Branches      []factsBranchState    `json:"branches,omitempty"`
	ManifestFacts *factSourceManifest   `json:"manifest_facts,omitempty"`
	Warnings      []string              `json:"warnings,omitempty"`
}

type factsBranchState struct {
	Branch        string                `json:"branch"`
	FactsArmReady bool                  `json:"facts_arm_ready"`
	Totals        factsStatusCounts     `json:"totals"`
	Vitality      *factsVitalitySummary `json:"vitality,omitempty"`
}

// factsVitalitySummary is the serve-receipt rollup surfaced in facts status:
// how much of the store has actually been emitted, and the most recent
// last-served evidence.
type factsVitalitySummary struct {
	ServedFacts    int        `json:"served_facts"` // distinct fact ids with receipts
	ServedTotal    int        `json:"served_total"` // total serve receipts
	PendingEvents  int        `json:"pending_events"`
	LastServedAt   *time.Time `json:"last_served_at,omitempty"`
	LastServedFact string     `json:"last_served_fact,omitempty"`
	LastSurface    string     `json:"last_surface,omitempty"`
	// Surfaces is the serve count per delivery surface, and it is the number
	// that says whether this store is being READ or merely asked. `recall` is
	// an agent explicitly querying; `hook-pre-edit` and `hook-post-failure`
	// are memories arriving unprompted at the moment of action.
	//
	// It is reported because the distinction was invisible: measured on this
	// repo's main branch on 2026-10-31, 525 of 539 serve events were `recall`
	// and exactly ONE was `hook-pre-edit`. A single ServedTotal cannot show
	// that, and the per-memory breakdown only showed it to someone willing to
	// aggregate `memories vitality --json` by hand.
	Surfaces map[string]int `json:"surfaces,omitempty"`
	// ReusedAnySurface and ReusedViaDelivery count memories served on more
	// than one distinct day — the reasoning-capture exit test — overall and
	// restricted to surfaces that deliver UNREQUESTED.
	//
	// Both are reported because the overall number is confounded on its own.
	// Measured here 2026-10-31: it rose from 2.1% to 2.89% inside one working
	// session, and every bit of that was `recall` — an agent explicitly
	// querying. An agent asking twice is not reuse; it is the re-asking the
	// milestone exists to remove, so a metric it can move by querying more
	// measures the opposite of the thing. Delivery-only is the honest figure,
	// and it read 0.00% when this was written.
	ReusedAnySurface  int `json:"reused_any_surface"`
	ReusedViaDelivery int `json:"reused_via_delivery"`
}

// vitalityDeliverySurfaces are the surfaces that hand a memory to an agent
// WITHOUT it asking. Everything else is a pull: the agent already suspected
// the memory existed, which is the behaviour reasoning capture exists to make
// unnecessary.
//
// Named explicitly rather than inferred from a prefix: a future `hook-`
// surface that merely logs would otherwise be counted as delivery and quietly
// inflate the exit test.
var vitalityDeliverySurfaces = map[string]bool{
	"hook-pre-edit":     true,
	"hook-post-failure": true,
	"hook-prompt":       true,
	"brief":             true,
}

// factsVitalitySummaryForBranch folds the branch's vitality view into the
// status summary. Receipts are best-effort telemetry, so any problem returns
// nil plus a warning instead of failing status.
func factsVitalitySummaryForBranch(brainDir, branch string) (*factsVitalitySummary, []string) {
	rollup, stats, err := loadVitalityView(brainDir, branch)
	if err != nil {
		return nil, []string{fmt.Sprintf("memories vitality ledger unreadable on %s: %v", branch, err)}
	}
	var warnings []string
	if stats.RollupCorrupt {
		warnings = append(warnings, fmt.Sprintf("memories vitality rollup corrupt on %s; run `memories vitality --compact` to rebuild", branch))
	}
	summary := &factsVitalitySummary{PendingEvents: stats.PendingEvents}
	for id, entry := range rollup.Facts {
		summary.ServedFacts++
		summary.ServedTotal += entry.Served
		for surface, count := range entry.Surfaces {
			if summary.Surfaces == nil {
				summary.Surfaces = map[string]int{}
			}
			summary.Surfaces[surface] += count
		}
		// The exit test, counted per memory rather than per event: a memory
		// counts once however many surfaces reused it.
		reusedAny, reusedDelivery := false, false
		for surface, span := range entry.SurfaceSpans {
			if !span.MultiDay() {
				continue
			}
			reusedAny = true
			if vitalityDeliverySurfaces[surface] {
				reusedDelivery = true
			}
		}
		// A memory served by two DIFFERENT surfaces on different days is reuse
		// too, and no single span shows it. The whole-memory first/last catches
		// that; it cannot distinguish delivery from pull, so it only raises the
		// overall figure.
		if !reusedAny && !entry.FirstServed.IsZero() && !entry.LastServed.IsZero() &&
			entry.FirstServed.UTC().Format(time.DateOnly) != entry.LastServed.UTC().Format(time.DateOnly) {
			reusedAny = true
		}
		if reusedAny {
			summary.ReusedAnySurface++
		}
		if reusedDelivery {
			summary.ReusedViaDelivery++
		}
		last := entry.LastServed
		better := summary.LastServedAt == nil || last.After(*summary.LastServedAt) ||
			(last.Equal(*summary.LastServedAt) && id < summary.LastServedFact)
		if better {
			at := last
			summary.LastServedAt = &at
			summary.LastServedFact = id
			summary.LastSurface = entry.LastSurface
		}
	}
	return summary, warnings
}

type factsStatusCounts struct {
	Facts             int `json:"facts"`
	Active            int `json:"active"`
	Superseded        int `json:"superseded"`
	Retracted         int `json:"retracted"`
	Distilled         int `json:"distilled"`
	Authored          int `json:"authored"`
	Imported          int `json:"imported"`
	Proposals         int `json:"proposals"`
	ProvenanceAnchors int `json:"provenance_anchors"`
	VerifiedAnchors   int `json:"verified_anchors"`
	UnsignedAnchors   int `json:"unsigned_anchors"`
}

func newFactsStatusCommand(opts Options) *cobra.Command {
	var (
		branch      string
		allBranches bool
		jsonOut     bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Summarize durable memory readiness without refreshing or writing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if allBranches && branch != "" {
				return fmt.Errorf("--branch and --all-branches cannot be combined")
			}
			// The store is flat: branch is an explicit metadata filter, never
			// implied from the checked-out branch.
			repoDir, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
			if err != nil {
				return err
			}
			resolvedBranch := strings.TrimSpace(branch)
			now := time.Now
			if opts.Now != nil {
				now = opts.Now
			}
			report, err := buildFactsStatusReport(cmd.Context(), opts.Runner, repoDir, brainDir, resolvedBranch, allBranches, now())
			if err != nil {
				return err
			}
			if jsonOut {
				return cmdutil.WriteJSON(cmd, report)
			}
			printFactsStatus(cmd, report)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Only summarize memories stamped with this branch (default: all)")
	cmd.Flags().BoolVar(&allBranches, "all-branches", false, "Summarize every branch in the local memory store")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func buildFactsStatusReport(ctx context.Context, runner CommandRunner, repoDir, brainDir, branch string, allBranches bool, now time.Time) (factsStatusReport, error) {
	report := factsStatusReport{
		SchemaVersion: 1,
		GeneratedAt:   now.UTC(),
		Repo:          repoDir,
		BrainPath:     brainDir,
		RepoHead:      strings.TrimSpace(string(base.GitOutput(ctx, runner, repoDir, "rev-parse", "HEAD"))),
		BrainManifest: evalBrainManifestSHA256(brainDir),
		Branch:        branch,
		AllBranches:   allBranches,
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return report, err
	}
	if manifest.Sources != nil && manifest.Sources.Facts != nil {
		report.ManifestFacts = manifest.Sources.Facts
	}

	stored, err := factstore.Load(brainDir)
	if err != nil {
		return report, err
	}
	if allBranches {
		report.Branch = ""
		byBranch := map[string][]factRecord{}
		for _, rec := range stored {
			byBranch[rec.Branch] = append(byBranch[rec.Branch], rec)
		}
		branches := make([]string, 0, len(byBranch))
		for branch := range byBranch {
			branches = append(branches, branch)
		}
		sort.Strings(branches)
		for _, branch := range branches {
			counts, err := factsStatusCountsForBranch(brainDir, branch, byBranch[branch])
			if err != nil {
				return report, err
			}
			report.Branches = append(report.Branches, factsBranchState{
				Branch:        branch,
				FactsArmReady: counts.Active > 0,
				Totals:        counts,
			})
			report.Totals.add(counts)
		}
		vitality, vitalityWarnings := factsVitalitySummaryForBranch(brainDir, "")
		report.Vitality = vitality
		report.Warnings = append(report.Warnings, vitalityWarnings...)
	} else {
		facts := stored
		if branch != "" {
			facts = facts[:0:0]
			for _, rec := range stored {
				if rec.Branch == branch {
					facts = append(facts, rec)
				}
			}
		}
		counts, err := factsStatusCountsForBranch(brainDir, branch, facts)
		if err != nil {
			return report, err
		}
		report.Totals = counts
		vitality, vitalityWarnings := factsVitalitySummaryForBranch(brainDir, branch)
		report.Vitality = vitality
		report.Warnings = append(report.Warnings, vitalityWarnings...)
	}

	report.FactsArmReady = report.Totals.Active > 0
	// The totals above are what the store produced. If the manifest declares
	// more, say so here too: this is the fact-health command, and a total that
	// silently omits lost facts reads as a clean bill of health.
	if integrity := factstore.InspectBrain(brainDir); !integrity.OK() {
		report.Warnings = append(report.Warnings, integrity.Warning())
	}
	report.Warnings = append(report.Warnings, factsStatusWarnings(report)...)
	return report, nil
}

func factsStatusCountsForBranch(brainDir, branch string, facts []factRecord) (factsStatusCounts, error) {
	counts := factsStatusCounts{}
	for _, fact := range facts {
		// A stored line carrying no id is not a fact (see factstore.IsFactRecord).
		// Counting it would report unaddressable junk as an ACTIVE fact,
		// because the status switch below reads an empty status as active.
		if !factstore.IsFactRecord(fact) {
			continue
		}
		counts.Facts++
		switch fact.Status {
		case factStatusSuperseded:
			counts.Superseded++
		case factStatusRetracted:
			counts.Retracted++
		default:
			counts.Active++
		}
		switch fact.Origin {
		case factstore.OriginDistilled:
			counts.Distilled++
		case factstore.OriginAuthored:
			counts.Authored++
		case factstore.OriginImported:
			counts.Imported++
		}
		for _, anchor := range fact.Provenance {
			counts.ProvenanceAnchors++
			if anchor.Verified {
				counts.VerifiedAnchors++
			} else {
				counts.UnsignedAnchors++
			}
		}
	}
	return counts, nil
}

func (counts *factsStatusCounts) add(other factsStatusCounts) {
	counts.Facts += other.Facts
	counts.Active += other.Active
	counts.Superseded += other.Superseded
	counts.Retracted += other.Retracted
	counts.Distilled += other.Distilled
	counts.Authored += other.Authored
	counts.Imported += other.Imported
	counts.Proposals += other.Proposals
	counts.ProvenanceAnchors += other.ProvenanceAnchors
	counts.VerifiedAnchors += other.VerifiedAnchors
	counts.UnsignedAnchors += other.UnsignedAnchors
}

func factsStatusWarnings(report factsStatusReport) []string {
	var warnings []string
	if !report.FactsArmReady {
		warnings = append(warnings, "memories retriever has no active memories; facts-vs-raw release proof cannot be collected yet")
	}
	if report.ManifestFacts == nil {
		warnings = append(warnings, "brain manifest has no facts source")
	}
	if report.AllBranches && report.ManifestFacts != nil && report.ManifestFacts.Facts != report.Totals.Facts {
		warnings = append(warnings, "brain manifest facts source count differs from disk")
	}
	return warnings
}

func printFactsStatus(cmd *cobra.Command, report factsStatusReport) {
	target := report.Branch
	if report.AllBranches {
		target = "all branches"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "memories status for %s\n", target)
	fmt.Fprintf(cmd.OutOrStdout(), "brain: %s\n", report.BrainPath)
	printFactsStatusCounts(cmd, report.Totals)
	if v := report.Vitality; v != nil && v.ServedTotal > 0 {
		last := ""
		if v.LastServedAt != nil {
			last = fmt.Sprintf(" last_served=%s memory=%s surface=%s", v.LastServedAt.UTC().Format(time.RFC3339), v.LastServedFact, v.LastSurface)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "vitality: served_memories=%d served_total=%d pending_events=%d%s\n",
			v.ServedFacts, v.ServedTotal, v.PendingEvents, last)
		if line := formatVitalitySurfaces(v.Surfaces); line != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "surfaces: %s\n", line)
		}
		// Both figures, always, and delivery second so it reads last. The
		// overall number alone is movable by an agent querying more, which is
		// the behaviour this layer exists to remove.
		fmt.Fprintf(cmd.OutOrStdout(),
			"reuse: served_over_2plus_days=%d delivered_over_2plus_days=%d (of %d active)\n",
			v.ReusedAnySurface, v.ReusedViaDelivery, report.Totals.Active)
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", warning)
	}
}

// formatVitalitySurfaces renders the per-surface serve counts, busiest first.
//
// Ordered by count and then by name, never by map iteration: the point of the
// line is that a reader can compare two runs, and Go randomizes map order, so
// an unsorted line would reshuffle on every invocation and make the comparison
// impossible.
func formatVitalitySurfaces(surfaces map[string]int) string {
	if len(surfaces) == 0 {
		return ""
	}
	names := make([]string, 0, len(surfaces))
	for name := range surfaces {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if surfaces[names[i]] != surfaces[names[j]] {
			return surfaces[names[i]] > surfaces[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, surfaces[name]))
	}
	return strings.Join(parts, " ")
}

func printFactsStatusCounts(cmd *cobra.Command, counts factsStatusCounts) {
	fmt.Fprintf(cmd.OutOrStdout(),
		"active=%d total=%d distilled=%d authored=%d superseded=%d retracted=%d proposals=%d verified_anchors=%d unsigned_anchors=%d\n",
		counts.Active,
		counts.Facts,
		counts.Distilled,
		counts.Authored,
		counts.Superseded,
		counts.Retracted,
		counts.Proposals,
		counts.VerifiedAnchors,
		counts.UnsignedAnchors,
	)
}
