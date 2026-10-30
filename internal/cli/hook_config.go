package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/command/hook"
	"github.com/entireio/entire-brain/internal/cli/entities"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// hookCommandConfig binds the cli-private seams the extracted hook commands
// still depend on (COR-2268): target resolution, the locus-relevance engine,
// the legacy lexical scorer, the vitality receipt, and the whole engine-bound
// session-end arm.
func hookCommandConfig(opts Options) hook.Config {
	return hook.Config{
		Options: opts,
		Deps: hook.Deps{
			ResolveTarget:         resolveRetrieveTarget,
			FactsRelevantToChange: hookFactsRelevantToChange,
			FactLexicalScore:      factLexicalScore,
			RecordServed:          recordServedHookFacts,
			SessionEnd:            runHookSessionEnd,
		},
	}
}

// hookFactsRelevantToChange binds the locus-relevance engine with no semantic
// symbols: the hook is warm-path only (no semantic-store open; symbol-level
// pre-edit enrichment arrives with the worktree overlay seam).
func hookFactsRelevantToChange(files []string, facts []factRecord, limit int) []factRecord {
	return factsRelevantToChange(files, nil, facts, limit)
}

// recordServedHookFacts binds the cli-private vitality receipt arm for the
// extracted hook commands: the receipt clock, the worktree HEAD, and the
// best-effort recording path.
func recordServedHookFacts(errW io.Writer, ctx context.Context, opts base.Options, brainDir, branch, surface, repoDir, taskText string, served []factRecord) {
	recordServedFacts(errW, vitalityNow(opts), brainDir, branch, surface,
		vitalityHead(ctx, opts.Runner, repoDir), taskText, factRecordIDs(served))
}

// runHookSessionEnd is the engine-bound session-end arm, injected whole as one
// seam: it binds the memory coordinator's worker-origin/pause gates, the watch
// incremental export, the durable lifecycle hint, the entity index, hosted
// sync, and the bounded per-session distill (cli-private distillCommandOptions
// and defaults).
func runHookSessionEnd(cmd *cobra.Command, opts base.Options, sessionID, branch, agent, model, effort string) error {
	// Paused: skipping loses nothing. The captured session stays
	// canonical; reconciliation re-exports it after `start`, and the next
	// gated or manual distill extracts its facts.
	if memoryWorkerOrigin() || base.BackgroundPaused(opts.Env) {
		return nil
	}
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := base.ResolveLocalTargetRepoDir(cmd.Context(), opts.Runner, target)
	if err != nil || !local {
		return nil // no local repo: silence, never an error
	}
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	// Export only changed sessions and publish the short-term overlay so
	// recall and distill can see the just-ended session immediately. The
	// durable coordinator performs the heavier long-term consolidation.
	sub := &cobra.Command{}
	sub.SetContext(cmd.Context())
	sub.SetOut(cmd.ErrOrStderr())
	sub.SetErr(cmd.ErrOrStderr())
	_, deltaErr := watchShortTermDelta(cmd.Context(), sub, perRepo, repoDir)

	// Record the lifecycle event even when export failed. The hint is
	// content-free and durable; startup/watch reconciliation or a later
	// worker pass can repair the missed export without retaining content.
	storage, storageErr := repoStoragePaths(cmd.Context(), perRepo.Runner, perRepo.Env, repoDir)
	resolvedBranch := strings.TrimSpace(branch)
	if resolvedBranch == "" {
		if current, gitErr := base.GitScalar(cmd.Context(), perRepo.Runner, repoDir, "branch", "--show-current"); gitErr == nil {
			resolvedBranch = strings.TrimSpace(current)
		}
	}
	if storageErr == nil {
		_, warning, hintErr := recordMemoryLifecycleAndLaunch(
			storage.BrainDir, repoDir, storage.Key, sessionID, resolvedBranch, "session_end", perRepo.Now().UTC(),
		)
		if hintErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "session-end: memory hint failed: %v\n", hintErr)
		} else if warning != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "session-end: %s\n", warning)
		}
	}
	// Deterministic, token-free and bounded: index the commits the
	// session just produced so "which checkpoint changed this symbol"
	// is answerable immediately. It runs AFTER the durable lifecycle
	// hint (that must never wait on a slow provider) and independently
	// of the export result, since it reads git, not transcripts. Never
	// fails the hook.
	if entityErr := entities.RefreshIndexQuietly(cmd.Context(), entityConfig(perRepo), repoDir); entityErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "session-end: entity index refresh skipped: %v\n", entityErr)
	}
	// Hosted sync runs on every exit path — the pull half matters even
	// when this session's export or distill could not: current branch,
	// then the default branch (the spec's per-trigger coverage).
	// hostedFactsSyncAndPull soaks every gated/soft failure itself;
	// the print covers programming errors only. Stderr only — the
	// hook's stdout is machine-read.
	syncHosted := func() {
		if storageErr != nil {
			return
		}
		syncBranches := []string{distillDefaultBranch}
		if resolvedBranch != "" && resolvedBranch != distillDefaultBranch {
			syncBranches = []string{resolvedBranch, distillDefaultBranch}
		}
		for _, syncBranch := range syncBranches {
			if _, err := hostedFactsSyncAndPull(cmd.Context(), cmd.ErrOrStderr(), perRepo, repoDir, storage, syncBranch); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "session-end: hosted memory sync failed (local brain unaffected): %v\n", err)
			}
		}
	}
	if deltaErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "session-end: incremental export failed: %v\n", deltaErr)
		syncHosted()
		return nil
	}
	distillOpts := distillCommandOptions{
		session:             sessionID,
		agent:               agent,
		model:               model,
		effort:              effort,
		timeout:             defaultDistillTimeout,
		maxChunkBytes:       defaultDistillChunkSize,
		confidenceThreshold: defaultFactConfidenceThreshold,
		// Constructed in Go, so the flag defaults never applied: without
		// this the hook's distill was rejected outright by runDistill's
		// concurrency guard, and the "closing the write loop" step
		// silently never ran.
		concurrency: defaultDistillConcurrency,
	}
	if err := runDistill(cmd.Context(), sub, opts, distillOpts, repoDir); err != nil {
		// No agent on PATH, session not exported yet, etc.: report on
		// stderr for the curious, succeed for the harness.
		fmt.Fprintf(cmd.ErrOrStderr(), "session-end: distill skipped: %v\n", err)
	}
	syncHosted()
	return nil
}
