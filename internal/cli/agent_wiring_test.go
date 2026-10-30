package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/agentsetup"
	"github.com/entireio/entire-brain/internal/cli/command/hook"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// agent_wiring_test.go covers the first-run defect that made a successful
// `setup` useless: the brain was built and NOTHING TOLD AN AGENT IT EXISTED.
// setup.go carried no reference to the agentsetup package at all, so a
// setup-only repository had no guide, no AGENTS.md/CLAUDE.md pointer and no
// pre-edit hook, and every agent that opened it walked straight past the brain.

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestSetupWritesAgentInstructionsSoAgentsDiscoverTheBrain is UX-07: a plain
// `setup` must leave the repository carrying a pointer an agent will read.
func TestSetupWritesAgentInstructionsSoAgentsDiscoverTheBrain(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	out := runSetupForTest(t, f, opts, rec)

	guide := filepath.Join(f.repoDir, agentsetup.Path)
	if _, err := os.Stat(guide); err != nil {
		t.Fatalf("setup left no agent guide at %s: %v", agentsetup.Path, err)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		content := readFileString(t, filepath.Join(f.repoDir, name))
		if !strings.Contains(content, agentsetup.Path) {
			t.Fatalf("%s does not point at the agent guide, so no agent learns the brain exists:\n%s", name, content)
		}
	}
	settings := filepath.Join(f.repoDir, claudeSettingsDir, claudeSettingsFile)
	if !strings.Contains(readFileString(t, settings), "pre-edit") {
		t.Fatalf("setup wired no PreToolUse pre-edit hook into %s", settings)
	}
	if !strings.Contains(out, "agent instructions") {
		t.Fatalf("setup said nothing about agent instructions:\n%s", out)
	}
}

// TestSetupAgentWiringIsIdempotent proves running setup twice is safe: the
// managed block is replaced in place and the settings entry is not duplicated.
// Without this the second run would append a second pointer block and a second
// PreToolUse entry, and the hook would run twice per edit.
func TestSetupAgentWiringIsIdempotent(t *testing.T) {
	f := newSetupTestFixture(t)
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	runSetupForTest(t, f, opts, &recordedSetup{})
	first := map[string]string{}
	for _, name := range []string{agentsetup.Path, "AGENTS.md", "CLAUDE.md", claudeSettingsDir + "/" + claudeSettingsFile} {
		first[name] = readFileString(t, filepath.Join(f.repoDir, filepath.FromSlash(name)))
	}

	runSetupForTest(t, f, opts, &recordedSetup{})
	for name, before := range first {
		after := readFileString(t, filepath.Join(f.repoDir, filepath.FromSlash(name)))
		if before != after {
			t.Fatalf("second setup rewrote %s:\n--- first ---\n%s\n--- second ---\n%s", name, before, after)
		}
	}
	if n := strings.Count(first["AGENTS.md"], agentPointerBeginMarkerForTest); n != 1 {
		t.Fatalf("AGENTS.md carries %d managed pointer blocks, want exactly 1", n)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(first[claudeSettingsDir+"/"+claudeSettingsFile]), &settings); err != nil {
		t.Fatal(err)
	}
	preEdits := 0
	for _, matcher := range settings.Hooks["PreToolUse"] {
		for _, hook := range matcher.Hooks {
			if commandInvokesPreEditHook(hook.Command) {
				preEdits++
			}
		}
	}
	if preEdits != 1 {
		t.Fatalf("settings.json carries %d pre-edit hooks, want exactly 1", preEdits)
	}
}

// agentPointerBeginMarkerForTest is the installer's managed-block marker. It is
// spelled out here rather than imported so a change to the marker shows up as a
// failing idempotence test rather than silently agreeing with itself.
const agentPointerBeginMarkerForTest = "<!-- entire-agent:begin -->"

