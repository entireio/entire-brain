package agentsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSettings(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings is not valid JSON: %v\n%s", err, data)
	}
	return m
}

func writeSettings(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPreEditHookIsWiredIntoAFreshRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	changed, err := MergePreEditHook(root, "entire brain")
	if err != nil || !changed {
		t.Fatalf("fresh repo: changed=%v err=%v", changed, err)
	}
	hooks, _ := readSettings(t, root)["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	if !preEditHookPresent(pre) {
		t.Fatalf("hook not present after install: %+v", pre)
	}
	// It must fire on direct edits. The entries `entire enable` writes use
	// "Agent" only, which never fires at the moment a fact about this file
	// would change the edit.
	group := pre[0].(map[string]any)
	if m, _ := group["matcher"].(string); !strings.Contains(m, "Edit") || !strings.Contains(m, "Write") {
		t.Fatalf("matcher %q does not cover direct file edits", m)
	}
}

// Brain does not own this file. Clobbering another product's hooks to install
// our own would be a worse bug than the missing hook.
func TestPreEditHookMergeNeverClobbersExistingSettings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSettings(t, root, `{
  "permissions": {"allow": ["Bash(git status)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Agent", "hooks": [{"type": "command", "command": "entire hooks pre-agent"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "entire hooks stop"}]}
    ]
  }
}`)

	if _, err := MergePreEditHook(root, "entire brain"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got := readSettings(t, root)

	if _, ok := got["permissions"]; !ok {
		t.Fatal("an unrelated top-level key was dropped")
	}
	hooks := got["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; !ok {
		t.Fatal("an unrelated hook event was dropped")
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("want the existing entry plus ours, got %d: %+v", len(pre), pre)
	}
	first := pre[0].(map[string]any)
	if m, _ := first["matcher"].(string); m != "Agent" {
		t.Fatalf("the pre-existing entry was modified: %+v", first)
	}
}

// init-agents is run repeatedly. Duplicate hooks would fire the command twice
// per edit and double the token cost.
func TestPreEditHookMergeIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if _, err := MergePreEditHook(root, "entire brain"); err != nil {
		t.Fatal(err)
	}
	changed, err := MergePreEditHook(root, "entire brain")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second run reported a change; the hook would accumulate")
	}
	hooks := readSettings(t, root)["hooks"].(map[string]any)
	if pre, _ := hooks["PreToolUse"].([]any); len(pre) != 1 {
		t.Fatalf("want 1 entry after two runs, got %d", len(pre))
	}
}

// Idempotency keys on the command, so a hand-written or differently-flagged
// invocation still counts as present and is not duplicated.
func TestPreEditHookMergeRespectsAHandWrittenEquivalent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSettings(t, root, `{"hooks": {"PreToolUse": [
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "entire brain hook pre-edit --file \"$F\" --budget 50"}]}
    ]}}`)

	changed, err := MergePreEditHook(root, "entire brain")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("overrode a hand-written equivalent hook")
	}
}

// Unparseable or unexpected content is reported, never overwritten.
func TestPreEditHookMergeRefusesRatherThanOverwrite(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"invalid json":     `{"hooks": `,
		"hooks not object": `{"hooks": "nope"}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSettings(t, root, body)
			changed, err := MergePreEditHook(root, "entire brain")
			if err == nil {
				t.Fatal("want an error naming the problem")
			}
			if changed {
				t.Fatal("reported a change while refusing")
			}
			data, _ := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
			if string(data) != body {
				t.Fatalf("the file was modified despite refusing:\n%s", data)
			}
		})
	}
}

// A raw os.WriteFile here wrote 255 bytes OUTSIDE the repository when
// `.claude` is a symlink to another directory -- measured before the fix. Every
// other write in this package goes through an os.Root for exactly this reason
// (files.go:74), and the hook write did not.
//
// ATTRIBUTION, measured rather than assumed. There are three contained calls
// here -- the read, the MkdirAll and the write -- and the measured result is:
//
//	all three reverted  -> this test and the read test both go RED
//	any ONE kept        -> still refused, both tests green
//
// So no single call is NECESSARY for this fixture and each is SUFFICIENT. That
// is defence in depth rather than three independent guards, and it is worth
// stating plainly: a sweep that mutates one call at a time will report all
// three as unheld, which is how a real escape could later be introduced by
// removing them together. The decisive check is reverting all three.
func TestPreEditHookWriteCannotEscapeTheRepository(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".claude")); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}
	// Non-vacuity: the symlink must actually be there and point out of the
	// repo, or a refusal proves nothing.
	if info, err := os.Lstat(filepath.Join(root, ".claude")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fixture is not a symlink (err=%v)", err)
	}

	changed, err := MergePreEditHook(root, "entire-brain")
	if err == nil {
		t.Error("a settings path that escapes the repository must be refused")
	}
	if changed {
		t.Error("nothing was legitimately changed, so changed must be false")
	}
	if data, readErr := os.ReadFile(filepath.Join(outside, "settings.json")); readErr == nil {
		t.Errorf("wrote %d bytes outside the repository", len(data))
	}
}

// Reading through an escaping symlink would merge a settings file from outside
// the repository and write the result back, so containment covers both ends.
//
// This is the test that pins the contained READ specifically: reverting it to
// os.ReadFile turns the clobber test red, and leaves the outside file as the
// source of the merge.
func TestPreEditHookDoesNotMergeSettingsFromOutsideTheRepository(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "settings.json"),
		[]byte(`{"marker":"FROM OUTSIDE THE REPOSITORY"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".claude")); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}

	if _, err := MergePreEditHook(root, "entire-brain"); err == nil {
		t.Fatal("reading settings through an escaping symlink must be refused")
	}
	// And the outside file must be exactly as it was.
	data, err := os.ReadFile(filepath.Join(outside, "settings.json"))
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(data) != `{"marker":"FROM OUTSIDE THE REPOSITORY"}` {
		t.Errorf("the outside file was rewritten: %s", data)
	}
}

