package agentsetup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// `entire brain hook pre-edit` has shipped since PR #24 and NOTHING invokes it.
// It is the only push path in a product that is otherwise entirely pull: every
// other surface requires the agent to know to ask, which is exactly what the
// measured 0.34% tool-use rate says it does not do. A hook fires whether or not
// the agent thought of it.
//
// Two things make this safe to wire by default. The hook's own contract is
// silent-and-never-fails: no relevant fact means no output and exit 0, and any
// data problem is silence rather than an error, so a harness cannot be broken
// by it. And the emission is token-budgeted, so it cannot flood a context.
const (
	claudeSettingsPath = ".claude/settings.json"

	// preEditMatcher names the tools that edit files. The existing PreToolUse
	// entries written by `entire enable` use "Agent" only, which never fires
	// for a direct edit -- the moment a fact about this file is worth having.
	preEditMatcher = "Edit|Write|MultiEdit|NotebookEdit"
)

// preEditHookCommand is matched as a substring for idempotency, so it must stay
// stable even if flags are added around it.
const preEditHookCommand = "hook pre-edit"

// MergePreEditHook adds the pre-edit hook to .claude/settings.json without
// disturbing anything already there.
//
// Brain does not own this file -- `entire enable` writes it, and a user may
// have hand-edited it. So this MERGES: it parses what is there, appends one
// entry if an equivalent is absent, and writes the rest back untouched. It
// never rewrites, reorders, or reformats another product's entries, and it is
// idempotent, so running init-agents repeatedly cannot accumulate duplicates.
//
// Any shape it does not recognise is left strictly alone and reported, rather
// than overwritten: silently replacing a user's hook config to install our own
// would be a worse bug than the missing hook.
// readHookSettings reads the settings file through the os.Root, bounded, and
// maps the result to the shape the caller's switch expects.
//
// It does not use readContainedFile: that helper runs resolveContainedName,
// which only admits agent-instruction files. See the note at the write.
func readHookSettings(repoRoot *os.Root, name string) ([]byte, error) {
	f, err := repoRoot.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxClaudeSettingsBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxClaudeSettingsBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes; refusing to rewrite it", claudeSettingsPath, maxClaudeSettingsBytes)
	}
	return content, nil
}

// maxClaudeSettingsBytes bounds the contained read. A settings file larger
// than this is not one to parse and rewrite in place; refusing beats loading an
// unbounded file into memory to merge one hook entry into it.
const maxClaudeSettingsBytes = 4 << 20

func MergePreEditHook(root, brainCmd string) (changed bool, err error) {
	if root == "" {
		return false, nil
	}

	// Every write into the repository tree goes through an os.Root, like the
	// rest of this package (files.go:74). A raw os.WriteFile here was measured
	// writing 255 bytes OUTSIDE the repository when `.claude` is a symlink to
	// another directory. os.Root resolves each component with openat relative
	// to the opened directory, so a link that stays inside is still followed
	// while one that escapes is refused.
	//
	// The READ goes through it too: reading through an escaping symlink would
	// merge a settings file from outside the repository and write the result
	// back, so containment has to cover both ends.
	repoRoot, err := os.OpenRoot(root)
	if err != nil {
		return false, fmt.Errorf("open repository root: %w", err)
	}
	defer repoRoot.Close()

	name := filepath.FromSlash(claudeSettingsPath)

	settings := map[string]any{}
	existing, readErr := readHookSettings(repoRoot, name)
	switch {
	case readErr == nil:
		if len(existing) > 0 {
			if err := json.Unmarshal(existing, &settings); err != nil {
				return false, fmt.Errorf("%s is not valid JSON, leaving it untouched: %w", claudeSettingsPath, err)
			}
		}
	case os.IsNotExist(readErr):
		// A fresh repo: we create the file.
	default:
		return false, fmt.Errorf("read %s: %w", claudeSettingsPath, readErr)
	}

	hooks, err := claudeHooksSection(settings)
	if err != nil {
		return false, err
	}
	pre, _ := hooks["PreToolUse"].([]any)
	if preEditHookPresent(pre) {
		return false, nil
	}

	hooks["PreToolUse"] = append(pre, map[string]any{
		"matcher": preEditMatcher,
		"hooks": []any{map[string]any{
			"type": "command",
			// No --file: the harness delivers tool_input.file_path on STDIN,
			// and there is no CLAUDE_FILE_PATH variable to expand. Passing one
			// expanded to "" on every edit, so the hook errored every time and
			// served nothing -- installed, and a no-op. Verified against
			// Claude Code 2.1.288; see hookFileFromStdin.
			"command": brainCmd + " " + preEditHookCommand,
		}},
	})
	settings["hooks"] = hooks

	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode %s: %w", claudeSettingsPath, err)
	}
	encoded = append(encoded, '\n')
	if err := repoRoot.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(name), err)
	}
	// repoRoot.WriteFile, NOT writeContainedFile.
	//
	// The review's prescribed remedy was "os.OpenRoot() + resolveContainedName
	// like other file writes in this package". The containment is right and is
	// applied; resolveContainedName is not, and adopting it breaks the feature
	// outright: it enforces an agent-instruction-file allowlist (AGENTS.md,
	// CLAUDE.md, the guide) and refuses anything else with "not an
	// agent-instruction file". .claude/settings.json is not one. Measured --
	// three pre-existing merge tests went red on exactly that error.
	//
	// os.Root's own methods give the openat-based component resolution that
	// closes the escape, with no allowlist.
	if err := repoRoot.WriteFile(name, encoded, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", claudeSettingsPath, err)
	}
	return true, nil
}

// claudeHooksSection returns the hooks object, refusing rather than replacing
// when the key holds something unexpected.
func claudeHooksSection(settings map[string]any) (map[string]any, error) {
	raw, ok := settings["hooks"]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s has a \"hooks\" key that is not an object; leaving it untouched", claudeSettingsPath)
	}
	return hooks, nil
}

// preEditHookPresent reports whether any entry already runs the hook, however
// it is spelled or whichever product wrote it. Matching on the command rather
// than on our exact entry is what keeps this idempotent across versions.
func preEditHookPresent(entries []any) bool {
	for _, entry := range entries {
		group, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range inner {
			hook, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hook["command"].(string); ok && strings.Contains(cmd, preEditHookCommand) {
				return true
			}
		}
	}
	return false
}