// TestSetupAgentWiringFailureIsAWarningNotAFailedSetup: the brain stays usable
// when the repository will not accept the instruction files. The guide write is
// made impossible by occupying `.entire` with a regular file.
func TestSetupAgentWiringFailureIsAWarningNotAFailedSetup(t *testing.T) {
	f := newSetupTestFixture(t)
	if err := os.WriteFile(filepath.Join(f.repoDir, ".entire"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	out := runSetupForTest(t, f, opts, &recordedSetup{})
	if !strings.Contains(out, "agent instructions not written") {
		t.Fatalf("a failed wiring must be reported, not swallowed:\n%s", out)
	}
	if !strings.Contains(out, "setup") {
		t.Fatalf("a failed wiring must name the command that repairs it:\n%s", out)
	}
}

// TestSetupRemembersDeclinedDaemonAndBackfill is UX-19. Declining the persistent
// login service and the token spend, then following our own "re-running setup is
// safe" advice, used to install the service and start the spend anyway.
func TestSetupRemembersDeclinedDaemonAndBackfill(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	declined := defaultSetupOptions()
	declined.noDaemon = true
	declined.noBackfill = true
	declined.agent = "codex"
	rec := &recordedSetup{agent: "codex"}
	runSetupForTest(t, f, declined, rec)
	if rec.installCalls != 0 || rec.spawnCalls != 0 {
		t.Fatalf("the declining run itself must install and spend nothing: install=%d spawn=%d", rec.installCalls, rec.spawnCalls)
	}

	// The bare re-run our own output recommends.
	bare := defaultSetupOptions()
	bare.agent = "codex"
	rerun := &recordedSetup{agent: "codex"}
	out := runSetupForTest(t, f, bare, rerun)
	if rerun.installCalls != 0 {
		t.Fatalf("a bare re-run installed the persistent watcher the user declined (%d installs)\n%s", rerun.installCalls, out)
	}
	if rerun.spawnCalls != 0 {
		t.Fatalf("a bare re-run started the token spend the user declined (%d spawns)\n%s", rerun.spawnCalls, out)
	}
}

// TestSetupExplicitAffirmativeOverridesRememberedDecline is the negative
// control for UX-19: remembering a decline must not make it unescapable.
func TestSetupExplicitAffirmativeOverridesRememberedDecline(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	declined := defaultSetupOptions()
	declined.noDaemon = true
	declined.noBackfill = true
	declined.agent = "codex"
	runSetupForTest(t, f, declined, &recordedSetup{agent: "codex"})

	affirmed := defaultSetupOptions()
	affirmed.agent = "codex"
	affirmed.daemon = true
	affirmed.backfill = true
	rec := &recordedSetup{agent: "codex"}
	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, affirmed)
	if err := runSetup(context.Background(), cmd, f.opts, affirmed, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("runSetup: %v\n%s", err, out.String())
	}
	if rec.installCalls != 1 {
		t.Fatalf("--daemon must override the remembered decline: installs=%d\n%s", rec.installCalls, out.String())
	}
	if rec.spawnCalls != 1 {
		t.Fatalf("--backfill must override the remembered decline: spawns=%d\n%s", rec.spawnCalls, out.String())
	}
}

// TestSetupRejectsContradictoryToggles: a run that both declines and demands the
// same thing has no safe reading, and guessing is how a reader gets the service
// they declined.
func TestSetupRejectsContradictoryToggles(t *testing.T) {
	f := newSetupTestFixture(t)
	opts := defaultSetupOptions()
	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, opts)
	for _, name := range []string{setupFlagNoDaemon, setupFlagDaemon} {
		if err := cmd.Flags().Set(name, "true"); err != nil {
			t.Fatal(err)
		}
	}
	err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, (&recordedSetup{}).steps(f))
	if err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Fatalf("--no-daemon with --daemon must be refused, got %v", err)
	}
}

// TestSetupRemembersDeclinedAgentHooks keeps the third decline in the same
// contract as the other two.
func TestSetupRemembersDeclinedAgentHooks(t *testing.T) {
	f := newSetupTestFixture(t)
	declined := defaultSetupOptions()
	declined.noDaemon = true
	declined.noBackfill = true
	declined.noAgentHooks = true
	runSetupForTest(t, f, declined, &recordedSetup{})
	settings := filepath.Join(f.repoDir, claudeSettingsDir, claudeSettingsFile)
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Fatalf("--no-agent-hooks still wrote %s (err %v)", settings, err)
	}

	bare := defaultSetupOptions()
	runSetupForTest(t, f, bare, &recordedSetup{})
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Fatalf("a bare re-run wired the hook the user declined: %v", err)
	}
}

