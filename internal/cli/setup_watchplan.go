package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// setup_watchplan.go holds the machine-level watch plan: the ONE file the ONE
// background watcher reads to learn what it is supposed to watch.
//
// It exists because the daemon's identity is machine-wide while its old
// arguments were per-repo. `brainWatchDaemonArgs` used to render
// `workspace watch <ws> --interval ... --distill-every ... --model ...` straight
// into the launchd plist / systemd unit, but the label was derived from the
// daemon NAME alone. Two repos that asked for two different workspaces (or the
// same workspace with different tuning) therefore resolved to the SAME unit path
// and rendered DIFFERENT bytes: whichever repo ran `setup` last silently
// overwrote the other's watcher, and the first repo was never watched again
// while its `status` still said "running".
//
// The fix is to make the unit workspace-independent. The rendered unit is now
// byte-identical on every machine and for every repo — `workspace watch
// --distill` with no positional argument — and everything that varies per
// workspace (interval, distill cadence, agent, model, effort, session cap) moves
// here, into a file the running daemon re-reads on every outer tick. A second
// repo's `setup` therefore ADDS a row to a shared plan instead of rewriting a
// shared unit, which is what setup.go's "not two daemons and two workspace
// entries" invariant always claimed.
//
// The plan also records the daemon NAME that is currently installed. That is
// what makes `--daemon-name` a rename rather than a fork: the per-repo setup
// record only knows what THIS repo installed, so a second repo naming a
// different daemon used to install a second watcher that nothing could see.
const (
	// setupWatchPlanFile is machine-level ON PURPOSE — it lives beside the
	// machine-level watcher log, not inside any repo's state directory, because
	// exactly one process reads it.
	setupWatchPlanFile     = "watch-plan.json"
	setupWatchPlanLockName = "watch-plan.lock"
	setupWatchPlanVersion  = 1
)

