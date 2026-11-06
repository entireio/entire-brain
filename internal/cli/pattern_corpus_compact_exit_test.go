package cli

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// compactCodexFailingTestTranscript is a compact v1 codex session whose one
// turn runs a failing go test. The codex converter writes every tool result as
// status "success", so the exit code is only in the inlined output text.
const compactCodexFailingTestTranscript = `{"v":1,"agent":"codex","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"text":"Run the tests"}]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:05Z","content":[{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"go test ./..."},"result":{"output":"Process exited with code 1\nFAIL","status":"success"}},{"type":"text","text":"The tests fail."}]}
`

func describeCommands(commands []corpusCommand) string {
	var out []string
	for _, c := range commands {
		exit := "nil"
		if c.exitCode != nil {
			exit = fmt.Sprint(*c.exitCode)
		}
		out = append(out, fmt.Sprintf("%s exit=%s failed=%v", c.head, exit, c.failed))
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// A compact codex command must get the same exit code and failed flag as the
// same command from a native codex function_call / function_call_output pair.
func TestCorpusEpisodeOpsCompactCodexExitCodesMatchNative(t *testing.T) {
	cases := []struct {
		name, output, want string
	}{
		{"failing", "Process exited with code 1\nFAIL", `["go test exit=1 failed=true"]`},
		{"passing", "Process exited with code 0\nok", `["go test exit=0 failed=false"]`},
		{"exit code colon", "Exit code: 2\nWall time: 1s", `["go test exit=2 failed=true"]`},
		{"no exit text", "ok  \tpkg\t0.1s", `["go test exit=nil failed=false"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := json.Marshal(c.output)
			if err != nil {
				t.Fatal(err)
			}
			native := `{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":` + string(out) + `}}`
			_, nativeCmds, _ := corpusEpisodeOps(native)
			if got := describeCommands(nativeCmds); got != c.want {
				t.Fatalf("native codex commands = %s, want %s", got, c.want)
			}
			compact := `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"c1","name":"exec_command","input":{"cmd":"go test ./..."},"result":{"output":` + string(out) + `,"status":"success"}}]}`
			_, compactCmds, _ := corpusEpisodeOps(compact)
			if got := describeCommands(compactCmds); got != c.want {
				t.Errorf("compact codex commands = %s, want %s (same as native)", got, c.want)
			}
		})
	}
}

// Only compact codex results are read. For claude-code and pi, whether a
// result.status of "error" (or exit-code text) should mark a command failed is
// an open question, so their commands keep no exit code and failed=false, as
// they did before.
func TestCorpusEpisodeOpsCompactNonCodexResultsLeftAlone(t *testing.T) {
	for _, agent := range []string{"claude-code", "pi"} {
		t.Run(agent, func(t *testing.T) {
			work := `{"v":1,"agent":"` + agent + `","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."},"result":{"output":"Exit code 1\nFAIL","status":"error"}}]}`
			_, commands, _ := corpusEpisodeOps(work)
			if got, want := describeCommands(commands), `["go test exit=nil failed=false"]`; got != want {
				t.Errorf("commands = %s, want %s", got, want)
			}
		})
	}
}

// Through the corpus build: the failing compact codex command is stored as a
// failure and counted in exit_fails. A corpus indexed by IndexerVersion 10
// stored it with no exit code, and the transcript bytes do not change on
// upgrade, so only a version bump makes the next build re-index it instead of
// skipping it as unchanged.
func TestPatternCorpusRecordsCompactCodexCommandFailures(t *testing.T) {
	now := time.Now()
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", agent: "codex", relPath: "sessions/main/s1.jsonl", transcript: compactCodexFailingTestTranscript}})
	state := func() string {
		t.Helper()
		db := openCorpus(t, brainDir)
		defer db.Close()
		var head string
		var exit, failed, exitFails any
		if err := db.QueryRow(`SELECT c.head, c.exit_code, c.failed, e.exit_fails FROM episode_commands c JOIN episodes e ON e.id = c.episode_id`).Scan(&head, &exit, &failed, &exitFails); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s exit_code=%v failed=%v exit_fails=%v", head, exit, failed, exitFails)
	}
	const want = "go test exit_code=1 failed=1 exit_fails=1"
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != want {
		t.Fatalf("corpus command = %q, want %q", got, want)
	}

	db := openCorpus(t, brainDir)
	for _, stmt := range []string{`UPDATE indexed_sessions SET parser_version=10`, `UPDATE meta SET value='10' WHERE key='pattern_indexer_version'`, `UPDATE episode_commands SET exit_code=NULL, failed=0`, `UPDATE episodes SET exit_fails=0`} {
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
		t.Fatalf("version 10 corpus kept its missing failure after rebuild: corpus command = %q, want %q", got, want)
	}
}
