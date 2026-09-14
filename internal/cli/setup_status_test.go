package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestFactsBackfillStatusCountsDistilledSessions(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3", "s4")
	if status := factsBackfillStatusForBrain(f.storage.BrainDir); status.Sessions != 4 || status.Distilled != 0 || status.Pending() != 4 {
		t.Fatalf("a fresh brain has nothing distilled, got %+v", status)
	}
	f.markDistilled(t, "s1", "s3")
	status := factsBackfillStatusForBrain(f.storage.BrainDir)
	if status.Sessions != 4 || status.Distilled != 2 || status.Pending() != 2 {
		t.Fatalf("expected 2/4 distilled, got %+v", status)
	}
}

func TestFactsBackfillStatusCountsLegacyCacheEntries(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	// A pre-upgrade cache keyed by bare session id still means the session was
	// distilled; the progress counter must not report it as pending work.
	cache := distillCache{Version: distillCacheVersion, Sessions: map[string]string{"s1": "legacy-fingerprint"}}
	if err := saveDistillCache(f.storage.BrainDir, cache); err != nil {
		t.Fatalf("save cache: %v", err)
	}
	if status := factsBackfillStatusForBrain(f.storage.BrainDir); status.Distilled != 1 {
		t.Fatalf("legacy cache entries must count as distilled, got %+v", status)
	}
}

func TestFactsBackfillStatusOnAnEmptyBrain(t *testing.T) {
	t.Parallel()
	if status := factsBackfillStatusForBrain(t.TempDir()); status.Sessions != 0 || status.Pending() != 0 {
		t.Fatalf("an unbuilt brain must report zeros, not an error, got %+v", status)
	}
}

