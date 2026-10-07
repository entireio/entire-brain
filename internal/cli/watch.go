package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// watch keeps the brain fresh automatically. Token-frugality is structural, not a quota:
// the deterministic refresh (sessions + semantic index + durable history reconciliation, seed agent
// ALWAYS "none") spends ZERO agent tokens and runs whenever the checkpoint/HEAD fingerprint changes.
// The ONLY token-spending
// work — distill and/or agent seed synthesis (--seed-agent) — is OFF by default and, when enabled, runs
// at most once per --distill-every, on the cheap --model/--effort, capped by --budget, and a persisted
// cursor means a restart never re-spends within the interval. Both token steps share the one gate, so
// --seed-agent is bounded exactly like --distill (it is NOT per-change spend).
type watchCommandOptions struct {
	interval time.Duration
	once     bool
	// consolidateEvery throttles the FULL deterministic refresh (consolidation:
	// sessions + semantic + history + clearing short-term memory). The
	// fingerprint includes checkpoint refs, so during active agent work every
	// turn would otherwise trigger the heavy path each tick; the per-tick
	// short-term delta carries freshness in between. 0 = consolidate on every
	// change (the pre-short-term behavior). A full short-term buffer always
	// forces consolidation regardless of the interval.
	consolidateEvery time.Duration
	distill          bool
	distillEvery     time.Duration
	distillAgent     string
	distillJobs      int
	// distillMaxSessions caps how many NOT-YET-DISTILLED sessions one gated
	// pass processes (0 = no cap). --distill-every and the persisted cursor
	// bound how OFTEN the daemon spends; this is the only thing that bounds how
	// MUCH one spend costs. Without it a supervised watcher's first gated tick
	// distills the entire remaining corpus of every repo in the workspace —
	// exactly the spend `setup` told the user it had capped at 25.
	distillMaxSessions int
	seedAgent          string
	model              string
	effort             string
	// budget caps gated agent runs for the life of THIS PROCESS (distill + seed;
	// each spends tokens); 0 = unlimited. It is not window-scoped and never
	// resets while the process lives, which makes it the wrong guard for a
	// supervised daemon — see brainWatchDaemonArgs, which deliberately omits it.
	budget int
}

// watchCursor persists across restarts so the daemon never re-refreshes unchanged state and never
// re-runs the gated agent work within --distill-every after a restart.
type watchCursor struct {
	PendingSeed      bool      `json:"pending_seed,omitempty"`
	PendingDistill   bool      `json:"pending_distill,omitempty"`
	LastFingerprint  string    `json:"last_fingerprint"`
	LastRefreshAt    time.Time `json:"last_refresh_at,omitempty"`
	LastAgentSpendAt time.Time `json:"last_agent_spend_at,omitempty"`
	// ConsolidationRepairs counts CONSECUTIVE full refreshes that ran early
	// because the short-term delta failed. It is the backoff state for
	// watchRepairWait: a single failure repairs promptly, a persistent one
	// settles back to the normal consolidation cadence instead of firing the
	// heavy refresh every tick. Reset to 0 by the first healthy delta.
	ConsolidationRepairs int `json:"consolidation_repairs,omitempty"`
}

// watchRepairBackoffMax caps ConsolidationRepairs so the persisted counter stays
// bounded and the doubling in watchRepairWait can never overflow.
const watchRepairBackoffMax = 16

const watchCursorLockName = "watch.lock"

// watchSteps are the side-effecting operations of one tick. Injected so the loop's gating logic
// (change detection, agent interval/budget, cursor persistence) is unit-testable without running a real
// refresh/seed/distill. refresh is ALWAYS the free (seed-agent none) deterministic refresh; seed and
// distill are the gated, token-spending steps.
type watchSteps struct {
	now         func() time.Time
	fingerprint func(context.Context) string
	// delta is the cheap short-term memory update (incremental export + overlay
	// index of changed transcripts). It runs EVERY tick, before the change
	// gate, so an in-flight session's turns are recallable near-real-time; the
	// full refresh below is consolidation and is throttled by
	// --consolidate-every unless the delta reports the buffer full.
	delta func(context.Context) (shortTermStats, error)
	// reconcile durably queues long-term conversation work independently of
	// the git/checkpoint fingerprint. Exported transcripts can change while
	// that fingerprint stays stable (for example, a late host export).
	reconcile func(context.Context) error
	refresh   func(context.Context) error
	seed      func(context.Context) error
	distill   func(context.Context) error
	paused    func() bool
}

