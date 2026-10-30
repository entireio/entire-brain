package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/command/facts"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// factsCommandConfig binds the cli-private seams the extracted facts commands
// still depend on (COR-2268): target resolution over repoStoragePaths, the
// distill agent runners, the brain-manifest writer, and recall's ranking,
// proposal, receipt, blind-spot, evidence, and query-expansion arms.
func factsCommandConfig(opts Options) facts.Config {
	return facts.Config{
		Options: opts,
		Deps: facts.Deps{
			ResolveFactsTarget:       resolveFactsTarget,
			InARepository:            inARepository,
			AgentSurfaceTarget:       agentSurfaceTarget,
			DefaultRefreshAgent:      defaultRefreshAgent,
			DistillAgentRunner:       defaultDistillAgentRunner,
			UpdateFactSourceManifest: updateFactSourceManifestLocked,
			DistillTimeout:           defaultDistillTimeout,
			RankFacts:                rankFacts,
			LoadFactProposals:        loadFactProposals,
			RecordServedFacts:        recordServedFactsReceipt,
			EmptyResultBlindSpot:     emptyResultBlindSpotOnBranch,
			RunRecallEvidence:        runRecallEvidence,
			ExpandQuery:              expandRecallQuery,
		},
	}
}

// resolveFactsTarget resolves the repo, brain directory, and the branch facts
// are scoped to (the live git branch unless overridden). It is shared by the
// fact read/write commands.
func resolveFactsTarget(ctx context.Context, opts Options, target, branchOverride string) (repoDir, brainDir, branch string, err error) {
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return "", "", "", err
	}
	if !local {
		return "", "", "", fmt.Errorf("memories require a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return "", "", "", err
	}
	branch = strings.TrimSpace(branchOverride)
	if branch == "" {
		if current, gitErr := base.GitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); gitErr == nil {
			branch = strings.TrimSpace(current)
		}
	}
	if branch == "" {
		branch = distillDefaultBranch
	}
	return repoDir, storage.BrainDir, branch, nil
}

// recordServedFactsReceipt binds the cli-private vitality receipt arm for the
// extracted read commands: the receipt clock, the worktree HEAD, and the
// served-id extraction.
func recordServedFactsReceipt(errW io.Writer, ctx context.Context, opts Options, brainDir, branch, surface, repoDir, taskText string, served []factRecord) {
	recordServedFacts(errW, vitalityNow(opts), brainDir, branch, surface,
		vitalityHead(ctx, opts.Runner, repoDir), taskText, factRecordIDs(served))
}

// expandRecallQuery binds the cli-private query-expansion arm (prompt, agent
// args, runner, cache) for the extracted recall command and returns the
// effective query.
func expandRecallQuery(ctx context.Context, agent string, agentCommand []string, model, repoDir, query string) (string, error) {
	expandArgs, expErr := base.DistillAgentCommandArgs(agent, agentCommand, queryExpansionPrompt())
	if expErr != nil {
		return "", fmt.Errorf("expand agent: %w", expErr)
	}
	expandArgs = base.InjectAgentModel(expandArgs, agent, model)
	exp, expErr := expandQuery(ctx, defaultDistillAgentRunner(agent), expandArgs, repoDir, query, loadExpansionCache(""))
	if expErr != nil {
		return "", fmt.Errorf("expand query: %w", expErr)
	}
	return expandedQuery(query, exp), nil
}
