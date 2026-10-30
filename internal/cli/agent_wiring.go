package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/entireio/entire-brain/internal/agentsetup"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// agent_wiring.go closes the first-run gap that made a successful `setup`
// invisible: the brain was built, indexed, watched and backfilled, and NO AGENT
// EVER KNEW IT EXISTED. `setup` originally wrote no guide, no pointer and no
// hook; a user who ran only `setup` got a brain that every agent in the
// repository would walk straight past. This wiring is now a setup phase (the
// former `init-agents` command, folded in).
//
// Two things make an agent aware of the brain:
//
//  1. INSTRUCTIONS. agentsetup.Install writes .entire/agent-guide.md and a
//     marker-delimited pointer block in AGENTS.md / CLAUDE.md. Those three
//     files are the whole of it — the installer is scoped to them, writes
//     through an os.Root so a committed symlink cannot redirect it, and
//     replaces its own block in place, so re-running is a no-op.
//
//  2. DELIVERY AT THE MOMENT OF ACTION. `hook pre-edit` surfaces facts anchored
//     to the file an agent is about to change, but nothing wired it: the verb
//     existed and no harness ever called it. Merging a PreToolUse entry into
//     the repository's .claude/settings.json is what turns it on.
//
// Both are repository-local and cost nothing at rest. Neither is the daemon:
// nothing here registers a service, survives a reboot, or spends a token.

