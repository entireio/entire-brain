package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// compactCodexToolLine is one compact codex assistant line holding a single
// tool call with an inlined result.
func compactCodexToolLine(t *testing.T, name, input, output string) string {
	t.Helper()
	quotedName, err := json.Marshal(name)
	if err != nil {
		t.Fatal(err)
	}
	quotedOutput, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	return `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"c1","name":` + string(quotedName) + `,"input":` + input + `,"result":{"output":` + string(quotedOutput) + `,"status":"success"}}]}`
}

// A compact codex result's output is the command's own stdout on the item path,
// which names every command Bash, and Codex's wrapper text with the exit code
// for Codex's own tools. The skip follows the name, whatever its case or
// spacing and however the input is encoded, and not the input's shape, which
// Codex's own tools can share: shell_command takes a "command" string too.
func TestCorpusEpisodeOpsCompactCodexExitTextSkipsItemPathCommands(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(`{"command":"go test ./..."}`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, tool, input, output, want string
	}{
		{"item path", "Bash", `{"command":"go test ./..."}`, "child process exited with code 1\nok\n", `["go test exit=nil failed=false"]`},
		{"item path, lowercase name", "bash", `{"command":"go test ./..."}`, "child process exited with code 1\nok\n", `["go test exit=nil failed=false"]`},
		{"item path, padded name", " Bash ", `{"command":"go test ./..."}`, "Exit code: 1\n", `["go test exit=nil failed=false"]`},
		{"item path, JSON-encoded input", "Bash", string(encoded), "Exit code: 1\n", `["go test exit=nil failed=false"]`},
		{"exec_command", "exec_command", `{"cmd":"go test ./..."}`, "Process exited with code 1\nFAIL", `["go test exit=1 failed=true"]`},
		{"shell argv", "shell", `{"command":["go","test","./..."]}`, "Exit code: 1\nWall time: 1s\nOutput:\nFAIL", `["go test exit=1 failed=true"]`},
		{"Codex tool with a command string", "shell", `{"command":"go test ./..."}`, "Exit code: 1\nWall time: 1s\nOutput:\nFAIL", `["go test exit=1 failed=true"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, commands, _ := corpusEpisodeOps(compactCodexToolLine(t, c.tool, c.input, c.output))
			if got := describeCommands(commands); got != c.want {
				t.Errorf("commands = %s, want %s", got, c.want)
			}
		})
	}
}

// The compact apply_patch fallback reads the patch whether the converter
// wraps it as {"input": <patch>} or writes it as a bare string.
func TestCorpusEpisodeOpsCompactApplyPatchInputShapes(t *testing.T) {
	t.Parallel()
	patch, err := json.Marshal("*** Begin Patch\n*** Add File: docs/new.md\n+x\n*** Update File: internal/cli/export.go\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	encodedWrapper, err := json.Marshal(`{"input":` + string(patch) + `}`)
	if err != nil {
		t.Fatal(err)
	}
	const want = "add docs/new.md,edit internal/cli/export.go"
	for name, input := range map[string]string{
		"wrapped":              `{"input":` + string(patch) + `}`,
		"bare string":          string(patch),
		"JSON-encoded wrapper": string(encodedWrapper),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, files := corpusEpisodeOps(compactCodexToolLine(t, "apply_patch", input, "Success."))
			var got []string
			for _, f := range files {
				got = append(got, f.action+" "+f.path)
			}
			if strings.Join(got, ",") != want {
				t.Errorf("files = %q, want %q", strings.Join(got, ","), want)
			}
		})
	}
}

// Codex's wrapper writes its exit line in a header before "Output:", and the
// command's own output follows. Exit-like text in that output must neither
// shadow the header's exit code nor stand in for one the header lacks, for a
// native function_call_output and a compact result alike.
func TestCorpusEpisodeOpsExitCodeReadFromWrapperHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, output, want string
	}{
		{"header exit wins over printed exit", "Exit code: 2\nWall time: 1s\nOutput:\nchild process exited with code 0\n", `["go test exit=2 failed=true"]`},
		{"running command has no exit code", "Chunk ID: 7\nWall time: 10.0 seconds\nProcess running with session ID 4\nOriginal token count: 3\nOutput:\nchild process exited with code 1\n", `["go test exit=nil failed=false"]`},
		{"exit line in the header", "Chunk ID: 7\nWall time: 0.5 seconds\nProcess exited with code 1\nOriginal token count: 3\nOutput:\nFAIL\n", `["go test exit=1 failed=true"]`},
		{"no Output line", "Process exited with code 1\nFAIL", `["go test exit=1 failed=true"]`},
		{"empty output after the header", "Exit code: 0\nWall time: 1s\nOutput:", `["go test exit=0 failed=false"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, err := json.Marshal(c.output)
			if err != nil {
				t.Fatal(err)
			}
			native := `{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":` + string(out) + `}}`
			if _, commands, _ := corpusEpisodeOps(native); describeCommands(commands) != c.want {
				t.Errorf("native codex commands = %s, want %s", describeCommands(commands), c.want)
			}
			compact := compactCodexToolLine(t, "exec_command", `{"cmd":"go test ./..."}`, c.output)
			if _, commands, _ := corpusEpisodeOps(compact); describeCommands(commands) != c.want {
				t.Errorf("compact codex commands = %s, want %s", describeCommands(commands), c.want)
			}
		})
	}
}

// Through the corpus build: a corpus indexed by IndexerVersion 11 stored the
// exit code printed in a running command's output, and the transcript bytes do
// not change on upgrade, so only the version bump makes the next build
// re-index the session instead of skipping it as unchanged.
func TestPatternCorpusReindexesVersion11PrintedExitCodes(t *testing.T) {
	now := time.Now()
	output, err := json.Marshal("Process running with session ID 4\nOutput:\nchild process exited with code 1\n")
	if err != nil {
		t.Fatal(err)
	}
	transcript := `{"v":1,"agent":"codex","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"text":"Run the tests"}]}
{"v":1,"agent":"codex","type":"assistant","ts":"2026-10-01T10:00:05Z","content":[{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"go test ./..."},"result":{"output":` + string(output) + `,"status":"success"}}]}
`
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", agent: "codex", relPath: "sessions/main/s1.jsonl", transcript: transcript}})
	state := func() string {
		t.Helper()
		db := openCorpus(t, brainDir)
		defer db.Close()
		var exit, failed any
		if err := db.QueryRow(`SELECT exit_code, failed FROM episode_commands`).Scan(&exit, &failed); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("exit_code=%v failed=%v", exit, failed)
	}
	const want = "exit_code=<nil> failed=0"
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != want {
		t.Fatalf("corpus command = %q, want %q", got, want)
	}
	db := openCorpus(t, brainDir)
	for _, stmt := range []string{`UPDATE indexed_sessions SET parser_version=11`, `UPDATE meta SET value='11' WHERE key='pattern_indexer_version'`, `UPDATE episode_commands SET exit_code=1, failed=1`} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != want {
		t.Fatalf("version 11 corpus kept its printed exit code after rebuild: corpus command = %q, want %q", got, want)
	}
}