func TestBuildBrainOnboardingStatusReportsBackfillAndDaemon(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	f.markDistilled(t, "s1")
	stateDir := filepath.Dir(f.storage.HeadPath)
	if err := writeSetupBackfillState(stateDir, setupBackfillState{
		SchemaVersion: setupBackfillStateVersion,
		StartedAt:     setupTestNow.Add(-time.Minute),
		PID:           os.Getpid(), // a pid that is definitely alive
		Agent:         "codex",
	}); err != nil {
		t.Fatalf("write backfill state: %v", err)
	}
	// A live pid is not on its own a live backfill — pids are reused, and a
	// reboot hands the low ones straight back out. What makes this pid a
	// RUNNING backfill is that a distill pass holds the pass lock, so take it
	// for the duration of this test the way a real pass would.
	if err := os.MkdirAll(filepath.Join(f.storage.BrainDir, brainLockDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	passLock, err := acquireFileLock(filepath.Join(f.storage.BrainDir, brainLockDirName, brainDistillLockName), "distill_pass_locked", 0)
	if err != nil {
		t.Fatalf("take the distill pass lock: %v", err)
	}
	defer func() { _ = passLock.Close() }()
	if err := saveWatchCursor(filepath.Join(stateDir, "watch.json"), watchCursor{LastRefreshAt: setupTestNow.Add(-5 * time.Minute)}); err != nil {
		t.Fatalf("save watch cursor: %v", err)
	}
	manifest, err := loadBrainManifest(f.storage.BrainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	onboarding := buildBrainOnboardingStatus(context.Background(), f.opts, f.storage, manifest, defaultSetupOptions())

	if onboarding.Facts.Distilled != 1 || onboarding.Facts.Sessions != 2 {
		t.Fatalf("expected 1/2 distilled, got %+v", onboarding.Facts)
	}
	if !onboarding.Facts.Running {
		t.Fatalf("a live backfill pid with pending work must read as running: %+v", onboarding.Facts)
	}
	if onboarding.LastTickAt.IsZero() {
		t.Fatalf("the watch cursor is the daemon's last tick; it must surface")
	}
	if onboarding.Daemon.Installed {
		t.Fatalf("no daemon was installed in this fixture: %+v", onboarding.Daemon)
	}
	var built []string
	for _, component := range onboarding.Components {
		if component.State == "built" {
			built = append(built, component.Name)
		}
	}
	if strings.Join(built, ",") != "sessions" {
		t.Fatalf("only the sessions source exists in this fixture, got %v", onboarding.Components)
	}
}

// TestInstantPhaseComponentsSeparateFailedFromMissing is requirement and bug in
// one: a component the last setup could not build must read "failed", with a
// reason, and not hide behind "missing" (never attempted) or "built" (the stale
// snapshot file the brain actually refuses).
func TestInstantPhaseComponentsSeparateFailedFromMissing(t *testing.T) {
	t.Parallel()
	failure := newSetupComponent(brainComponentSemantic, errors.New(setupSemanticMismatchError), setupBrainBinaryName)
	record := setupInstantRecord{
		UpdatedAt:  setupTestNow,
		Components: []setupComponent{newSetupComponent(brainComponentSessions, nil, setupBrainBinaryName), failure},
	}
	manifest := &exportManifest{Sources: &brainSources{
		Sessions: &sessionSourceManifest{GeneratedAt: setupTestNow},
		// The stale snapshot the skew leaves behind: present on disk, rejected
		// on every read, written BEFORE the failure was recorded.
		Semantic: &semanticSourceManifest{GeneratedAt: setupTestNow.Add(-time.Hour)},
	}}

	components := instantPhaseComponents(manifest, record)

	if state := statusComponentState(components, brainComponentSemantic); state != "failed" {
		t.Fatalf("a component that failed must not read %q", state)
	}
	if state := statusComponentState(components, brainComponentSessions); state != "built" {
		t.Fatalf("sessions built, got %q", state)
	}
	if state := statusComponentState(components, brainComponentSeed); state != "missing" {
		t.Fatalf("a component nobody attempted is missing, not failed, got %q", state)
	}
	for _, component := range components {
		if component.Name == brainComponentSemantic && !strings.Contains(component.Detail, "repo key mismatch") {
			t.Fatalf("the failed component must carry its reason: %+v", component)
		}
	}
	// Self-healing: a source rebuilt AFTER the recorded failure reads built
	// again, so a stale record can never pin a healthy brain as broken.
	manifest.Sources.Semantic.GeneratedAt = setupTestNow.Add(time.Hour)
	if state := statusComponentState(instantPhaseComponents(manifest, record), brainComponentSemantic); state != "built" {
		t.Fatalf("a source rebuilt since the failure must read built, got %q", state)
	}
}

// TestBuildBrainOnboardingStatusIgnoresADeadBackfill guards against a stale
// marker: setup writes a pid, the process finishes, and status must not keep
// claiming a backfill is running.
func TestBuildBrainOnboardingStatusIgnoresADeadBackfill(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	stateDir := filepath.Dir(f.storage.HeadPath)
	if err := writeSetupBackfillState(stateDir, setupBackfillState{PID: 0x7FFFFFFF}); err != nil {
		t.Fatalf("write backfill state: %v", err)
	}
	onboarding := buildBrainOnboardingStatus(context.Background(), f.opts, f.storage, nil, defaultSetupOptions())
	if onboarding.Facts.Running {
		t.Fatalf("a dead pid must not read as a running backfill: %+v", onboarding.Facts)
	}
}

func TestRenderBrainOnboardingStatusLines(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	renderBrainOnboardingStatus(out, &brainStatusOnboarding{
		Facts:      factsBackfillStatus{Sessions: 12, Distilled: 3, Running: true, PID: 99},
		Daemon:     daemonState{Manager: daemonManagerLaunchd, Label: "io.entire.brain-watch", Installed: true, Current: true, Running: true},
		LastTickAt: setupTestNow.Add(-2 * time.Minute),
		Components: []brainStatusComponent{{Name: "sessions", State: "built"}, {Name: "semantic", State: "missing"}},
	}, setupTestNow, setupBrainBinaryName)

	rendered := out.String()
	for _, want := range []string{
		"facts: 3/12 sessions distilled (backfill running, pid 99)",
		"daemon: running (io.entire.brain-watch); last tick 2m0s ago",
		"instant: sessions=built semantic=missing",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q in:\n%s", want, rendered)
		}
	}
}

func TestRenderBrainOnboardingStatusWithoutADaemon(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	renderBrainOnboardingStatus(out, &brainStatusOnboarding{
		Facts: factsBackfillStatus{Sessions: 0},
	}, setupTestNow, setupBrainBinaryName)
	rendered := out.String()
	if !strings.Contains(rendered, "facts: 0/0 sessions distilled (no captured sessions yet)") {
		t.Fatalf("unexpected facts line:\n%s", rendered)
	}
	if !strings.Contains(rendered, "daemon: not installed") {
		t.Fatalf("unexpected daemon line:\n%s", rendered)
	}
}

// TestBrainStatusTextIncludesOnboardingSection proves the progress lives on the
// EXISTING status verb rather than a duplicate command.
func TestBrainStatusTextIncludesOnboardingSection(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	f.markDistilled(t, "s1")

	report, err := buildAvailableBrainStatusReport(context.Background(), f.opts, f.repoDir)
	if err != nil {
		t.Fatalf("buildAvailableBrainStatusReport: %v", err)
	}
	if report.Onboarding == nil {
		t.Fatal("status must carry the onboarding section")
	}
	if report.Onboarding.Facts.Distilled != 1 || report.Onboarding.Facts.Sessions != 2 {
		t.Fatalf("expected 1/2 distilled in status, got %+v", report.Onboarding.Facts)
	}
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	renderBrainStatusText(cmd, report, true)
	if !strings.Contains(out.String(), "facts: 1/2 sessions distilled") {
		t.Fatalf("status text must show backfill progress:\n%s", out.String())
	}
}

func TestInspectSessionEndHookDetectsRepoSettings(t *testing.T) {
	repoDir := t.TempDir()
	redirectHomeDir(t) // never consult the real ~/.claude -- on ANY platform
	if state := inspectSessionEndHook(repoDir); state.Installed {
		t.Fatalf("no settings file means not wired: %+v", state)
	}

	settings := filepath.Join(repoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	// A settings file with other hooks but no SessionEnd is still not wired.
	if err := os.WriteFile(settings, []byte(`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"x"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := inspectSessionEndHook(repoDir); state.Installed {
		t.Fatalf("other hooks are not the session-end hook: %+v", state)
	}

	if err := os.WriteFile(settings, []byte(`{"hooks":{"SessionEnd":[{"matcher":"","hooks":[{"type":"command","command":"entire hooks claude-code session-end"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := inspectSessionEndHook(repoDir)
	if !state.Installed || state.Source != settings {
		t.Fatalf("expected the repo settings file to be reported: %+v", state)
	}
	if state.Command != "entire hooks claude-code session-end" {
		t.Fatalf("the matched command must be reported: %+v", state)
	}
}

// TestInspectSessionEndHookRejectsAnotherToolsHook is the guard for the bug the
// review found: ANY non-empty SessionEnd command satisfied the check, so setup
// told the user distill-on-session-end was live when the wired hook belonged to
// an unrelated tool and would never call us.
func TestInspectSessionEndHookRejectsAnotherToolsHook(t *testing.T) {
	repoDir := t.TempDir()
	redirectHomeDir(t)
	settings := filepath.Join(repoDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := `{"hooks":{"SessionEnd":[{"matcher":"","hooks":[{"type":"command","command":"some-other-tool report --session-end"}]}]}}`
	if err := os.WriteFile(settings, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := inspectSessionEndHook(repoDir); state.Installed {
		t.Fatalf("another tool's SessionEnd hook must not read as ours: %+v", state)
	}
}

// TestEntireSessionEndHookCommandMatchesOurSpellings pins the literal guard both
// ways: every way our own hook is spelled must match, and near-misses must not.
func TestEntireSessionEndHookCommandMatchesOurSpellings(t *testing.T) {
	t.Parallel()
	settings := func(command string) []byte {
		return []byte(`{"hooks":{"SessionEnd":[{"matcher":"","hooks":[{"type":"command","command":` +
			mustJSONString(command) + `}]}]}}`)
	}
	ours := []string{
		"entire hooks claude-code session-end",
		"/usr/local/bin/entire hooks claude-code session-end",
		"\"/opt/my tools/entire\" hooks claude-code session-end",
		"entire-brain hook session-end",
		"sh -c 'entire hooks claude-code session-end'",
	}
	for _, command := range ours {
		got, ok := entireSessionEndHookCommand(settings(command))
		if !ok || got != command {
			t.Fatalf("our own hook must match: %q (ok=%v got=%q)", command, ok, got)
		}
	}
	theirs := []string{
		"some-other-tool report --session-end",
		"entirely-different --hook",   // substring of "entire" is not the binary
		"my-entire-wrapper hook send", // neither is a longer basename
		"entire enable",               // our binary, but not a hook invocation
	}
	for _, command := range theirs {
		if _, ok := entireSessionEndHookCommand(settings(command)); ok {
			t.Fatalf("must not claim another tool's hook: %q", command)
		}
	}
}

func TestEntireSessionEndHookCommandIgnoresEmptyCommands(t *testing.T) {
	t.Parallel()
	if _, ok := entireSessionEndHookCommand([]byte(`{"hooks":{"SessionEnd":[{"matcher":"","hooks":[{"type":"command","command":"   "}]}]}}`)); ok {
		t.Fatal("an entry with no command is not a wired hook")
	}
	if _, ok := entireSessionEndHookCommand([]byte(`not json`)); ok {
		t.Fatal("unparseable settings must read as not wired, never panic")
	}
}

func mustJSONString(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// TestRenderBrainOnboardingStatusDistinguishesHistoryFromHealth guards a
// confusing line a live run produced: the watch cursor is real history even
// after the daemon is uninstalled, so "not installed; last tick 4m ago" read as
// a contradiction.
func TestRenderBrainOnboardingStatusDistinguishesHistoryFromHealth(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	renderBrainOnboardingStatus(out, &brainStatusOnboarding{
		LastTickAt: setupTestNow.Add(-4 * time.Minute),
	}, setupTestNow, setupBrainBinaryName)
	if !strings.Contains(out.String(), "daemon: not installed (last watcher tick 4m0s ago)") {
		t.Fatalf("a stale tick must read as history, not health:\n%s", out.String())
	}
}

// `status` used to decide "backfill running" from a bare kill(pid, 0) on a
// recorded pid. After a reboot that pid belongs to something else, so status
// reported a backfill that did not exist — and `setup`'s re-entrancy guard, on
// the same signal, refused to start a new one, forever, in silence.
func TestStatusDoesNotReportAReusedPidAsARunningBackfill(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	f.markDistilled(t, "s1")
	stateDir := filepath.Dir(f.storage.HeadPath)
	if err := writeSetupBackfillState(stateDir, setupBackfillState{
		SchemaVersion: setupBackfillStateVersion,
		StartedAt:     setupTestNow.Add(-time.Minute),
		// Alive, and emphatically NOT a distill pass: this is the test binary.
		// That is exactly the shape a reused pid has.
		PID:   os.Getpid(),
		Agent: "codex",
	}); err != nil {
		t.Fatalf("write backfill state: %v", err)
	}
	manifest, err := loadBrainManifest(f.storage.BrainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	onboarding := buildBrainOnboardingStatus(context.Background(), f.opts, f.storage, manifest, defaultSetupOptions())

	if onboarding.Facts.Running {
		t.Fatalf("a stale pid with no distill pass behind it must not be reported as a running backfill: %+v", onboarding.Facts)
	}
	if onboarding.Facts.Pending() != 1 {
		t.Fatalf("the pending work is still pending and must still be reported: %+v", onboarding.Facts)
	}
}
