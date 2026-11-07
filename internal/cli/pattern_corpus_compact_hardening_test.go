package cli

import (
	"encoding/json"
	"strings"
	"testing"
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

// Whether a compact codex result's output is Codex's wrapper text or the
// command's own stdout follows from the call's input, not its tool name: the
// item path writes {"command": <script>}, and Codex's own tools give a command
// as "cmd" (exec_command) or an argv array (shell). A converter that renames
// "Bash" must not bring back the stdout misread.
func TestCorpusEpisodeOpsCompactCodexExitTextKeyedOnInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, tool, input, output, want string
	}{
		{"item path", "Bash", `{"command":"go test ./..."}`, "child process exited with code 1\nok\n", `["go test exit=nil failed=false"]`},
		{"item path, lowercase name", "bash", `{"command":"go test ./..."}`, "child process exited with code 1\nok\n", `["go test exit=nil failed=false"]`},
		{"item path, padded name", " Bash ", `{"command":"go test ./..."}`, "Exit code: 1\n", `["go test exit=nil failed=false"]`},
		{"item path, renamed to shell", "shell", `{"command":"go test ./..."}`, "Exit code: 1\n", `["go test exit=nil failed=false"]`},
		{"exec_command", "exec_command", `{"cmd":"go test ./..."}`, "Process exited with code 1\nFAIL", `["go test exit=1 failed=true"]`},
		{"shell argv", "shell", `{"command":["go","test","./..."]}`, "Exit code: 1\nWall time: 1s\nOutput:\nFAIL", `["go test exit=1 failed=true"]`},
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