// defaultWatchOptions are the shared defaults for `watch` and `workspace watch`.
func defaultWatchOptions() watchCommandOptions {
	return watchCommandOptions{
		interval:         5 * time.Minute,
		consolidateEvery: 30 * time.Minute,
		distillEvery:     24 * time.Hour,
		distillAgent:     "codex",
		distillJobs:      1,
		seedAgent:        "none",
	}
}

// bindWatchFlags registers the watch flags on a command, shared so `watch` and `workspace watch` expose
// the identical token-frugality controls.
func bindWatchFlags(cmd *cobra.Command, w *watchCommandOptions) {
	cmd.Flags().DurationVar(&w.interval, "interval", w.interval, "Poll interval between ticks")
	cmd.Flags().DurationVar(&w.consolidateEvery, "consolidate-every", w.consolidateEvery, "Minimum interval between full consolidations (heavy refresh); the per-tick short-term delta covers freshness in between. 0 = consolidate on every change; a full short-term buffer always consolidates")
	cmd.Flags().BoolVar(&w.once, "once", false, "Run a single pass and exit (no daemon loop)")
	cmd.Flags().BoolVar(&w.distill, "distill", false, "Run distill (SPENDS TOKENS) when new sessions land — gated by --distill-every + --budget")
	cmd.Flags().DurationVar(&w.distillEvery, "distill-every", w.distillEvery, "Minimum interval between gated agent runs (distill and/or seed synthesis)")
	cmd.Flags().StringVar(&w.distillAgent, "agent", w.distillAgent, "Agent for the distill step (used only with --distill)")
	cmd.Flags().IntVar(&w.distillJobs, "jobs", w.distillJobs, "Parallel distill extraction jobs when --distill is enabled; reconciliation and writes remain deterministic")
	cmd.Flags().IntVar(&w.distillMaxSessions, "max-sessions", w.distillMaxSessions, "Cap how many not-yet-distilled sessions ONE gated distill pass processes (0 = no cap). --distill-every bounds how often the daemon spends; this bounds how much each spend costs")
	cmd.Flags().StringVar(&w.seedAgent, "seed-agent", w.seedAgent, "Agent for gated seed synthesis (SPENDS TOKENS); none = deterministic seed only. Bounded by --distill-every + --budget, NOT per-change")
	cmd.Flags().StringVar(&w.model, "model", "", "Fast/cheap model for the gated agent steps (distill/seed)")
	cmd.Flags().StringVar(&w.effort, "effort", "", "Reasoning effort for the gated agent steps (codex --config model_reasoning_effort=, claude --effort)")
	cmd.Flags().IntVar(&w.budget, "budget", 0, "Cap on gated agent runs for the LIFE OF THIS PROCESS (distill + seed); 0 = unlimited. It never resets, so on a long-lived daemon --budget 1 means one run EVER, not one per window — the durable per-window guard is --distill-every + the persisted cursor. Use it only for a bounded foreground run.")
}

