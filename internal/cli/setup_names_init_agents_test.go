package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/tui"
)

// setup writes no AGENTS.md and no CLAUDE.md: activation is recorded in the
// guide rather than probed, so an agent learns nothing about this brain until
// `init-agents` runs.
//
// The reported case: a user ran setup, saw it succeed, found no agent files and
// concluded they had run it from the wrong directory. They had not. The step
// simply was not named anywhere they were looking -- not in setup's output, not
// in its Next block, not in `status`. `setup --help` and the README are not
// where someone stands when this goes wrong.
func TestSetupNextBlockNamesInitAgents(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	renderSetupSummary(&out, tui.NewRenderer(&out), setupReport{}, nil, setupWatchPlan{}, "entire brain")

	got := out.String()
	if !strings.Contains(got, "init-agents") {
		t.Fatalf("setup's Next block does not name init-agents, so nothing tells a first run how to "+
			"get AGENTS.md/CLAUDE.md written:\n%s", got)
	}
	// It must be FIRST: the other steps are things to do with a brain an agent
	// cannot see yet.
	idx := strings.Index(got, "init-agents")
	for _, later := range []string{"overview", "brief", "status"} {
		if j := strings.Index(got, later); j >= 0 && j < idx {
			t.Errorf("%q is listed before init-agents; the agent cannot use the brain until the guide exists", later)
		}
	}
	// And it must say WHY, or it reads as one more optional command.
	if !strings.Contains(got, "AGENTS.md") {
		t.Error("the init-agents row does not say what it writes, so its purpose is not obvious")
	}
}
