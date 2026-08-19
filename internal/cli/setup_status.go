package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// setup_status.go answers the question setup creates: "it said the backfill is
// running in the background — is it, and how far has it got?"
//
// The answer lives on the EXISTING `status` verb rather than a new one. Status
// is the surface agents and humans already look at; doctor stays the deep
// environment health check. The section is deliberately cheap to compute: two
// small file reads plus, only when a unit file actually exists, one service
// manager query.

// factsBackfillStatus is the "facts: <distilled>/<total> sessions distilled"
// counter plus whatever is known about a running backfill process.
type factsBackfillStatus struct {
	Sessions  int       `json:"sessions"`
	Distilled int       `json:"distilled"`
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	LogPath   string    `json:"log_path,omitempty"`
}

func (f factsBackfillStatus) Pending() int {
	if f.Sessions <= f.Distilled {
		return 0
	}
	return f.Sessions - f.Distilled
}

// brainStatusComponent is one instant-phase source and whether it is built.
type brainStatusComponent struct {
	Name   string `json:"name"`
	State  string `json:"state"` // built | missing
	Detail string `json:"detail,omitempty"`
}

// brainStatusOnboarding is the `setup` progress projection: how far the fact
// backfill has got, whether the background watcher is healthy, and whether the
// instant phase's components exist.
type brainStatusOnboarding struct {
	Facts      factsBackfillStatus    `json:"facts_backfill"`
	Daemon     daemonState            `json:"daemon"`
	LastTickAt time.Time              `json:"last_tick_at,omitempty"`
	Components []brainStatusComponent `json:"components,omitempty"`
}

// factsBackfillStatusForBrain counts how many exported sessions have already
// been distilled. The distill cache is the authority: a session is distilled
// exactly when its cache entry exists, which is also the thing that stops a
// re-run from re-spending on it.
func factsBackfillStatusForBrain(brainDir string) factsBackfillStatus {
	status := factsBackfillStatus{}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return status
	}
	sessions := manifest.Sources.Sessions.Sessions
	if kept, err := filterTombstonedSessions(brainDir, append([]exportSession(nil), sessions...)); err == nil {
		sessions = kept
	}
	cache := loadDistillCache(brainDir)
	for _, session := range sessions {
		status.Sessions++
		branch := resolveDistillBranch(manifest, session)
		if _, ok := cachedDistillSessionFingerprint(cache, branch, session.SessionID); ok {
			status.Distilled++
			continue
		}
		// A pre-upgrade entry still means the session was distilled; matching
		// its legacy fingerprint would need the transcript, so presence of the
		// legacy key is enough for a progress counter.
		if cache.Sessions != nil {
			if _, ok := cache.Sessions[session.SessionID]; ok {
				status.Distilled++
			}
		}
	}
	return status
}

// readSetupBackfillState loads the marker the last `setup` wrote and reports
// whether that process is still alive. A stale marker (the process finished or
// was killed) reads as not running rather than as an error.
func readSetupBackfillState(stateDir string) (setupBackfillState, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, setupBackfillStateFile))
	if err != nil {
		return setupBackfillState{}, false
	}
	var state setupBackfillState
	if err := json.Unmarshal(data, &state); err != nil {
		return setupBackfillState{}, false
	}
	return state, true
}

// buildBrainOnboardingStatus assembles the section. runner may be nil (tests,
// or a report built without a command runner); the daemon then reports
// installed-or-not without a liveness probe.
func buildBrainOnboardingStatus(ctx context.Context, opts Options, storage repoStorage, manifest *exportManifest, setupOpts setupCommandOptions) brainStatusOnboarding {
	onboarding := brainStatusOnboarding{Facts: factsBackfillStatusForBrain(storage.BrainDir)}
	stateDir := filepath.Dir(storage.HeadPath)
	if state, ok := readSetupBackfillState(stateDir); ok {
		onboarding.Facts.PID = state.PID
		onboarding.Facts.StartedAt = state.StartedAt
		onboarding.Facts.Agent = state.Agent
		onboarding.Facts.LogPath = state.LogPath
		onboarding.Facts.Running = onboarding.Facts.Pending() > 0 && processAlive(state.PID)
	}
	if cursor := loadWatchCursor(filepath.Join(stateDir, "watch.json")); !cursor.LastRefreshAt.IsZero() {
		onboarding.LastTickAt = cursor.LastRefreshAt
	}
	if plan, err := brainWatchDaemonPlan(opts, setupOpts, storage); err == nil {
		onboarding.Daemon = inspectDaemon(ctx, opts.Runner, plan)
	} else {
		onboarding.Daemon = daemonState{Manager: daemonManagerUnsupported, Detail: err.Error()}
	}
	onboarding.Components = instantPhaseComponents(manifest)
	return onboarding
}

