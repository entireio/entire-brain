package base

import (
	"errors"
	"fmt"
)

// DistillAgentPromptMarker separates the agent's real argv from its system
// prompt inside the slice DistillAgentCommandArgs returns. The prompt is
// deliberately NOT a process argument: on Windows `claude`/`codex` installed
// from npm resolve to `.cmd` shims, which Go runs through cmd.exe, and
// cmd.exe caps the whole command line at 8191 characters.
// templates/entire-brain-distill.md alone is ~7.5 KiB, so a prompt on argv
// made `brain distill` fail unconditionally there with "The command line is
// too long" (issue #322). prepareAgentExec strips this marker and the prompt
// that follows it and delivers the prompt out of band instead.
const DistillAgentPromptMarker = "--entire-brain-agent-prompt-off-argv"

// ClaudeDisableHooksSettings is the inline --settings value every built-in
// claude-code argv carries (issue #552). The child is a headless extraction
// worker, but `--setting-sources user` still loads the user's plugins and
// settings hooks, so a session-memory plugin's SessionStart context reached
// every extraction prompt and its Stop/SessionEnd hooks recorded each call as
// a session. disableAllHooks turns every hook off on every Claude Code version
// Brain supports. Settings `model`, `env` and `apiKeyHelper` still apply, so
// Bedrock, Vertex and custom-endpoint setups keep working. The process
// environment adds CLAUDE_CODE_SAFE_MODE=1 on top, which on Claude Code
// 2.1.169+ also leaves out plugins and the user's CLAUDE.md. The safe-mode
// flag itself is not used because older versions reject it, and --bare is
// not used because it skips logged-in (OAuth) auth.
const ClaudeDisableHooksSettings = `{"disableAllHooks":true}`

// DistillAgentCommandArgs builds the argv that runs the seed agent with the
// rendered distillation prompt. The prompt stays OFF the command line: it is
// appended behind DistillAgentPromptMarker and prepareAgentExec moves it to a
// 0600 temp file (claude, via --system-prompt-file) or onto stdin (codex)
// before the process is spawned — see DistillAgentPromptMarker for why. The
// transcript chunk is supplied on stdin. The no-agent mode ("none") has no
// command — distillation is agent-required.
func DistillAgentCommandArgs(agent string, agentCommand []string, prompt string) ([]string, error) {
	if err := RejectAgentForNoEgress(agent); err != nil {
		return nil, err
	}
	switch agent {
	case "command":
		if len(agentCommand) == 0 {
			return nil, errors.New("--agent-command is required when --agent command")
		}
		return append([]string(nil), agentCommand...), nil
	case "codex":
		return []string{"codex", "exec", "--json", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", DistillAgentPromptMarker, prompt}, nil
	case "claude-code":
		return []string{"claude", "--print", "--output-format", "json", "--no-session-persistence", "--setting-sources", "user", "--settings", ClaudeDisableHooksSettings, "--strict-mcp-config", "--mcp-config", "{\"mcpServers\":{}}", "--disable-slash-commands", "--permission-mode", "dontAsk", "--tools", "", DistillAgentPromptMarker, prompt}, nil
	case "ollama":
		// No marker: the ollama runner never spawns a process. It POSTs to a
		// loopback /api/generate endpoint and sends this element as the JSON
		// `system` field, so no command line is ever built and no OS argv limit
		// applies.
		return []string{"ollama", "", prompt}, nil
	case "none", "":
		return nil, errors.New("distillation requires an agent; --agent none has nothing to run")
	default:
		return nil, fmt.Errorf("unsupported --agent %q", agent)
	}
}

// SplitAgentPromptArg separates the agent argv from the system prompt that
// DistillAgentCommandArgs parked behind DistillAgentPromptMarker. The returned
// argv is a copy, so callers may append their own flags without aliasing the
// builder's slice. Agents whose builder output carries no marker (the `command`
// agent, which never received the prompt, and `ollama`, which spawns no
// process) come back unchanged with ok=false.
func SplitAgentPromptArg(args []string) (argv []string, prompt string, ok bool) {
	n := len(args)
	if n < 2 || args[n-2] != DistillAgentPromptMarker {
		return args, "", false
	}
	return append([]string(nil), args[:n-2]...), args[n-1], true
}

// InjectAgentModel inserts a `--model <model>` flag into a codex/claude-code argv
// so the distill run can pin a specific model (e.g. gpt-5.3-codex-spark). It is a
// no-op for an empty model or any other agent (the `command` agent carries its
// own argv). The flag is inserted early — after `codex exec` / after `claude` —
// where both CLIs accept it, ahead of the trailing prompt argument.
func InjectAgentModel(args []string, agent, model string) []string {
	if model == "" {
		return args
	}
	switch agent {
	case "codex":
		if len(args) >= 2 && args[0] == "codex" && args[1] == "exec" {
			return append(args[:2:2], append([]string{"--model", model}, args[2:]...)...)
		}
	case "claude-code":
		if len(args) >= 1 && args[0] == "claude" {
			return append(args[:1:1], append([]string{"--model", model}, args[1:]...)...)
		}
	case "ollama":
		if len(args) >= 3 && args[0] == "ollama" {
			out := append([]string(nil), args...)
			out[1] = model
			return out
		}
	}
	return args
}

// InjectAgentEffort pins the reasoning effort for a codex/claude-code argv built by
// DistillAgentCommandArgs/seedAgentCommandArgs — codex via `--config model_reasoning_effort=<e>`,
// claude-code via `--effort <e>` (the same forms the benchmark harness uses). It mirrors
// InjectAgentModel: a no-op for an empty effort, the `command` agent, or an unrecognized shape,
// and inserts early so the trailing prompt argument stays last.
func InjectAgentEffort(args []string, agent, effort string) []string {
	if effort == "" {
		return args
	}
	switch agent {
	case "codex":
		if len(args) >= 2 && args[0] == "codex" && args[1] == "exec" {
			return append(args[:2:2], append([]string{"--config", "model_reasoning_effort=" + effort}, args[2:]...)...)
		}
	case "claude-code":
		if len(args) >= 1 && args[0] == "claude" {
			return append(args[:1:1], append([]string{"--effort", effort}, args[1:]...)...)
		}
	}
	return args
}
