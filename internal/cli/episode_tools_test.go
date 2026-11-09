package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
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

// A brain refreshed by the pre-compact reader persisted compact episodes with
// empty tool and command sequences. The session fingerprint and privacy
// identity are unchanged by the upgrade, so a normal refresh must still see the
// recorded projection as stale and rebuild it.
func TestRefreshPatternLayerRebuildsEpisodesFromOlderExtractor(t *testing.T) {
	now := time.Now()
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", agent: "codex", relPath: "sessions/main/s1.jsonl", transcript: compactOpsCodexTranscript}})
	if _, err := writeBrainEpisodesAndSource(brainDir, now); err != nil {
		t.Fatal(err)
	}
	// Model what the old reader left on disk: the same episodes, no tool ops,
	// and a patterns source with no extraction version.
	episodes, err := loadBrainEpisodes(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	var stale bytes.Buffer
	for _, ep := range episodes {
		ep.ToolSequence, ep.CommandSequence = nil, nil
		if err := json.NewEncoder(&stale).Encode(ep); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(patternsEpisodesPath)), stale.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(brainDir, bmanifest.FileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(jsonMap(jsonMap(doc["sources"])["patterns"]), "extraction_version")
	if raw, err = json.Marshal(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	rebuilt, err := refreshPatternLayer(brainDir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt {
		t.Error("refresh treated the old extractor's episode projection as current")
	}
	episodes, err = loadBrainEpisodes(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, ep := range episodes {
		got = append(got, ep.ToolSequence, ep.CommandSequence)
	}
	want := [][]string{{"exec_command"}, {"grep"}, {"apply_patch"}, nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("episode tool/command sequences after refresh = %q, want %q", got, want)
	}
}
