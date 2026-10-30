package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// brainStatusHosted is the status report's hosted-brain connection section,
// reporting the unified state model (COR-2143). State is the EFFECTIVE state:
// "local_only" when the machine-level kill switch is set, else the repo state.
// RepoState is the durable per-repo half ("connected": binding present and not
// opted out; "disconnected": opted out, never connected, or unreadable), kept
// visible under the kill switch so unsetting the variable has a predictable
// outcome. Enablement only flows from repo state; the environment can only
// subtract.
type brainStatusHosted struct {
	State     string `json:"state"`
	RepoState string `json:"repo_state"`
	NoEgress  bool   `json:"no_egress"`
	RepoID    string `json:"repo_id,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	// SyncedBranches are the declared fact branches whose per-branch marker
	// matches the current binding; a marker left by an older target does not
	// count.
	SyncedBranches []string `json:"synced_branches,omitempty"`
	// NoAccessUntil is the ACTIVE no-access pause (COR-2279): background sync
	// skips the repo until then because the server denied access. An expired
	// pause is dead state (the next successful sync retires it) and is not
	// reported.
	NoAccessUntil time.Time `json:"no_access_until,omitzero"`
}

const (
	hostedStateConnected    = "connected"
	hostedStateLocalOnly    = "local_only"
	hostedStateDisconnected = "disconnected"
)

// buildHostedStatus reports this repo's hosted connection state with the same
// gate order as hostedFactsSyncAndPull: the kill switch wins, then the binding
// decides. The binding stays visible under the kill switch so the reader sees
// what it suppresses. A corrupt binding degrades to disconnected with a warning
// rather than failing status over a sync-state file.
func buildHostedStatus(brainDir string, factBranches []string, now time.Time) (*brainStatusHosted, string) {
	var warning string
	hosted := &brainStatusHosted{RepoState: hostedStateDisconnected}
	binding, present, err := brainstore.ReadHostedRepoBinding(brainDir)
	switch {
	case err != nil:
		warning = "hosted binding unreadable (run '" + base.BrainInvocation(os.LookupEnv, "connect") + "' to rebind): " + err.Error()
	case present && binding.OptedOut:
		// Coordinates survive an opt-out (reconnect needs no placement
		// resolution), so the target stays visible here too.
		hosted.RepoID = binding.RepoID
		hosted.BaseURL = binding.BaseURL
	case present:
		hosted.RepoState = hostedStateConnected
		hosted.RepoID = binding.RepoID
		hosted.BaseURL = binding.BaseURL
		if binding.NoAccessUntil.After(now) {
			hosted.NoAccessUntil = binding.NoAccessUntil
		}
		// A broken marker is corruption, not "never synced" — report it like
		// the unreadable top-level binding above instead of posing as a
		// healthy unsynced branch. The two warnings cannot co-occur: markers
		// are only inspected when the binding itself was readable.
		var broken []string
		var brokenErr error
		for _, branch := range factBranches {
			marker, markerPresent, markerErr := readHostedFactsBinding(brainDir, branch)
			switch {
			case markerErr != nil:
				broken = append(broken, branch)
				if brokenErr == nil {
					brokenErr = markerErr
				}
			case markerPresent && marker.RepoID == binding.RepoID && marker.BaseURL == binding.BaseURL:
				hosted.SyncedBranches = append(hosted.SyncedBranches, branch)
			}
		}
		if len(broken) > 0 {
			warning = fmt.Sprintf("hosted sync marker unreadable for branch(es) %s: %v", strings.Join(broken, ", "), brokenErr)
		}
	}
	hosted.NoEgress = base.BrainNoEgressMode()
	hosted.State = hosted.RepoState
	if hosted.NoEgress {
		hosted.State = hostedStateLocalOnly
	}
	return hosted, warning
}

// hostedStateLabel is the short-report value for the "Brainstore:" item; the
// cause and remedy live in the verbose line (hostedStatusLine).
func hostedStateLabel(state string) string {
	switch state {
	case hostedStateConnected:
		return "connected"
	case hostedStateDisconnected:
		return "disconnected"
	default:
		return "local-only"
	}
}

// hostedStatusLine is the verbose report's one-line hosted state: the label
// plus the cause or remedy. Coordinates are machine detail and stay in --json.
func hostedStatusLine(h *brainStatusHosted) string {
	switch h.State {
	case hostedStateConnected:
		if !h.NoAccessUntil.IsZero() {
			return fmt.Sprintf("Connected to Brainstore, but sync is paused until %s — the server denied access to this repo; run '%s' once access is granted",
				h.NoAccessUntil.UTC().Format(time.RFC3339), base.BrainInvocation(os.LookupEnv, "connect"))
		}
		if n := len(h.SyncedBranches); n > 0 {
			word := "branches"
			if n == 1 {
				word = "branch"
			}
			return fmt.Sprintf("Connected to Brainstore (%d %s synced)", n, word)
		}
		return "Connected to Brainstore (no branches synced yet)"
	case hostedStateDisconnected:
		return "Disconnected from Brainstore (run '" + base.BrainInvocation(os.LookupEnv, "connect") + "' to join)"
	default:
		// Local-only: name the variable actually set, and keep the underlying
		// repo state visible so unsetting the variable is predictable.
		return fmt.Sprintf("Local-only (%s set — unset it to resume; repo is %s)",
			base.NoEgressToggleName(), hostedStateLabel(h.RepoState))
	}
}
