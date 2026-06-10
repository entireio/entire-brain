package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type factsStatusReport struct {
	SchemaVersion int                 `json:"schema_version"`
	GeneratedAt   time.Time           `json:"generated_at"`
	Repo          string              `json:"repo"`
	BrainPath     string              `json:"brain_path"`
	RepoHead      string              `json:"repo_head,omitempty"`
	BrainManifest string              `json:"brain_manifest_sha256,omitempty"`
	Branch        string              `json:"branch,omitempty"`
	AllBranches   bool                `json:"all_branches,omitempty"`
	FactsArmReady bool                `json:"facts_arm_ready"`
	Totals        factsStatusCounts   `json:"totals"`
	Branches      []factsBranchState  `json:"branches,omitempty"`
	ManifestFacts *factSourceManifest `json:"manifest_facts,omitempty"`
	Warnings      []string            `json:"warnings,omitempty"`
}

type factsBranchState struct {
	Branch        string            `json:"branch"`
	FactsArmReady bool              `json:"facts_arm_ready"`
	Totals        factsStatusCounts `json:"totals"`
}

type factsStatusCounts struct {
	Facts             int `json:"facts"`
	Active            int `json:"active"`
	Superseded        int `json:"superseded"`
	Retracted         int `json:"retracted"`
	Distilled         int `json:"distilled"`
	Authored          int `json:"authored"`
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
		Short: "Summarize durable fact readiness without refreshing or writing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if allBranches && branch != "" {
				return fmt.Errorf("--branch and --all-branches cannot be combined")
			}
			repoDir, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			now := time.Now
			if opts.Now != nil {
				now = opts.Now
			}
			report, err := buildFactsStatusReport(cmd.Context(), opts.Runner, repoDir, brainDir, resolvedBranch, allBranches, now())
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd, report)
			}
			printFactsStatus(cmd, report)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to summarize (default: current branch)")
	cmd.Flags().BoolVar(&allBranches, "all-branches", false, "Summarize every branch in the local fact store")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func buildFactsStatusReport(ctx context.Context, runner CommandRunner, repoDir, brainDir, branch string, allBranches bool, now time.Time) (factsStatusReport, error) {
	report := factsStatusReport{
		SchemaVersion: 1,
		GeneratedAt:   now.UTC(),
		Repo:          repoDir,
		BrainPath:     brainDir,
		RepoHead:      strings.TrimSpace(string(runGitOutput(ctx, runner, repoDir, "rev-parse", "HEAD"))),
		BrainManifest: evalBrainManifestSHA256(brainDir),
		Branch:        branch,
		AllBranches:   allBranches,
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return report, err
	}
	if manifest.Sources != nil && manifest.Sources.Facts != nil {
		report.ManifestFacts = manifest.Sources.Facts
	}

	if allBranches {
		report.Branch = ""
		byBranch, err := loadAllFactBranches(brainDir)
		if err != nil {
			return report, err
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
	} else {
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			return report, err
		}
		counts, err := factsStatusCountsForBranch(brainDir, branch, facts)
		if err != nil {
			return report, err
		}
		report.Totals = counts
	}

	report.FactsArmReady = report.Totals.Active > 0
	report.Warnings = factsStatusWarnings(report)
	return report, nil
}

func factsStatusCountsForBranch(brainDir, branch string, facts []factRecord) (factsStatusCounts, error) {
	counts := factsStatusCounts{}
	for _, fact := range facts {
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
		case factOriginDistilled:
			counts.Distilled++
		case factOriginAuthored:
			counts.Authored++
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
	proposals, err := loadFactProposals(brainDir, branch)
	if err != nil {
		return counts, err
	}
	counts.Proposals = len(proposals)
	return counts, nil
}

func (counts *factsStatusCounts) add(other factsStatusCounts) {
	counts.Facts += other.Facts
	counts.Active += other.Active
	counts.Superseded += other.Superseded
	counts.Retracted += other.Retracted
	counts.Distilled += other.Distilled
	counts.Authored += other.Authored
	counts.Proposals += other.Proposals
	counts.ProvenanceAnchors += other.ProvenanceAnchors
	counts.VerifiedAnchors += other.VerifiedAnchors
	counts.UnsignedAnchors += other.UnsignedAnchors
}

func factsStatusWarnings(report factsStatusReport) []string {
	var warnings []string
	if !report.FactsArmReady {
		warnings = append(warnings, "facts retriever has no active facts; facts-vs-raw release proof cannot be collected yet")
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
	fmt.Fprintf(cmd.OutOrStdout(), "facts status for %s\n", target)
	fmt.Fprintf(cmd.OutOrStdout(), "brain: %s\n", report.BrainPath)
	printFactsStatusCounts(cmd, report.Totals)
	for _, warning := range report.Warnings {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", warning)
	}
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
