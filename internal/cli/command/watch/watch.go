package watch

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

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
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
type Flags struct {
	Interval time.Duration
	Once     bool
	// ConsolidateEvery throttles the FULL deterministic refresh (consolidation:
	// sessions + semantic + history + clearing short-term memory). The
	// fingerprint includes checkpoint refs, so during active agent work every
	// turn would otherwise trigger the heavy path each tick; the per-tick
	// short-term delta carries freshness in between. 0 = consolidate on every
	// change (the pre-short-term behavior). A full short-term buffer always
	// forces consolidation regardless of the interval.
	ConsolidateEvery time.Duration
	Distill          bool
	DistillEvery     time.Duration
	DistillAgent     string
	DistillJobs      int
	// DistillMaxSessions caps how many NOT-YET-DISTILLED sessions one gated
	// pass processes (0 = no cap). --distill-every and the persisted cursor
	// bound how OFTEN the daemon spends; this is the only thing that bounds how
	// MUCH one spend costs. Without it a supervised watcher's first gated tick
	// distills the entire remaining corpus of every repo in the workspace —
	// exactly the spend `setup` told the user it had capped at 25.
	DistillMaxSessions int
	SeedAgent          string
	Model              string
	Effort             string
	// Budget caps gated agent runs for the life of THIS PROCESS (distill + seed;
	// each spends tokens); 0 = unlimited. It is not window-scoped and never
	// resets while the process lives, which makes it the wrong guard for a
	// supervised daemon — see brainWatchDaemonArgs, which deliberately omits it.
	Budget int
}

// Cursor persists across restarts so the daemon never re-refreshes unchanged state and never
// re-runs the gated agent work within --distill-every after a restart.
type Cursor struct {
	PendingSeed    bool `json:"pending_seed,omitempty"`
	PendingDistill bool `json:"pending_distill,omitempty"`
	// LastFingerprint tracks the input already queued for agent work, even
	// when the deterministic refresh is deferred. PendingRefresh keeps that
	// unfinished refresh eligible without requeueing completed agent steps.
	LastFingerprint      string    `json:"last_fingerprint"`
	LastRefreshAt        time.Time `json:"last_refresh_at,omitempty"`
	LastAgentSpendAt     time.Time `json:"last_agent_spend_at,omitempty"`
	PendingRefresh       bool      `json:"pending_refresh,omitempty"`
	LastRefreshAttemptAt time.Time `json:"last_refresh_attempt_at,omitempty"`
	// ConsolidationRepairs counts CONSECUTIVE full refresh attempts triggered
	// by a failed short-term delta, including dirty-tree deferrals. It is the
	// backoff state for watchRepairWait: a single failure repairs promptly, a persistent one
	// settles back to the normal consolidation cadence instead of firing the
	// heavy refresh every tick. Reset by a refresh attempt with a healthy delta.
	ConsolidationRepairs int       `json:"consolidation_repairs,omitempty"`
	LastFactsSyncAt      time.Time `json:"last_facts_sync_at,omitempty"`
}

// watchRepairBackoffMax caps ConsolidationRepairs so the persisted counter stays
// bounded and the doubling in watchRepairWait can never overflow.
const watchRepairBackoffMax = 16

const watchCursorLockName = "watch.lock"

// Steps are the side-effecting operations of one tick. Injected so the loop's gating logic
// (change detection, agent interval/budget, cursor persistence) is unit-testable without running a real
// refresh/seed/distill. Refresh is ALWAYS the free (seed-agent none) deterministic refresh; Seed and
// Distill are the gated, token-spending steps.
type Steps struct {
	Now         func() time.Time
	Fingerprint func(context.Context) string
	// Delta is the cheap short-term memory update (incremental export + overlay
	// index of changed transcripts). It runs EVERY tick, before the change
	// gate, so an in-flight session's turns are recallable near-real-time; the
	// full refresh below is consolidation and is throttled by
	// --consolidate-every unless the delta reports the buffer full.
	Delta func(context.Context) (historystore.ShortTermStats, error)
	// Reconcile durably queues long-term conversation work independently of
	// the git/checkpoint fingerprint. Exported transcripts can change while
	// that fingerprint stays stable (for example, a late host export).
	Reconcile func(context.Context) error
	Refresh   func(context.Context) error
	Seed      func(context.Context) error
	Distill   func(context.Context) error
	// FactsSync publishes-and-pulls hosted facts (hostedFactsSyncAndPull) for
	// the current + default branch. Nil when the surface has no hosted step;
	// non-fatal and cursor-throttled (hostedSyncMinInterval).
	FactsSync func(context.Context) (bool, error)
	Paused    func() bool
}

