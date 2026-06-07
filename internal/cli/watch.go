package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// watch keeps the brain fresh automatically. Token-frugality is structural, not a quota:
// the deterministic refresh (sessions + semantic index + history index, seed agent "none") spends
// ZERO agent tokens and runs whenever the checkpoint/HEAD fingerprint changes; the only token-spending
// step (distill, optionally seed synthesis) is OFF by default and, when enabled, runs at most once per
// --distill-every, on the cheap --model/--effort, capped by --budget, and a persisted cursor means a
// restart never re-spends within the interval.

type watchCommandOptions struct {
	interval     time.Duration
	once         bool
	distill      bool
	distillEvery time.Duration
	distillAgent string
	seedAgent    string
	model        string
	effort       string
	budget       int // cap on distill runs this process (each spends tokens); 0 = unlimited; resets on restart
}

// watchCursor persists across restarts so the daemon never re-refreshes unchanged state and never
// re-distills within --distill-every after a restart.
type watchCursor struct {
	LastFingerprint string    `json:"last_fingerprint"`
	LastRefreshAt   time.Time `json:"last_refresh_at,omitempty"`
	LastDistillAt   time.Time `json:"last_distill_at,omitempty"`
}

// watchSteps are the side-effecting operations of one tick. Injected so the loop's gating logic
// (change detection, distill interval/budget, cursor persistence) is unit-testable without running a
// real refresh/distill.
type watchSteps struct {
	now         func() time.Time
	fingerprint func(context.Context) string
	refresh     func(context.Context) error
	distill     func(context.Context) error
}

// defaultWatchOptions are the shared defaults for `watch` and `workspace watch`.
func defaultWatchOptions() watchCommandOptions {
	return watchCommandOptions{
		interval:     5 * time.Minute,
		distillEvery: 24 * time.Hour,
		distillAgent: "codex",
		seedAgent:    "none",
	}
}

// bindWatchFlags registers the watch flags on a command, shared so `watch` and `workspace watch` expose
// the identical token-frugality controls.
func bindWatchFlags(cmd *cobra.Command, w *watchCommandOptions) {
	cmd.Flags().DurationVar(&w.interval, "interval", w.interval, "Poll interval between ticks")
	cmd.Flags().BoolVar(&w.once, "once", false, "Run a single pass and exit (no daemon loop)")
	cmd.Flags().BoolVar(&w.distill, "distill", false, "Also run distill (SPENDS TOKENS) when new sessions land and --distill-every has elapsed")
	cmd.Flags().DurationVar(&w.distillEvery, "distill-every", w.distillEvery, "Minimum interval between distill runs")
	cmd.Flags().StringVar(&w.distillAgent, "agent", w.distillAgent, "Agent for the distill step (used only with --distill)")
	cmd.Flags().StringVar(&w.seedAgent, "seed-agent", w.seedAgent, "Seed synthesis agent for the deterministic refresh (none = no tokens)")
	cmd.Flags().StringVar(&w.model, "model", "", "Fast/cheap model for the gated agent steps (distill/seed)")
	cmd.Flags().StringVar(&w.effort, "effort", "", "Reasoning effort for the gated agent steps")
	cmd.Flags().IntVar(&w.budget, "budget", 0, "Cap on distill runs this process, each of which spends tokens; 0 = unlimited. Counts reset on restart — the durable guard against re-spend is --distill-every + the cursor.")
}

func newWatchCommand(opts Options) *cobra.Command {
	w := defaultWatchOptions()
	cmd := &cobra.Command{
		Use:   "watch [path]",
		Short: "Keep the brain fresh automatically (deterministic refresh is free; agent steps are gated)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if len(args) == 1 {
				target = args[0]
			}
			return runWatch(cmd.Context(), cmd, opts, w, target)
		},
	}
	bindWatchFlags(cmd, &w)
	return cmd
}

