// Package facts hosts the durable-facts command surface as it moves out of
// package cli (COR-2268). The remaining cli-private seams — repo/target
// resolution, the distill agent runners, and the brain-manifest writer — are
// injected through Deps; everything else comes from factmerge, factstore, and
// base directly.
package facts

import (
	"context"
	"io"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// Deps are the cli-bound seams the facts commands still need. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// ResolveFactsTarget resolves the repo, brain directory, and the branch
	// facts are scoped to (the live git branch unless overridden).
	ResolveFactsTarget func(ctx context.Context, opts base.Options, target, branchOverride string) (repoDir, brainDir, branch string, err error)
	// InARepository reports whether the current directory is actually inside a
	// git working tree, erring only when repository identity is undecidable.
	InARepository func(ctx context.Context, opts base.Options) (bool, error)
	// AgentSurfaceTarget resolves the repository path argument for a command.
	AgentSurfaceTarget func(opts base.Options, args []string) string
	// DefaultRefreshAgent picks the agent an "auto" classification resolves to.
	DefaultRefreshAgent func(ctx context.Context, runner base.CommandRunner, repoDir string) string
	// DistillAgentRunner returns the runner that executes the named agent.
	DistillAgentRunner func(agent string) base.DistillAgentRunner
	// UpdateFactSourceManifest refreshes sources.facts in the brain manifest;
	// the caller already holds the brain write lock.
	UpdateFactSourceManifest func(brainDir string, now time.Time) error
	// DistillTimeout bounds one agent classification call.
	DistillTimeout time.Duration
	// RankFacts ranks a branch's facts against a query, BM25 lexical only.
	RankFacts func(facts []factmerge.Record, query string, limit int, includeAll bool) []factmerge.Record
	// LoadFactProposals reads a branch's pending proposals; a missing file
	// yields none.
	LoadFactProposals func(brainDir, branch string) ([]factmerge.Proposal, error)
	// RecordServedFacts writes the vitality receipt for facts a read surface
	// served, resolving the receipt clock and worktree HEAD from opts.
	RecordServedFacts func(errW io.Writer, ctx context.Context, opts base.Options, brainDir, branch, surface, repoDir, taskText string, served []factmerge.Record)
	// EmptyResultBlindSpot explains an empty result on a branch whose store
	// holds memories elsewhere.
	EmptyResultBlindSpot func(brainDir, branch string) string
	// RunRecallEvidence runs recall's deterministic evidence mode, which still
	// lives with the cli-resident evidence engine.
	RunRecallEvidence func(cmd *cobra.Command, brainDir, branch, query string, limit, budget int, jsonOut bool) error
	// ExpandQuery expands a query with agent-generated retrieval terms and
	// returns the effective query (unchanged when expansion is empty).
	ExpandQuery func(ctx context.Context, agent string, agentCommand []string, model, repoDir, query string) (string, error)
}

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}
