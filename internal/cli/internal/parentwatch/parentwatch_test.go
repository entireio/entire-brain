package parentwatch

import "testing"

// Arming rules from the parent-death design: a process whose initial ppid is 1
// is a daemon launchd (or systemd) owns, and a child spawned detached must
// outlive its spawner. Everything else the dispatcher starts dies with it.
func TestShouldArm(t *testing.T) {
	cases := []struct {
		name     string
		ppid     int
		detached string
		want     bool
	}{
		{name: "ordinary dispatched child arms", ppid: 42, want: true},
		{name: "daemon with ppid 1 never arms", ppid: 1, want: false},
		{name: "detached child never arms", ppid: 42, detached: "1", want: false},
		{name: "empty detached marker still arms", ppid: 42, detached: "", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldArm(tc.ppid, tc.detached); got != tc.want {
				t.Fatalf("shouldArm(%d, %q) = %v, want %v", tc.ppid, tc.detached, got, tc.want)
			}
		})
	}
}
