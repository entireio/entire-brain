package setupui

import (
	"os/exec"
	"strings"
	"testing"
)

// TestEveryGoldenIsPinnedToLF is a class guard, not a file guard.
//
// internal/setupui/testdata/pipe.golden shipped unpinned because
// .gitattributes named internal/cli/testdata only. A Windows checkout turned
// it into CRLF, the generated output stayed LF, and three Windows jobs failed
// on a byte comparison that is correct on every other platform. A
// path-specific rule protects the directory someone remembered; this asserts
// the property for every golden in the repository.
func TestEveryGoldenIsPinnedToLF(t *testing.T) {
	t.Parallel()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))

	listed, err := exec.Command("git", "-C", dir, "ls-files", "*.golden", "*.golden*").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	goldens := strings.Fields(string(listed))
	if len(goldens) == 0 {
		t.Fatal("non-vacuity: found no golden files at all, so this proves nothing")
	}

	args := append([]string{"-C", dir, "check-attr", "eol", "--"}, goldens...)
	out, err := exec.Command("git", args...).Output()
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
		t.Fatalf("these goldens are not pinned to LF and will become CRLF on a Windows checkout:\n  %s\nadd a rule to .gitattributes", strings.Join(unpinned, "\n  "))
	}
	t.Logf("checked %d golden files", len(goldens))
}
