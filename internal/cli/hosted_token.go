package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// hostedShellOutTimeout bounds every shell-out to the host CLI on the hosted
// sync path (auth token, whereami). The callers are the watch tick and the
// session-end hook — a tick must stay a tick, so a stalled host CLI fails
// fast instead of hanging the caller; the ambient ctx carries only signal
// cancellation, no deadline.
const hostedShellOutTimeout = 30 * time.Second

// mintHostedToken mints a fresh data-plane bearer by shelling out to the host
// CLI, which owns auth entirely: `entire auth token` prints ENTIRE_TOKEN
// verbatim when set, else the active context's stored login JWT, refreshed if
// near expiry. Tokens are expiring JWTs and the daemon's launchd env is baked
// at install time, so a token is never persisted or injected — it is minted
// per sync and held only in memory. The token is never logged and never echoed
// in errors.
func mintHostedToken(ctx context.Context, runner CommandRunner, repoDir, entireBinary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hostedShellOutTimeout)
	defer cancel()
	stdout, _, err := runner.Run(ctx, repoDir, entireBinary, "auth", "token")
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
