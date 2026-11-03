package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// The exact shape Claude Code 2.1.314 delivers for PostToolUseFailure, read out
// of the engine's own type declarations for this build and confirmed present in
// the installed binary. Kept verbatim as the fixture so a host change shows up
// here as a failing test rather than as a hook that silently stops matching.
const realFailureEvent = `{"session_id":"20373dcb","transcript_path":"/x/y.jsonl",
  "cwd":"/repo","permission_mode":"default",
  "hook_event_name":"PostToolUseFailure","tool_name":"Bash",
  "tool_input":{"command":"go test ./internal/entityindex/","description":"run tests"},
  "tool_use_id":"toolu_01","duration_ms":4210,
  "error":"--- FAIL: TestMigrate (0.12s)\n    graphdiff.go:126: migration graph diff has warnings"}`

func TestFailureEventCarriesTheCommandAndTheError(t *testing.T) {
	t.Parallel()

	got, ok := DecodeClaudeFailureEvent(strings.NewReader(realFailureEvent))
	if !ok {
		t.Fatal("the real payload must decode")
	}
	if got.Command != "go test ./internal/entityindex/" {
		t.Errorf("command = %q, want tool_input.command", got.Command)
	}
	if !strings.Contains(got.Failure, "migration graph diff has warnings") {
		t.Errorf("failure text = %q, want the error field", got.Failure)
	}
}

// An interrupt is the user pressing escape. The host still reports whatever the
// tool had emitted, so an is_interrupt event can carry a plausible-looking
// error -- which is exactly why this is checked BEFORE the error text. Reading
// it as a failure means the brain interjects at the moment a person has taken
// over, which is the behaviour that gets a hook deleted.
func TestFailureEventTreatsAnInterruptAsSilence(t *testing.T) {
	t.Parallel()

	const interrupted = `{"hook_event_name":"PostToolUseFailure","tool_name":"Bash",
	  "tool_input":{"command":"go test ./..."},"is_interrupt":true,
	  "error":"go test ./...\n^C interrupted by user"}`

	if got, ok := DecodeClaudeFailureEvent(strings.NewReader(interrupted)); ok {
		t.Fatalf("an interrupt must be silent, got %+v", got)
	}
}