// TestInitAgentsMergesPreToolUseHookPreservingOtherSettings is UX-18: the verb
// `hook pre-edit` existed and nothing called it. The merge must add the entry
// without destroying the reader's other settings — a typed-struct round trip
// would silently delete every key this binary does not know.
func TestInitAgentsMergesPreToolUseHookPreservingOtherSettings(t *testing.T) {
	repo := t.TempDir()
	existing := `{
  "model": "opus",
  "permissions": {"allow": ["Bash(gh pr merge:*)"]},
  "hooks": {
    "SessionEnd": [
      {"matcher": "", "hooks": [{"type": "command", "command": "entire hooks claude-code session-end"}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "rtk hook claude"}]}
    ]
  }
}`
	if err := os.MkdirAll(filepath.Join(repo, claudeSettingsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(repo, claudeSettingsDir, claudeSettingsFile)
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	path, changed, err := installPreEditHook(repo)
	if err != nil {
		t.Fatalf("installPreEditHook: %v", err)
	}
	if !changed || path != settingsPath {
		t.Fatalf("expected a change at %s, got changed=%v path=%s", settingsPath, changed, path)
	}
	merged := readFileString(t, settingsPath)
	for _, keep := range []string{`"opus"`, "Bash(gh pr merge:*)", "rtk hook claude", "session-end"} {
		if !strings.Contains(merged, keep) {
			t.Fatalf("the merge destroyed %q from the reader's settings:\n%s", keep, merged)
		}
	}
	if !strings.Contains(merged, "hook pre-edit --claude-hook") {
		t.Fatalf("no pre-edit hook was merged:\n%s", merged)
	}

	// Second call is a no-op: an entry already invokes the hook.
	if _, changed, err := installPreEditHook(repo); err != nil || changed {
		t.Fatalf("second merge must be a no-op, got changed=%v err=%v", changed, err)
	}
	if again := readFileString(t, settingsPath); again != merged {
		t.Fatalf("second merge rewrote the file:\n%s", again)
	}
}

// TestInstallPreEditHookRefusesSettingsItCannotUnderstand: overwriting a
// settings file whose shape we did not parse would cost the reader their whole
// harness configuration.
func TestInstallPreEditHookRefusesSettingsItCannotUnderstand(t *testing.T) {
	for name, body := range map[string]string{
		"invalid json":               "{ not json",
		"hooks is not an object":     `{"hooks": "none"}`,
		"PreToolUse is not an array": `{"hooks": {"PreToolUse": {"matcher": "Edit"}}}`,
	} {
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, claudeSettingsDir), 0o755); err != nil {
			t.Fatal(err)
		}
		settingsPath := filepath.Join(repo, claudeSettingsDir, claudeSettingsFile)
		if err := os.WriteFile(settingsPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := installPreEditHook(repo); err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
		if got := readFileString(t, settingsPath); got != body {
			t.Fatalf("%s: a refused merge still rewrote the file:\n%s", name, got)
		}
	}
}

// TestPreEditHookCommandSurvivesAManagedPluginInstall: on a managed install the
// brain binary is only on PATH inside the Entire CLI's own process, so a bare
// `entire-brain ...` hook command would be "command not found" on every edit.
func TestPreEditHookCommandSurvivesAManagedPluginInstall(t *testing.T) {
	// Both branches, on every host. Keying the assertions on runtime.GOOS meant
	// the windows one never ran anywhere we build, which is exactly how it came
	// to wire a command that fails on every edit for a managed install.
	for _, goos := range []string{"windows", "linux", "darwin"} {
		assertPreEditHookCommand(t, goos, preEditHookCommandFor(goos))
	}
	command := preEditHookCommand()
	// Both platforms need the fallback: a managed plugin install puts the brain
	// binary off PATH, and setupCommandPrefix detects that from an environment
	// marker with no GOOS condition, so windows reaches it too. Only the
	// interpreter differs -- cmd.exe cannot run the sh pipeline.
	if !strings.Contains(command, base.SetupEntireBinaryName+" "+base.SetupBrainPluginVerb+" hook pre-edit") {
		t.Fatalf("the wired command has no host-CLI fallback for a managed install: %s", command)
	}
	if !strings.Contains(command, base.SetupBrainBinaryName+" hook pre-edit") {
		t.Fatalf("the wired command never tries the brain binary directly: %s", command)
	}
	if runtime.GOOS == "windows" {
		if strings.Contains(command, "sh -c") || strings.Contains(command, "command -v") {
			t.Fatalf("a POSIX shell pipeline was wired on windows, where cmd.exe cannot run it: %s", command)
		}
		if !strings.HasPrefix(command, "cmd /c ") {
			t.Fatalf("the interpreter is not named, so this depends on how the host runs hooks: %s", command)
		}
		// A reader with neither binary must get a hook that does nothing, not
		// one that errors on every edit.
		if !strings.Contains(command, "exit /b 0") {
			t.Fatalf("a missing binary fails the hook instead of exiting clean: %s", command)
		}
	} else if !strings.Contains(command, "exit 0") {
		t.Fatalf("a missing binary fails the hook instead of exiting clean: %s", command)
	}
	if !commandInvokesPreEditHook(command) {
		t.Fatalf("the command we write is not recognised by our own idempotence check: %s", command)
	}
}

// TestCommandInvokesPreEditHookRejectsOtherEntireHooks: a repository with only
// the session-end hook wired must not be read as having the pre-edit hook, or
// setup would claim a delivery path nothing calls.
func TestCommandInvokesPreEditHookRejectsOtherEntireHooks(t *testing.T) {
	if commandInvokesPreEditHook("entire hooks claude-code session-end") {
		t.Fatal("the session-end hook was accepted as the pre-edit hook")
	}
	if commandInvokesPreEditHook("some-other-tool hook pre-edit") {
		t.Fatal("another tool's pre-edit hook was accepted as ours")
	}
	if !commandInvokesPreEditHook("/opt/bin/entire-brain hook pre-edit --claude-hook") {
		t.Fatal("our own hook spelled absolutely was rejected")
	}
}

// TestDecodeClaudeHookEventPath pins the field Claude Code actually delivers.
// The verb took only --file, so the wired hook would have seen no path at all.
func TestDecodeClaudeHookEventPath(t *testing.T) {
	event := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Edit",` +
		`"tool_input":{"file_path":"/repo/internal/cli/history_fts.go","old_string":"a","new_string":"b"}}`
	path, ok := hook.DecodeClaudeHookEventPath(strings.NewReader(event))
	if !ok || path != "/repo/internal/cli/history_fts.go" {
		t.Fatalf("tool_input.file_path not read: path=%q ok=%v", path, ok)
	}
	if path, ok := hook.DecodeClaudeHookEventPath(strings.NewReader(`{"tool_input":{"notebook_path":"/repo/a.ipynb"}}`)); !ok || path != "/repo/a.ipynb" {
		t.Fatalf("notebook_path not read: path=%q ok=%v", path, ok)
	}
	for _, bad := range []string{"", "   ", "not json", `{"tool_input":{}}`, `{"tool_name":"Bash"}`} {
		if path, ok := hook.DecodeClaudeHookEventPath(strings.NewReader(bad)); ok {
			t.Fatalf("%q must yield no path, got %q", bad, path)
		}
	}
}

// TestHookPreEditClaudeHookEmitsTheHostEnvelope is the other half of UX-18:
// plain stdout is NOT added to the agent's context for PreToolUse, so the text
// emission could never have reached an agent however it was wired.
func TestHookPreEditClaudeHookEmitsTheHostEnvelope(t *testing.T) {
	opts, _, brainDir := commandRegressionFixture(t)
	text := "`history_fts.go` keeps the BM25 relevance cutoff relative to the top score, not absolute."
	paths := []string{"architecture.boundaries.rationale"}
	fact := factRecord{
		ID: factRecordID(text, paths), Text: text, Paths: paths, Kind: factKindDecision,
		Branch: "main", Origin: factstore.OriginAuthored, Status: factStatusActive,
		CreatedAt: opts.Now(), UpdatedAt: opts.Now(),
	}
	if err := factstore.Write(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}

	cmd := hook.New(hookCommandConfig(opts))
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Edit",` +
		`"tool_input":{"file_path":"internal/cli/history_fts.go"}}`))
	out, err := execute(t, cmd, "pre-edit", "--claude-hook")
	if err != nil {
		t.Fatalf("pre-edit --claude-hook: %v\n%s", err, out)
	}
	var envelope struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("stdout is not the host's hook envelope: %v\n%s", err, out)
	}
	if envelope.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("hookEventName is required and must be PreToolUse, got %q", envelope.HookSpecificOutput.HookEventName)
	}
	if !strings.Contains(envelope.HookSpecificOutput.AdditionalContext, "BM25 relevance cutoff") {
		t.Fatalf("the matched fact did not reach additionalContext:\n%s", out)
	}
}

