package cli

import (
	"fmt"
	"os"
	"strings"
)

func brainNoEgressMode() bool {
	return envBool("ENTIRE_BRAIN_NO_EGRESS") || envBool("ENTIRE_BRAIN_LOCAL_ONLY")
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func rejectAgentForNoEgress(agent string) error {
	if !brainNoEgressMode() {
		return nil
	}
	switch agent {
	case "codex", "claude-code", "command", "auto":
		return fmt.Errorf("no_egress: --agent %s can send selected brain context outside local loopback; use --agent none, --agent ollama, or unset ENTIRE_BRAIN_NO_EGRESS/ENTIRE_BRAIN_LOCAL_ONLY", agent)
	default:
		return nil
	}
}