// hostedSyncMinInterval throttles the hosted fact sync step across ticks: the
// tick interval is minutes, a team's fact churn is hours.
const hostedSyncMinInterval = 15 * time.Minute

// DefaultFlags are the shared defaults for `watch` and `workspace watch`.
func DefaultFlags() Flags {
	return Flags{
		Interval:         5 * time.Minute,
		ConsolidateEvery: 30 * time.Minute,
		DistillEvery:     24 * time.Hour,
		// "auto", not "codex". A hardcoded vendor here was a THIRD codex default
		// that never reached defaultRefreshAgent at all, so reordering that
		// function alone would have left the watcher — the one unattended path
		// that spends tokens on its own — routing repository content to codex
		// forever. runDistill resolves "auto" on this path (distill_cmd.go), and
		// `setup` still overrides it per workspace through the watch plan.
		DistillAgent: "auto",
		DistillJobs:  1,
		SeedAgent:    "none",
	}
}

// BindFlags registers the watch flags on a command, shared so `watch` and `workspace watch` expose
// the identical token-frugality controls.
func BindFlags(cmd *cobra.Command, w *Flags) {
	cmd.Flags().DurationVar(&w.Interval, "interval", w.Interval, "Poll interval between ticks")
	cmd.Flags().DurationVar(&w.ConsolidateEvery, "consolidate-every", w.ConsolidateEvery, "Minimum interval between full consolidations (heavy refresh); the per-tick short-term delta covers freshness in between. 0 = consolidate on every change; a full short-term buffer always consolidates")
	cmd.Flags().BoolVar(&w.Once, "once", false, "Run a single pass and exit (no daemon loop)")
	cmd.Flags().BoolVar(&w.Distill, "distill", false, "Run distill (SPENDS TOKENS) when new sessions land — gated by --distill-every + --budget")
	cmd.Flags().DurationVar(&w.DistillEvery, "distill-every", w.DistillEvery, "Minimum interval between gated agent runs")
	cmd.Flags().StringVar(&w.DistillAgent, "agent", w.DistillAgent, "Agent for the distill step (used only with --distill)")
	cmd.Flags().IntVar(&w.DistillJobs, "jobs", w.DistillJobs, "Parallel distill extraction jobs when --distill is enabled; reconciliation and writes remain deterministic")
	cmd.Flags().IntVar(&w.DistillMaxSessions, "max-sessions", w.DistillMaxSessions, "Cap how many not-yet-distilled sessions ONE gated distill pass processes (0 = no cap). --distill-every bounds how often the daemon spends; this bounds how much each spend costs")
	cmd.Flags().StringVar(&w.SeedAgent, "seed-agent", w.SeedAgent, "Agent for gated seed synthesis (SPENDS TOKENS); none = deterministic seed only. Bounded by --distill-every + --budget, NOT per-change")
	_ = cmd.Flags().MarkHidden("seed-agent")
	cmd.Flags().StringVar(&w.Model, "model", "", "Fast/cheap model for the gated agent steps")
	cmd.Flags().StringVar(&w.Effort, "effort", "", "Reasoning effort for the gated agent steps (codex --config model_reasoning_effort=, claude --effort)")
	cmd.Flags().IntVar(&w.Budget, "budget", 0, "Cap on gated agent runs for the LIFE OF THIS PROCESS; 0 = unlimited. It never resets, so on a long-lived daemon --budget 1 means one run EVER, not one per window — the durable per-window guard is --distill-every + the persisted cursor. Use it only for a bounded foreground run.")
}

// New builds the `watch` command.
func New(cfg Config) *cobra.Command {
	w := DefaultFlags()
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
			return runWatch(cmd.Context(), cmd, cfg, w, target)
		},
	}
	BindFlags(cmd, &w)
	return cmd
}

