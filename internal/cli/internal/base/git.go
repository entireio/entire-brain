package base

import (
	"context"
	"regexp"
	"strings"
)

// RemoteSCPRegex matches scp-like git remotes (git@host:path), capturing the
// host and the path.
var RemoteSCPRegex = regexp.MustCompile(`^(?:[^@]+@)?([^:/]+):(.+)$`)

func GitScalar(ctx context.Context, runner CommandRunner, repoDir string, args ...string) (string, error) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(stdout)), nil
}

func GitOutput(ctx context.Context, runner CommandRunner, repoDir string, args ...string) []byte {
	stdout, _, err := runner.Run(ctx, repoDir, "git", args...)
	if err != nil {
		return nil
	}
	return stdout
}
