package cli

import (
	"strings"
	"testing"
)

// agentHookConfigPaths is one row per agent `entire enable` installs hooks
// for, with the path that agent's installer writes. The paths are read off the
// cli's own HookConfigRelPath implementations, not guessed:
//
//	agent/claudecode/hooks.go:692     .claude/settings.json
//	agent/codex/hooks.go:795          .codex/hooks.json
//	agent/cursor/hooks.go:412         .cursor/hooks.json
//	agent/geminicli/hooks.go:550      .gemini/settings.json
//	agent/copilotcli/hooks.go:363     .github/hooks/entire.json
//	agent/factoryaidroid/hooks.go:508 .factory/settings.json
//	agent/pi/hooks.go:174             .pi/extensions/entire/index.ts
//	agent/opencode/hooks.go:158       .opencode/plugins/entire.ts
//
// The table exists because the gate list covered two of these eight, so six
// agents left their user at a dirty-worktree refusal caused by the file Entire
// had just written for them. A new agent added without its path here is the
// same bug again, and this is what says so.
var agentHookConfigPaths = map[string]string{
	"claude-code": ".claude/settings.json",
	"codex":       ".codex/hooks.json",
	"cursor":      ".cursor/hooks.json",
	"gemini":      ".gemini/settings.json",
	"copilot-cli": ".github/hooks/entire.json",
	"factory":     ".factory/settings.json",
	"pi":          ".pi/extensions/entire/index.ts",
	"opencode":    ".opencode/plugins/entire.ts",
}

// Every supported agent's hook config must be exempt from the dirty gate. The
// install writes it; refusing the next command because of it is the install
// blocking itself.
func TestDirtyGateExemptsEveryAgentsHookConfig(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	gate, err := worktreeGateIgnore(repoDir)
	if err != nil {
		t.Fatalf("worktreeGateIgnore: %v", err)
	}
	for agent, path := range agentHookConfigPaths {
		if !gate.Ignored(path) {
			t.Errorf("%s: `entire enable --agent %s` writes %s, and the dirty gate still blocks on it", agent, agent, path)
		}
	}
}

// The user-visible outcome: a tree dirtied by nothing but the agent hook
// configs Entire wrote must present the gate with no blocking paths at all.
// One unignored source file still blocks, which is what the gate is for.
func TestDirtyGateSeesNothingInATreeDirtiedOnlyByAgentInstalls(t *testing.T) {
	t.Parallel()
	ignore := brainIgnore{patterns: entireAuthoredGatePaths}

	var status strings.Builder
	for _, path := range agentHookConfigPaths {
		status.WriteString("?? " + path + "\n")
	}
	if got := string(filterWorktreeStatus([]byte(status.String()), ignore)); got != "" {
		t.Fatalf("a tree dirtied only by agent installs still shows blocking paths:\n%s", got)
	}

	// And the gate has not been blunted into ignoring real work.
	withSource := status.String() + " M internal/auth/token.go\n"
	got := string(filterWorktreeStatus([]byte(withSource), ignore))
	if got != " M internal/auth/token.go\n" {
		t.Fatalf("the gate stopped reporting uncommitted source: %q", got)
	}
}

// Gate-only, still. Exempting these from the dirty gate must not delete them
// from the indexed corpus: that is the distinction the gate list was built on,
// and six new entries are six new chances to break it.
func TestAgentHookConfigsStayOutOfTheIndexingIgnore(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	indexing, err := loadBrainIgnore(repoDir)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	if _, err := worktreeGateIgnore(repoDir); err != nil {
		t.Fatalf("worktreeGateIgnore: %v", err)
	}
	for agent, path := range agentHookConfigPaths {
		if indexing.Ignored(path) {
			t.Errorf("%s: %s became ignored by the INDEXING filter, which deletes it from the indexed corpus", agent, path)
		}
	}
}
