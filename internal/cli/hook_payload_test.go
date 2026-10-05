package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The first wiring of the pre-edit hook passed `--file "$CLAUDE_FILE_PATH"`.
// No such variable exists. Measured against Claude Code 2.1.288 by installing a
// hook that dumped its own stdin and environment: the payload arrives as JSON on
// STDIN carrying tool_input.file_path, and the CLAUDE_* environment holds
// CLAUDE_PROJECT_DIR, CLAUDE_PID, CLAUDE_EFFORT, CLAUDE_TRANSCRIPT_PATH and
// CLAUDE_CODE_ENTRYPOINT -- nothing naming the edited file.
//
// So the flag expanded to "" on every edit and the hook errored every time,
// serving nothing. Installed and inert is the worst of both: `init-agents`
// reported success and no fact ever reached an agent.
func TestHookReadsTheFilePathFromTheHarnessPayload(t *testing.T) {
	t.Parallel()

	// The exact shape captured from Claude Code 2.1.288.
	const real = `{"session_id":"20373dcb","transcript_path":"/x/y.jsonl",
	  "cwd":"/repo","permission_mode":"bypassPermissions",
	  "hook_event_name":"PreToolUse","tool_name":"Write",
	  "tool_input":{"file_path":"/repo/internal/cli/semantic.go","content":"x"},
	  "tool_use_id":"toolu_01"}`

	got, saw := hookFileFromStdin(strings.NewReader(real))
	if got != "/repo/internal/cli/semantic.go" {
		t.Fatalf("file path from the real payload = %q, want tool_input.file_path", got)
	}
	if !saw {
		t.Error("a payload with bytes in it must report sawInput")
	}
}

// NotebookEdit names its target notebook_path, not file_path. The installed
// matcher includes NotebookEdit, so ignoring it would wire that tool to a hook
// that can never resolve a path.
func TestHookReadsNotebookPathToo(t *testing.T) {
	t.Parallel()

	const nb = `{"hook_event_name":"PreToolUse","tool_name":"NotebookEdit",
	  "tool_input":{"notebook_path":"/repo/analysis.ipynb"}}`

	if got, saw := hookFileFromStdin(strings.NewReader(nb)); got != "/repo/analysis.ipynb" || !saw {
		t.Fatalf("notebook path = %q (saw=%v), want tool_input.notebook_path", got, saw)
	}
}

// Malformed input is silence, never an error, and must still report sawInput so
// the command exits 0. This runs on every edit; a hook that errors on every
// edit gets deleted from the settings within a day.
func TestHookPayloadFailuresAreSilent(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"not json":         "this is not json at all",
		"json but no tool": `{"hook_event_name":"PreToolUse"}`,
		"empty file_path":  `{"tool_input":{"file_path":"   "}}`,
		"wrong types":      `{"tool_input":{"file_path":123}}`,
		"truncated":        `{"tool_input":{"file_path":"/a/b.go"`,
	} {
		t.Run(name, func(t *testing.T) {
			got, saw := hookFileFromStdin(strings.NewReader(in))
			if got != "" {
				t.Fatalf("want no path for %s, got %q", name, got)
			}
			if !saw {
				t.Errorf("%s: stdin had bytes but sawInput=false, so the command would error on every edit", name)
			}
		})
	}
	// Genuinely nothing: no bytes, and no claim that a harness called us.
	for name, in := range map[string]string{"empty": "", "whitespace": "  \n\t "} {
		t.Run(name, func(t *testing.T) {
			if got, saw := hookFileFromStdin(strings.NewReader(in)); got != "" || saw {
				t.Fatalf("%s must be (\"\", false), got (%q, %v)", name, got, saw)
			}
		})
	}
	if got, saw := hookFileFromStdin(nil); got != "" || saw {
		t.Fatalf("a nil reader must be (\"\", false), got (%q, %v)", got, saw)
	}
}

func TestHookPayloadRejectsOversizedInputWithoutParsingItsPrefix(t *testing.T) {
	const limit = 1 << 20
	const payload = `{"tool_input":{"file_path":"/repo/main.go"}}`
	boundary := payload + strings.Repeat(" ", limit-len(payload))
	if got, saw := hookFileFromStdin(strings.NewReader(boundary)); got != "/repo/main.go" || !saw {
		t.Fatalf("payload at the limit: path=%q saw=%v", got, saw)
	}
	for _, oversized := range []string{boundary + " ", boundary + `{"tool_input":{"file_path":"/repo/other.go"}}`} {
		if got, saw := hookFileFromStdin(strings.NewReader(oversized)); got != "" || !saw {
			t.Fatalf("oversized payload must be silent, not parsed as its valid prefix: path=%q saw=%v", got, saw)
		}
	}
}

