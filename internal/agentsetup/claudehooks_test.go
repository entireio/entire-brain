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
