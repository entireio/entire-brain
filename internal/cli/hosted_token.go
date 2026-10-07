package cli

import (
	"context"
	"fmt"
	"strings"
)

// mintJurisdictionToken mints a fresh data-plane bearer by shelling out to the
// host CLI. Tokens are expiring JWTs and the daemon's launchd env is baked at
// install time, so a token can never be persisted or injected via env — it is
// minted per sync and held only in memory. The token is never logged and never
// echoed in errors.
//
// The jurisdiction names a saved login context (`entire auth contexts`); the
// former `--jurisdiction` flag is deprecated in the host CLI.
func mintJurisdictionToken(ctx context.Context, runner CommandRunner, repoDir, entireBinary, jurisdiction string) (string, error) {
	args := []string{"auth", "token"}
	if jurisdiction != "" {
		args = append(args, "--context", jurisdiction)
	}
	stdout, _, err := runner.Run(ctx, repoDir, entireBinary, args...)
	if err != nil {
		return "", fmt.Errorf("mint hosted sync token (`%s auth token`): %w", entireBinary, err)
	}
	// The update banner shares stdout with released CLIs; the token is the
	// last non-empty line.
	token := ""
	for _, line := range strings.Split(string(stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			token = line
		}
	}
	if token == "" {
		return "", fmt.Errorf("`%s auth token` printed no token; log in with `%s auth login`", entireBinary, entireBinary)
	}
	if !looksLikeJWT(token) {
		return "", fmt.Errorf("`%s auth token` output does not look like a token; log in with `%s auth login`", entireBinary, entireBinary)
	}
	return token, nil
}

func looksLikeJWT(s string) bool {
	return strings.Count(s, ".") == 2 && !strings.ContainsAny(s, " \t")
}
