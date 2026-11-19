package setupui

import (
	"os/exec"
	"strings"
	"testing"
)

// TestEveryGoSourceFileIsPinnedToLF guards the gofmt step of
// `mise run test:prepush` on Windows.
//
// gofmt always emits LF, so it reports a CRLF file as unformatted on content
// nobody touched. core.autocrlf=true is the Git for Windows default, and with
// *.go unpinned a stock Windows clone checks out every Go file with CRLF:
// `gofmt -l -s .` then lists all of them, and the pre-push gate
// CONTRIBUTING.md asks for cannot pass on that clone at all. Nothing a pull
// request is checked on can see this, because the CI runners check out LF.
//
// It lives beside TestEveryGoldenIsPinnedToLF so that the repository's two
// end-of-line class guards sit together, where someone editing .gitattributes
// will meet both. Like that guard it asserts a property of every file in the
// class rather than of the paths somebody remembered.
func TestEveryGoSourceFileIsPinnedToLF(t *testing.T) {
	t.Parallel()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))

	listed, err := exec.Command("git", "-C", dir, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	sources := strings.Fields(string(listed))
	if len(sources) == 0 {
		t.Fatal("non-vacuity: found no Go files at all, so this proves nothing")
	}

	// check-attr takes the paths as arguments, and this repository has over a
	// thousand of them, so feed them on stdin instead of blowing the argument
	// limit as the list grows.
	command := exec.Command("git", "-C", dir, "check-attr", "--stdin", "eol")
	command.Stdin = strings.NewReader(strings.Join(sources, "\n"))
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
		head := unpinned
		if len(head) > 10 {
			head = head[:10]
		}
		t.Fatalf("%d Go source files are not pinned to LF, so a default Windows clone (core.autocrlf=true) checks them out with CRLF and `gofmt -l -s .` reports every one of them:\n  %s\n  ...\nadd `*.go text eol=lf` to .gitattributes", len(unpinned), strings.Join(head, "\n  "))
	}
	t.Logf("checked %d Go source files", len(sources))
}
