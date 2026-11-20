package setupui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryShellScriptIsPinnedToLF guards the scripts a contributor runs.
//
// A CRLF checkout leaves a CR at the end of every line, and `sh` reads that CR
// as part of the last word on the line. The script then dies before doing any
// work: a CRLF copy of mise-tasks/lint/gofmt fails at line 3 with
// ": command not found", one of scripts/install-local.sh at line 2 with
// "set: -: invalid option", both exit 2. core.autocrlf=true is the Git for
// Windows default, so this is what a stock Windows clone gets. No CI check can
// see it, because the runners check out LF.
//
// The class is "a shell will execute this", not "the name ends in .sh": six of
// this repository's scripts are the mise tasks, which carry shebangs and no
// extension at all. Asking the filesystem what starts with "#!" is what keeps a
// script added somewhere new from being quietly unprotected -- the lesson of
// TestEveryGoldenIsPinnedToLF, beside which this lives.
func TestEveryShellScriptIsPinnedToLF(t *testing.T) {
	t.Parallel()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))

	listed, err := exec.Command("git", "-C", dir, "ls-files").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var scripts []string
	for _, rel := range strings.Split(string(listed), "\n") {
		if rel == "" {
			continue
		}
		if strings.HasSuffix(rel, ".sh") {
			scripts = append(scripts, rel)
			continue
		}
		// Everything a shell will run, found the only way that cannot be
		// fooled by a name: read the first two bytes.
		f, err := os.Open(filepath.Join(dir, rel))
		if err != nil {
			continue // not checked out here; nothing to pin
		}
		var head [2]byte
		n, _ := f.Read(head[:])
		f.Close()
		if n == 2 && head[0] == '#' && head[1] == '!' {
			scripts = append(scripts, rel)
		}
	}
	if len(scripts) == 0 {
		t.Fatal("non-vacuity: found no shell scripts at all, so this proves nothing")
	}

	// check-attr takes the paths as arguments; feed them on stdin so a growing
	// list cannot blow the argument limit.
	command := exec.Command("git", "-C", dir, "check-attr", "--stdin", "eol")
	command.Stdin = strings.NewReader(strings.Join(scripts, "\n"))
	out, err := command.Output()
	if err != nil {
		t.Fatalf("git check-attr: %v", err)
	}
	var unpinned []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		// "<path>: eol: <value>"
		if !strings.HasSuffix(line, ": eol: lf") {
			unpinned = append(unpinned, line)
		}
	}
	if len(unpinned) > 0 {
		t.Fatalf("%d shell scripts are not pinned to LF, so a default Windows clone (core.autocrlf=true) checks them out with CRLF and `sh` fails on them before they run:\n  %s\nadd a rule to .gitattributes covering them", len(unpinned), strings.Join(unpinned, "\n  "))
	}
	t.Logf("checked %d shell scripts", len(scripts))
}