func runWatch(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions, target string) error {
	if w.interval <= 0 {
		w.interval = 5 * time.Minute
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("watch requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	cursorPath := filepath.Join(filepath.Dir(storage.HeadPath), "watch.json")
	steps := watchStepsForRepo(cmd, opts, w, repoDir, now)
	// Wire SIGINT/SIGTERM so a long-running daemon stops cleanly between ticks instead of being killed
	// mid-refresh; this is what makes the loop's ctx-cancel paths live (the root command runs with a
	// Background context otherwise).
	ctx, stop := watchSignalContext(ctx)
	defer stop()
	fmt.Fprintf(cmd.OutOrStdout(), "[watch] %s — interval %s, distill=%v (every %s, agent=%s, model=%q, budget=%d)\n",
		storage.Key, w.interval, w.distill, w.distillEvery, w.distillAgent, w.model, w.budget)
	return watchLoop(ctx, cmd.OutOrStdout(), w, cursorPath, steps)
}

// watchSignalContext returns a context cancelled on SIGINT/SIGTERM so `watch` and `workspace watch`
// shut down cleanly between ticks (finish the current tick, then return) rather than dying abruptly.
func watchSignalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

func watchLoop(ctx context.Context, out io.Writer, w watchCommandOptions, cursorPath string, steps watchSteps) error {
	agentCalls := 0
	for {
		if ctx.Err() != nil {
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		}
		watchTick(ctx, out, w, cursorPath, steps, &agentCalls)
		if w.once {
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		case <-time.After(w.interval):
		}
	}
}

// watchTick runs one pass: cheap change detection, then (only on change) the free deterministic refresh
// and the gated distill. agentCalls is shared across ticks/members so --budget caps total token spend.
func watchTick(ctx context.Context, out io.Writer, w watchCommandOptions, cursorPath string, steps watchSteps, agentCalls *int) {
	cursor := loadWatchCursor(cursorPath)
	fp := steps.fingerprint(ctx)
	changed := fp != cursor.LastFingerprint || cursor.LastRefreshAt.IsZero()
	if !changed {
		fmt.Fprintln(out, "[watch] no change; nothing to do")
		return
	}
	if err := steps.refresh(ctx); err != nil {
		// Refresh failed: do NOT spend tokens distilling an unrefreshed brain, and do NOT advance the
		// cursor. The next tick retries the (free) refresh; distill only ever runs on a brain that was
		// actually refreshed this tick — so a persistent refresh failure can never re-distill.
		fmt.Fprintf(out, "[watch] refresh failed (skipping distill this tick): %v\n", err)
		return
	}
	cursor.LastFingerprint = fp
	cursor.LastRefreshAt = steps.now().UTC()
	_ = saveWatchCursor(cursorPath, cursor)
	fmt.Fprintln(out, "[watch] refreshed (deterministic, no agent tokens)")
	if ok, reason := watchShouldDistill(w, cursor, *agentCalls, steps.now().UTC()); w.distill && !ok {
		fmt.Fprintf(out, "[watch] distill skipped: %s\n", reason)
	} else if ok {
		if err := steps.distill(ctx); err != nil {
			fmt.Fprintf(out, "[watch] distill failed: %v\n", err)
		} else {
			cursor.LastDistillAt = steps.now().UTC()
			*agentCalls++
			_ = saveWatchCursor(cursorPath, cursor)
			fmt.Fprintln(out, "[watch] distilled facts (agent step; spent tokens)")
		}
	}
}

// watchStepsForRepo builds the per-repo side effects (fingerprint/refresh/distill) the watch loop drives.
// Shared by the single-repo `watch` and the workspace fan-out so both stay token-frugal the same way.
func watchStepsForRepo(cmd *cobra.Command, opts Options, w watchCommandOptions, repoDir string, now func() time.Time) watchSteps {
	return watchSteps{
		now:         now,
		fingerprint: func(c context.Context) string { return watchFingerprint(c, opts.Runner, repoDir) },
		refresh:     func(c context.Context) error { return watchDeterministicRefresh(c, cmd, opts, w, repoDir) },
		distill:     func(c context.Context) error { return watchDistill(c, cmd, opts, w, repoDir) },
	}
}

// watchShouldDistill decides whether the token-spending distill step runs this tick: only when
// --distill is set AND --distill-every has elapsed since the last distill (cursor-tracked across
// restarts) AND the --budget cap is not yet reached. This is the structural token-frugality guarantee.
func watchShouldDistill(w watchCommandOptions, cursor watchCursor, agentCalls int, now time.Time) (bool, string) {
	if !w.distill {
		return false, "disabled"
	}
	if !(cursor.LastDistillAt.IsZero() || now.Sub(cursor.LastDistillAt) >= w.distillEvery) {
		return false, "--distill-every not elapsed"
	}
	if w.budget > 0 && agentCalls >= w.budget {
		return false, fmt.Sprintf("--budget %d reached", w.budget)
	}
	return true, ""
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

// watchDeterministicRefresh refreshes sessions + semantic + history index with the seed agent gated by
// --seed-agent (default "none" = no tokens). It reuses runRefresh, pointing it at repoDir via the env.
func watchDeterministicRefresh(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions, repoDir string) error {
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	refreshOpts := refreshCommandOptions{
		outputDir:       defaultExportDir,
		checkpointLimit: defaultCheckpointLimit,
		entireBinary:    "entire",
		semBinary:       "entire",
		scope:           exportScopeAll,
		historyIndex:    true,
		semantic:        true,
		seed: seedCommandOptions{
			includeTests:       true,
			maxFileBytes:       defaultSeedMaxFileBytes,
			maxFiles:           defaultSeedMaxFiles,
			format:             "markdown+json",
			agent:              w.seedAgent,
			model:              w.model,
			effort:             w.effort,
			agentQuickTimeout:  2 * time.Minute,
			agentDeepTimeout:   10 * time.Minute,
			agentTimeoutAction: "keep-quick",
			agentMaxInputBytes: defaultAgentMaxInput,
		},
	}
	sub := &cobra.Command{}
	sub.SetContext(ctx)
	sub.SetOut(cmd.OutOrStdout())
	sub.SetErr(cmd.ErrOrStderr())
	return runRefresh(ctx, sub, perRepo, refreshOpts)
}

func watchDistill(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions, repoDir string) error {
	distillOpts := distillCommandOptions{
		agent:               w.distillAgent,
		model:               w.model,
		effort:              w.effort,
		timeout:             defaultDistillTimeout,
		maxChunkBytes:       defaultDistillChunkSize,
		confidenceThreshold: defaultFactConfidenceThreshold,
	}
	sub := &cobra.Command{}
	sub.SetContext(ctx)
	sub.SetOut(cmd.OutOrStdout())
	sub.SetErr(cmd.ErrOrStderr())
	return runDistill(ctx, sub, opts, distillOpts, repoDir)
}

func loadWatchCursor(path string) watchCursor {
	var c watchCursor
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func saveWatchCursor(path string, c watchCursor) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}
