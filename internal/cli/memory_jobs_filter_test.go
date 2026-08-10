package cli

import (
	"strings"
	"testing"
)

func TestMemoryJobsRejectsUnknownStateInsteadOfReturningFalseEmpty(t *testing.T) {
	opts, _ := memoryAdminCommandFixture(t)
	cmd := newMemoryJobsCommand(opts)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	out, err := execute(t, cmd, "--state", "pendng", "--json")
	if err == nil || !strings.Contains(err.Error(), `unknown memory job state "pendng"`) {
		t.Fatalf("unknown state error=%v out=%q", err, out)
	}
	if out != "" {
		t.Fatalf("unknown state emitted a false-empty result: %q", out)
	}

	out, err = execute(t, newMemoryJobsCommand(opts), "--state", memoryJobStatePending, "--json")
	if err != nil || !strings.Contains(out, `"jobs": []`) {
		t.Fatalf("valid empty state filter err=%v out=%q", err, out)
	}
}