// The wiring must not reference a variable the harness does not define. A path
// that expands to "" is indistinguishable from a working hook until someone
// checks whether a fact was ever served.
func TestInstalledWiringDoesNotReferenceANonexistentVariable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if _, err := MergePreEditHookForTest(root, "entire brain"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	wiring := readSettingsForTest(t, root)
	if strings.Contains(wiring, "CLAUDE_FILE_PATH") {
		t.Error("the wiring passes $CLAUDE_FILE_PATH, which Claude Code does not define; " +
			"it expands to \"\" and the hook serves nothing on every edit")
	}
	if !strings.Contains(wiring, "hook pre-edit") {
		t.Fatalf("the pre-edit hook is not wired at all:\n%s", wiring)
	}
}

// END-TO-END. The helper tests above all stayed green when the CALL SITE was
// neutered, which is the defect class where the bug lives in the wiring between
// two correct functions rather than in either of them.
//
// This drives the real command over a real brain holding a real fact, with the
// path delivered the way a harness delivers it: JSON on stdin, no --file.
func TestPreEditServesFactsFromAHarnessPayloadOnStdin(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("internal/cli/distill_cmd.go owns the distill flag surface.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	payload := `{"hook_event_name":"PreToolUse","tool_name":"Edit",` +
		`"tool_input":{"file_path":"internal/cli/distill_cmd.go"}}`

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetIn(strings.NewReader(payload))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"hook", "pre-edit"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hook pre-edit with a stdin payload and no --file: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "distill flag surface") {
		t.Fatalf("no fact was served from the harness payload; the hook is installed and inert:\n%s", out.String())
	}
}

// Misuse stays loud and an unreadable payload stays silent, driven through the
// real command rather than the helper.
func TestPreEditCommandDistinguishesMisuseFromAnUnreadablePayload(t *testing.T) {
	f := newVerifyFixture(t)
	run := func(stdin string) error {
		var out bytes.Buffer
		cmd := NewRootCommand(f.opts)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"hook", "pre-edit"})
		return cmd.Execute()
	}
	if err := run(""); err == nil {
		t.Error("no stdin and no --file must be an error, so a human hand-testing the verb is told")
	}
	if err := run("not a hook payload at all"); err != nil {
		t.Errorf("an unrecognised payload must be silence, got: %v", err)
	}
}

// hookFileFromStdin does io.ReadAll, which on a real TTY waits for EOF. So
// running the verb by hand with no --file hung indefinitely where it previously
// failed fast with "--file is required". The existing tests did not catch it
// because they supply a strings.Reader, which EOFs at once.
//
// THE HANG ITSELF IS NOT REPRODUCIBLE IN-PROCESS: it needs the process's real
// stdin to be a character device, and `go test` does not provide one. Saying so
// rather than dressing up a weaker check as a behavioural test. What is
// testable is the predicate and that the call site consults it.
func TestStdinIsReadUnlessItWouldBlockOnAPerson(t *testing.T) {
	t.Parallel()

	// A harness always redirects stdin, and a test supplies its own reader.
	// Neither must ever be skipped, or the hook stops working entirely.
	for name, r := range map[string]io.Reader{
		"harness payload": strings.NewReader(`{"tool_input":{"file_path":"/a/b.go"}}`),
		"empty reader":    strings.NewReader(""),
		"nil reader":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			if hookStdinWouldBlock(r) {
				t.Error("a reader that is not the process's stdin must always be read")
			}
		})
	}

	// And for os.Stdin the answer must track whether it is a terminal -- which
	// is the whole decision. Compared against the helper rather than asserted
	// to a constant, because the test runner's stdin is not ours to assume.
	if got, want := hookStdinWouldBlock(os.Stdin), stdinIsTerminal(); got != want {
		t.Errorf("hookStdinWouldBlock(os.Stdin) = %v, want stdinIsTerminal() = %v", got, want)
	}
}

// The call site must consult the predicate. A correct predicate nothing calls
// leaves the hang exactly as it was, and that class of gap has already bitten
// this stack more than once.
func TestTheHookCallSiteGuardsTheStdinRead(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("hook_cmd.go")
	if err != nil {
		t.Fatalf("read hook_cmd.go: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "func hookStdinWouldBlock") {
		t.Fatal("read the wrong file; this guard would be vacuous")
	}
	if !strings.Contains(src, `!hookStdinWouldBlock(cmd.InOrStdin())`) {
		t.Error("the stdin read is not gated, so `hook pre-edit` with no --file hangs on a terminal " +
			"instead of reporting that --file is required")
	}
}

func TestPreEditMatchesRepositoryPathsFromAbsolutePayloads(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("`internal/cli/distill_cmd.go` owns the distill flag surface.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		want       bool
	}{
		{"relative", "internal/cli/distill_cmd.go", true},
		{"absolute", filepath.Join(f.repoDir, "internal", "cli", "distill_cmd.go"), true},
		{"unrelated", filepath.Join(f.repoDir, "other.go"), false},
		{"outside", filepath.Join(t.TempDir(), "internal", "cli", "distill_cmd.go"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"tool_input": map[string]string{"file_path": tc.path}})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := NewRootCommand(f.opts)
			cmd.SetIn(bytes.NewReader(payload))
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"hook", "pre-edit"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(out.String(), "distill flag surface"); got != tc.want {
				t.Fatalf("served=%v, want %v: %s", got, tc.want, out.String())
			}
		})
	}
}