// Everything a hook must say nothing about. Each of these reaches the verb in
// normal operation, and every one of them has to exit 0 with no output.
func TestFailureEventSilentCases(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"not json":           "this is not json at all",
		"truncated":          `{"error":"boom"`,
		"no error field":     `{"hook_event_name":"PostToolUseFailure","tool_name":"Bash"}`,
		"blank error":        `{"error":"   \n\t "}`,
		"wrong error type":   `{"error":123}`,
		"empty object":       `{}`,
		"empty":              "",
		"whitespace":         "   \n\t ",
		"interrupt no error": `{"is_interrupt":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := DecodeClaudeFailureEvent(strings.NewReader(in)); ok {
				t.Fatalf("%s must be silent, got %+v", name, got)
			}
		})
	}
	if got, ok := DecodeClaudeFailureEvent(nil); ok {
		t.Fatalf("a nil reader must be silent, got %+v", got)
	}
}

// Not every failing tool names a command or a path, and the tool name is still
// worth matching on: a memory can name the tool itself ("WebFetch is blocked on
// this host"). Falling back to "" instead would drop that half of the query.
func TestFailureEventFallsBackThroughCommandThenPathThenToolName(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ event, want string }{
		"command wins over path": {
			`{"tool_name":"Bash","tool_input":{"command":"make build","file_path":"/repo/x.go"},"error":"e"}`,
			"make build",
		},
		"path when no command": {
			`{"tool_name":"Edit","tool_input":{"file_path":"internal/cli/hook.go"},"error":"e"}`,
			"internal/cli/hook.go",
		},
		"tool name when neither": {
			`{"tool_name":"WebFetch","tool_input":{"url":"https://example.test"},"error":"e"}`,
			"WebFetch",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := DecodeClaudeFailureEvent(strings.NewReader(tc.event))
			if !ok {
				t.Fatalf("%s must decode", name)
			}
			if got.Command != tc.want {
				t.Errorf("command = %q, want %q", got.Command, tc.want)
			}
		})
	}
}

// The tail is capped like the piped path, and the cut must not split a rune:
// this text goes into an agent's context and is read by a person debugging a
// hook, so a leading U+FFFD is a defect even though it is harmless.
func TestFailureTailIsCappedOnARuneBoundary(t *testing.T) {
	t.Parallel()

	if got := failureTail("short"); got != "short" {
		t.Errorf("a short failure must pass through, got %q", got)
	}
	// The rune width has to be chosen so the byte cut lands MID-rune, or this
	// test cannot fail. A 2-byte rune does not: hookFailureTailBytes is even,
	// so the offset is always a boundary and the assertion holds whether the
	// guard exists or not -- which is how the first version of this test
	// passed with the guard deleted. 3 bytes does not divide it, so "€" puts
	// the cut one byte into a rune.
	if hookFailureTailBytes%3 == 0 {
		t.Fatalf("hookFailureTailBytes (%d) is now divisible by 3, so this fixture no longer cuts mid-rune; pick a rune width that does not divide it", hookFailureTailBytes)
	}
	oversized := strings.Repeat("€", hookFailureTailBytes) // 3 bytes each
	got := failureTail(oversized)
	if len(got) > hookFailureTailBytes {
		t.Errorf("tail is %d bytes, want <= %d", len(got), hookFailureTailBytes)
	}
	if !utf8.ValidString(got) {
		t.Errorf("tail is not valid UTF-8, so it was cut mid-rune: %q", got[:12])
	}
	if !strings.HasSuffix(oversized, got) {
		t.Error("the tail must be a suffix of the failure text")
	}
}

// THE BUG THIS PINS: the envelope is keyed by event name, and a host silently
// drops one whose key does not match the event it fired. Emitting a PreToolUse
// envelope from a PostToolUseFailure hook would be installed-and-inert -- the
// precise failure the pre-edit wiring already shipped once, for eight days.
func TestPostFailureClaudeHookAnswersWithThePostToolUseFailureEnvelope(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := t.TempDir()

	facts := []factmerge.Record{{
		ID:     "fact:0001",
		Paths:  []string{"project.regression.gotcha"},
		Kind:   "gotcha",
		Text:   "migration graph diff refuses on warnings that name untouched files",
		Locus:  []string{"graphdiff", "migration", "warnings"},
		Branch: "main",
		Origin: "authored",
		Status: "active",
	}}
	if err := factstore.Write(brainDir, facts); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	var servedSurface string
	cfg := Config{Deps: Deps{
		ResolveTarget: func(context.Context, base.Options, string) (string, string, string, error) {
			return repoDir, brainDir, "main", nil
		},
		// Well above hookPostFailureMinScore, so the match is not what is
		// under test here -- the envelope shape is.
		FactLexicalScore: func(factmerge.Record, string, []string) int { return 500 },
		RecordServed: func(_ io.Writer, _ context.Context, _ base.Options, _, _, surface, _, _ string, served []factmerge.Record) {
			servedSurface = surface
			if len(served) == 0 {
				t.Error("an emission that carried a memory must leave a receipt for it")
			}
		},
	}}

	var out bytes.Buffer
	cmd := NewPostFailure(cfg)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(realFailureEvent))
	cmd.SetArgs([]string{"--claude-hook"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("post-failure --claude-hook: %v", err)
	}

	var envelope struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("the reply is not a hook envelope: %v\n%s", err, out.String())
	}
	if got := envelope.HookSpecificOutput.HookEventName; got != "PostToolUseFailure" {
		t.Errorf("hookEventName = %q, want PostToolUseFailure; the host drops a mismatched key silently", got)
	}
	if !strings.Contains(envelope.HookSpecificOutput.AdditionalContext, "untouched files") {
		t.Errorf("the envelope carries no memory:\n%s", envelope.HookSpecificOutput.AdditionalContext)
	}
	if servedSurface != "hook-post-failure" {
		t.Errorf("receipt surface = %q, want hook-post-failure", servedSurface)
	}
}

// What the --claude-hook guard actually refuses, and what it does not.
//
// hookStdinWouldBlock is `in == os.Stdin && base.StdinIsTerminal()`, so it
// refuses ONLY the process's own stdin when that is a character device. A
// reader handed in by a caller -- a test's, or a harness's pipe -- is always
// read. That is why the subtests below all see silence rather than a refusal:
// SetIn supplies a file that is not os.Stdin.
//
// THE REAL HANG IS NOT REPRODUCIBLE IN-PROCESS: it needs the process's own
// stdin to be a terminal, and `go test` does not provide one. Saying so rather
// than dressing up a weaker check as a behavioural test -- the position this
// package already takes for pre-edit.
//
// And the predicate is imprecise in the direction that matters: measured here,
// os.Stdin under `go test` is /dev/null with mode Dcrw-rw-rw-, and
// base.StdinIsTerminal() answers TRUE for it, because it tests the
// character-device bit rather than calling isatty. That is why the piped path
// carries no guard at all: one built on this predicate refuses ordinary empty
// input, which is this verb's documented silent case.
func TestPostFailureReadsAStdinThatWouldNotBlock(t *testing.T) {
	devNull := func(t *testing.T) *os.File {
		t.Helper()
		file, err := os.Open(os.DevNull)
		if err != nil {
			t.Skipf("no %s on this host: %v", os.DevNull, err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return file
	}

	// THE CONTRACT: an empty emission is SILENT, never an error. A character
	// device that is not a terminal is ordinary empty input.
	t.Run("empty stdin is silent, not misuse", func(t *testing.T) {
		resolved := false
		cfg := Config{Deps: Deps{
			ResolveTarget: func(context.Context, base.Options, string) (string, string, string, error) {
				resolved = true
				return t.TempDir(), t.TempDir(), "main", nil
			},
			FactLexicalScore: func(factmerge.Record, string, []string) int { return 0 },
			RecordServed: func(io.Writer, context.Context, base.Options, string, string, string, string, string, []factmerge.Record) {
			},
		}}
		var out bytes.Buffer
		cmd := NewPostFailure(cfg)
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetIn(devNull(t))
		cmd.SetArgs(nil)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("empty stdin must be silent, got %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("empty stdin must emit nothing, got %q", out.String())
		}
		if resolved {
			t.Error("an empty query must not even open the brain")
		}
	})

	// --claude-hook is the one place a refusal is right, and it diverges from
	// the piped path on purpose. The flag's entire contract is "a host event
	// arrives on stdin", so a reader that cannot carry one is misuse, and
	// saying so beats a hand-run that blocks. pre-edit --claude-hook refuses
	// the same case on the same (imprecise) predicate; matching it keeps the
	// two spellings of one flag behaving alike.
	//
	// The cost is honest: because the predicate cannot tell /dev/null from a
	// terminal, a harness that pipes /dev/null to --claude-hook is refused
	// too. That is acceptable only because a harness wiring this flag always
	// sends an event -- that is what the flag is for.
	t.Run("--claude-hook with a readable but empty stdin is silent", func(t *testing.T) {
		var out bytes.Buffer
		cmd := NewPostFailure(Config{})
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetIn(devNull(t))
		cmd.SetArgs([]string{"--claude-hook"})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("an empty event must be silent, got %v", err)
		}
		if out.Len() != 0 {
			t.Errorf("an empty event must emit nothing, got %q", out.String())
		}
	})

	// --command alone is a legitimate, if weak, query: the verb matches on the
	// command text, and it must reach the brain.
	t.Run("--command alone is accepted", func(t *testing.T) {
		resolved := false
		cfg := Config{Deps: Deps{
			ResolveTarget: func(context.Context, base.Options, string) (string, string, string, error) {
				resolved = true
				return t.TempDir(), t.TempDir(), "main", nil
			},
			FactLexicalScore: func(factmerge.Record, string, []string) int { return 0 },
			RecordServed: func(io.Writer, context.Context, base.Options, string, string, string, string, string, []factmerge.Record) {
			},
		}}
		cmd := NewPostFailure(cfg)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetIn(devNull(t))
		cmd.SetArgs([]string{"--command", "go test ./..."})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("--command alone must be accepted, got %v", err)
		}
		if !resolved {
			t.Error("--command alone never reached the brain, so the query was dropped")
		}
	})

	// The predicate itself, measured rather than assumed. base.StdinIsTerminal
	// tests the CHARACTER-DEVICE bit, so it answers true for /dev/null -- which
	// is why no guard on the piped path can use it without refusing ordinary
	// empty input. Pinned here so a future reader does not "fix" that path with
	// this predicate again.
	t.Run("the predicate cannot tell /dev/null from a terminal", func(t *testing.T) {
		if !StdinIsPipe(strings.NewReader("")) {
			t.Error("a supplied reader must always be readable")
		}
		if hookStdinWouldBlock(strings.NewReader("")) {
			t.Error("a supplied reader must never be treated as blocking")
		}
		// Both predicates key on the char-device bit, so both call /dev/null a
		// terminal. If this ever starts failing, a real isatty landed and the
		// piped-path guard becomes safe to add.
		file := devNull(t)
		if info, err := file.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
			t.Skipf("%s is not a character device here (err %v)", os.DevNull, err)
		}
		if StdinIsPipe(file) {
			t.Error("StdinIsPipe is documented as char-device-based; /dev/null must read as not-a-pipe")
		}
	})
}

// An interrupt must produce NO output at all, not an empty envelope: the host
// adds additionalContext to the agent's context verbatim, so an empty one is
// noise injected at the moment the person took over.
func TestPostFailureClaudeHookStaysSilentOnAnInterrupt(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := t.TempDir()

	var resolved bool
	cfg := Config{Deps: Deps{
		ResolveTarget: func(context.Context, base.Options, string) (string, string, string, error) {
			resolved = true
			return repoDir, brainDir, "main", nil
		},
		FactLexicalScore: func(factmerge.Record, string, []string) int { return 500 },
		RecordServed: func(io.Writer, context.Context, base.Options, string, string, string, string, string, []factmerge.Record) {
			t.Error("an interrupt must not leave a serve receipt")
		},
	}}

	var out bytes.Buffer
	cmd := NewPostFailure(cfg)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"PostToolUseFailure","tool_name":"Bash",
	  "tool_input":{"command":"go test ./..."},"is_interrupt":true,"error":"^C"}`))
	cmd.SetArgs([]string{"--claude-hook"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("an interrupt must exit 0, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("an interrupt must emit nothing, got %q", out.String())
	}
	if resolved {
		t.Error("an interrupt must be decided before the brain is even opened")
	}
}