// setupAgentsState reports what agent wiring this run found or installed. It is
// a sibling of setupHookState (the session-end hook's report) and is reported
// the same way: a failure here is a warning, never a failed setup.
type setupAgentsState struct {
	// Installed is true when the repository now carries the agent guide.
	Installed bool `json:"installed"`
	// Files are the managed instruction files that exist after this run,
	// repository-relative, so a reader can see the pointer for themselves.
	Files []string `json:"files,omitempty"`
	// Changed are the files this run actually wrote. An empty Changed with
	// Installed true is the idempotent re-run: everything was already correct.
	Changed []string `json:"changed,omitempty"`
	// Hook is the Claude Code PreToolUse settings file that invokes
	// `hook pre-edit`, when one is wired.
	Hook string `json:"pre_edit_hook,omitempty"`
	// HookChanged is true when this run added the PreToolUse entry.
	HookChanged bool   `json:"pre_edit_hook_changed,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// agentWiringTargets are the repository-relative instruction files
// agentsetup.Install manages, in report order.
var agentWiringTargets = []string{agentsetup.Path, "AGENTS.md", "CLAUDE.md"}

// wireAgentInstructions installs the agent guide and pointer blocks, and —
// unless declined — the PreToolUse hook. It never returns an error: every
// failure is carried in Detail so the caller can downgrade it to a warning.
//
// It is safe to run unattended and safe to run repeatedly. Both writers are
// idempotent by construction rather than by luck: the instruction files carry
// a begin/end marker whose contents are replaced in place, and the settings
// merge is skipped outright when an entry already invokes this hook.
//
// An empty mode inherits the repository's saved guidance mode; --strict and
// --normal pass an explicit one, which the install persists in the guide's
// activation record.
func wireAgentInstructions(repoDir string, wireHook bool, mode agentsetup.Mode) setupAgentsState {
	state := setupAgentsState{}
	render := func() (string, error) {
		return agentsetup.Preview(repoDir, "brain", agentsetup.Options{Mode: mode})
	}
	changed, err := agentsetup.InstallChanged(repoDir, render)
	if err != nil {
		state.Detail = err.Error()
		return state
	}
	state.Installed = true
	state.Changed = changed
	for _, name := range agentWiringTargets {
		if _, err := os.Stat(filepath.Join(repoDir, name)); err == nil {
			state.Files = append(state.Files, name)
		}
	}
	if !wireHook {
		return state
	}
	path, hookChanged, hookErr := installPreEditHook(repoDir)
	if hookErr != nil {
		// The guide landed; only the hook did not. Say which, rather than
		// reporting the whole step as failed.
		state.Detail = "pre-edit hook not wired: " + hookErr.Error()
		return state
	}
	state.Hook = path
	state.HookChanged = hookChanged
	return state
}

// claudeSettingsDir / claudeSettingsFile locate the repository-local Claude Code
// settings file. Project settings MERGE with the user's ~/.claude/settings.json
// rather than replacing it, so adding an entry here cannot cost the reader a
// hook they configured themselves.
const (
	claudeSettingsDir  = ".claude"
	claudeSettingsFile = "settings.json"
)

// preEditHookMatcher names the tools whose invocation should first consult the
// brain. A matcher made only of letters, digits, '_', '-', spaces, ',' and '|'
// is an exact list rather than a regex, so these four tool names match exactly
// themselves and nothing else.
const preEditHookMatcher = "Edit|Write|MultiEdit|NotebookEdit"

// maxClaudeSettingsBytes bounds the one repository-authored JSON file this
// command reads. Its path is fixed but its SIZE is chosen by the repository,
// and `setup` runs against clones the operator has not read — the same reason
// agentsetup bounds AGENTS.md, and the same ceiling.
const maxClaudeSettingsBytes = 4 << 20

// preEditHookCommand is what the PreToolUse entry runs.
//
// It cannot simply be "entire-brain hook pre-edit": on a managed install the
// binary lives in <xdg_data>/entire/plugins/bin, a directory the Entire CLI
// prepends to PATH inside its OWN process and nowhere else, so the reader who
// types `entire brain setup` has no `entire-brain` on PATH at all. The command
// therefore tries the standalone binary, then the host CLI's plugin spelling,
// and exits 0 when neither is installed — a hook that prints "command not
// found" on every edit gets deleted within a day, and the hook contract is that
// an environment problem is silence, never noise.
func preEditHookCommand() string { return preEditHookCommandFor(runtime.GOOS) }

// preEditHookCommandFor takes the platform as an argument so BOTH branches are
// reachable from a test on any host. The windows branch shipped wrong because
// nothing here could execute it: the only guard that ran was the POSIX one.
func preEditHookCommandFor(goos string) string {
	if goos == "windows" {
		// cmd.exe has no `command -v`, so the POSIX pipeline cannot run here --
		// but the fallback is still needed. This used to wire the bare spelling
		// and justify it with "windows has no managed-plugin PATH problem,
		// because the daemon is not supported there". That conflated two
		// different things: the daemon is unsupported on windows, but the
		// managed PLUGIN install is not. setupCommandPrefix detects it from an
		// environment marker with no GOOS condition at all, and windows release
		// binaries ship, so a windows reader with a managed install got a hook
		// that failed on every edit.
		//
		// cmd /c is the direct analogue of the sh -c below: naming the
		// interpreter explicitly means this does not depend on whether the host
		// runs hook commands through a shell. `||` is cmd.exe's run-on-failure
		// operator, and a missing command exits 9009, so the chain advances.
		// `exit /b 0` is the counterpart of the POSIX `exit 0`: a reader with
		// neither binary gets a hook that does nothing, not one that errors on
		// every edit.
		return `cmd /c "` + base.SetupBrainBinaryName + " hook pre-edit --claude-hook || " +
			base.SetupEntireBinaryName + " " + base.SetupBrainPluginVerb + ` hook pre-edit --claude-hook || exit /b 0"`
	}
	// `command -v` establishes only that SOMETHING by that name is on PATH --
	// never that it is a build which understands this hook. The first spelling
	// used to run under `exec`, which replaces the shell, so a binary that was
	// present but failed took the whole hook down and the plugin fallback below
	// became unreachable. That is not hypothetical: a v0.1.0 `entire-brain`
	// left in ~/.local/bin shadowed a managed v0.4.0 plugin on this machine and
	// exited 1 on every single edit for eight days, because it predates
	// --claude-hook. The windows branch above chains with `||` and would have
	// recovered; only the POSIX branch could not.
	//
	// So: no `exec`, and the chain advances on failure, exactly as windows
	// does. stderr of the first attempt is dropped because its failure is
	// RECOVERABLE -- the fallback is about to answer, and a wrong-version
	// binary must not narrate that on every edit. The last attempt keeps its
	// stderr, so a genuine fault stays diagnosable under `claude --debug`.
	//
	// The trailing `exit 0` is now unconditional, and that is the second half
	// of the fix. A PreToolUse hook that exits non-zero reports a problem to
	// the agent on every edit; memory delivery failing is never a reason to
	// interrupt the edit it was trying to inform.
	plugin := base.SetupEntireBinaryName + " " + base.SetupBrainPluginVerb
	return "sh -c 'if command -v " + base.SetupBrainBinaryName + " >/dev/null 2>&1 && " + base.SetupBrainBinaryName +
		" hook pre-edit --claude-hook 2>/dev/null; then exit 0; fi; if command -v " + base.SetupEntireBinaryName +
		" >/dev/null 2>&1; then " + plugin + " hook pre-edit --claude-hook; fi; exit 0'"
}

