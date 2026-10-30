package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/entireio/entire-brain/internal/cli/entities"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"github.com/spf13/cobra"
)

// watchConfig binds the watch command at the assembly boundary: the command
// package owns the loop, flags, and cursor gating; the per-repo
// refresh/seed/distill engine and the storage-path resolution stay here.
func watchConfig(opts Options) watch.Config {
	return watch.Config{Options: opts, Deps: watch.Deps{
		Storage: func(ctx context.Context, opts base.Options, repoDir string) (string, string, error) {
			storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
			if err != nil {
				return "", "", err
			}
			return filepath.Join(filepath.Dir(storage.HeadPath), "watch.json"), storage.Key, nil
		},
		StepsForRepo: watchStepsForRepo,
	}}
}

// watchStepsForRepo builds the per-repo side effects the watch loop drives. refresh is ALWAYS the free
// (seed-agent none) deterministic refresh; seed and distill are the gated token steps. Shared by the
// single-repo `watch` and the workspace fan-out so both stay token-frugal the same way.
func watchStepsForRepo(cmd *cobra.Command, opts base.Options, w watch.Flags, repoDir string, now func() time.Time) watch.Steps {
	return watch.Steps{
		Now:         now,
		Fingerprint: func(c context.Context) string { return watchFingerprint(c, opts.Runner, repoDir) },
		Delta: func(c context.Context) (historystore.ShortTermStats, error) {
			return watchShortTermDelta(c, cmd, opts, repoDir)
		},
		Reconcile: func(c context.Context) error {
			_, warning, err := reconcileMemoryAndLaunch(c, opts, repoDir, "watch")
			if warning != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: memory coordinator: %s\n", warning)
			}
			return err
		},
		// The daemon's own tick is unattended: nobody watches its stderr, so it
		// must not run the destructive transcript sweep.
		Refresh: func(c context.Context) error { return watchDeterministicRefresh(c, cmd, opts, repoDir, true) },
		Seed:    func(c context.Context) error { return watchSeed(c, cmd, opts, w, repoDir) },
		Distill: func(c context.Context) error { return watchDistill(c, cmd, opts, w, repoDir) },
		FactsSync: func(c context.Context) (bool, error) {
			storage, err := repoStoragePaths(c, opts.Runner, opts.Env, repoDir)
			if err != nil {
				return false, err
			}
			branch := hostedSyncBranch(c, opts.Runner, repoDir)
			outcome, err := hostedFactsSyncAndPull(c, cmd.ErrOrStderr(), opts, repoDir, storage, branch)
			if err != nil {
				return outcome.Attempted, err
			}
			if branch != distillDefaultBranch {
				also, err := hostedFactsSyncAndPull(c, cmd.ErrOrStderr(), opts, repoDir, storage, distillDefaultBranch)
				return outcome.Attempted || also.Attempted, err
			}
			return outcome.Attempted, nil
		},
		Paused: func() bool { return base.BackgroundPaused(opts.Env) },
	}
}

// watchShortTermDelta runs the short-term memory path for one repo: quiet
// (output discarded; the tick logs one summary line), deterministic, and
// token-free. The returned stats let the tick escalate to consolidation when
// the buffer overflows.
//
// The watcher runs unattended as a launchd daemon, exactly like the memory
// worker prepass: nobody is watching its (discarded) stderr, so it must never
// run the destructive transcript cache sweep. Only user-invoked export/refresh
// sweeps.
func watchShortTermDelta(ctx context.Context, cmd *cobra.Command, opts Options, repoDir string) (historystore.ShortTermStats, error) {
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	deltaCmd := &cobra.Command{Use: "delta"}
	deltaCmd.SetOut(io.Discard)
	deltaCmd.SetErr(io.Discard)
	deltaCmd.SetContext(ctx)
	return runRefreshDeltaStats(deltaCmd, perRepo, true, true)
}

