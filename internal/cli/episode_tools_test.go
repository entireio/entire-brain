package cli

import (
	"reflect"
	"testing"
	"time"
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

// Compact v1 transcripts (the default export) keep an assistant turn's
// content at the record's top level, with no "message" wrapper. Uses the
// fixtures from compact_transcript_ops_fixtures_test.go.
func TestEpisodeToolSequencesCompactTranscript(t *testing.T) {
	cases := []struct {
		name, fixture string
		tools         []string
		commands      []string
	}{
		{"claude-code", compactOpsClaudeTranscript, []string{"bash"}, []string{"go test"}},
		{"codex", compactOpsCodexTranscript, []string{"exec_command", "apply_patch"}, []string{"grep"}},
		{"pi", compactOpsPiTranscript, []string{"read"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tools, commands := episodeToolAndCommandSequences(c.fixture)
			if !reflect.DeepEqual(tools, c.tools) {
				t.Errorf("tools = %v, want %v", tools, c.tools)
			}
			if !reflect.DeepEqual(commands, c.commands) {
				t.Errorf("commands = %v, want %v", commands, c.commands)
			}
		})
	}
}

func TestBuildBrainEpisodesCompactTranscriptToolSequences(t *testing.T) {
	now := time.Now()
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", agent: "codex", relPath: "sessions/main/s1.jsonl", transcript: compactOpsCodexTranscript}})
	episodes, _, err := buildBrainEpisodes(brainDir, now)
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, ep := range episodes {
		got = append(got, ep.ToolSequence, ep.CommandSequence)
	}
	want := [][]string{{"exec_command"}, {"grep"}, {"apply_patch"}, nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("episode tool/command sequences = %q, want %q", got, want)
	}
}
