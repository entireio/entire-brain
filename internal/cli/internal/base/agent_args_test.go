package base

import (
	"slices"
	"strings"
	"testing"
)

func TestDistillAgentCommandArgs(t *testing.T) {
	codex, err := DistillAgentCommandArgs("codex", nil, "PROMPT")
	if err != nil || codex[0] != "codex" || codex[len(codex)-1] != "PROMPT" {
		t.Fatalf("codex args wrong: %v err=%v", codex, err)
	}
	if !slices.Contains(codex, "--json") {
		t.Fatalf("codex args must request provider usage JSONL: %v", codex)
	}
	claude, err := DistillAgentCommandArgs("claude-code", nil, "PROMPT")
	if err != nil || claude[0] != "claude" || claude[len(claude)-1] != "PROMPT" {
		t.Fatalf("claude args wrong: %v err=%v", claude, err)
	}
	if !slices.Contains(claude, "--output-format") || !slices.Contains(claude, "json") {
		t.Fatalf("claude args must request provider usage JSON: %v", claude)
	}
	// The empty MCP config must be {"mcpServers":{}} — bare {} is rejected by the
	// current claude CLI ("mcpServers: expected record, received undefined").
	if !slices.Contains(claude, `{"mcpServers":{}}`) {
		t.Fatalf("claude args missing valid empty mcp-config: %v", claude)
	}
	// Issue #552: user plugin and settings hooks must not run inside the
	// extraction worker. --safe-mode is rejected before Claude Code 2.1.169 and
	// --bare skips logged-in auth, so neither may appear.
	if i := slices.Index(claude, "--settings"); i < 0 || i+1 >= len(claude) || claude[i+1] != `{"disableAllHooks":true}` {
		t.Fatalf("claude args must disable all hooks with --settings: %v", claude)
	}
	for _, banned := range []string{"--safe-mode", "--bare"} {
		if slices.Contains(claude, banned) {
			t.Fatalf("claude args must not use %s: %v", banned, claude)
		}
	}
	cmd, err := DistillAgentCommandArgs("command", []string{"my-agent", "--flag"}, "PROMPT")
	if err != nil || len(cmd) != 2 || cmd[0] != "my-agent" {
		t.Fatalf("command args wrong: %v err=%v", cmd, err)
	}
	if _, err := DistillAgentCommandArgs("command", nil, "PROMPT"); err == nil {
		t.Errorf("expected error when --agent command has no command")
	}
	if _, err := DistillAgentCommandArgs("none", nil, "PROMPT"); err == nil {
		t.Errorf("expected error for --agent none (distillation is agent-required)")
	}
	if _, err := DistillAgentCommandArgs("bogus", nil, "PROMPT"); err == nil {
		t.Errorf("expected error for unsupported agent")
	}
}

func TestDistillAgentCommandArgsRejectsRemoteAgentsInNoEgressMode(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	for _, agent := range []string{"codex", "claude-code", "command"} {
		_, err := DistillAgentCommandArgs(agent, []string{"local-agent"}, "PROMPT")
		if err == nil || !strings.Contains(err.Error(), "no_egress") {
			t.Fatalf("agent %s should be rejected in no-egress mode, got %v", agent, err)
		}
	}
	if _, err := DistillAgentCommandArgs("ollama", nil, "PROMPT"); err != nil {
		t.Fatalf("ollama should remain available in no-egress mode: %v", err)
	}
}

func TestInjectAgentModel(t *testing.T) {
	codex, _ := DistillAgentCommandArgs("codex", nil, "PROMPT")
	got := InjectAgentModel(codex, "codex", "gpt-5.3-codex-spark")
	if got[0] != "codex" || got[1] != "exec" || got[2] != "--model" || got[3] != "gpt-5.3-codex-spark" {
		t.Fatalf("codex model not inserted after exec: %v", got)
	}
	if got[len(got)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", got)
	}

	claude, _ := DistillAgentCommandArgs("claude-code", nil, "PROMPT")
	gotc := InjectAgentModel(claude, "claude-code", "sonnet")
	if gotc[0] != "claude" || gotc[1] != "--model" || gotc[2] != "sonnet" {
		t.Fatalf("claude model not inserted after claude: %v", gotc)
	}
	if gotc[len(gotc)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", gotc)
	}

	ollama, _ := DistillAgentCommandArgs("ollama", nil, "PROMPT")
	gotOllama := InjectAgentModel(ollama, "ollama", "llama3.2")
	if !slices.Equal(gotOllama, []string{"ollama", "llama3.2", "PROMPT"}) {
		t.Fatalf("ollama model not inserted into runner args: %v", gotOllama)
	}
	if !slices.Equal(ollama, []string{"ollama", "", "PROMPT"}) {
		t.Fatalf("ollama model injection should not mutate original args: %v", ollama)
	}

	// Empty model and the `command` agent are no-ops.
	if base, _ := DistillAgentCommandArgs("codex", nil, "PROMPT"); !slices.Equal(InjectAgentModel(base, "codex", ""), base) {
		t.Errorf("empty model should be a no-op")
	}
	cmd, _ := DistillAgentCommandArgs("command", []string{"my-agent"}, "PROMPT")
	if !slices.Equal(InjectAgentModel(cmd, "command", "x"), cmd) {
		t.Errorf("command agent should be a no-op for model injection")
	}
}

func TestInjectAgentEffort(t *testing.T) {
	codex, _ := DistillAgentCommandArgs("codex", nil, "PROMPT")
	got := InjectAgentEffort(codex, "codex", "low")
	if got[0] != "codex" || got[1] != "exec" || got[2] != "--config" || got[3] != "model_reasoning_effort=low" {
		t.Fatalf("codex effort not inserted as --config after exec: %v", got)
	}
	if got[len(got)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", got)
	}

	claude, _ := DistillAgentCommandArgs("claude-code", nil, "PROMPT")
	gotc := InjectAgentEffort(claude, "claude-code", "low")
	if gotc[0] != "claude" || gotc[1] != "--effort" || gotc[2] != "low" {
		t.Fatalf("claude effort not inserted after claude: %v", gotc)
	}
	if gotc[len(gotc)-1] != "PROMPT" {
		t.Fatalf("prompt must stay last: %v", gotc)
	}

	// Empty effort and the `command` agent are no-ops.
	if base, _ := DistillAgentCommandArgs("codex", nil, "PROMPT"); !slices.Equal(InjectAgentEffort(base, "codex", ""), base) {
		t.Errorf("empty effort should be a no-op")
	}
	cmd, _ := DistillAgentCommandArgs("command", []string{"my-agent"}, "PROMPT")
	if !slices.Equal(InjectAgentEffort(cmd, "command", "x"), cmd) {
		t.Errorf("command agent should be a no-op for effort injection")
	}

	// model + effort compose (the distill/seed path applies both): both flags present, prompt last.
	both := InjectAgentEffort(InjectAgentModel(codex, "codex", "gpt-5.4-mini"), "codex", "low")
	if !slices.Contains(both, "--model") || !slices.Contains(both, "gpt-5.4-mini") || !slices.Contains(both, "model_reasoning_effort=low") || both[len(both)-1] != "PROMPT" {
		t.Fatalf("model+effort should compose with prompt last: %v", both)
	}
}