// The hook command must be the RESOLVED invocation, not a hardcoded one.
// `entire brain` is correct only with a host entire CLI on PATH; a standalone
// install needs `entire-brain`, and the wrong one is a command that does not
// exist, failing silently on every edit.
func TestPreEditHookUsesTheGivenInvocation(t *testing.T) {
	t.Parallel()

	for _, brainCmd := range []string{"entire-brain", "entire brain"} {
		root := t.TempDir()
		if _, err := MergePreEditHook(root, brainCmd); err != nil {
			t.Fatalf("merge with %q: %v", brainCmd, err)
		}
		data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		want := brainCmd + " hook pre-edit"
		if !strings.Contains(string(data), want) {
			t.Errorf("settings must carry %q, got:\n%s", want, data)
		}
	}
}

// `pre, _ := hooks["PreToolUse"].([]any)` discarded the type assertion's
// second value, so a PreToolUse that existed but was not an array became nil
// and the append wrote a brand-new one-element array OVER it -- destroying the
// user's configuration.
//
// This function's doc comment promises the opposite, and claudeHooksSection
// already makes this check one level up for "hooks". The promise held for the
// container and not for the key being modified.
func TestPreEditHookRefusesAnUnrecognisedPreToolUseInsteadOfOverwritingIt(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"object": `{"hooks":{"PreToolUse":{"matcher":"Edit"}}}`,
		"string": `{"hooks":{"PreToolUse":"Edit"}}`,
		"number": `{"hooks":{"PreToolUse":7}}`,
		"bool":   `{"hooks":{"PreToolUse":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			settings := filepath.Join(root, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(settings, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			changed, err := MergePreEditHook(root, "entire-brain")
			if err == nil {
				t.Error("an unrecognised PreToolUse must be refused, not replaced")
			}
			if changed {
				t.Error("nothing was installed, so changed must be false")
			}
			// The decisive assertion: the file is BYTE-IDENTICAL.
			after, readErr := os.ReadFile(settings)
			if readErr != nil {
				t.Fatalf("read back: %v", readErr)
			}
			if string(after) != raw {
				t.Errorf("the user's configuration was rewritten:\n before: %s\n after:  %s", raw, after)
			}
		})
	}
}

// Absent or null is not an unrecognised shape: there is nothing to preserve,
// so the hook installs normally. Without this the refusal above would block
// every fresh install.
func TestPreEditHookStillInstallsWhenPreToolUseIsAbsentOrNull(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"no hooks key": `{}`,
		"empty hooks":  `{"hooks":{}}`,
		"null key":     `{"hooks":{"PreToolUse":null}}`,
		"empty array":  `{"hooks":{"PreToolUse":[]}}`,
		"other hooks":  `{"hooks":{"PostToolUse":[{"matcher":"Edit"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			settings := filepath.Join(root, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(settings, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			changed, err := MergePreEditHook(root, "entire-brain")
			if err != nil {
				t.Fatalf("a fresh or empty PreToolUse must install: %v", err)
			}
			if !changed {
				t.Error("the hook was not installed")
			}
			after, readErr := os.ReadFile(settings)
			if readErr != nil {
				t.Fatalf("read back: %v", readErr)
			}
			if !strings.Contains(string(after), "hook pre-edit") {
				t.Errorf("the hook is missing from the merged settings:\n%s", after)
			}
			// A sibling key must survive the merge.
			if strings.Contains(raw, "PostToolUse") && !strings.Contains(string(after), "PostToolUse") {
				t.Errorf("a sibling hook key was lost:\n%s", after)
			}
		})
	}
}
