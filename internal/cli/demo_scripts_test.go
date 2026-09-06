package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The demo scripts run every `entire` and `entire-brain` verb through a
// sandbox() helper built on `env -i`, which means the sandbox's env block is
// the ONLY place a child's environment comes from. It sets TERM, so under a
// captured pty (`script -q run.log ./scripts/demo-agent-session.sh` — the way
// these demos are recorded and validated) stdout is a real terminal and
// `entire checkpoint list` pages its output through $PAGER. With PAGER unset
// that is `less` (cmd/entire/cli/explain.go: outputWithPager -> buildPagerCmd),
// which blocks on "Press RETURN to continue" and hangs the demo indefinitely
// mid-run — observed twice, at ~42 and ~59 minutes, with no further output.
//
// This is a shell fix with no Go call path, so it has no other regression
// guard: nothing in the build or the test suite notices if the three
// non-blocking env entries are dropped from sandbox() again. Hence this test,
// which reads the shipped scripts and fails if they are.
func TestDemoScriptSandboxesKeepEveryChildNonBlocking(t *testing.T) {
	t.Parallel()

	// Set in sandbox()'s `env -i` block, these cover every child and
	// grandchild in one place, rather than a per-command --no-pager flag that
	// the next paging verb would silently miss.
	required := []string{
		"PAGER=cat",             // `entire` reads PAGER; `cat` never waits for input
		"GIT_PAGER=cat",         // same for any git child that pages
		"GIT_TERMINAL_PROMPT=0", // a git child wanting credentials must fail, not block
	}

	for _, name := range []string{"demo-agent-session.sh", "demo-setup.sh"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join("..", "..", "scripts", name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			body, err := sandboxEnvBlock(string(raw))
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, entry := range required {
				if !strings.Contains(body, entry) {
					t.Errorf("%s: sandbox() `env -i` block does not set %s.\n"+
						"Without it a child of this sandbox can block on the pty and the demo "+
						"hangs forever mid-run instead of finishing unattended.\nblock was:\n%s",
						path, entry, body)
				}
			}
			if name == "demo-agent-session.sh" && !strings.Contains(body, `${PATH:-`) {
				t.Errorf("%s: sandbox() discards the caller's PATH; git and other prerequisites outside system directories become unreachable\nblock was:\n%s", path, body)
			}
		})
	}
}

func TestDemoSetupSerializesConcurrentStubCounters(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "scripts", "demo-setup.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `mkdir "$counter_lock"`) || !strings.Contains(body, `rmdir "$counter_lock"`) {
		t.Fatalf("demo stub counter is not protected by an atomic lock")
	}
}

func TestInstallChecksCompilerEvenWhenGoIsMissing(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "scripts", "install.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	goCheck := strings.Index(body, "if ! have go; then")
	compilerCheck := strings.Index(body, `if ! have "$go_cc"`)
	problemsCheck := strings.Index(body, `if [ -n "$problems" ]`)
	if goCheck < 0 || compilerCheck < goCheck || problemsCheck < compilerCheck {
		t.Fatalf("compiler prerequisite is not checked independently before preflight exits")
	}
	if strings.Contains(body, `rm -rf "$graph_cache"`) {
		t.Fatal("installer may delete an arbitrary user-supplied cache path")
	}
	if !strings.Contains(body, `-f "$graph_root/$graph_cache_marker"`) {
		t.Fatal("installer refreshes cache checkouts without proving it created them")
	}
}

// Once `entire enable` has run in scripts/demo-agent-session.sh, the demo
// repository carries Entire's git hooks, so EVERY later `git commit` runs
// `entire` as a hook child. Two things then depend on that commit going through
// sandbox():
//
//   - the prepare-commit-msg hook asks "Link this commit to session context?
//     [Y]es / [n]o / [a]lways" on /dev/tty — the CONTROLLING TERMINAL, not
//     stdin — so no input redirection can answer it and the demo blocks forever
//     on a human's terminal. sandbox()'s GIT_TERMINAL_PROMPT=0 is what the CLI
//     reads as "this caller cannot answer prompts"
//     (cmd/entire/cli/interactive/interactive.go, isAgentSubprocessEnv), and it
//     auto-links instead of asking.
//   - an unsandboxed hook child runs against the developer's REAL HOME and
//     ENTIRE_CONFIG_DIR, which is exactly the leak the sandbox exists to stop.
func TestDemoAgentSessionCommitsRunInsideTheSandbox(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "scripts", "demo-agent-session.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	commits := 0
	for _, line := range logicalShellLines(string(raw)) {
		if !strings.Contains(line, "git commit") {
			continue
		}
		commits++
		if !strings.Contains(line, "sandbox ") {
			t.Errorf("%s: this `git commit` does not run through sandbox(), so its "+
				"Entire git hooks get the real HOME and can block on /dev/tty:\n\t%s", path, line)
		}
	}
	// Guard the guard: if the commits are ever renamed out from under this
	// matcher, an empty sweep must not read as a pass.
	if commits < 2 {
		t.Fatalf("%s: expected at least 2 `git commit` invocations to check, found %d — "+
			"the matcher has drifted from the script", path, commits)
	}
}

// logicalShellLines drops comment lines and joins backslash continuations, so a
// run_step spread over two lines is matched as the single command it is.
func logicalShellLines(script string) []string {
	var out []string
	var pending strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, "\\") {
			pending.WriteString(strings.TrimSuffix(trimmed, "\\"))
			pending.WriteString(" ")
			continue
		}
		pending.WriteString(strings.TrimSpace(line))
		out = append(out, pending.String())
		pending.Reset()
	}
	if pending.Len() > 0 {
		out = append(out, pending.String())
	}
	return out
}

// sandboxEnvBlock returns the text of the `env -i ... "$@")` assignment block
// inside the script's sandbox() helper. Scoping the assertion to that block is
// the point: the same three strings appearing in a comment, or exported for
// one command only, would not protect the other children.
func sandboxEnvBlock(script string) (string, error) {
	const marker = "sandbox() {"
	start := strings.Index(script, marker)
	if start < 0 {
		return "", errNoSandboxHelper
	}
	rest := script[start:]
	envAt := strings.Index(rest, "env -i")
	if envAt < 0 {
		return "", errNoSandboxEnv
	}
	rest = rest[envAt:]
	end := strings.Index(rest, `"$@")`)
	if end < 0 {
		return "", errNoSandboxEnvEnd
	}
	return rest[:end], nil
}

var (
	errNoSandboxHelper = &demoScriptError{"no sandbox() helper found"}
	errNoSandboxEnv    = &demoScriptError{"sandbox() has no `env -i` block"}
	errNoSandboxEnvEnd = &demoScriptError{"sandbox()'s `env -i` block is unterminated"}
)

type demoScriptError struct{ msg string }

func (e *demoScriptError) Error() string { return e.msg }
