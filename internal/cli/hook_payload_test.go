package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/hook"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// END-TO-END. The helper tests (command/hook) all stayed green when the CALL
// SITE was neutered, which is the defect class where the bug lives in the
// wiring between two correct functions rather than in either of them.
//
// This drives the real command over a real brain holding a real fact, with the
// path delivered the way a harness delivers it: JSON on stdin, no --file.
func TestPreEditServesFactsFromAHarnessPayloadOnStdin(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("internal/cli/distill_cmd.go owns the distill flag surface.", "main", f.now)
	if err := factstore.Write(f.brainDir, "main", []factRecord{fact}); err != nil {
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
	// Silence is gated on --claude-hook, which is how the REAL hook invokes
	// this verb. That is a better contract than the unconditional silence this
	// test used to assert: the hook must never break an agent's edit over a
	// payload it cannot read, and a human hand-running the verb must still be
	// told what they got wrong.
	runHook := func(stdin string) error {
		var out bytes.Buffer
		cmd := NewRootCommand(f.opts)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"hook", "pre-edit", "--claude-hook"})
		return cmd.Execute()
	}
	if err := runHook("not a hook payload at all"); err != nil {
		t.Errorf("the real hook must stay silent on a payload it cannot read, got: %v", err)
	}
	if err := run("not a hook payload at all"); err == nil {
		t.Error("a hand-run with no --file must still say so, or the verb is undebuggable by hand")
	}
}

// The call site must gate the read on whether stdin is actually readable.
//
// Asserted through the predicate and the fast-fail path, not by grepping
// hook.go for an identifier: that earlier form pinned a NAME, and when the
// predicate was renamed and inverted the guard went red on correct code.
//
// A terminal is a character device, so it is not a pipe and must never be
// read; a pipe is the harness and must be. The hang this prevents is reading
// the first, not the second.
func TestTheHookCallSiteGuardsTheStdinRead(t *testing.T) {
	t.Parallel()

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	if !hook.StdinIsPipe(pr) {
		t.Error("a pipe is the harness handing over a payload; it must be read")
	}

	// The fast-fail path: --claude-hook with no --file and nothing pipeable
	// must return promptly rather than read a stream that never EOFs.
	f := newVerifyFixture(t)
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		cmd := NewRootCommand(f.opts)
		cmd.SetIn(neverEOFReader{})
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"hook", "pre-edit", "--file", "internal/cli/command/hook/hook.go", "--claude-hook"})
		done <- cmd.Execute()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("`hook pre-edit --file ... --claude-hook` read stdin it was never meant to touch")
	}
}

// neverEOFReader stands in for a terminal: a read that never returns.
type neverEOFReader struct{}

func (neverEOFReader) Read([]byte) (int, error) { select {} }

func TestPreEditMatchesRepositoryPathsFromAbsolutePayloads(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("`internal/cli/distill_cmd.go` owns the distill flag surface.", "main", f.now)
	if err := factstore.Write(f.brainDir, "main", []factRecord{fact}); err != nil {
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
