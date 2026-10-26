package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
)

// entireBinaryName is the Entire CLI that owns the session lifecycle hooks. The
// brain never installs them itself (see setupHookState) but it must be able to
// tell whether the hook it found is ours and whether the binary it names exists.
const entireBinaryName = "entire"

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
	Sessions  int `json:"sessions"`
	Distilled int `json:"distilled"`
	// Queued is how many sessions the CURRENT setup run's backfill will
	// actually distill — Pending() capped by the spend budget. Transient:
	// populated only while a setup run reports on the backfill it just
	// started, so the summary can say "2 of 272" instead of claiming the
	// whole pool.
	Queued    int       `json:"queued,omitempty"`
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
// "failed" is deliberately distinct from "missing": a source that was never
// attempted and one whose build blew up need opposite reactions from the
// reader, and reporting both as missing is how a broken semantic index looked
// like an empty one.
type brainStatusComponent struct {
	Name   string `json:"name"`
	State  string `json:"state"` // built | failed | missing
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// brainStatusOnboarding is the `setup` progress projection: how far the fact
// backfill has got, whether the background watcher is healthy, and whether the
// instant phase's components exist.
type brainStatusOnboarding struct {
	Facts      factsBackfillStatus    `json:"facts_backfill"`
	Daemon     daemonState            `json:"daemon"`
	LastTickAt time.Time              `json:"last_tick_at,omitempty"`
	Components []brainStatusComponent `json:"components,omitempty"`
	// WatchPlan is what the machine's ONE watcher covers. json:"-" on purpose:
	// the status JSON is a published contract and a new field in it is a
	// breaking change for anything parsing it today, so this is carried for the
	// text renderer only.
	WatchPlan setupWatchPlan `json:"-"`
	// HostCLIMissing reports that the Entire CLI is not on PATH. Sessions, the
	// semantic index and the entity index are all built THROUGH it, so the
	// verdict needs this to avoid recommending a rebuild that provably cannot
	// run. Stated in the negative so the zero value is the ordinary machine,
	// and json:"-" for the same published-contract reason as WatchPlan.
	HostCLIMissing bool `json:"-"`
	// AgentsUnwired reports that the agent instructions have not landed: the
	// guide or the managed pointer in AGENTS.md / CLAUDE.md is absent, so no
	// agent has been told this brain exists.
	//
	// This is the product's worst failure mode and it is SILENT -- setup
	// succeeds, every source reads healthy, and the brain is simply never
	// consulted. These fields are for the text renderer; JSON and MCP receive
	// the same diagnostic through the status report's existing warnings field.
	AgentsUnwired bool `json:"-"`
	// AgentsMissing names what is absent, so the line can say which half.
	AgentsMissing []string `json:"-"`
	// json:"-" for the same published-contract reason as WatchPlan.
	Paused bool `json:"-"`
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
		if fp, ok := cachedDistillSessionFingerprint(cache, branch, session.SessionID); ok {
			// A tentative entry (every chunk came back empty once) does not
			// stop a re-run from spending on the session, so it does not
			// count as distilled until the retry confirms it.
			if !isDistillEmptyFingerprint(fp) {
				status.Distilled++
			}
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
	onboarding := brainStatusOnboarding{Facts: factsBackfillStatusForBrain(storage.BrainDir), HostCLIMissing: !hostCLIOnPath()}
	stateDir := filepath.Dir(storage.HeadPath)
	if state, ok := readSetupBackfillState(stateDir); ok {
		onboarding.Facts.PID = state.PID
		onboarding.Facts.StartedAt = state.StartedAt
		onboarding.Facts.Agent = state.Agent
		onboarding.Facts.LogPath = state.LogPath
		onboarding.Facts.Running = onboarding.Facts.Pending() > 0 && backfillRunning(storage.BrainDir, state.PID)
	}
	if cursor := loadWatchCursor(filepath.Join(stateDir, "watch.json")); !cursor.LastRefreshAt.IsZero() {
		onboarding.LastTickAt = cursor.LastRefreshAt
	}
	var watchPlanErr error
	onboarding.WatchPlan, watchPlanErr = loadSetupWatchPlan(opts.Env)
	if plan, err := brainWatchDaemonPlan(opts, setupOpts); err == nil {
		onboarding.Daemon = inspectDaemon(ctx, opts.Runner, plan)
	} else {
		onboarding.Daemon = daemonState{Manager: daemonManagerUnsupported, Detail: err.Error()}
	}
	if watchPlanErr != nil {
		onboarding.Daemon.Detail = strings.TrimSpace(onboarding.Daemon.Detail + "; watch plan unavailable: " + watchPlanErr.Error())
	}
	onboarding.Components = instantPhaseComponents(manifest, readSetupInstantRecord(stateDir), componentRunTimes(storage.BrainDir))
	onboarding.Paused = backgroundPaused(opts.Env)
	return onboarding
}

// instantPhaseComponents reports which of the instant phase's deterministic
// sources actually exist, in build order, so a partial setup is visible rather
// than inferred from a missing query result.
//
// The last setup's per-component record is layered on top: a component it could
// not build reads "failed", not "missing", and it wins over a source file that
// is merely PRESENT — the repo-key skew that motivated this leaves a stale
// snapshot on disk that the brain then refuses, so presence alone would report
// a component as built while every query against it failed. The record loses
// only to a source rebuilt since (generated at or after the record), which is
// how the line goes back to "built" without anyone clearing state by hand.
// componentRunTimes collects, per instant-phase component that publishes one,
// the moment its subsystem last completed a run. It is what lets a recorded
// failure age out instead of being reported forever.
//
// Only components whose own layer records a run time appear here. Extending it
// is how another component joins the rule; until then it keeps the previous
// behaviour, which errs toward still reporting the failure.
func componentRunTimes(brainDir string) map[string]time.Time {
	runs := map[string]time.Time{}
	if run, ok, err := lastPatternRunChecked(brainDir); err == nil && ok {
		if at, parseErr := time.Parse(time.RFC3339, run.At); parseErr == nil {
			runs[brainComponentPatterns] = at.UTC()
		}
	}
	return runs
}

// componentRebuiltSince reports whether a component's own subsystem produced a
// successful run at or after the moment a failure was recorded for it.
//
// Absent or zero means "this component publishes no run time", which is
// deliberately NOT treated as a rebuild: reporting a failure that has since
// been fixed is annoying, but hiding one that has not been is worse.
func componentRebuiltSince(runs map[string]time.Time, name string, recordedAt time.Time) bool {
	at, ok := runs[name]
	if !ok || at.IsZero() {
		return false
	}
	return !at.Before(recordedAt)
}

func instantPhaseComponents(manifest *exportManifest, record setupInstantRecord, componentRuns map[string]time.Time) []brainStatusComponent {
	failures := map[string]setupComponent{}
	// Skipped is layered exactly like failed: a component setup deliberately
	// left out (an installed Graph without the store contract) must keep its
	// reason and hint on the status row, and a bare "missing" from manifest
	// absence would drop both.
	skips := map[string]setupComponent{}
	for _, component := range record.Components {
		if component.failed() {
			failures[component.Name] = component
		}
		if component.State == "skipped" {
			skips[component.Name] = component
		}
	}
	present := map[string]bool{}
	generated := map[string]time.Time{}
	if manifest != nil && manifest.Sources != nil {
		sources := manifest.Sources
		present["sessions"] = sources.Sessions != nil
		present["seed"] = sources.Seed != nil
		present["docs"] = sources.Docs != nil
		present["semantic"] = sources.Semantic != nil
		present["history"] = sources.History != nil
		if sources.Sessions != nil {
			generated["sessions"] = sources.Sessions.GeneratedAt
		}
		if sources.Seed != nil {
			generated["seed"] = sources.Seed.GeneratedAt
		}
		if sources.Docs != nil {
			generated["docs"] = sources.Docs.GeneratedAt
		}
		if sources.Semantic != nil {
			generated["semantic"] = sources.Semantic.GeneratedAt
		}
		if sources.History != nil {
			generated["history"] = sources.History.GeneratedAt
		}
	}
	manifestBacked := []string{"sessions", "seed", "docs", "semantic", "history"}
	components := make([]brainStatusComponent, 0, len(manifestBacked)+len(record.Components))
	for _, name := range manifestBacked {
		// A recorded failure or skip clears when the source proves itself
		// newer than the record, and GeneratedAt is that proof — except for
		// one semantic shape. A semantic source WITH a timestamp is a
		// persisted snapshot, which can be the exact stale artifact the
		// record warns about (the repo-key skew), so it keeps the timestamp
		// rule. A semantic source WITHOUT one is the LIVE `graph store`
		// descriptor (agent_surface.go), fetched moments ago: its presence IS
		// current proof the store answers, and the timestamp rule would read
		// its zero time as "older than the record" and pin a recorded skip
		// forever, Graph upgrade or not.
		rebuilt := present[name] && !generated[name].Before(record.UpdatedAt)
		if name == brainComponentSemantic && present[name] && generated[name].IsZero() {
			rebuilt = true
		}
		failure, hasFailure := failures[name]
		if hasFailure && rebuilt {
			hasFailure = false
		}
		skip, hasSkip := skips[name]
		if hasSkip && rebuilt {
			hasSkip = false
		}
		switch {
		case hasFailure:
			components = append(components, brainStatusComponent{
				Name:   name,
				State:  "failed",
				Detail: failure.Detail,
				Hint:   failure.Hint,
			})
		case hasSkip:
			components = append(components, brainStatusComponent{
				Name:   name,
				State:  "skipped",
				Detail: skip.Detail,
				Hint:   skip.Hint,
			})
		case present[name]:
			components = append(components, brainStatusComponent{Name: name, State: "built"})
		default:
			components = append(components, brainStatusComponent{Name: name, State: "missing"})
		}
	}
	// Everything else the last setup actually attempted, in the order it ran.
	// The five above are the sources the manifest can vouch for; setup's instant
	// phase builds more than five (patterns, entities, memory, branches, ...)
	// and this list dropped them entirely. So `setup` could print
	// "FAILED entities" and `status` -- the command setup points at for exactly
	// this -- showed no entities line at all, which is the opposite of the one
	// shared vocabulary the component ids are declared to be.
	for _, component := range record.Components {
		if slices.Contains(manifestBacked, component.Name) {
			continue
		}
		state := "built"
		detail, hint := component.Detail, component.Hint
		// The same staleness rule the five above get. It used to apply only to
		// manifest-backed sources, so a failure recorded here was reported
		// FOREVER, however many successful rebuilds happened afterwards. On a
		// real machine that left `patterns` reading "failed" for four days
		// over a manifest field that no longer existed anywhere, while
		// `brain patterns status` read the artifacts directly and said
		// "current" -- two surfaces, opposite verdicts, same moment.
		//
		// A component is only still failed if its subsystem has not produced a
		// successful run since the failure was recorded. A component that
		// publishes no run time keeps the old behaviour rather than being
		// assumed healthy: silence is not success.
		if component.failed() && !componentRebuiltSince(componentRuns, component.Name, record.UpdatedAt) {
			state = "failed"
		} else if component.failed() {
			detail, hint = "", ""
		} else if component.State == "skipped" {
			state = "skipped"
		}
		components = append(components, brainStatusComponent{
			Name:   component.Name,
			State:  state,
			Detail: detail,
			Hint:   hint,
		})
	}
	return components
}

// markMissingDeclaredIndexes downgrades an instant component whose manifest
// entry declares an artifact the store no longer holds.
//
// Manifest presence alone cannot show whether a declared index still exists.
// Inspect the files so deleted docs and history indexes are not reported as built.
//
// "failed", not "missing": missing means never attempted, and a manifest entry
// is proof it WAS. Reporting a deleted index as never-built is how a corrupt
// brain reads as a new one, and the new-brain case is protected instead by the
// only rule that can protect it — an undeclared source is never inspected.
//
// A component the last setup already recorded as failed keeps that record:
// the build's own reason is more specific than "the file is not there now".
func markMissingDeclaredIndexes(onboarding *brainStatusOnboarding, brainDir string, manifest *exportManifest) {
	if onboarding == nil {
		return
	}
	for _, defect := range inspectDeclaredIndexes(brainDir, manifest) {
		for i, component := range onboarding.Components {
			if component.Name != string(defect.Source) || component.State != "built" {
				continue
			}
			onboarding.Components[i].State = "failed"
			onboarding.Components[i].Detail = defect.Warning()
		}
	}
}

// renderBrainOnboardingStatus prints the section `entire-brain status` gained:
// backfill progress, daemon health, and instant-phase component freshness.
func renderBrainOnboardingStatus(out io.Writer, onboarding *brainStatusOnboarding, now time.Time, brainCmd string) {
	brainCmd = clibase.SetupBrainCommand(brainCmd)
	if onboarding == nil {
		return
	}
	fmt.Fprintln(out, "\nOnboarding")
	// FIRST, because it decides whether any of the rest is ever read. A brain
	// no agent knows about is healthy on every other line and useless in
	// practice, and nothing else in this report would say so.
	if onboarding.AgentsUnwired {
		fmt.Fprintf(out, "  agents: NOT WIRED — run `%s setup` so your agent uses this brain\n", brainCmd)
		if len(onboarding.AgentsMissing) > 0 {
			fmt.Fprintf(out, "          missing: %s\n", strings.Join(onboarding.AgentsMissing, ", "))
		}
	}
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
	fmt.Fprintf(out, "  daemon: %s", describeDaemonState(onboarding.Daemon, brainCmd))
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
	// Same reason as setup's Next block: the only place --uninstall-daemon was
	// ever written down was `setup --help`.
	if onboarding.Daemon.Installed {
		fmt.Fprintln(out, "    remove it with `"+brainCmd+" setup --uninstall-daemon`")
	}
	if len(onboarding.Components) > 0 {
		parts := make([]string, 0, len(onboarding.Components))
		failed := false
		for _, component := range onboarding.Components {
			parts = append(parts, component.Name+"="+component.State)
			failed = failed || component.State == "failed"
		}
		fmt.Fprintf(out, "  instant: %s\n", strings.Join(parts, " "))
		// A failed component is only useful if the reader can get at the reason,
		// and the reason is one command away rather than in this line.
		if failed {
			fmt.Fprintln(out, "    a component failed — run `"+brainCmd+" doctor` for the reason")
		}
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
	// Command is the matched hook command, so a report can show WHICH hook was
	// accepted rather than asking the reader to trust a bare boolean.
	Command string `json:"command,omitempty"`
	// EntireCLI reports whether `entire` is actually on PATH. A wired hook that
	// invokes a binary this machine does not have is worse than no hook: it
	// looks healthy in status and silently distills nothing.
	EntireCLI bool `json:"entire_cli_on_path"`
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
	_, lookErr := exec.LookPath(entireBinaryName)
	onPath := lookErr == nil
	for _, path := range sessionEndHookSettingsFiles(repoDir) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if command, ok := entireSessionEndHookCommand(data); ok {
			state := setupHookState{Installed: true, Source: path, Command: command, EntireCLI: onPath}
			if !onPath {
				state.Detail = "hook is wired but `" + entireBinaryName + "` is not on PATH; sessions will not distill"
			}
			return state
		}
	}
	state := setupHookState{Detail: "no Entire SessionEnd hook found in .claude settings", EntireCLI: onPath}
	if !onPath {
		state.Detail += "; `" + entireBinaryName + "` is not on PATH either"
	}
	return state
}

// entireSessionEndHookCommand looks for a SessionEnd hook entry that runs OUR
// hook, and returns its command.
//
// Any non-empty command used to satisfy this check, which made the report a
// coin flip: a repo whose SessionEnd hook belongs to some unrelated tool was
// reported as "session-end hook already wired", so setup told the user the
// distill-on-session-end path was live when nothing would ever call it. The
// value shape is still the host's, so the match stays deliberately loose about
// everything except the one thing that matters — the command invokes the Entire
// CLI and its hook verb.
func entireSessionEndHookCommand(data []byte) (string, bool) {
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", false
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
				command := strings.TrimSpace(hook.Command)
				if command != "" && commandInvokesEntireHook(command) {
					return command, true
				}
			}
		}
	}
	return "", false
}

// commandInvokesEntireHook recognises the Entire CLI's hook invocation without
// pinning the exact argv, which the CLI is free to change. It must reject
// another tool's hook (the bug) while accepting ours however it is spelled:
// bare `entire`, an absolute path, or wrapped in a shell line.
func commandInvokesEntireHook(command string) bool {
	lowered := strings.ToLower(command)
	if !strings.Contains(lowered, "hook") {
		return false
	}
	for _, field := range strings.FieldsFunc(lowered, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '"' || r == '\'' || r == ';' || r == '|' || r == '&' || r == '(' || r == ')'
	}) {
		base := path.Base(filepath.ToSlash(field))
		base = strings.TrimSuffix(base, ".exe")
		if base == entireBinaryName || base == "entire-brain" {
			return true
		}
	}
	return false
}