// TestHookPreEditClaudeHookStaysSilentWithoutAMatch keeps the harness-safety
// contract: an empty envelope is noise, and a pathless event is not misuse.
func TestHookPreEditClaudeHookStaysSilentWithoutAMatch(t *testing.T) {
	opts, _, brainDir := commandRegressionFixture(t)
	// A brain with facts that do NOT match. Without this the hook returns early
	// on "no facts at all" and the emission path is never reached, so the test
	// would pass however the emitter behaved.
	text := "The export cursor is advanced only after a successful flush."
	paths := []string{"architecture.data.flow"}
	if err := factstore.Write(brainDir, "main", []factRecord{{
		ID: factRecordID(text, paths), Text: text, Paths: paths, Kind: factKindDecision,
		Branch: "main", Origin: factstore.OriginAuthored, Status: factStatusActive,
		CreatedAt: opts.Now(), UpdatedAt: opts.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if facts, err := factstore.Load(brainDir, "main"); err != nil || len(facts) == 0 {
		t.Fatalf("fixture must hold facts for this test to mean anything: %d facts, err %v", len(facts), err)
	}
	cmd := hook.New(hookCommandConfig(opts))
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`))
	out, err := execute(t, cmd, "pre-edit", "--claude-hook")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("a pathless event must be silent success, got %q (err %v)", out, err)
	}

	cmd = hook.New(hookCommandConfig(opts))
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"internal/cli/nothing_matches_here.go"}}`))
	out, err = execute(t, cmd, "pre-edit", "--claude-hook")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("no matching fact must emit nothing, got %q (err %v)", out, err)
	}
}

