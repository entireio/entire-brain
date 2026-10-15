package cli

import (
	"strings"
	"testing"
)

// This exists because it happened, twice in one day, on a real machine.
//
// The plist PATH was isolated by ENTIRE_BRAIN_DAEMON_DIR; the LABEL was not.
// Both the throwaway and the real install rendered io.entire.brain-watch.<hash>
// from the daemon NAME alone, so `launchctl bootstrap` of the throwaway unit
// REPLACED the real registration and repointed it at a scratch binary in a
// directory that was about to be deleted. Undoing it took a bootout plus a
// bootstrap of the real plist.

func planFor(t *testing.T, unitDir string) daemonPlan {
	t.Helper()
	plan, err := planBrainWatchDaemon("darwin", "/Users/someone", "", unitDir, daemonSpec{
		Name:   daemonDefaultName,
		Binary: "/usr/local/bin/entire-brain",
	})
	if err != nil {
		t.Fatalf("plan(%q): %v", unitDir, err)
	}
	return plan
}

func TestAThrowawayDaemonCannotCollideWithTheRealOne(t *testing.T) {
	t.Parallel()
	real := planFor(t, "")
	throwaway := planFor(t, "/tmp/scratch-run-1/daemon")

	if throwaway.Label == real.Label {
		t.Fatalf("a throwaway install renders the real label, so bootstrapping it replaces the real registration: %q", real.Label)
	}
	if throwaway.UnitPath == real.UnitPath {
		t.Fatalf("the throwaway unit path is the real one: %q", real.UnitPath)
	}
}

// The default must keep rendering exactly the label it always has. Changing it
// would orphan every daemon already registered -- a migration that cannot be
// justified by a fix whose whole point is not breaking things.
func TestTheDefaultLabelIsUnchanged(t *testing.T) {
	t.Parallel()
	if got, want := planFor(t, "").Label, launchdLabel(daemonDefaultName); got != want {
		t.Fatalf("the default label changed, orphaning every existing install: got %q want %q", got, want)
	}
}

// Install, uninstall and the running-probe share one plan, so the scoped label
// must be a pure function of its inputs: two plans from the same directory have
// to agree, or an uninstall would miss what the install registered.
func TestTheScopedLabelIsStableForOneDirectory(t *testing.T) {
	t.Parallel()
	a := planFor(t, "/tmp/scratch-run-1/daemon")
	b := planFor(t, "/tmp/scratch-run-1/daemon")
	if a.Label != b.Label {
		t.Fatalf("the same directory produced two labels, so uninstall would miss the install: %q then %q", a.Label, b.Label)
	}
	if !strings.HasPrefix(a.Label, launchdLabel(daemonDefaultName)) {
		t.Fatalf("the scoped label replaced the base derivation instead of extending it: %q", a.Label)
	}
	// Two different throwaways must not collide with each other either.
	if c := planFor(t, "/tmp/scratch-run-2/daemon"); c.Label == a.Label {
		t.Fatalf("two different throwaway directories share a label: %q", a.Label)
	}
}

// Fixing launchd and leaving systemd is how the two halves drift apart.
func TestTheSystemdUnitIsScopedToo(t *testing.T) {
	t.Parallel()
	mk := func(unitDir string) string {
		plan, err := planBrainWatchDaemon("linux", "/home/someone", "/home/someone/.config", unitDir, daemonSpec{
			Name:   daemonDefaultName,
			Binary: "/usr/local/bin/entire-brain",
		})
		if err != nil {
			t.Fatalf("plan(%q): %v", unitDir, err)
		}
		return plan.Label
	}
	if mk("/tmp/scratch/systemd") == mk("") {
		t.Fatal("a throwaway systemd unit takes the real unit's name")
	}
	if got, want := mk(""), systemdUnitName(daemonDefaultName); got != want {
		t.Fatalf("the default systemd unit name changed, orphaning existing installs: got %q want %q", got, want)
	}
}