func newWatchCommand(opts Options) *cobra.Command {
	w := defaultWatchOptions()
	cmd := &cobra.Command{
		Use:   "watch [path]",
		Short: "Keep the brain up to date automatically",
		Long:  "Keep the brain fresh automatically (deterministic refresh is free; agent steps are gated)",
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
	if w.distillJobs <= 0 {
		return fmt.Errorf("--jobs must be greater than 0")
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
	fmt.Fprintf(cmd.OutOrStdout(), "[watch] %s — interval %s, distill=%v (every %s, agent=%s, jobs=%d, model=%q, max-sessions=%s, run-budget=%s)\n",
		storage.Key, w.interval, w.distill, w.distillEvery, w.distillAgent, w.distillJobs, w.model,
		watchCapLabel(w.distillMaxSessions), watchCapLabel(w.budget))
	return watchLoop(ctx, cmd.OutOrStdout(), w, cursorPath, steps)
}

// watchCapLabel renders a cap where a non-positive value means "no cap".
//
// The banner printed a bare "budget=0", and 0 is the value a setup-installed
// watcher ALWAYS has: setup deliberately never passes --budget, because that
// counter is process-lifetime and never resets (see brainWatchDaemonArgs). So
// the one number the daemon's log offered about spend was both meaningless and
// easy to read as "zero allowed", while the cap that actually binds it --
// --max-sessions, carried per workspace in the machine watch plan -- was not
// printed at all.
func watchCapLabel(value int) string {
	if value <= 0 {
		return "uncapped"
	}
	return strconv.Itoa(value)
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

// watchTick runs one pass: cheap change detection, then (only on change) the free deterministic refresh,
// then the GATED token-spending work (seed synthesis and/or distill). agentCalls is shared across
// ticks/members so --budget caps total token spend across the whole run.
func watchTick(ctx context.Context, out io.Writer, w watchCommandOptions, cursorPath string, steps watchSteps, agentCalls *int) {
	if steps.paused != nil && steps.paused() {
		fmt.Fprintln(out, "[watch] paused — `entire-brain start` resumes")
		return
	}
	// Short-term memory first, every tick: cheap (change detection + only
	// changed transcripts), and it must never block or fail the tick; a delta
	// failure just means the long-term path repairs freshness later.
	bufferFull := false
	// No delta step configured means there is no short-term path that could
	// have failed, so the throttle applies at its normal cadence rather than
	// treating every tick as a repair.
	deltaHealthy := true
	deltaNeedsReconcile := false
	if steps.delta != nil {
		stats, err := steps.delta(ctx)
		if err != nil {
			deltaHealthy = false
			deltaNeedsReconcile = true
			fmt.Fprintf(out, "[watch] short-term memory update failed (continuing): %v\n", err)
		} else {
			// A delta that failed to scan any transcript did NOT fully carry
			// the new work, so it must not defer consolidation.
			deltaHealthy = stats.Failed == 0
			bufferFull = stats.Truncated
			deltaNeedsReconcile = stats.Files > 0 || stats.Failed > 0 || stats.Truncated
			fmt.Fprintf(out, "[watch] short-term memory updated (%d records from %d changed transcripts)\n", stats.Records, stats.Files)
			if stats.Failed > 0 {
				fmt.Fprintf(out, "[watch] short-term memory incomplete: %d transcripts failed to scan; consolidation will repair\n", stats.Failed)
			}
		}
	}
	// Delta state is itself a source-change signal. Do this before the git
	// fingerprint gate so a changed/failed/overflowed export is never stranded
	// in the short-term overlay waiting for an unrelated commit or restart.
	if deltaNeedsReconcile && steps.reconcile != nil {
		if err := steps.reconcile(ctx); err != nil {
			fmt.Fprintf(out, "[watch] memory reconciliation failed (durable overlay retained): %v\n", err)
		} else {
			fmt.Fprintln(out, "[watch] long-term memory reconciliation queued")
		}
	}
	cursor := loadWatchCursor(cursorPath)
	fp := steps.fingerprint(ctx)
	changed := fp != cursor.LastFingerprint || cursor.LastRefreshAt.IsZero()
	if !changed && !(cursor.PendingSeed && w.seedAgent != "none" || cursor.PendingDistill && w.distill) {
		fmt.Fprintln(out, "[watch] no change; nothing to do")
		return
	}
	// Consolidation throttle: the fingerprint includes checkpoint refs, so an
	// active agent session flips it every turn; without a throttle the heavy
	// full refresh would run every tick exactly when the machine is busiest.
	// A failed delta means nothing carried the new work, so the full refresh is
	// the repair path and must not wait the whole interval (Bugbot PR #78),
	// but it is throttled too, just on a shorter clock. The delta and the full
	// refresh contend for the SAME brain write lock, so an unthrottled repair
	// would fire the heavy path every tick for as long as the delta keeps
	// failing, contending for the very lock that broke it. watchRepairWait
	// escalates after one tick and then backs off to the normal cadence. An
	// overflowed buffer still forces consolidation immediately: recall
	// completeness is at risk, and a successful consolidation clears the
	// overlay, so it cannot repeat the way a failing delta can. The first ever
	// refresh always runs.
	if changed && w.consolidateEvery > 0 && !cursor.LastRefreshAt.IsZero() && !bufferFull {
		wait := w.consolidateEvery
		state := "short-term memory is current"
		if !deltaHealthy {
			wait = watchRepairWait(w, cursor.ConsolidationRepairs)
			state = fmt.Sprintf("short-term memory failed; repair refresh backing off after %d consecutive repairs", cursor.ConsolidationRepairs)
		}
		if since := steps.now().UTC().Sub(cursor.LastRefreshAt); since < wait {
			fmt.Fprintf(out, "[watch] consolidation deferred (%s since last full refresh; due in %s; %s)\n",
				since.Round(time.Second), (wait - since).Round(time.Second), state)
			return
		}
	}
	if changed {
		if err := steps.refresh(ctx); err != nil {
			// Refresh failed: do NOT spend tokens on an unrefreshed brain, and do NOT advance the cursor.
			// The next tick retries the free refresh before attempting pending agent work.
			fmt.Fprintf(out, "[watch] refresh failed (skipping agent work this tick): %v\n", err)
			return
		}
	}
	reserved, reason, err := reserveWatchAgentSpend(cursorPath, fp, w, agentCalls, steps.now().UTC(), !deltaHealthy)
	if err != nil {
		fmt.Fprintf(out, "[watch] cursor update failed (skipping agent work this tick): %v\n", err)
		return
	}
	if changed {
		fmt.Fprintln(out, "[watch] refreshed (deterministic, no agent tokens)")
	}

	if !w.agentWorkEnabled() {
		return
	}
	if !reserved {
		fmt.Fprintf(out, "[watch] agent work skipped: %s\n", reason)
		return
	}
	// Gated token-spending work: agent seed synthesis and/or distill, at most once per --distill-every,
	// counted against --budget. A transient failure of one step is intentionally best-effort: we still
	// advance the spend cursor + budget below so a failed step retries on the NEXT interval, not every
	// tick (and a step that already burned tokens before failing can't be re-run for free).
	pending := loadWatchCursor(cursorPath)
	seedDone, distillDone := false, false
	seedAttempted := false
	if w.seedAgent != "none" && pending.PendingSeed {
		seedAttempted = true
		if err := steps.seed(ctx); err != nil {
			fmt.Fprintf(out, "[watch] seed synthesis failed: %v\n", err)
		} else {
			seedDone = true
			fmt.Fprintln(out, "[watch] synthesized seed (agent step; spent tokens)")
		}
	}
	if w.distill && pending.PendingDistill {
		switch err := steps.distill(ctx); {
		case errors.Is(err, errDistillPassBusy):
			// When distill was the only agent step, nothing was spent, so nothing
			// may be charged. Hand the window
			// back instead of logging "spent tokens" and going quiet until the
			// next one — otherwise a watcher installed beside `setup`'s
			// hours-long backfill loses its first window (and, if the backfill
			// outlives it, every window) to a pass that never ran.
			// A seed step attempted immediately before this may already have spent
			// tokens even when it returned an error, so its reservation must stay.
			if seedAttempted {
				fmt.Fprintln(out, "[watch] distill skipped: another pass holds this brain; the spend window remains consumed by seed synthesis")
			} else {
				releaseWatchAgentSpend(cursorPath, cursor.LastAgentSpendAt, agentCalls)
				fmt.Fprintln(out, "[watch] distill skipped: another pass holds this brain; the spend window was NOT consumed")
			}
		case errors.Is(err, errDistillDeferred):
			fmt.Fprintln(out, "[watch] distilled capped batch; remaining sessions pending for the next spend window")
		case err != nil:
			fmt.Fprintf(out, "[watch] distill failed: %v\n", err)
		default:
			distillDone = true
			fmt.Fprintln(out, "[watch] distilled facts (agent step; spent tokens)")
		}
	}
	if err := withWatchCursorLock(cursorPath, func() error {
		current := loadWatchCursor(cursorPath)
		// A newer refresh owns its own pending work.
		if current.LastFingerprint != pending.LastFingerprint || !current.LastRefreshAt.Equal(pending.LastRefreshAt) {
			return nil
		}
		if seedDone {
			current.PendingSeed = false
		}
		if distillDone {
			current.PendingDistill = false
		}
		return saveWatchCursor(cursorPath, current)
	}); err != nil {
		fmt.Fprintf(out, "[watch] completion cursor update failed: %v\n", err)
	}
}

// releaseWatchAgentSpend gives back a window reserved by reserveWatchAgentSpend
// when the gated step turned out to do nothing at all. previous is the
// LastAgentSpendAt read at the top of this tick, so the cursor lands exactly
// where it was; the budget counter is decremented for the same reason. Failures
// are deliberately silent: the worst case is the pre-existing behaviour (a
// window charged for a no-op), and a watcher must never die on cursor
// bookkeeping.
func releaseWatchAgentSpend(cursorPath string, previous time.Time, agentCalls *int) {
	_ = withWatchCursorLock(cursorPath, func() error {
		cursor := loadWatchCursor(cursorPath)
		cursor.LastAgentSpendAt = previous
		if *agentCalls > 0 {
			*agentCalls--
		}
		return saveWatchCursor(cursorPath, cursor)
	})
}

// agentWorkEnabled reports whether any token-spending step is turned on.
func (w watchCommandOptions) agentWorkEnabled() bool {
	return w.distill || w.seedAgent != "none"
}

// watchStepsForRepo builds the per-repo side effects the watch loop drives. refresh is ALWAYS the free
// (seed-agent none) deterministic refresh; seed and distill are the gated token steps. Shared by the
// single-repo `watch` and the workspace fan-out so both stay token-frugal the same way.
func watchStepsForRepo(cmd *cobra.Command, opts Options, w watchCommandOptions, repoDir string, now func() time.Time) watchSteps {
	return watchSteps{
		now:         now,
		fingerprint: func(c context.Context) string { return watchFingerprint(c, opts.Runner, repoDir) },
		delta:       func(c context.Context) (shortTermStats, error) { return watchShortTermDelta(c, cmd, opts, repoDir) },
		reconcile: func(c context.Context) error {
			_, warning, err := reconcileMemoryAndLaunch(c, opts, repoDir, "watch")
			if warning != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: memory coordinator: %s\n", warning)
			}
			return err
		},
		refresh: func(c context.Context) error { return watchDeterministicRefresh(c, cmd, opts, repoDir) },
		seed:    func(c context.Context) error { return watchSeed(c, cmd, opts, w, repoDir) },
		distill: func(c context.Context) error { return watchDistill(c, cmd, opts, w, repoDir) },
		paused:  func() bool { return backgroundPaused(opts.Env) },
	}
}

// watchShortTermDelta runs the short-term memory path for one repo: quiet
// (output discarded; the tick logs one summary line), deterministic, and
// token-free. The returned stats let the tick escalate to consolidation when
// the buffer overflows.
func watchShortTermDelta(ctx context.Context, cmd *cobra.Command, opts Options, repoDir string) (shortTermStats, error) {
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	deltaCmd := &cobra.Command{Use: "delta"}
	deltaCmd.SetOut(io.Discard)
	deltaCmd.SetErr(io.Discard)
	deltaCmd.SetContext(ctx)
	return runRefreshDeltaStats(deltaCmd, perRepo, true)
}

// watchShouldSpend decides whether the token-spending agent work (seed and/or distill) runs this tick:
// only when at least one is enabled AND --distill-every has elapsed since the last agent spend
// (cursor-tracked across restarts) AND the --budget cap is not yet reached. This is the structural
// token-frugality guarantee — it bounds seed synthesis exactly like distill.
func watchShouldSpend(w watchCommandOptions, cursor watchCursor, agentCalls int, now time.Time) (bool, string) {
	if !w.agentWorkEnabled() {
		return false, "no agent steps enabled"
	}
	if !(cursor.LastAgentSpendAt.IsZero() || now.Sub(cursor.LastAgentSpendAt) >= w.distillEvery) {
		return false, "--distill-every not elapsed"
	}
	if w.budget > 0 && agentCalls >= w.budget {
		return false, fmt.Sprintf("--budget %d reached", w.budget)
	}
	return true, ""
}

// watchRepairWait is the minimum time between full consolidations while the
// short-term delta is failing. It starts at one tick (a transient delta failure
// repairs on the very next pass) and doubles per consecutive repair up to the
// normal --consolidate-every, so a persistently failing delta degrades to the
// ordinary consolidation cadence instead of thrashing the shared brain write
// lock on every tick.
func watchRepairWait(w watchCommandOptions, repairs int) time.Duration {
	wait := w.interval
	if wait <= 0 {
		wait = time.Minute
	}
	if repairs > watchRepairBackoffMax {
		repairs = watchRepairBackoffMax
	}
	for i := 0; i < repairs && wait < w.consolidateEvery; i++ {
		wait *= 2
	}
	if wait > w.consolidateEvery {
		wait = w.consolidateEvery
	}
	return wait
}

// reserveWatchAgentSpend advances the refresh cursor after a successful full
// refresh and decides whether the gated token work may run. repair reports
// whether THIS refresh ran early because the short-term delta failed; it drives
// the watchRepairWait backoff and is cleared by the first healthy delta.
func reserveWatchAgentSpend(cursorPath, fingerprint string, w watchCommandOptions, agentCalls *int, now time.Time, repair bool) (bool, string, error) {
	var cursor watchCursor
	var reserved bool
	var reason string
	err := withWatchCursorLock(cursorPath, func() error {
		cursor = loadWatchCursor(cursorPath)
		changed := fingerprint != cursor.LastFingerprint || cursor.LastRefreshAt.IsZero()
		if changed {
			cursor.PendingSeed = cursor.PendingSeed || w.seedAgent != "none"
			cursor.PendingDistill = cursor.PendingDistill || w.distill
			cursor.LastFingerprint = fingerprint
			cursor.LastRefreshAt = now
			if repair {
				if cursor.ConsolidationRepairs < watchRepairBackoffMax {
					cursor.ConsolidationRepairs++
				}
			} else {
				cursor.ConsolidationRepairs = 0
			}
		}
		if !w.agentWorkEnabled() {
			if changed {
				return saveWatchCursor(cursorPath, cursor)
			}
			return nil
		}
		ok, skipReason := watchShouldSpend(w, cursor, *agentCalls, now)
		if !ok {
			reason = skipReason
			if changed {
				return saveWatchCursor(cursorPath, cursor)
			}
			return nil
		}
		cursor.LastAgentSpendAt = now
		*agentCalls++
		reserved = true
		return saveWatchCursor(cursorPath, cursor)
	})
	return reserved, reason, err
}

func withWatchCursorLock(cursorPath string, fn func() error) error {
	root := filepath.Dir(cursorPath)
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("watch cursor directory must not be a symlink: %s", root)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(root, brainLockDirName); err != nil {
		return err
	}
	lock, err := acquireFileLock(filepath.Join(root, brainLockDirName, watchCursorLockName), "watch_locked", brainWriteLockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	return fn()
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
func watchDeterministicRefresh(ctx context.Context, cmd *cobra.Command, opts Options, repoDir string) error {
	return watchDeterministicRefreshComponents(ctx, cmd, opts, repoDir, watchTickBuildsHistoryProjection, false, nil)
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

func watchDeterministicRefreshComponents(ctx context.Context, cmd *cobra.Command, opts Options, repoDir string, historyIndex, bestEffort bool, component func(name string, err error)) error {
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	refreshOpts := watchDeterministicRefreshOptions(historyIndex)
	refreshOpts.component = component
	refreshOpts.bestEffort = bestEffort
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
	entityErr := refreshEntityIndexQuietly(ctx, perRepo, repoDir)
	switch {
	case component != nil:
		component(brainComponentEntities, entityErr)
	case entityErr != nil:
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: entity index refresh skipped: %v\n", entityErr)
	}
	_, warning, err := reconcileMemoryAndLaunch(ctx, perRepo, repoDir, "watch")
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
// watchShouldSpend). Reuses runSeed; --update refreshes the existing seed in place.
func watchSeed(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions, repoDir string) error {
	seedOpts := seedCommandOptions{
		update:             true,
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
	}
	sub := &cobra.Command{}
	sub.SetContext(ctx)
	sub.SetOut(cmd.OutOrStdout())
	sub.SetErr(cmd.ErrOrStderr())
	return runSeed(ctx, sub, opts, seedOpts, repoDir)
}

// errDistillPassBusy reports that the gated distill did NOTHING because another
// process already held the brain's distill pass lock. `distill` itself treats
// that as success — someone else is doing the work — but the watcher must not:
// it had already reserved the --distill-every window, so a silent success burned
// a whole window (24h by default) on a no-op and logged "spent tokens". `setup`
// creates that collision by design, spawning an hours-long detached backfill and
// installing a watcher that ticks immediately after.
var errDistillPassBusy = errors.New("another distill pass holds this brain")

// errDistillDeferred retains pending work after a capped successful pass.
var errDistillDeferred = errors.New("distill has budget-deferred sessions")

func watchDistill(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions, repoDir string) error {
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
		return errDistillPassBusy
	}
	if deferred {
		return errDistillDeferred
	}
	return nil
}

func watchDistillOptions(w watchCommandOptions) distillCommandOptions {
	jobs := w.distillJobs
	if jobs <= 0 {
		jobs = 1
	}
	// A negative cap is a typo, not "unlimited". Only an explicit 0 means that.
	maxSessions := w.distillMaxSessions
	if maxSessions < 0 {
		maxSessions = 0
	}
	return distillCommandOptions{
		agent:               w.distillAgent,
		model:               w.model,
		effort:              w.effort,
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
		jobs:        jobs,
		// concurrency is what the distill pipeline (and runDistill's guard)
		// actually reads; --jobs is only its compatibility alias. Leaving it at
		// the zero value made every gated distill the watcher ever attempted
		// fail with "--concurrency must be greater than 0" before making a
		// single agent call.
		concurrency: jobs,
	}
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
