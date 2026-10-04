package agentsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A brain nothing reads is the product's worst failure mode, and it is SILENT:
// setup succeeds, every source reads healthy, and no agent consults the brain
// because nothing told it the brain exists. The published guide teaches
// `setup` then `status` without `init-agents`, so a reader following it lands
// here by default and concludes the product does not work.
func TestAgentsWiredDetectsAnUnwiredRepository(t *testing.T) {
	t.Parallel()

	// Nothing installed: both halves missing.
	bare := t.TempDir()
	wired, missing := AgentsWired(bare)
	if wired {
		t.Error("a repository with no guide and no pointer is not wired")
	}
	if len(missing) != 2 {
		t.Errorf("both halves should be named, got %v", missing)
	}

	// The guide alone is not enough -- nothing points at it.
	guideOnly := t.TempDir()
	writeFile(t, filepath.Join(guideOnly, ".entire", "agent-guide.md"), "# guide\n")
	if wired, missing := AgentsWired(guideOnly); wired {
		t.Error("a guide no file points at does not wire anything")
	} else if len(missing) != 1 || !strings.Contains(missing[0], "AGENTS.md") {
		t.Errorf("the missing half should be the pointer, got %v", missing)
	}

	// A pointer with no guide is equally incomplete.
	pointerOnly := t.TempDir()
	writeFile(t, filepath.Join(pointerOnly, "CLAUDE.md"), Pointer)
	if wired, missing := AgentsWired(pointerOnly); wired {
		t.Error("a pointer at a guide that does not exist is not wired")
	} else if len(missing) != 1 || !strings.Contains(missing[0], "agent-guide.md") {
		t.Errorf("the missing half should be the guide, got %v", missing)
	}

	// Either instruction file satisfies it: they are documented aliases, and a
	// repo may legitimately keep only the one its agent reads.
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, ".entire", "agent-guide.md"), "# guide\n")
		writeFile(t, filepath.Join(root, name), Pointer)
		if wired, missing := AgentsWired(root); !wired {
			t.Errorf("%s alone should satisfy the pointer, missing=%v", name, missing)
		}
	}

	// A file that exists but carries someone's own text, not the managed
	// block, does not count.
	unmanaged := t.TempDir()
	writeFile(t, filepath.Join(unmanaged, ".entire", "agent-guide.md"), "# guide\n")
	writeFile(t, filepath.Join(unmanaged, "AGENTS.md"), "# my own instructions\n")
	if wired, _ := AgentsWired(unmanaged); wired {
		t.Error("an AGENTS.md without the managed block does not point at the guide")
	}

	// No root is not a claim either way, and must not report "wired".
	if wired, _ := AgentsWired(""); wired {
		t.Error("an empty root must not report wired")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
