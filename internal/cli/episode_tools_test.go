package cli

import (
	"reflect"
	"testing"
)

func TestNormalizeCommand(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"git status --short --branch", "git status"},
		{"git rev-parse --abbrev-ref HEAD && git rev-parse origin/HEAD", "git rev-parse"},
		{"go test ./...", "go test"},
		{"go build ./cmd/entire-brain", "go build"},
		{"sed -n '1,220p' internal/cli/semantic.go", "sed"},
		{"rg -n \"foo\" .", "rg"},
		{"/usr/local/bin/go vet ./...", "go vet"},
		{"./scripts/release.sh --dry-run", "release.sh"},
		{"GOFLAGS=-mod=mod go test ./...", "go test"},
		{"sudo docker ps", "docker ps"},
		{"make", "make"},
		{"", ""},
		{"&& go test", ""},
	}
	for _, c := range cases {
		if got := normalizeCommand(c.in); got != c.want {
			t.Errorf("normalizeCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEpisodeToolAndCommandSequences(t *testing.T) {
	// Codex work segment: a string-arg exec_command, a chained command, and an
	// apply_patch (tool-only, no command).
	work := `{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status --short\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","output":"clean"}}
{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test ./... && go vet ./...\"}"}}`
	tools, commands := episodeToolAndCommandSequences(work)
	wantTools := []string{"exec_command", "apply_patch", "exec_command"}
	wantCommands := []string{"git status", "go test"}
	if !reflect.DeepEqual(tools, wantTools) {
		t.Errorf("tools = %v, want %v", tools, wantTools)
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Errorf("commands = %v, want %v", commands, wantCommands)
	}
}

func TestEpisodeToolSequencesClaudeDialect(t *testing.T) {
	// Claude assistant message with a tool_use block carrying an object input.
	work := `{"type":"assistant","message":{"content":[{"type":"text","text":"running tests"},{"type":"tool_use","name":"bash","input":{"command":"go test ./..."}}]}}`
	tools, commands := episodeToolAndCommandSequences(work)
	if !reflect.DeepEqual(tools, []string{"bash"}) {
		t.Errorf("tools = %v, want [bash]", tools)
	}
	if !reflect.DeepEqual(commands, []string{"go test"}) {
		t.Errorf("commands = %v, want [go test]", commands)
	}
}