// setupWatchPlanEntry is one workspace's tuning: everything the daemon used to
// receive as unit arguments, per workspace instead of per machine.
type setupWatchPlanEntry struct {
	Workspace    string    `json:"workspace"`
	Interval     string    `json:"interval,omitempty"`
	DistillEvery string    `json:"distill_every,omitempty"`
	Agent        string    `json:"agent,omitempty"`
	Model        string    `json:"model,omitempty"`
	Effort       string    `json:"effort,omitempty"`
	MaxSessions  int       `json:"max_sessions,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// setupWatchPlan is the whole machine's watch plan.
type setupWatchPlan struct {
	SchemaVersion int `json:"schema_version"`
	// DaemonName is the identity currently installed on this machine. It is
	// here rather than only in each repo's setup record because retiring a
	// renamed watcher is a MACHINE decision: repo B renaming the daemon has to
	// remove the unit repo A installed, and repo B cannot read repo A's record.
	DaemonName string                `json:"daemon_name,omitempty"`
	Workspaces []setupWatchPlanEntry `json:"workspaces"`
}

// entry returns the tuning for one workspace, and whether the plan carries it.
func (p setupWatchPlan) entry(workspace string) (setupWatchPlanEntry, bool) {
	for _, candidate := range p.Workspaces {
		if candidate.Workspace == workspace {
			return candidate, true
		}
	}
	return setupWatchPlanEntry{}, false
}

// setupWatchPlanPath resolves the plan file. It is deliberately in the plugin
// STATE dir, the same directory as the machine-wide watcher log, so a plan and
// the log of the process that reads it are found together.
func setupWatchPlanPath(env EntireEnv) (string, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", fmt.Errorf("resolve plugin state dir: %w", err)
	}
	return filepath.Join(dirs.State, setupWatchPlanFile), nil
}

// loadSetupWatchPlan reads the plan. A missing file is an EMPTY plan, not an
// error: a machine that has never run `setup` has nothing to watch, and the
// daemon must say so calmly rather than crash-loop under KeepAlive.
func loadSetupWatchPlan(env EntireEnv) (setupWatchPlan, error) {
	path, err := setupWatchPlanPath(env)
	if err != nil {
		return setupWatchPlan{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return setupWatchPlan{SchemaVersion: setupWatchPlanVersion}, nil
		}
		return setupWatchPlan{}, fmt.Errorf("read %s: %w", path, err)
	}
	var plan setupWatchPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return setupWatchPlan{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if plan.SchemaVersion != setupWatchPlanVersion {
		return setupWatchPlan{}, fmt.Errorf("unsupported watch plan schema_version %d in %s", plan.SchemaVersion, path)
	}
	return plan, nil
}

// updateSetupWatchPlan applies mutate to the plan under the machine-level plan
// lock and writes the result. Two `setup` runs in two repos at the same time is
// the ordinary case on a developer machine, and an unlocked read-modify-write
// silently drops one of the two workspaces — the exact class of bug this file
// exists to end.
func updateSetupWatchPlan(env EntireEnv, mutate func(*setupWatchPlan)) (setupWatchPlan, error) {
	path, err := setupWatchPlanPath(env)
	if err != nil {
		return setupWatchPlan{}, err
	}
	stateDir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Join(stateDir, brainLockDirName), 0o700); err != nil {
		return setupWatchPlan{}, fmt.Errorf("create %s: %w", filepath.Join(stateDir, brainLockDirName), err)
	}
	lock, err := acquireFileLock(filepath.Join(stateDir, brainLockDirName, setupWatchPlanLockName), "watch_plan_locked", brainWriteLockTimeout)
	if err != nil {
		return setupWatchPlan{}, err
	}
	defer func() { _ = lock.Close() }()

	plan, err := loadSetupWatchPlan(env)
	if err != nil {
		return setupWatchPlan{}, err
	}
	mutate(&plan)
	plan.SchemaVersion = setupWatchPlanVersion
	sort.Slice(plan.Workspaces, func(i, j int) bool { return plan.Workspaces[i].Workspace < plan.Workspaces[j].Workspace })
	if err := writeJSONFile(path, plan); err != nil {
		return setupWatchPlan{}, err
	}
	return plan, nil
}

// recordSetupWatchPlan upserts one workspace's tuning and the installed daemon
// name, returning the plan as it now stands on disk. It is called BEFORE the
// daemon is installed, so the watcher's very first tick already has something to
// read instead of an empty plan it would report as "nothing to watch".
func recordSetupWatchPlan(env EntireEnv, daemonName string, entry setupWatchPlanEntry) (setupWatchPlan, error) {
	return updateSetupWatchPlan(env, func(plan *setupWatchPlan) {
		if name := strings.TrimSpace(daemonName); name != "" {
			plan.DaemonName = name
		}
		for i := range plan.Workspaces {
			if plan.Workspaces[i].Workspace == entry.Workspace {
				plan.Workspaces[i] = entry
				return
			}
		}
		plan.Workspaces = append(plan.Workspaces, entry)
	})
}

// setupWatchPlanEntryFor turns this run's options into the row the daemon reads.
// It carries the SAME budget setup reported and paid the foreground backfill
// with — the cap a user was shown has to bind on the process that keeps
// spending after setup exits.
func setupWatchPlanEntryFor(setupOpts setupCommandOptions, now time.Time) setupWatchPlanEntry {
	entry := setupWatchPlanEntry{
		Workspace:   setupOpts.workspace,
		Model:       strings.TrimSpace(setupOpts.model),
		Effort:      strings.TrimSpace(setupOpts.effort),
		MaxSessions: setupResolvedBackfillBudget(setupOpts),
		UpdatedAt:   now.UTC(),
	}
	if setupOpts.interval > 0 {
		entry.Interval = setupOpts.interval.String()
	}
	if setupOpts.distillEvery > 0 {
		entry.DistillEvery = setupOpts.distillEvery.String()
	}
	if agent := strings.TrimSpace(setupOpts.agent); agent != "" && agent != "auto" {
		entry.Agent = agent
	}
	return entry
}

// applyWatchPlanEntry overlays one workspace's recorded tuning onto the base
// watch options the unit supplies. A field the plan does not carry keeps the
// daemon's own default, so an older plan written by an older `setup` still runs.
func applyWatchPlanEntry(base watchCommandOptions, entry setupWatchPlanEntry) watchCommandOptions {
	w := base
	if d, err := time.ParseDuration(strings.TrimSpace(entry.Interval)); err == nil && d > 0 {
		w.interval = d
	}
	if d, err := time.ParseDuration(strings.TrimSpace(entry.DistillEvery)); err == nil && d > 0 {
		w.distillEvery = d
	}
	if agent := strings.TrimSpace(entry.Agent); agent != "" {
		w.distillAgent = agent
		if agent == "none" {
			w.distill = false
		}
	}
	if model := strings.TrimSpace(entry.Model); model != "" {
		w.model = model
	}
	if effort := strings.TrimSpace(entry.Effort); effort != "" {
		w.effort = effort
	}
	if entry.MaxSessions > 0 {
		w.distillMaxSessions = entry.MaxSessions
	}
	return w
}

// watchPlanSleep is how long the supervised loop waits between outer passes: the
// SHORTEST interval any workspace asked for. Taking the minimum is what keeps a
// single machine-wide watcher honest — a workspace that asked to be checked
// every minute is still checked every minute after a second workspace joins
// asking for an hour. The cost is that the slower workspace is visited more
// often than it asked; every visit that finds no change is free (the
// deterministic refresh is gated by a fingerprint and the agent step by
// --distill-every plus each repo's persisted cursor), so the trade buys
// freshness with no extra tokens.
func watchPlanSleep(plan setupWatchPlan, fallback time.Duration) time.Duration {
	shortest := time.Duration(0)
	for _, entry := range plan.Workspaces {
		d, err := time.ParseDuration(strings.TrimSpace(entry.Interval))
		if err != nil || d <= 0 {
			continue
		}
		if shortest == 0 || d < shortest {
			shortest = d
		}
	}
	if shortest > 0 {
		return shortest
	}
	if fallback > 0 {
		return fallback
	}
	return 5 * time.Minute
}

// describeWatchPlanCoverage says, in one line, what the machine's single watcher
// actually covers. The daemon line used to print only a hashed launchd label —
// "running (io.entire.brain-watch.a2d3fd66)" — which tells a reader nothing, and
// tells them least at the exact moment they most need it: right after a SECOND
// repo ran `setup`, when the old behaviour silently stopped watching the first
// one. Naming the workspaces is how a user of the first repo can see they are
// still covered.
func describeWatchPlanCoverage(plan setupWatchPlan) string {
	if len(plan.Workspaces) == 0 {
		return ""
	}
	names := make([]string, 0, len(plan.Workspaces))
	for _, entry := range plan.Workspaces {
		names = append(names, entry.Workspace)
	}
	// Long member lists are elided rather than wrapped: the point of the line is
	// "am I in there", and the count answers that when the names do not fit.
	const shown = 4
	label := "workspace"
	if len(names) != 1 {
		label = "workspaces"
	}
	listed := names
	suffix := ""
	if len(names) > shown {
		listed = names[:shown]
		suffix = fmt.Sprintf(", +%d more", len(names)-shown)
	}
	return fmt.Sprintf("watching %d %s (%s%s) every %s",
		len(names), label, strings.Join(listed, ", "), suffix, watchPlanSleep(plan, 0))
}