// errSettingsShape reports a settings file whose `hooks` tree is not the shape
// the host documents. Refusing is the point: the alternative is overwriting a
// structure we did not understand.
var errSettingsShape = errors.New("unexpected shape")

// installPreEditHook merges one PreToolUse entry into the repository's
// .claude/settings.json, preserving every other key byte-for-value.
//
// The merge is a map round-trip, NOT an unmarshal into a typed struct: the
// settings file is the reader's, it carries keys this binary has never heard
// of (env, permissions, model, statusLine, other tools' hooks), and decoding it
// into a struct that knows only `hooks` would silently delete all of them on
// write. Unknown keys survive because they are never interpreted.
//
// Returns the settings path, whether this call changed it, and an error only
// when the file exists and cannot be understood — never when it is simply
// absent.
// installPreEditHookRaceHook is nil outside tests.
var installPreEditHookRaceHook func()

func installPreEditHook(repoDir string) (string, bool, error) {
	path := filepath.Join(repoDir, claudeSettingsDir, claudeSettingsFile)
	// Everything below goes through an os.Root: `.claude/settings.json` is a
	// repository-committed path like AGENTS.md, and a symlink there must not be
	// able to redirect this write outside the repository.
	root, err := os.OpenRoot(repoDir)
	if err != nil {
		return path, false, err
	}
	defer root.Close()
	name := claudeSettingsDir + "/" + claudeSettingsFile

	settings := map[string]any{}
	existing, err := readBoundedRootFile(root, name, maxClaudeSettingsBytes)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(existing))) > 0 {
			if err := json.Unmarshal(existing, &settings); err != nil {
				return path, false, fmt.Errorf("%s: %w", path, err)
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// A repository with no Claude settings is the common first run.
	default:
		return path, false, fmt.Errorf("%s: %w", path, err)
	}

	hooks, err := settingsObject(settings, "hooks")
	if err != nil {
		return path, false, fmt.Errorf("%s: hooks: %w", path, err)
	}
	entries, err := settingsArray(hooks, "PreToolUse")
	if err != nil {
		return path, false, fmt.Errorf("%s: hooks.PreToolUse: %w", path, err)
	}
	if preToolUseInvokesPreEdit(entries) {
		return path, false, nil
	}
	entries = append(entries, map[string]any{
		"matcher": preEditHookMatcher,
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": preEditHookCommand(),
		}},
	})
	hooks["PreToolUse"] = entries
	settings["hooks"] = hooks

	// An encoder with HTML escaping OFF, not json.MarshalIndent.
	//
	// The merge's contract is that it preserves every other key byte-for-value,
	// and json.MarshalIndent breaks it: it escapes <, > and & as \u003c, \u003e
	// and \u0026 for safety inside a <script> tag, which this file is never
	// read in. Every hook command in a real settings file contains
	// `>/dev/null 2>&1`, so merging one entry rewrote SEVEN unrelated commands
	// into \u003e/dev/null 2\u003e\u00261. The JSON is equivalent and the hooks
	// still run, so nothing breaks -- which is why it went unnoticed. What it
	// costs is the reader's diff: a one-line addition arrived as 58 insertions
	// and 49 deletions, in a file they are expected to review and commit.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(settings); err != nil {
		return path, false, err
	}
	// Encode already terminates with a newline.
	encoded := buf.Bytes()
	if err := root.MkdirAll(claudeSettingsDir, 0o755); err != nil {
		return path, false, err
	}
	// Keep the mode the reader chose. A settings file they deliberately made
	// 0600 must not come back 0644 because we rewrote it.
	mode := os.FileMode(0o644)
	if info, err := root.Stat(name); err == nil {
		mode = info.Mode().Perm()
	}
	// Write-then-rename so a crash cannot leave the reader with a truncated
	// settings file — the one file whose corruption would break their whole
	// agent harness, not just ours. The temp name carries randomness because
	// two repositories can be set up concurrently and a fixed name would let
	// one run's partial write become the other's source.
	tmp := claudeSettingsDir + "/.settings.json.entire-brain." + randomSuffix() + ".tmp"
	if err := root.WriteFile(tmp, encoded, mode); err != nil {
		return path, false, err
	}
	// Re-read before swapping. Between the read above and this rename another
	// writer -- Claude Code itself, or another tool editing permissions -- can
	// have changed the file, and renaming over it would silently drop their
	// edit. Raised as a finding on this change.
	//
	// Optimistic rather than locked: there is no lock protocol shared with the
	// other writers of this file, so inventing one here would protect nothing.
	// Comparing what we read against what is there now closes the window we
	// actually opened, and refuses rather than overwriting when it has moved.
	// Test seam: lets a guard land a concurrent write in exactly the window
	// between the read above and the rename below.
	if installPreEditHookRaceHook != nil {
		installPreEditHookRaceHook()
	}
	switch current, rerr := readBoundedRootFile(root, name, maxClaudeSettingsBytes); {
	case rerr == nil && !bytes.Equal(current, existing):
		_ = root.Remove(tmp)
		return path, false, fmt.Errorf("%s changed while the pre-edit hook was being merged; nothing was written — re-run to merge against the current file", path)
	case rerr != nil && !errors.Is(rerr, os.ErrNotExist):
		_ = root.Remove(tmp)
		return path, false, fmt.Errorf("%s: %w", path, rerr)
	case rerr != nil && len(existing) > 0:
		// It existed when read and does not now: someone removed it. Writing
		// our merge back would resurrect settings they just deleted.
		_ = root.Remove(tmp)
		return path, false, fmt.Errorf("%s was removed while the pre-edit hook was being merged; nothing was written", path)
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return path, false, err
	}
	return path, true, nil
}