// TestWiringTheHookIsReportedAsDoneNotSkipped pins a line that contradicted
// itself. progress.Skip appends "skipped", so reporting a hook it had just
// WRITTEN through Skip rendered "pre-edit hook wired (...) skipped" -- a line
// that both claims and denies the same action. Found by reading a live first
// run, not by any test: every assertion here passed while the line was wrong.
func TestWiringTheHookIsReportedAsDoneNotSkipped(t *testing.T) {
	f := newSetupTestFixture(t)
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	first := runSetupForTest(t, f, opts, &recordedSetup{})

	line := setupLineContaining(t, first, "pre-edit hook wired")
	if strings.Contains(line, "skipped") {
		t.Fatalf("setup wrote the hook, then called it skipped: %q", line)
	}
	// The marker moved. Under the phase renderer a wired hook is a DETAIL row
	// hanging off the AGENTS phase, and the done/skipped mark lives on the
	// phase line, not on every row. The invariant is unchanged -- setup must
	// not call work it did "skipped" -- so it is asserted where the mark now
	// is. A row-level check here would pass on any renderer that prints no
	// marks at all, which is the weaker claim.
	if mark := setupRenderedPhaseMark(t, first, "Agents"); mark == setupSkipMark {
		t.Fatalf("the agents phase wired the hook, then reported skipped: %q\n%s", mark, first)
	}

	// The other direction, asserted where the mark now lives: a second run
	// wires nothing, so the row says "already wired" and the phase says
	// skipped. Both halves still distinguish work done from work not needed;
	// only the column carrying the mark changed.
	second := runSetupForTest(t, f, opts, &recordedSetup{})
	setupLineContaining(t, second, "pre-edit hook already wired")
	if mark := setupRenderedPhaseMark(t, second, "Agents"); mark != setupSkipMark {
		t.Fatalf("a run that wired nothing must say skipped, got mark %q\n%s", mark, second)
	}
}

// setupSkipMark is the glyph the phase renderer puts on a skipped phase when
// the capability block reports no unicode, which is what tests render with.
const setupSkipMark = "-"

// setupRenderedPhaseMark returns the mark column of a phase line. The done/skipped
// distinction is a GLYPH in the phase renderer, not the word "skipped", so a
// Contains check for that word passes on every run and tests nothing.
func setupRenderedPhaseMark(t *testing.T, out, phase string) string {
	t.Helper()
	line := strings.TrimRight(setupLineContaining(t, out, phase), " ")
	trimmed := strings.TrimLeft(line, " ")
	if trimmed == "" {
		t.Fatalf("phase line for %q is blank:\n%s", phase, out)
	}
	return string([]rune(trimmed)[0])
}