// watchFingerprint is a token-free change signal: the local checkpoint ref, the fetched (origin)
// checkpoint ref, and the worktree HEAD. Including the origin ref means a `git fetch` that brings in
// checkpoint activity from elsewhere also triggers a refresh. When all are unchanged since the cursor,
// there is nothing new to ingest.
func watchFingerprint(ctx context.Context, runner CommandRunner, repoDir string) string {
	rev := func(ref string) string {
		if runner == nil {
			return ""
		}
		out, _, err := runner.Run(ctx, repoDir, "git", "rev-parse", "--verify", "--quiet", ref)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return rev(v1MainRef) + ":" + rev(v1OriginRef) + ":" + rev("HEAD")
}

// watchDeterministicRefresh refreshes sessions + semantic state with the seed
// agent ALWAYS "none", then durably reconciles long-term conversation work.
// The coordinator owns projection publication, retries, and crash recovery;
// watch only nudges it. Agent seed synthesis remains a separate gated step.
//
// skipTranscriptSweep is the caller's own: this function is shared by the
// watch daemon's unattended per-tick consolidation (skip) and
// `workspace refresh --full`'s user-invoked per-member refresh (sweep), so the
// decision cannot live inside this shared body -- see
// watchDeterministicRefreshComponents.
func watchDeterministicRefresh(ctx context.Context, cmd *cobra.Command, opts Options, repoDir string, skipTranscriptSweep bool) error {
	return watchDeterministicRefreshComponents(ctx, cmd, opts, repoDir, watchTickBuildsHistoryProjection, false, nil, skipTranscriptSweep)
}

// watchDeterministicRefreshComponents runs the free refresh with explicit failure
// policy and an optional outcome observer. Setup chooses best effort; ticks fail fast.
//
// historyIndex separates the two callers that were previously identical. A
// watch TICK passes false: it runs every few minutes, the per-tick delta
// already makes new conversations recallable, and the coordinator consolidates
// the durable projection on its own schedule. `setup` passes true, because it
// is a one-shot onboarding build (the same shape as a manual `refresh`, which
// has always defaulted history on) and it ends by telling the user to run
// `entire-brain brief`. With history unbuilt that brief silently drops its
// transcript half and prints "history index missing" instead — setup's own
// recommended next command, answering with a warning about setup's own output.
// watchDeterministicRefreshOptions is the free path's refresh configuration,
// built here rather than inline so both callers' intent — above all whether the
// history projection is built — is readable and testable in one place.
func watchDeterministicRefreshOptions(historyIndex bool) refreshCommandOptions {
	return refreshCommandOptions{
		outputDir:       defaultExportDir,
		checkpointLimit: defaultCheckpointLimit,
		entireBinary:    "entire",
		graphBinary:     "entire",
		scope:           exportScopeAll,
		// A tick must not bypass the durable work ledger: the per-tick delta
		// already makes new conversations recallable while the coordinator
		// consolidates asynchronously. A one-shot onboarding build has no next
		// tick to wait for, so `setup` asks for it explicitly.
		historyIndex: historyIndex,
		semantic:     true,
		seed: seedCommandOptions{
			includeTests:       true,
			maxFileBytes:       defaultSeedMaxFileBytes,
			maxFiles:           defaultSeedMaxFiles,
			format:             "markdown+json",
			agent:              "none", // free: deterministic seed only; agent seed is gated via watchSeed
			agentQuickTimeout:  2 * time.Minute,
			agentDeepTimeout:   10 * time.Minute,
			agentTimeoutAction: "keep-quick",
			agentMaxInputBytes: defaultAgentMaxInput,
		},
	}
}

// skipTranscriptSweep classifies by INVOKER, not by this function: the watch
// daemon's tick is unattended and passes true; `setup` and
// `workspace refresh --full` are user-invoked and pass false, same as
// `refresh`/`path --ensure`/`export`. Hardcoding a value here once silently
// disabled the sweep for setup.go:1925 and workspace.go's runWorkspaceRefresh
// too, neither of which is unattended.
func watchDeterministicRefreshComponents(ctx context.Context, cmd *cobra.Command, opts Options, repoDir string, historyIndex, bestEffort bool, component func(name string, err error), skipTranscriptSweep bool) error {
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	refreshOpts := watchDeterministicRefreshOptions(historyIndex)
	refreshOpts.component = component
	refreshOpts.bestEffort = bestEffort
	refreshOpts.skipTranscriptSweep = skipTranscriptSweep
	sub := &cobra.Command{}
	sub.SetContext(ctx)
	sub.SetOut(cmd.OutOrStdout())
	sub.SetErr(cmd.ErrOrStderr())
	if err := runRefresh(ctx, sub, perRepo, refreshOpts); err != nil {
		return err
	}
	// Deterministic and token-free, like everything else in this step: index any
	// checkpoint commits that landed since the entity index's high-water mark.
	// Bounded and failure-silent — a missing `entire graph` provider must never
	// fail a tick that otherwise refreshed the brain. Under a reporter it is one
	// more degradable component, so `setup --json`, `status` and `doctor` can
	// name it instead of losing the reason to a discarded stderr line; with no
	// reporter the strict watch path keeps its warning and carries on exactly as
	// before.
	// Setup supplies the component reporter and consumes these progress lines
	// as transient labels. Ordinary watch ticks retain their quiet behavior.
	var entityTask, memoryTask *refreshProgressTask
	if component != nil {
		entityTask = newProgress(cmd.OutOrStdout(), "refresh").Begin("entity index")
	}
	entityErr := entities.RefreshIndexQuietly(ctx, entityConfig(perRepo), repoDir)
	if entityTask != nil {
		entityTask.Finish(entityErr)
	}
	switch {
	case component != nil:
		component(brainComponentEntities, entityErr)
	case entityErr != nil:
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: entity index refresh skipped: %v\n", entityErr)
	}
	if component != nil {
		memoryTask = newProgress(cmd.OutOrStdout(), "refresh").Begin("memory coordinator")
	}
	_, warning, err := reconcileMemoryAndLaunch(ctx, perRepo, repoDir, "watch")
	if memoryTask != nil {
		memoryTask.Finish(err)
	}
	if warning != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: memory coordinator: %s\n", warning)
	}
	if component != nil {
		component(brainComponentMemory, err)
	}
	if bestEffort {
		return nil
	}
	return err
}