// randomSuffix returns a short unguessable token for a temporary file name.
func randomSuffix() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice, and a fixed suffix is still
		// correct for the single-writer case — it only loses the concurrency
		// guarantee, so it must not fail the install.
		return "fallback"
	}
	return hex.EncodeToString(buf[:])
}

// readBoundedRootFile reads at most limit bytes through root and refuses a file
// that is larger, so an untrusted settings file cannot be read to EOF.
func readBoundedRootFile(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file is larger than %d bytes", limit)
	}
	return data, nil
}

// settingsObject returns settings[key] as an object, creating it when absent and
// refusing when it is present but not an object.
func settingsObject(settings map[string]any, key string) (map[string]any, error) {
	value, ok := settings[key]
	if !ok || value == nil {
		return map[string]any{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errSettingsShape
	}
	return object, nil
}

// settingsArray returns object[key] as an array, empty when absent and refusing
// when it is present but not an array.
func settingsArray(object map[string]any, key string) ([]any, error) {
	value, ok := object[key]
	if !ok || value == nil {
		return nil, nil
	}
	array, ok := value.([]any)
	if !ok {
		return nil, errSettingsShape
	}
	return array, nil
}

// preToolUseInvokesPreEdit reports whether any existing PreToolUse entry already
// runs this binary's pre-edit hook, however it is spelled — bare, absolute, or
// wrapped in a shell line. This is what makes a second `setup` a no-op instead
// of appending a duplicate entry on every run.
//
// It deliberately does NOT compare against preEditHookCommand(): a reader who
// adapted the command (a different matcher, an absolute path, their own
// wrapper) still has the hook wired, and re-adding ours beside theirs would run
// it twice.
func preToolUseInvokesPreEdit(entries []any) bool {
	for _, entry := range entries {
		matcher, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := matcher["hooks"].([]any)
		if !ok {
			continue
		}
		for _, item := range inner {
			hook, ok := item.(map[string]any)
			if !ok {
				continue
			}
			command, _ := hook["command"].(string)
			if commandInvokesPreEditHook(command) {
				return true
			}
		}
	}
	return false
}

// commandInvokesPreEditHook recognises OUR pre-edit hook and rejects another
// tool's. commandInvokesEntireHook alone is not enough: it accepts any Entire
// hook verb, so a repository with only the session-end hook wired would be
// reported as having the pre-edit hook too, and `setup` would claim a delivery
// path that nothing would ever call.
func commandInvokesPreEditHook(command string) bool {
	if !strings.Contains(strings.ToLower(command), "pre-edit") {
		return false
	}
	return commandInvokesEntireHook(command)
}
