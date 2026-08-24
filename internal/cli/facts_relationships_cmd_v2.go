package cli

// This is the deliberately narrow Phase 3B surface for relationship
// observations. They are local-only advisory state: unlike facts proposals,
// they cannot be accepted, rejected, synchronized, or otherwise executed.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ashtom/entire-brain/internal/factmerge"
	"github.com/spf13/cobra"
)

type factsRelationshipViewV2 struct {
	ID      string                        `json:"id"`
	Kind    string                        `json:"kind"`
	FactIDs []string                      `json:"fact_ids"`
	Subject factmerge.RelationshipSubject `json:"subject"`
	Owners  []factmerge.RelationshipOwner `json:"owners"`
}

type factsRelationshipsListReportV2 struct {
	Branch        string                    `json:"branch"`
	Relationships []factsRelationshipViewV2 `json:"relationships"`
	SkippedBlocks int                       `json:"skipped_blocks,omitempty"`
}

func newFactsRelationshipsCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relationships",
		Short: "Inspect local advisory same-subject observations",
		Long: `relationships exposes only local Phase 3B advisory observations.

They never change facts, do not enter the merge/supersede proposal queue, and
are neither synchronized nor sent to a hosted service.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	var branch string
	var jsonOut bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List valid local advisory observations for a branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			return runFactsRelationshipsListV2(cmd, brainDir, resolvedBranch, jsonOut)
		},
	}
	cmd.AddCommand(list)
	list.Flags().StringVar(&branch, "branch", "", "Branch whose local observations to list (default: current branch)")
	list.Flags().BoolVar(&jsonOut, "json", false, "Emit observations as JSON")
	return cmd
}

func runFactsRelationshipsListV2(cmd *cobra.Command, brainDir, branch string, jsonOut bool) error {
	policy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	policy.RequireDerivedClean = true
	if err := requirePrivacyDerivedRead(brainDir); err != nil {
		return err
	}
	return bufferRetrievalCommandOutput(cmd, []retrievalPrivacyPolicy{policy}, func() error {
		return renderFactsRelationshipsListV2(cmd, brainDir, branch, jsonOut)
	})
}

func renderFactsRelationshipsListV2(cmd *cobra.Command, brainDir, branch string, jsonOut bool) error {
	store, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		return err
	}
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		return err
	}
	byID := make(map[string]factRecord, len(facts))
	for _, fact := range facts {
		if fact.ID == "" {
			return fmt.Errorf("relationships: live fact has no ID")
		}
		if _, duplicate := byID[fact.ID]; duplicate {
			return fmt.Errorf("relationships: duplicate live fact ID %q", fact.ID)
		}
		byID[fact.ID] = fact
	}

	ids := make([]string, 0, len(store.entries))
	for id, relation := range store.entries {
		if relation.Branch != branch {
			continue
		}
		if err := validateLiveRelationshipV2(relation, byID, branch); err != nil {
			return fmt.Errorf("relationships: invalid local observation %s: %w", id, err)
		}
		ids = append(ids, id)
	}
	// Listing is intentionally strict rather than best-effort. The candidate
	// apply path rebuilds this local cache from current facts, so a missing row
	// is as stale/corrupt as an extra row: do not present a partial advisory
	// picture to the operator.
	budget, err := distillRelationshipBuildBudgetForReplacementV2(store, branch)
	if err != nil {
		return fmt.Errorf("relationships: budget live observations: %w", err)
	}
	expected, stats, err := buildDistillRelationshipsForBranchWithBudgetV2(facts, budget)
	if err != nil {
		return fmt.Errorf("relationships: rebuild live observations: %w", err)
	}
	stored := distillRelationshipsForBranchV2(store, branch)
	if !reflect.DeepEqual(stored, expected) {
		return fmt.Errorf("relationships: local observation store does not match current live relationship evidence")
	}
	sort.Strings(ids)
	views := make([]factsRelationshipViewV2, 0, len(ids))
	for _, id := range ids {
		relation := store.entries[id]
		views = append(views, factsRelationshipViewV2{
			ID:      relation.ID,
			Kind:    relation.Kind,
			FactIDs: append([]string(nil), relation.FactIDs...),
			Subject: relation.Subject,
			Owners:  append([]factmerge.RelationshipOwner(nil), relation.Owners...),
		})
	}
	if jsonOut {
		return writeJSON(cmd, factsRelationshipsListReportV2{Branch: branch, Relationships: views, SkippedBlocks: stats.SkippedBlocks})
	}
	if len(views) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no local relationship observations on %s\n", branch)
		if stats.SkippedBlocks > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%d bounded subject block(s) skipped\n", stats.SkippedBlocks)
		}
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%d local relationship observation(s) on %s\n", len(views), branch)
	for _, relation := range views {
		owners := make([]string, 0, len(relation.Owners))
		for _, owner := range relation.Owners {
			owners = append(owners, owner.CandidateID+"/"+owner.SourceSessionID)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s <-> %s (kind=%s top_level=%s strong_locus=%s owners=%s)\n",
			relation.Kind, relation.ID, relation.FactIDs[0], relation.FactIDs[1], relation.Subject.Kind,
			relation.Subject.TopLevel, relation.Subject.StrongLocus, strings.Join(owners, ","))
	}
	if stats.SkippedBlocks > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%d bounded subject block(s) skipped\n", stats.SkippedBlocks)
	}
	return nil
}

// validateLiveRelationshipV2 makes inspection fail closed: every stored
// observation must still describe two distinct, active local facts with the
// exact stored subject block, and each owner must still be present as a v2
// application anchor on at least one member of the pair. It intentionally
// reads no proposal queue and calls no transport.
func validateLiveRelationshipV2(relation factmerge.RelationshipProposal, byID map[string]factRecord, branch string) error {
	if err := factmerge.ValidateRelationshipProposal(relation); err != nil {
		return err
	}
	if relation.Branch != branch {
		return fmt.Errorf("branch mismatch")
	}
	left, leftOK := byID[relation.FactIDs[0]]
	right, rightOK := byID[relation.FactIDs[1]]
	if !leftOK || !rightOK || left.ID == right.ID || left.Branch != branch || right.Branch != branch || left.Status != factStatusActive || right.Status != factStatusActive {
		return fmt.Errorf("referenced facts are not distinct active facts on %s", branch)
	}
	// Match the runtime relationship builder's normalization exactly so list
	// verifies the same live subject evidence candidate apply uses.
	left.Kind = factKindOrInferred(left)
	left.Locus = append([]string(nil), factLocusOf(left)...)
	right.Kind = factKindOrInferred(right)
	right.Locus = append([]string(nil), factLocusOf(right)...)
	rebuilt, ok, err := factmerge.BuildPossibleSameSubject(left, right, relation.Owners)
	if err != nil {
		return err
	}
	if !ok || rebuilt.ID != relation.ID || rebuilt.Kind != relation.Kind || rebuilt.Subject != relation.Subject || !sameRelationshipOwnersV2(rebuilt.Owners, relation.Owners) {
		return fmt.Errorf("stored subject block no longer matches live facts")
	}
	for _, owner := range relation.Owners {
		if !relationshipOwnerHasLiveAnchorV2(owner, left, right) {
			return fmt.Errorf("owner %q/%q has no live v2 application anchor", owner.CandidateID, owner.SourceSessionID)
		}
	}
	return nil
}

func sameRelationshipOwnersV2(left, right []factmerge.RelationshipOwner) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func relationshipOwnerHasLiveAnchorV2(owner factmerge.RelationshipOwner, facts ...factRecord) bool {
	for _, fact := range facts {
		for _, anchor := range fact.Provenance {
			candidateID, _, ok := distillCandidateApplicationAnchorDetailsV2(anchor.DistillTurnID)
			if ok && candidateID == owner.CandidateID && anchor.SessionID == owner.SourceSessionID {
				return true
			}
		}
	}
	return false
}
