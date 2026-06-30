package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// securityToggleWarned dedups the unrecognized-value warning so a garbage env
// value cannot flood stderr (brainNoEgressMode runs on every agent/export check).
var securityToggleWarned sync.Map

func brainNoEgressMode() bool {
	return securityToggleEnabled("ENTIRE_BRAIN_NO_EGRESS") || securityToggleEnabled("ENTIRE_BRAIN_LOCAL_ONLY")
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// securityToggleEnabled reads a fail-closed security toggle. Canonical
// true/false values are honored and an unset/empty value is off (these guards are
// opt-in). Any other, unrecognized value is treated as ENABLED and a warning is
// emitted — so a typo'd "enabled"/"ture" can never silently leave a no-egress
// guard switched off, which would fail open to egress.
func securityToggleEnabled(name string) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	switch strings.ToLower(raw) {
	case "", "0", "false", "no", "off", "disable", "disabled":
		return false
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	default:
		if _, seen := securityToggleWarned.LoadOrStore(name+"="+raw, struct{}{}); !seen {
			fmt.Fprintf(os.Stderr, "warning: %s=%q is not a recognized boolean; treating as enabled (fail-closed)\n", name, raw)
		}
		return true
	}
}

// rejectAgentForNoEgress fails closed: under no-egress mode only agents that are
// known to stay on local loopback (ollama / none) are permitted. Any other value
// — including a newly-added or unknown agent name — is denied, so the guard
// cannot be bypassed by an agent the allowlist has not yet been taught about.
func rejectAgentForNoEgress(agent string) error {
	if !brainNoEgressMode() {
		return nil
	}
	switch strings.TrimSpace(agent) {
	case "ollama", "none", "":
		return nil
	default:
		return fmt.Errorf("no_egress: --agent %s can send selected brain context outside local loopback; use --agent ollama for local loopback models, --agent none where supported, or unset ENTIRE_BRAIN_NO_EGRESS/ENTIRE_BRAIN_LOCAL_ONLY", agent)
	}
}