// instantPhaseComponents reports which of the instant phase's deterministic
// sources actually exist, in build order, so a partial setup is visible rather
// than inferred from a missing query result.
func instantPhaseComponents(manifest *exportManifest) []brainStatusComponent {
	present := map[string]bool{}
	if manifest != nil && manifest.Sources != nil {
		present["sessions"] = manifest.Sources.Sessions != nil
		present["seed"] = manifest.Sources.Seed != nil
		present["docs"] = manifest.Sources.Docs != nil
		present["semantic"] = manifest.Sources.Semantic != nil
		present["history"] = manifest.Sources.History != nil
	}
	components := make([]brainStatusComponent, 0, 5)
	for _, name := range []string{"sessions", "seed", "docs", "semantic", "history"} {
		state := "missing"
		if present[name] {
			state = "built"
		}
		components = append(components, brainStatusComponent{Name: name, State: state})
	}
	return components
}

// renderBrainOnboardingStatus prints the section `entire-brain status` gained:
// backfill progress, daemon health, and instant-phase component freshness.
func renderBrainOnboardingStatus(out io.Writer, onboarding *brainStatusOnboarding, now time.Time) {
	if onboarding == nil {
		return
	}
	fmt.Fprintln(out, "\nOnboarding")
	line := fmt.Sprintf("  facts: %d/%d sessions distilled", onboarding.Facts.Distilled, onboarding.Facts.Sessions)
	switch {
	case onboarding.Facts.Running:
		line += fmt.Sprintf(" (backfill running, pid %d)", onboarding.Facts.PID)
	case onboarding.Facts.Sessions == 0:
		line += " (no captured sessions yet)"
	case onboarding.Facts.Pending() > 0:
		line += fmt.Sprintf(" (%d pending)", onboarding.Facts.Pending())
	}
	fmt.Fprintln(out, line)
	fmt.Fprintf(out, "  daemon: %s", describeDaemonState(onboarding.Daemon))
	// The watch cursor is written only by a watcher tick, so it is real history
	// even when no daemon is installed right now — say which it is, or "not
	// installed; last tick 4m ago" reads as a contradiction.
	if !onboarding.LastTickAt.IsZero() {
		age := humanizeAge(now.Sub(onboarding.LastTickAt))
		if onboarding.Daemon.Running {
			fmt.Fprintf(out, "; last tick %s ago", age)
		} else {
			fmt.Fprintf(out, " (last watcher tick %s ago)", age)
		}
	}
	fmt.Fprintln(out)
	if len(onboarding.Components) > 0 {
		parts := make([]string, 0, len(onboarding.Components))
		for _, component := range onboarding.Components {
			parts = append(parts, component.Name+"="+component.State)
		}
		fmt.Fprintf(out, "  instant: %s\n", strings.Join(parts, " "))
	}
}

func humanizeAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return age.Round(time.Second).String()
	case age < time.Hour:
		return age.Round(time.Minute).String()
	default:
		return age.Round(time.Hour).String()
	}
}

// setupHookState reports whether a host agent's session-end hook is wired.
// setup deliberately does NOT install it: hook registration belongs to the
// Entire CLI's repository enablement (`entire enable`), which owns the whole
// lifecycle hook set. Duplicating it here would produce two hooks racing to
// distill the same session.
type setupHookState struct {
	Installed bool   `json:"installed"`
	Source    string `json:"source,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// sessionEndHookSettingsFiles are the Claude Code settings files that can carry
// the hook, nearest scope first.
func sessionEndHookSettingsFiles(repoDir string) []string {
	paths := []string{
		filepath.Join(repoDir, ".claude", "settings.json"),
		filepath.Join(repoDir, ".claude", "settings.local.json"),
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".claude", "settings.json"))
	}
	return paths
}

// inspectSessionEndHook reports the wiring status of the session-end hook
// without changing it. A missing or unreadable settings file is simply "not
// wired" — this is a report, never a failure.
func inspectSessionEndHook(repoDir string) setupHookState {
	for _, path := range sessionEndHookSettingsFiles(repoDir) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if hookSettingsDeclareSessionEnd(data) {
			return setupHookState{Installed: true, Source: path}
		}
	}
	return setupHookState{Detail: "no SessionEnd hook found in .claude settings"}
}

// hookSettingsDeclareSessionEnd looks for a SessionEnd hook entry. The value
// shape is the host's, not ours, so the check stays structural (the key exists
// and carries at least one command) rather than matching an exact command
// string that the Entire CLI is free to change.
func hookSettingsDeclareSessionEnd(data []byte) bool {
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}
	names := make([]string, 0, len(settings.Hooks))
	for name := range settings.Hooks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.EqualFold(name, "SessionEnd") {
			continue
		}
		for _, matcher := range settings.Hooks[name] {
			for _, hook := range matcher.Hooks {
				if strings.TrimSpace(hook.Command) != "" {
					return true
				}
			}
		}
	}
	return false
}
