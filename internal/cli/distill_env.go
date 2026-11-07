package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// distillChildEnvKey carries a replacement child-process environment for the
// distill pipeline's agent invocations. It rides the context (like the usage
// collector) so the override stays scoped to distill runs: the exec helper is
// shared with eval, outline, patterns, skills, and query expansion, whose
// latency/quality tradeoffs were never measured.
type distillChildEnvKey struct{}

// withDistillAgentChildEnv attaches the claude-code child environment to the
// distill run's context. Other agents keep the inherited environment: ollama is
// HTTP, codex has no equivalent knob, and a --agent-command argv is user-owned.
func withDistillAgentChildEnv(ctx context.Context, agent string) context.Context {
	if agent != "claude-code" {
		return ctx
	}
	return context.WithValue(ctx, distillChildEnvKey{}, distillClaudeChildEnv(os.Environ()))
}

// distillClaudeChildEnv builds the claude child environment:
//
//   - MAX_THINKING_TOKENS=0 disables extended thinking. Measured on a fixed
//     20-session corpus (COR-2214): ~97% of extraction output tokens were
//     invisible thinking generated at output speed, making API time ~92% of
//     each call; disabling it cut the run 7x (545s -> 76s) with fact yield
//     intact and the output distribution unchanged beyond run-to-run noise.
//     A caller-set MAX_THINKING_TOKENS wins over the default.
//   - CLAUDECODE is dropped so distill works when spawned from inside a Claude
//     Code session (session-end hook): claude refuses to start nested when it
//     is set, and names unsetting it as the supported bypass. The child is a
//     headless --print worker with --no-session-persistence, so none of the
//     shared-session state the guard protects exists here.
//     CLAUDE_CODE_ENTRYPOINT is dropped with it: only CLAUDECODE gates the
//     refusal today (validated against a live session), but this package's own
//     detectedAgentHarness reads either as "inside Claude Code", and the child
//     is not.
func distillClaudeChildEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	hasThinkingBudget := false
	for _, kv := range environ {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		if strings.HasPrefix(kv, "MAX_THINKING_TOKENS=") {
			hasThinkingBudget = true
		}
		out = append(out, kv)
	}
	if !hasThinkingBudget {
		out = append(out, "MAX_THINKING_TOKENS=0")
	}
	return out
}

// claudeSafeModeEnv is set to 1 in the environment of every built-in
// claude-code child (issue #552), next to the hook-disabling --settings value
// in the argv builders (see base.ClaudeDisableHooksSettings). On Claude Code
// 2.1.169+ it also leaves out plugins and the user's global CLAUDE.md, which
// otherwise reach every extraction prompt; older versions ignore it, and the
// --settings value still keeps hooks off there. It goes in the environment and
// not as --safe-mode because versions before 2.1.169 reject that flag.
const claudeSafeModeEnv = "CLAUDE_CODE_SAFE_MODE"

// claudeSafeModeChildEnv returns a copy of environ with CLAUDE_CODE_SAFE_MODE=1
// as its only safe-mode entry. An inherited value is replaced, not kept: the
// child is Brain's own extraction worker, and a --agent command argv is the way
// to run claude with a different setup.
func claudeSafeModeChildEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if strings.HasPrefix(kv, claudeSafeModeEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, claudeSafeModeEnv+"=1")
}

// distillAgentProcessEnv is the environment for one agent call made through
// execDistillAgentWithPrompt, or nil to inherit the parent's. A distill run's
// context override (withDistillAgentChildEnv) is the base. A built-in claude
// argv also gets CLAUDE_CODE_SAFE_MODE=1, whichever command built it: distill,
// reconcile, remember classification, abstracts, eval, query expansion,
// outline, patterns and skills all run here. Custom --agent command argv is
// user-owned and keeps the base unchanged.
func distillAgentProcessEnv(ctx context.Context, args []string, carriesPrompt bool) []string {
	env, _ := ctx.Value(distillChildEnvKey{}).([]string)
	if !carriesPrompt || !isBuiltInClaudeArgv(args) {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	return claudeSafeModeChildEnv(env)
}

// isBuiltInClaudeArgv reports whether args came from the built-in claude-code
// builder: the system prompt is parked behind the private marker and the
// executable is claude (claude.exe on Windows).
func isBuiltInClaudeArgv(args []string) bool {
	argv, _, ok := base.SplitAgentPromptArg(args)
	return ok && len(argv) > 0 && strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") == "claude"
}
