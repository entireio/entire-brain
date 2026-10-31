package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wiredSettings is the shape of the hooks tree these tests read back.
type wiredSettings struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func readWiredSettings(t *testing.T, repo string) wiredSettings {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, claudeSettingsDir, claudeSettingsFile))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings wiredSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings are not valid JSON: %v\n%s", err, raw)
	}
	return settings
}

func countVerbEntries(settings wiredSettings, event, verb string) int {
	n := 0
	for _, matcher := range settings.Hooks[event] {
		for _, hook := range matcher.Hooks {
			if commandInvokesHookVerb(hook.Command, verb) {
				n++
			}
		}
	}
	return n
}

// post-failure is the capture half of the loop: pre-edit reads memories back,
// `review --remember` writes them, and post-failure is what surfaces a recorded
// dead end at the moment an agent walks into it. It was built and left wired
// nowhere, which makes it dead code with tests.
func TestSetupWiresThePostFailureHookToPostToolUseFailure(t *testing.T) {
	repo := t.TempDir()
	if _, changed, err := installBrainHooks(repo); err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	settings := readWiredSettings(t, repo)

	if got := countVerbEntries(settings, "PostToolUseFailure", "post-failure"); got != 1 {
		t.Fatalf("PostToolUseFailure carries %d post-failure entries, want exactly 1; hooks=%v", got, settings.Hooks)
	}
	// And it must NOT have been filed under PostToolUse. The envelope is keyed
	// by event name and a host drops a mismatched key without a word, so the
	// wrong event here is an installed, inert hook.
	if got := countVerbEntries(settings, "PostToolUse", "post-failure"); got != 0 {
		t.Errorf("post-failure was wired to PostToolUse (%d entries); that event fires on success too and reports outcomes in tool_response", got)
	}
	for _, matcher := range settings.Hooks["PostToolUseFailure"] {
		if !strings.Contains(matcher.Matcher, "Bash") {
			t.Errorf("matcher %q omits Bash, the case the verb exists for -- and a matcher of only file-writing tools is never forwarded to remote workers", matcher.Matcher)
		}
	}
}

// THE REGRESSION THIS PINS: every repository set up before post-failure existed
// already has a pre-edit entry. If the merge short-circuits on "some brain hook
// is present" rather than checking each verb, none of those repositories ever
// gets the second hook, and `setup` reports success while wiring nothing.
func TestSetupAddsPostFailureToARepositoryThatAlreadyHasPreEdit(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, claudeSettingsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Exactly what an older `setup` left behind: pre-edit wired, nothing else.
	legacy := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": preEditHookMatcher,
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": preEditHookCommand(),
				}},
			}},
		},
	}
	raw, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, claudeSettingsDir, claudeSettingsFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	_, changed, err := installBrainHooks(repo)
	if err != nil {
		t.Fatalf("install over a legacy settings file: %v", err)
	}
	if !changed {
		t.Fatal("a repository with only pre-edit wired was reported unchanged, so post-failure never lands there")
	}
	settings := readWiredSettings(t, repo)
	if got := countVerbEntries(settings, "PostToolUseFailure", "post-failure"); got != 1 {
		t.Errorf("post-failure entries = %d, want 1", got)
	}
	// The pre-edit entry it already had must not be duplicated.
	if got := countVerbEntries(settings, "PreToolUse", "pre-edit"); got != 1 {
		t.Errorf("pre-edit entries = %d, want 1 (the existing one, not a second)", got)
	}
	// And the reader's unrelated key survives.
	var full map[string]any
	rawAfter, err := os.ReadFile(filepath.Join(repo, claudeSettingsDir, claudeSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawAfter, &full); err != nil {
		t.Fatal(err)
	}
	if full["model"] != "opus" {
		t.Errorf("the reader's own keys did not survive the merge: %v", full)
	}
}

// A second setup must be a no-op for BOTH verbs, or every run appends a
// duplicate and the agent gets each memory twice.
func TestSetupIsIdempotentForBothWiredHooks(t *testing.T) {
	repo := t.TempDir()
	if _, _, err := installBrainHooks(repo); err != nil {
		t.Fatal(err)
	}
	before := readWiredSettings(t, repo)
	if _, changed, err := installBrainHooks(repo); err != nil || changed {
		t.Fatalf("a second install reported changed=%v err=%v, want false/nil", changed, err)
	}
	after := readWiredSettings(t, repo)
	for _, wired := range brainWiredHooks() {
		if got, want := countVerbEntries(after, wired.event, wired.verb), countVerbEntries(before, wired.event, wired.verb); got != want {
			t.Errorf("%s/%s entries went %d -> %d across two installs", wired.event, wired.verb, want, got)
		}
		if got := countVerbEntries(after, wired.event, wired.verb); got != 1 {
			t.Errorf("%s/%s has %d entries, want 1", wired.event, wired.verb, got)
		}
	}
}

// Every wired verb needs a runnable command on both platforms. The windows
// branch of the pre-edit command shipped wrong once because nothing could
// execute it; this covers the new verb on the same terms.
func TestPostFailureHookCommandIsRunnableOnBothPlatforms(t *testing.T) {
	t.Parallel()

	for _, goos := range []string{"linux", "darwin", "windows"} {
		command := postFailureHookCommandFor(goos)
		if !strings.Contains(command, "hook post-failure --claude-hook") {
			t.Errorf("%s: does not invoke the wired verb: %s", goos, command)
		}
		if !commandInvokesHookVerb(command, "post-failure") {
			t.Errorf("%s: our own idempotence check does not recognise it, so setup would rewire it every run: %s", goos, command)
		}
		// A missing binary must exit clean. A hook that errors every time an
		// unrelated command fails is a hook that gets deleted, and it would
		// add noise at exactly the moment the agent is already recovering.
		if goos == "windows" {
			if !strings.Contains(command, "exit /b 0") {
				t.Errorf("windows: a missing binary fails the hook instead of exiting clean: %s", command)
			}
			if strings.Contains(command, "sh -c") || strings.Contains(command, "command -v") {
				t.Errorf("windows: a POSIX shell pipeline cmd.exe cannot run: %s", command)
			}
		} else if !strings.Contains(command, "exit 0") {
			t.Errorf("%s: a missing binary fails the hook instead of exiting clean: %s", goos, command)
		}
	}
}

// The two verbs must not be mistaken for one another, in either direction, or
// the merge either skips a hook it should add or adds one twice.
func TestHookVerbRecognitionDoesNotConfuseTheTwoVerbs(t *testing.T) {
	t.Parallel()

	preEdit := preEditHookCommandFor("linux")
	postFailure := postFailureHookCommandFor("linux")

	if commandInvokesHookVerb(preEdit, "post-failure") {
		t.Error("the pre-edit command was read as post-failure, so post-failure would never be wired")
	}
	if commandInvokesHookVerb(postFailure, "pre-edit") {
		t.Error("the post-failure command was read as pre-edit, so pre-edit would never be wired")
	}
	// A hyphen is not a word boundary, so a longer verb must not match a
	// shorter one it merely starts with.
	if commandInvokesHookVerb("entire-brain hook post-failure-summary --claude-hook", "post-failure") {
		t.Error("a longer verb was read as post-failure; setup would skip wiring the real one")
	}
	// Another tool's identically-named verb is still not ours.
	if commandInvokesHookVerb("some-other-tool hook post-failure", "post-failure") {
		t.Error("another tool's post-failure hook was accepted as ours")
	}
	if !commandInvokesHookVerb("/opt/bin/entire-brain hook post-failure --claude-hook", "post-failure") {
		t.Error("our own hook spelled absolutely was rejected")
	}
}