func runWatch(ctx context.Context, cmd *cobra.Command, cfg Config, w Flags, target string) error {
	opts := cfg.Options
	if w.Interval <= 0 {
		w.Interval = 5 * time.Minute
	}
	if w.DistillJobs <= 0 {
		return fmt.Errorf("--jobs must be greater than 0")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("watch requires a local repository path: %s", target)
	}
	cursorPath, repoKey, err := cfg.Deps.Storage(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	// Wire SIGINT/SIGTERM so a long-running daemon stops cleanly between ticks instead of being killed
	// mid-refresh; this is what makes the loop's ctx-cancel paths live (the root command runs with a
	// Background context otherwise).
	ctx, stop := SignalContext(ctx)
	defer stop()
	// Every line this daemon writes carries when, which process and build,
	// which tick and which repo (see LogFields). The banner is written
	// before the first tick, so it carries no tick number; watchLoop stamps
	// everything from there on, and the two writers never nest.
	banner := NewLogPrefixWriter(cmd.OutOrStdout(), LogFields{Build: opts.Version, Repo: repoKey}, now)
	fmt.Fprintf(banner, "[watch] %s — interval %s, distill=%v (every %s, agent=%s, jobs=%d, model=%q, max-sessions=%s, run-budget=%s)\n",
		repoKey, w.Interval, w.Distill, w.DistillEvery, w.DistillAgent, w.DistillJobs, w.Model,
		CapLabel(w.DistillMaxSessions), CapLabel(w.Budget))
	return watchLoop(ctx, cmd.OutOrStdout(), LogFields{Build: opts.Version, Repo: repoKey}, now, w, cursorPath,
		func(sink io.Writer) Steps {
			return cfg.Deps.StepsForRepo(StepCommand(cmd, sink), opts, w, repoDir, now)
		})
}

// CapLabel renders a cap where a non-positive value means "no cap".
//
// The banner printed a bare "budget=0", and 0 is the value a setup-installed
// watcher ALWAYS has: setup deliberately never passes --budget, because that
// counter is process-lifetime and never resets (see brainWatchDaemonArgs). So
// the one number the daemon's log offered about spend was both meaningless and
// easy to read as "zero allowed", while the cap that actually binds it --
// --max-sessions, carried per workspace in the machine watch plan -- was not
// printed at all.
func CapLabel(value int) string {
	if value <= 0 {
		return "uncapped"
	}
	return strconv.Itoa(value)
}

// SignalContext returns a context cancelled on SIGINT/SIGTERM so `watch` and `workspace watch`
// shut down cleanly between ticks (finish the current tick, then return) rather than dying abruptly.
func SignalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// watchLoop drives the single-repo daemon. It owns the attribution of every
// line its ticks write -- stamped once per line by watchLogPrefixWriter -- so
// a tick cannot forget it and a line added later inherits it. The steps are
// built FROM the attributed sink rather than passed in already built, so the
// refresh/seed/distill progress lines land in the same stream, carrying the
// same tick and repo, instead of going unattributed to the raw stdout.
//
// out must be the RAW sink: wrapping it again outside would stamp every line
// twice.
func watchLoop(ctx context.Context, out io.Writer, fields LogFields, now func() time.Time, w Flags, cursorPath string, stepsFor func(io.Writer) Steps) error {
	tick := 0
	fields.Tick = &tick
	out = NewLogPrefixWriter(out, fields, now)
	steps := stepsFor(out)
	agentCalls := 0
	for {
		tick++
		if ctx.Err() != nil {
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		}
		Tick(ctx, out, w, cursorPath, steps, &agentCalls)
		if w.Once {
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		case <-time.After(w.Interval):
		}
	}
}

// Tick runs one pass: cheap change detection, then (only on change) the free deterministic refresh,
// then the GATED token-spending work (seed synthesis and/or distill). agentCalls is shared across
// ticks/members so --budget caps total token spend across the whole run.
func Tick(ctx context.Context, out io.Writer, w Flags, cursorPath string, steps Steps, agentCalls *int) {
	if steps.Paused != nil && steps.Paused() {
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
	if steps.Delta != nil {
		stats, err := steps.Delta(ctx)
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
	if deltaNeedsReconcile && steps.Reconcile != nil {
		if err := steps.Reconcile(ctx); err != nil {
			fmt.Fprintf(out, "[watch] memory reconciliation failed (durable overlay retained): %v\n", err)
		} else {
			fmt.Fprintln(out, "[watch] long-term memory reconciliation queued")
		}
	}
	cursor := LoadCursor(cursorPath)
	// The hosted step pulls teammates' facts as much as it publishes local
	// ones, so it must run on idle (no-change) ticks too — a machine that only
	// receives would otherwise never sync. The defer below makes it run on
	// every exit path from here down, including a failed refresh: a failed
	// refresh never advances the main cursor, so a machine whose refresh
	// always fails (e.g. the graph plugin is missing) takes that path on every
	// tick and would otherwise be starved of the team's facts forever
	// (COR-2114; a hand-placed call per return had already missed a path
	// twice). It is token-free and throttled by its own cursor timestamp,
	// advanced regardless of outcome so a failing hosted endpoint cannot turn
	// every tick into an egress attempt. The success path still calls it
	// explicitly BEFORE the agent steps — the `ran` guard makes the deferred
	// call a no-op then — so the sync never inherits a context cancelled
	// during a long agent step and burns its throttle window on an attempt
	// that never happened.
	factsSyncRan := false
	runFactsSync := func() {
		if factsSyncRan {
			return
		}
		factsSyncRan = true
		now := steps.Now().UTC()
		if steps.FactsSync == nil || now.Sub(cursor.LastFactsSyncAt) < hostedSyncMinInterval {
			return
		}
		attempted, err := steps.FactsSync(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(out, "[watch] hosted fact sync failed (local brain unaffected): %v\n", err)
		case attempted:
			fmt.Fprintln(out, "[watch] hosted fact sync ran")
		}
		markFactsSyncAttempt(cursorPath, now)
	}
	defer runFactsSync()
	fp := steps.Fingerprint(ctx)
	changed := fp != cursor.LastFingerprint || cursor.LastRefreshAt.IsZero() || cursor.PendingRefresh
	if !changed && !(cursor.PendingSeed && w.SeedAgent != "none" || cursor.PendingDistill && w.Distill) {
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
	refreshDue := changed
	if cursor.PendingRefresh && !cursor.LastRefreshAttemptAt.IsZero() && !bufferFull {
		// A dirty-tree refusal must survive a restart or a stash (neither
		// changes the fingerprint), but must not replay the heavy refresh on
		// every tick. Keep its attempt clock separate from last success.
		wait := w.ConsolidateEvery
		if wait <= 0 {
			wait = w.Interval
		}
		if wait <= 0 {
			wait = time.Minute
		}
		// Pending work obeys the same urgent repair policy as ordinary
		// consolidation: a full buffer bypasses the wait, while failed deltas
		// retry promptly and back off if refresh keeps being refused.
		if !deltaHealthy && w.ConsolidateEvery > 0 {
			wait = watchRepairWait(w, cursor.ConsolidationRepairs)
		}
		if since := steps.Now().UTC().Sub(cursor.LastRefreshAttemptAt); since < wait {
			fmt.Fprintf(out, "[watch] refresh still pending after uncommitted worktree; retry due in %s\n", (wait - since).Round(time.Second))
			// Only the heavy refresh is waiting. Previously queued agent work
			// still gets its own interval/budget check below, as on the first
			// dirty tick, without advancing the refresh attempt clock.
			refreshDue = false
		}
	} else if changed && w.ConsolidateEvery > 0 && !cursor.LastRefreshAt.IsZero() && !bufferFull {
		wait := w.ConsolidateEvery
		state := "short-term memory is current"
		if !deltaHealthy {
			wait = watchRepairWait(w, cursor.ConsolidationRepairs)
			state = fmt.Sprintf("short-term memory failed; repair refresh backing off after %d consecutive repairs", cursor.ConsolidationRepairs)
		}
		if since := steps.Now().UTC().Sub(cursor.LastRefreshAt); since < wait {
			fmt.Fprintf(out, "[watch] consolidation deferred (%s since last full refresh; due in %s; %s)\n",
				since.Round(time.Second), (wait - since).Round(time.Second), state)
			return
		}
	}
	refresh := refreshUnchanged
	if refreshDue {
		refresh = refreshSucceeded
		if err := steps.Refresh(ctx); err != nil {
			// A refusal to index an uncommitted worktree is not a failure: it
			// is the expected answer while someone is working, and it recurs
			// every tick until they commit. Treating it as a failure aborted
			// the tick and prevented other background work while the tree stayed
			// dirty (issue #327). Retain the unfinished refresh independently of
			// agent work, and never record this refusal as a successful refresh.
			// Everything else still aborts, because an
			// unrefreshed brain must not have tokens spent against it.
			if strings.Contains(err.Error(), brainstore.DirtyWorktreeCode+":") {
				refresh = refreshDeferred
				fmt.Fprintln(out, "[watch] refresh deferred (uncommitted worktree); will retry; continuing with background work")
			} else {
				fmt.Fprintf(out, "[watch] refresh failed (skipping agent work this tick): %v\n", err)
				return
			}
		}
	}
	runFactsSync()
	reserved, reason, err := reserveWatchAgentSpend(cursorPath, fp, w, agentCalls, steps.Now().UTC(), !deltaHealthy, refresh)
	if err != nil {
		fmt.Fprintf(out, "[watch] cursor update failed (skipping agent work this tick): %v\n", err)
		return
	}
	if refresh == refreshSucceeded {
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
	pending := LoadCursor(cursorPath)
	seedDone, distillDone := false, false
	seedAttempted := false
	if w.SeedAgent != "none" && pending.PendingSeed {
		seedAttempted = true
		if err := steps.Seed(ctx); err != nil {
			fmt.Fprintf(out, "[watch] seed synthesis failed: %v\n", err)
		} else {
			seedDone = true
			fmt.Fprintln(out, "[watch] synthesized seed (agent step; spent tokens)")
		}
	}
	if w.Distill && pending.PendingDistill {
		switch err := steps.Distill(ctx); {
		case errors.Is(err, ErrDistillPassBusy):
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
		case errors.Is(err, ErrDistillDeferred):
			fmt.Fprintln(out, "[watch] distilled capped batch; remaining sessions pending for the next spend window")
		case err != nil:
			fmt.Fprintf(out, "[watch] distill failed: %v\n", err)
		default:
			distillDone = true
			fmt.Fprintln(out, "[watch] distilled facts (agent step; spent tokens)")
		}
	}
	if err := withWatchCursorLock(cursorPath, func() error {
		current := LoadCursor(cursorPath)
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
		return SaveCursor(cursorPath, current)
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
		cursor := LoadCursor(cursorPath)
		cursor.LastAgentSpendAt = previous
		if *agentCalls > 0 {
			*agentCalls--
		}
		return SaveCursor(cursorPath, cursor)
	})
}

// agentWorkEnabled reports whether any token-spending step is turned on.
func (w Flags) agentWorkEnabled() bool {
	return w.Distill || w.SeedAgent != "none"
}

// StepCommand hands the refresh/seed/distill steps the daemon's own
// attributed log sink. Those steps take a *cobra.Command only to find their
// output (they write progress through cmd.OutOrStdout and warnings through
// cmd.ErrOrStderr), and on the daemon path both belong in watch.log with the
// same stamp as the tick lines around them -- the `refresh: seed baseline
// failed` line is the exact line COR-2176 was filed about. Stderr and stdout
// are the same sink here because a supervised daemon's two streams are
// already redirected to the one file.
func StepCommand(parent *cobra.Command, sink io.Writer) *cobra.Command {
	sub := &cobra.Command{}
	sub.SetOut(sink)
	sub.SetErr(sink)
	if parent != nil {
		if ctx := parent.Context(); ctx != nil {
			sub.SetContext(ctx)
		}
	}
	return sub
}

// markFactsSyncAttempt persists the hosted-sync throttle timestamp. Failures
// are silent for the same reason as releaseWatchAgentSpend: a watcher must
// never die on cursor bookkeeping.
func markFactsSyncAttempt(cursorPath string, now time.Time) {
	_ = withWatchCursorLock(cursorPath, func() error {
		cursor := LoadCursor(cursorPath)
		cursor.LastFactsSyncAt = now
		return SaveCursor(cursorPath, cursor)
	})
}

// watchShouldSpend decides whether the token-spending agent work (seed and/or distill) runs this tick:
// only when at least one is enabled AND --distill-every has elapsed since the last agent spend
// (cursor-tracked across restarts) AND the --budget cap is not yet reached. This is the structural
// token-frugality guarantee — it bounds seed synthesis exactly like distill.
func watchShouldSpend(w Flags, cursor Cursor, agentCalls int, now time.Time) (bool, string) {
	if !w.agentWorkEnabled() {
		return false, "no agent steps enabled"
	}
	if !(cursor.LastAgentSpendAt.IsZero() || now.Sub(cursor.LastAgentSpendAt) >= w.DistillEvery) {
		return false, "--distill-every not elapsed"
	}
	if w.Budget > 0 && agentCalls >= w.Budget {
		return false, fmt.Sprintf("--budget %d reached", w.Budget)
	}
	return true, ""
}

// watchRepairWait is the minimum time between full consolidations while the
// short-term delta is failing. It starts at one tick (a transient delta failure
// repairs on the very next pass) and doubles per consecutive repair up to the
// normal --consolidate-every, so a persistently failing delta degrades to the
// ordinary consolidation cadence instead of thrashing the shared brain write
// lock on every tick.
func watchRepairWait(w Flags, repairs int) time.Duration {
	wait := w.Interval
	if wait <= 0 {
		wait = time.Minute
	}
	if repairs > watchRepairBackoffMax {
		repairs = watchRepairBackoffMax
	}
	for i := 0; i < repairs && wait < w.ConsolidateEvery; i++ {
		wait *= 2
	}
	if wait > w.ConsolidateEvery {
		wait = w.ConsolidateEvery
	}
	return wait
}

type refreshOutcome uint8

const (
	refreshUnchanged refreshOutcome = iota
	refreshSucceeded
	refreshDeferred
)

// reserveWatchAgentSpend records the refresh outcome and decides whether gated
// token work may run. A deferred refresh advances the observed fingerprint for
// agent work but retains PendingRefresh and the previous success time. repair reports
// whether THIS refresh ran early because the short-term delta failed; it drives
// the watchRepairWait backoff and is cleared by a refresh with a healthy delta.
func reserveWatchAgentSpend(cursorPath, fingerprint string, w Flags, agentCalls *int, now time.Time, repair bool, refresh refreshOutcome) (bool, string, error) {
	var cursor Cursor
	var reserved bool
	var reason string
	err := withWatchCursorLock(cursorPath, func() error {
		cursor = LoadCursor(cursorPath)
		changed := fingerprint != cursor.LastFingerprint || cursor.LastRefreshAt.IsZero() && !cursor.PendingRefresh
		refreshAttempted := refresh != refreshUnchanged
		if refreshAttempted && changed {
			cursor.PendingSeed = cursor.PendingSeed || w.SeedAgent != "none"
			cursor.PendingDistill = cursor.PendingDistill || w.Distill
			cursor.LastFingerprint = fingerprint
		}
		if refreshAttempted {
			cursor.LastRefreshAttemptAt = now
			cursor.PendingRefresh = refresh == refreshDeferred
			if repair {
				if cursor.ConsolidationRepairs < watchRepairBackoffMax {
					cursor.ConsolidationRepairs++
				}
			} else {
				cursor.ConsolidationRepairs = 0
			}
		}
		if refresh == refreshSucceeded {
			cursor.LastRefreshAt = now
		}
		if !(cursor.PendingSeed && w.SeedAgent != "none" || cursor.PendingDistill && w.Distill) {
			reason = "no pending agent work"
			if refreshAttempted {
				return SaveCursor(cursorPath, cursor)
			}
			return nil
		}
		ok, skipReason := watchShouldSpend(w, cursor, *agentCalls, now)
		if !ok {
			reason = skipReason
			if refreshAttempted {
				return SaveCursor(cursorPath, cursor)
			}
			return nil
		}
		cursor.LastAgentSpendAt = now
		*agentCalls++
		reserved = true
		return SaveCursor(cursorPath, cursor)
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
	if err := base.RejectExistingSymlinkPathComponents(root, brainstore.BrainLockDirName); err != nil {
		return err
	}
	lock, err := brainstore.AcquireFileLock(filepath.Join(root, brainstore.BrainLockDirName, watchCursorLockName), "watch_locked", brainstore.BrainWriteLockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	return fn()
}

// ErrDistillPassBusy reports that the gated distill did NOTHING because another
// process already held the brain's distill pass lock. `distill` itself treats
// that as success — someone else is doing the work — but the watcher must not:
// it had already reserved the --distill-every window, so a silent success burned
// a whole window (24h by default) on a no-op and logged "spent tokens". `setup`
// creates that collision by design, spawning an hours-long detached backfill and
// installing a watcher that ticks immediately after.
var ErrDistillPassBusy = errors.New("another distill pass holds this brain")

// ErrDistillDeferred retains pending work after a capped successful pass.
var ErrDistillDeferred = errors.New("distill has budget-deferred sessions")

func LoadCursor(path string) Cursor {
	var c Cursor
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func SaveCursor(path string, c Cursor) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return brainstore.WriteFileAtomic(path, append(data, '\n'), 0o600)
}
