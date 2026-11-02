package cli

import (
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// `publish` is retired: the hosted brain-bundle endpoint it posted to was
// never deployed, and memories travel the per-fact wire (`memories sync`).
// Like init-agents, the old invocation must fail loud with the replacement,
// not cobra's generic unknown-command/unknown-flag errors (COR-2290).
func TestPublishTombstone(t *testing.T) {
	t.Setenv(base.EnvCLIVersion, "0.3.0")

	for _, args := range [][]string{
		{"publish"},
		// The removed command's main invocation shapes carried flags; they
		// must reach the removal remedy, not die as "unknown flag".
		{"publish", "--repo-id", "x", "--api-url", "https://example.com", "--token", "t"},
	} {
		cmd := NewRootCommand(Options{Version: "test-version"})
		_, err := execute(t, cmd, args...)
		if err == nil {
			t.Fatalf("%v should fail; the command was removed", args)
		}
		for _, want := range []string{"removed", "memories sync"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v error should mention %q, got: %v", args, want, err)
			}
		}
		if strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%v should reach the removal remedy, not cobra's unknown-flag error: %v", args, err)
		}
	}

	out, err := execute(t, NewRootCommand(Options{Version: "test-version"}), "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	if strings.Contains(out, "\n  publish ") {
		t.Errorf("top-level help still advertises the removed publish command:\n%s", out)
	}
}