// setupLineContaining returns the one output line holding want, failing when it
// is absent -- so a rename cannot turn this guard vacuous by matching nothing.
func setupLineContaining(t *testing.T, out, want string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no line containing %q in setup output:\n%s", want, out)
	return ""
}

// --claude-hook used to bypass the terminal-stdin guard, so hand-running the
// exact command the docs suggest blocked for ever on a read that never returns,
// while the comment two lines above claimed a terminal stdin is never read.
// Raised as a finding on this PR, and correct.
//
// hook.StdinIsPipe treats ANY non-*os.File reader as a pipe, so a stub cannot stand
// in for a terminal here — it would take the pipe branch and prove nothing.
// /dev/null is a real character device, which is exactly what the guard tests.
func TestClaudeHookOnATerminalSaysSoInsteadOfHanging(t *testing.T) {
	t.Parallel()
	tty, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no character device available: %v", err)
	}
	defer tty.Close()
	if hook.StdinIsPipe(tty) {
		// Not a character device here. On Windows os.DevNull is "NUL", whose
		// mode does not necessarily report ModeCharDevice, so hook.StdinIsPipe takes
		// the pipe branch and there is no terminal to simulate. Skip rather than
		// fail: the guard is about POSIX terminal stdin, and a fixture that
		// cannot represent one proves nothing either way.
		t.Skip("no character device available to stand in for a terminal on this platform")
	}

	opts, _, _ := commandRegressionFixture(t)
	cmd := hook.New(hookCommandConfig(opts))
	cmd.SetIn(tty)
	out, err := execute(t, cmd, "pre-edit", "--claude-hook")
	if err == nil {
		t.Fatalf("a terminal stdin must be refused, not read: this is the hand-run that used to hang\n%s", out)
	}
	for _, want := range []string{"stdin is a terminal", "--file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say what to do instead; missing %q in: %v", want, err)
		}
	}
}

// Between reading .claude/settings.json and renaming over it, another writer —
// Claude Code itself, or another tool editing permissions — can change the
// file, and the rename would silently drop their edit. Raised as a finding on
// this change, and correct.
//
// Simulated by changing the file during the merge, which is the window the
// finding describes.
func TestAConcurrentSettingsEditIsNotOverwritten(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	settingsDir := filepath.Join(repo, claudeSettingsDir)
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(settingsDir, claudeSettingsFile)
	if err := os.WriteFile(settings, []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Another writer lands between our read and our rename.
	original := []byte(`{"model":"opus","permissions":{"allow":["Bash(ls:*)"]}}`)
	installPreEditHookRaceHook = func() {
		_ = os.WriteFile(settings, original, 0o644)
	}
	defer func() { installPreEditHookRaceHook = nil }()

	_, changed, err := installPreEditHook(repo)
	if err == nil {
		t.Fatal("a settings file that moved under us must be refused, not overwritten")
	}
	if changed {
		t.Fatal("a refused merge must not report a change")
	}
	got, readErr := os.ReadFile(settings)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("the concurrent writer's edit was lost:\n got %s\nwant %s", got, original)
	}
}

// blockingReader never reaches EOF, which is what a pipe inherited from a
// parent that has not closed its end behaves like.
type blockingReader struct{ release chan struct{} }

func (b blockingReader) Read([]byte) (int, error) { <-b.release; return 0, io.EOF }

// --file says what to check, so there is nothing left to learn from the event.
// Reading it anyway blocked until EOF or maxClaudeHookEventBytes and then threw
// the result away, because the inner guard refuses to overwrite a given --file.
// That is the same hand-run hang the terminal-stdin check above it prevents,
// reached through a different door. Raised as a finding on this PR, and correct.
func TestAGivenFileIsNotWaitedOnByAnEventRead(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	target := filepath.Join(repo, "checked.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blocked := blockingReader{release: make(chan struct{})}
	t.Cleanup(func() { close(blocked.release) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd := hook.New(hookCommandConfig(Options{Env: EntireEnv{RepoRoot: repo}, Runner: ExecRunner{}, Now: time.Now}))
		cmd.SetIn(blocked)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"pre-edit", "--claude-hook", "--file", target})
		_ = cmd.Execute()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("--file with --claude-hook blocked on an event read whose result it discards")
	}
}

