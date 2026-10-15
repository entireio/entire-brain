package cli

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// windowsCommandLineLen approximates the character count cmd.exe measures
// against its 8191-character ceiling: every argument quoted, separated by one
// space. It is deliberately a slight over-estimate so the assertion below fails
// before a real Windows run would.
func windowsCommandLineLen(argv []string) int {
	n := 0
	for _, arg := range argv {
		n += len(arg) + 3 // surrounding quotes plus the separating space
	}
	return n
}

// windowsCmdLineLimit is cmd.exe's hard cap. On Windows the npm-installed
// `claude` and `codex` are `.cmd` shims, so Go's exec runs them through
// cmd.exe and inherits this limit even though CreateProcess itself allows
// 32767 (issue #322).
const windowsCmdLineLimit = 8191

// TestDistillAgentCommandArgsKeepsPromptOffArgv is the regression guard for
// issue #322: `entire brain distill --agent claude-code` failed on every
// Windows machine with "The command line is too long" because the ~7.5 KiB
// rendered distillation prompt was passed as an argv element.
//
// It is deliberately platform-independent — it asserts the property that makes
// the Windows failure impossible (the prompt is not a process argument, and
// the resulting command line fits in cmd.exe's budget) rather than exercising
// cmd.exe, which cannot be done from macOS or Linux.
func TestDistillAgentCommandArgsKeepsPromptOffArgv(t *testing.T) {
	t.Parallel()
	prompt, err := renderDistillPrompt(defaultFactTaxonomy(time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("renderDistillPrompt: %v", err)
	}
	// Non-vacuity: the guard is only meaningful if the realistic prompt is big
	// enough to have blown the limit on its own.
	if len(prompt) < 7000 {
		t.Fatalf("distill prompt is only %d bytes; this test no longer reproduces the issue-322 condition", len(prompt))
	}
	// A distinctive slice of the prompt, used to prove no argv element smuggles
	// the prompt through under another flag.
	needle := prompt[:200]

	for _, agent := range []string{"codex", "claude-code"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()
			args, err := distillAgentCommandArgs(agent, nil, prompt)
			if err != nil {
				t.Fatalf("distillAgentCommandArgs(%s): %v", agent, err)
			}
			// distill always pins a model and an effort, so measure what really ships.
			args = injectAgentEffort(injectAgentModel(args, agent, "claude-sonnet-4-5-20260514"), agent, "low")

			transcript := []byte("1| transcript line one\n2| transcript line two\n")
			argv, stdin, cleanup, err := prepareAgentExec(args, transcript)
			if err != nil {
				t.Fatalf("prepareAgentExec(%s): %v", agent, err)
			}
			defer cleanup()

			for i, arg := range argv {
				if strings.Contains(arg, needle) {
					t.Fatalf("%s: system prompt is on argv at index %d — this is the issue-322 failure", agent, i)
				}
			}
			if n := windowsCommandLineLen(argv); n > windowsCmdLineLimit {
				t.Fatalf("%s: command line is %d chars, over cmd.exe's %d limit: %v", agent, n, windowsCmdLineLimit, argv)
			}
			// Guard the guard: without the fix this same argv would have been over
			// the limit, so the assertion above is not trivially satisfiable.
			if n := windowsCommandLineLen(argv) + len(prompt) + 3; n <= windowsCmdLineLimit {
				t.Fatalf("%s: argv+prompt is only %d chars; the limit could not have been hit and this test proves nothing", agent, n)
			}

			// The prompt must still reach the agent, out of band.
			switch agent {
			case "claude-code":
				path := ""
				for i := 0; i+1 < len(argv); i++ {
					if argv[i] == "--system-prompt-file" {
						path = argv[i+1]
					}
				}
				if path == "" {
					t.Fatalf("claude argv must carry --system-prompt-file: %v", argv)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read staged system prompt: %v", err)
				}
				if string(data) != prompt {
					t.Fatalf("staged system prompt does not match the rendered prompt (%d vs %d bytes)", len(data), len(prompt))
				}
				if info, err := os.Stat(path); err != nil {
					t.Fatalf("stat staged system prompt: %v", err)
				} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
					t.Fatalf("staged system prompt is %v, want 0600 — prompt text must not be world-readable", info.Mode().Perm())
				}
				if string(stdin) != string(transcript) {
					t.Fatalf("claude stdin must stay the transcript chunk, got %q", string(stdin))
				}
				cleanup()
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("cleanup must remove the staged system prompt, stat err=%v", err)
				}
			case "codex":
				if argv[len(argv)-1] != "-" {
					t.Fatalf("codex argv must end with `-` so instructions are read from stdin: %v", argv)
				}
				if !strings.HasPrefix(string(stdin), prompt) {
					t.Fatalf("codex stdin must start with the system prompt")
				}
				if !strings.Contains(string(stdin), string(transcript)) {
					t.Fatalf("codex stdin must still carry the transcript chunk")
				}
			}
		})
	}
}

// TestPrepareAgentExecLeavesUnmarkedArgvAlone pins the pass-through: a
// `--agent command` wrapper (which never received the prompt) and the
// handcrafted argv used elsewhere must not be rewritten.
func TestPrepareAgentExecLeavesUnmarkedArgvAlone(t *testing.T) {
	t.Parallel()
	in := []string{"my-agent", "--flag"}
	argv, stdin, cleanup, err := prepareAgentExec(in, []byte("chunk"))
	if err != nil {
		t.Fatalf("prepareAgentExec: %v", err)
	}
	defer cleanup()
	if strings.Join(argv, "\x00") != strings.Join(in, "\x00") {
		t.Fatalf("unmarked argv was rewritten: %v", argv)
	}
	if string(stdin) != "chunk" {
		t.Fatalf("unmarked stdin was rewritten: %q", string(stdin))
	}
}

// TestDistillAgentCommandArgsOllamaKeepsPromptInline pins the one agent that
// intentionally keeps its prompt inside the builder slice. execOllamaDistillAgent
// spawns no process — it POSTs to loopback /api/generate and reads args[2] as the
// JSON `system` field — so no command line exists and no OS limit applies. If the
// marker is ever added there, that runner must learn to split it or it will send
// the marker string to the model as the system prompt.
func TestDistillAgentCommandArgsOllamaKeepsPromptInline(t *testing.T) {
	t.Parallel()
	args, err := distillAgentCommandArgs("ollama", nil, "PROMPT")
	if err != nil {
		t.Fatalf("distillAgentCommandArgs(ollama): %v", err)
	}
	if _, _, ok := splitAgentPromptArg(args); ok {
		t.Fatalf("ollama args gained the off-argv marker; teach execOllamaDistillAgent to split it: %v", args)
	}
	if len(args) != 3 || args[0] != "ollama" || args[2] != "PROMPT" {
		t.Fatalf("execOllamaDistillAgent reads the system prompt from args[2]: %v", args)
	}
}