// watchSeed is the GATED agent seed synthesis step: it re-synthesizes the seed on the cheap --model/
// --effort using --seed-agent, run at most once per --distill-every and counted against --budget (see
// watch.Tick's gating). Reuses runSeed; --update refreshes the existing seed in place.
func watchSeed(ctx context.Context, cmd *cobra.Command, opts Options, w watch.Flags, repoDir string) error {
	seedOpts := seedCommandOptions{
		update:             true,
		includeTests:       true,
		maxFileBytes:       defaultSeedMaxFileBytes,
		maxFiles:           defaultSeedMaxFiles,
		format:             "markdown+json",
		agent:              w.SeedAgent,
		model:              w.Model,
		effort:             w.Effort,
		agentQuickTimeout:  2 * time.Minute,
		agentDeepTimeout:   10 * time.Minute,
		agentTimeoutAction: "keep-quick",
		agentMaxInputBytes: defaultAgentMaxInput,
	}
	sub := &cobra.Command{}
	sub.SetContext(ctx)
	sub.SetOut(cmd.OutOrStdout())
	sub.SetErr(cmd.ErrOrStderr())
	return runSeed(ctx, sub, opts, seedOpts, repoDir)
}

func watchDistill(ctx context.Context, cmd *cobra.Command, opts Options, w watch.Flags, repoDir string) error {
	distillOpts := watchDistillOptions(w)
	busy := false
	deferred := false
	distillOpts.onDeferredSessions = func(n int) { deferred = n > 0 }
	distillOpts.onPassSkipped = func() { busy = true }
	sub := &cobra.Command{}
	sub.SetContext(ctx)
	sub.SetOut(cmd.OutOrStdout())
	sub.SetErr(cmd.ErrOrStderr())
	if err := runDistill(ctx, sub, opts, distillOpts, repoDir); err != nil {
		return err
	}
	if busy {
		return watch.ErrDistillPassBusy
	}
	if deferred {
		return watch.ErrDistillDeferred
	}
	return nil
}

func watchDistillOptions(w watch.Flags) distillCommandOptions {
	jobs := w.DistillJobs
	if jobs <= 0 {
		jobs = 1
	}
	// A negative cap is a typo, not "unlimited". Only an explicit 0 means that.
	maxSessions := w.DistillMaxSessions
	if maxSessions < 0 {
		maxSessions = 0
	}
	return distillCommandOptions{
		agent:               w.DistillAgent,
		model:               w.Model,
		effort:              w.Effort,
		timeout:             defaultDistillTimeout,
		maxChunkBytes:       defaultDistillChunkSize,
		confidenceThreshold: defaultFactConfidenceThreshold,
		// The per-pass VOLUME cap. A capped pass is ordered newest-first,
		// matching what `setup` says it did and what a reader wants first; the
		// deferred remainder stays UNCACHED, so the next window picks it up
		// rather than it being skipped forever. Ordering is left alone when
		// there is no cap, so an uncapped `watch --distill` is unchanged.
		maxSessions: maxSessions,
		newestFirst: maxSessions > 0,
		// concurrency is what the distill pipeline (and runDistill's guard)
		// actually reads. Leaving it at the zero value made every gated distill
		// the watcher ever attempted fail with "--concurrency must be greater
		// than 0" before making a single agent call. There used to be a second
		// `jobs` field to keep in step with it; `--jobs` is now a spelling of
		// --concurrency rather than a field, so there is only one value to set.
		concurrency: jobs,
	}
}