// assertPreEditHookCommand holds for the named platform regardless of the host
// this test runs on.
func assertPreEditHookCommand(t *testing.T, goos, command string) {
	t.Helper()
	if !strings.Contains(command, base.SetupEntireBinaryName+" "+base.SetupBrainPluginVerb+" hook pre-edit") {
		t.Errorf("%s: no host-CLI fallback for a managed install: %s", goos, command)
	}
	if !strings.Contains(command, base.SetupBrainBinaryName+" hook pre-edit") {
		t.Errorf("%s: never tries the brain binary directly: %s", goos, command)
	}
	if !commandInvokesPreEditHook(command) {
		t.Errorf("%s: our own idempotence check does not recognise it, so setup would rewire it every run: %s", goos, command)
	}
	if goos == "windows" {
		if strings.Contains(command, "sh -c") || strings.Contains(command, "command -v") {
			t.Errorf("windows: a POSIX shell pipeline cmd.exe cannot run: %s", command)
		}
		if !strings.HasPrefix(command, "cmd /c ") {
			t.Errorf("windows: the interpreter is not named, so this depends on how the host runs hooks: %s", command)
		}
		if !strings.Contains(command, "exit /b 0") {
			t.Errorf("windows: a missing binary fails the hook instead of exiting clean: %s", command)
		}
		return
	}
	if !strings.HasPrefix(command, "sh -c ") {
		t.Errorf("%s: the interpreter is not named: %s", goos, command)
	}
	if !strings.Contains(command, "exit 0") {
		t.Errorf("%s: a missing binary fails the hook instead of exiting clean: %s", goos, command)
	}
}

// TestAgentsPhaseCountsAgentWiringAsWork covers the half of the phase mark
// that TestWiringTheHookIsReportedAsDoneNotSkipped does not reach: in that
// fixture the session-end hook is wired, so hookWired alone already decides
// the mark. The case that matters in the wild is the other one -- a repo where
// the session-end hook is absent but setup just wrote the agent instructions.
// That run did work, and must not call itself skipped.
func TestAgentsPhaseCountsAgentWiringAsWork(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		hookWired   bool
		agents      setupAgentsState
		wantDidWork bool
	}{
		{"session-end hook alone is work", true, setupAgentsState{}, true},
		{"instructions written is work", false, setupAgentsState{Installed: true, Changed: []string{"AGENTS.md"}}, true},
		{"pre-edit hook merged is work", false, setupAgentsState{Installed: true, HookChanged: true}, true},
		{"nothing changed is not work", false, setupAgentsState{Installed: true}, false},
		{"wiring that failed is not work", false, setupAgentsState{Changed: []string{"AGENTS.md"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := setupAgentsPhaseDidWork(c.hookWired, c.agents); got != c.wantDidWork {
				t.Errorf("setupAgentsPhaseDidWork(%v, %+v) = %v, want %v", c.hookWired, c.agents, got, c.wantDidWork)
			}
		})
	}
}

// TestSetupStrictModePersistsAcrossBareReruns: `setup --strict` saves the mode
// in the guide's activation record — the job the removed init-agents command
// used to do — and a bare re-run inherits it rather than resetting to normal.
// `setup --normal` is the explicit way back.
func TestSetupStrictModePersistsAcrossBareReruns(t *testing.T) {
	f := newSetupTestFixture(t)
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true
	opts.strict = true

	runSetupForTest(t, f, opts, &recordedSetup{})
	guidePath := filepath.Join(f.repoDir, agentsetup.Path)
	if guide := readFileString(t, guidePath); !strings.Contains(guide, `"mode":"strict"`) {
		t.Fatalf("--strict did not persist in the activation record:\n%s", guide)
	}

	opts.strict = false
	runSetupForTest(t, f, opts, &recordedSetup{})
	if guide := readFileString(t, guidePath); !strings.Contains(guide, `"mode":"strict"`) {
		t.Fatalf("a bare re-run reset the saved strict mode:\n%s", guide)
	}

	opts.normal = true
	runSetupForTest(t, f, opts, &recordedSetup{})
	if guide := readFileString(t, guidePath); strings.Contains(guide, `"mode":"strict"`) {
		t.Fatalf("--normal did not reset the saved mode:\n%s", guide)
	}
}
