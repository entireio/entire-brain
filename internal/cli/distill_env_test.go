package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

func TestDistillClaudeChildEnvDisablesThinkingAndStripsClaudeCode(t *testing.T) {
	got := distillClaudeChildEnv([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "HOME=/u"})
	want := []string{"PATH=/bin", "HOME=/u", "MAX_THINKING_TOKENS=0"}
	if !slices.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}

	// A caller-set budget wins over the pipeline default.
	got = distillClaudeChildEnv([]string{"MAX_THINKING_TOKENS=1024"})
	if !slices.Equal(got, []string{"MAX_THINKING_TOKENS=1024"}) {
		t.Fatalf("caller budget overridden: %v", got)
	}
}

func TestWithDistillAgentChildEnvOnlyAppliesToClaude(t *testing.T) {
	ctx := context.Background()
	for _, agent := range []string{"codex", "ollama", "command", "none"} {
		if withDistillAgentChildEnv(ctx, agent) != ctx {
			t.Fatalf("agent %q must keep the inherited environment", agent)
		}
	}
	if withDistillAgentChildEnv(ctx, "claude-code").Value(distillChildEnvKey{}) == nil {
		t.Fatal("claude-code must carry a child environment")
	}
}

func TestExecDistillAgentAppliesContextChildEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based agent stub")
	}
	ctx := context.WithValue(context.Background(), distillChildEnvKey{},
		distillClaudeChildEnv([]string{"PATH=/usr/bin:/bin", "CLAUDECODE=1"}))
	out, err := execDistillAgent(ctx, t.TempDir(),
		[]string{"sh", "-c", `printf '%s|%s' "$MAX_THINKING_TOKENS" "${CLAUDECODE-unset}"`},
		nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out != "0|unset" {
		t.Fatalf("child env = %q, want %q", out, "0|unset")
	}
}

func TestClaudeSafeModeChildEnvReplacesInheritedValue(t *testing.T) {
	t.Parallel()
	in := []string{"PATH=/bin", "CLAUDE_CODE_SAFE_MODE=0", "HOME=/u"}
	got := claudeSafeModeChildEnv(in)
	want := []string{"PATH=/bin", "HOME=/u", "CLAUDE_CODE_SAFE_MODE=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	if in[1] != "CLAUDE_CODE_SAFE_MODE=0" {
		t.Fatalf("input slice was modified: %v", in)
	}
}

func TestDistillAgentProcessEnvPutsOnlyBuiltInClaudeInSafeMode(t *testing.T) {
	t.Parallel()
	claude, err := base.DistillAgentCommandArgs("claude-code", nil, "PROMPT")
	if err != nil {
		t.Fatal(err)
	}
	codex, err := base.DistillAgentCommandArgs("codex", nil, "PROMPT")
	if err != nil {
		t.Fatal(err)
	}
	distillEnv := distillClaudeChildEnv([]string{"PATH=/bin", "CLAUDECODE=1"})
	distillCtx := context.WithValue(context.Background(), distillChildEnvKey{}, distillEnv)

	// A distill run keeps its own child env and adds safe mode on top, without
	// touching the shared slice that concurrent --jobs calls read.
	got := distillAgentProcessEnv(distillCtx, claude, true)
	if want := []string{"PATH=/bin", "MAX_THINKING_TOKENS=0", "CLAUDE_CODE_SAFE_MODE=1"}; !slices.Equal(got, want) {
		t.Fatalf("distill claude env = %v, want %v", got, want)
	}
	if slices.Contains(distillEnv, "CLAUDE_CODE_SAFE_MODE=1") {
		t.Fatalf("distill context env was modified: %v", distillEnv)
	}

	// Every other built-in claude caller has no context env: it inherits the
	// parent environment plus safe mode.
	got = distillAgentProcessEnv(context.Background(), claude, true)
	if len(got) != len(os.Environ())+1-countSafeModeEntries(os.Environ()) || got[len(got)-1] != "CLAUDE_CODE_SAFE_MODE=1" {
		t.Fatalf("inherited claude env must be the parent env plus safe mode, got %d entries ending %q", len(got), got[len(got)-1])
	}

	// A model pin and an absolute claude path are still the built-in builder.
	pinned := base.InjectAgentModel(slices.Clone(claude), "claude-code", "haiku")
	pinned[0] = filepath.Join(t.TempDir(), "claude")
	if got := distillAgentProcessEnv(context.Background(), pinned, true); len(got) == 0 || got[len(got)-1] != "CLAUDE_CODE_SAFE_MODE=1" {
		t.Fatalf("pinned claude argv must run in safe mode, got %v", got)
	}

	// codex and custom --agent command argv keep the base environment, even
	// a custom claude argv that happens to carry the marker.
	if got := distillAgentProcessEnv(context.Background(), codex, true); got != nil {
		t.Fatalf("codex must inherit the parent env, got %v", got)
	}
	if got := distillAgentProcessEnv(distillCtx, claude, false); !slices.Equal(got, distillEnv) {
		t.Fatalf("custom command env = %v, want the distill env unchanged %v", got, distillEnv)
	}
	if got := distillAgentProcessEnv(context.Background(), []string{"sh", "-c", "true"}, true); got != nil {
		t.Fatalf("unmarked argv must inherit the parent env, got %v", got)
	}
}

func countSafeModeEntries(environ []string) int {
	n := 0
	for _, kv := range environ {
		if strings.HasPrefix(kv, claudeSafeModeEnv+"=") {
			n++
		}
	}
	return n
}

// TestExecDistillAgentRunsBuiltInClaudeInSafeMode runs the real exec path with a
// stand-in claude that reports the environment it was started with.
func TestExecDistillAgentRunsBuiltInClaudeInSafeMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("sh-based agent stub")
	}
	stub := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"%s|%s\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}' \"${CLAUDE_CODE_SAFE_MODE-unset}\" \"${MAX_THINKING_TOKENS-unset}\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	args, err := base.DistillAgentCommandArgs("claude-code", nil, "PROMPT")
	if err != nil {
		t.Fatal(err)
	}
	args[0] = stub

	// Outside distill (remember, abstracts, eval, patterns, ...): safe mode only.
	out, err := execDistillAgent(context.Background(), t.TempDir(), args, []byte("chunk"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1|" + envOr("MAX_THINKING_TOKENS", "unset"); out != want {
		t.Fatalf("child env = %q, want %q", out, want)
	}

	// Inside distill: the distill child env and safe mode together.
	ctx := context.WithValue(context.Background(), distillChildEnvKey{},
		distillClaudeChildEnv([]string{"PATH=/usr/bin:/bin", "CLAUDE_CODE_SAFE_MODE=0"}))
	out, err = execDistillAgent(ctx, t.TempDir(), args, []byte("chunk"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if out != "1|0" {
		t.Fatalf("distill child env = %q, want %q", out, "1|0")
	}
}

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
