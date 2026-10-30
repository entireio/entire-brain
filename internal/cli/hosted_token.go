package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// mintHostedToken mints a fresh data-plane bearer by shelling out to the host
// CLI, which owns auth entirely: `entire auth token` prints ENTIRE_TOKEN
// verbatim when set, else the active context's stored login JWT, refreshed if
// near expiry. Tokens are expiring JWTs and the daemon's launchd env is baked
// at install time, so a token is never persisted or injected — it is minted
// per sync and held only in memory. The token is never logged and never echoed
// in errors.
func mintHostedToken(ctx context.Context, runner CommandRunner, repoDir, entireBinary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, base.HostedShellOutTimeout)
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
	// When the host CLI's login session is dead (refresh-token family expired
	// or revoked), `auth token` falls back to printing the raw stored JWT —
	// already expired, so every brainstore call would 401 and page the
	// server-side auth monitor on each sync. A locally provable expiry is not
	// the auth-plumbing break that 401 path exists to surface; skip the sync
	// before any HTTP instead.
	if jwtExpired(token, time.Now()) {
		return "", fmt.Errorf("`%s auth token` printed an expired token; log in with `%s auth login`", entireBinary, entireBinary)
	}
	return token, nil
}

func looksLikeJWT(s string) bool {
	return strings.Count(s, ".") == 2 && !strings.ContainsAny(s, " \t")
}

// jwtExpiryLeeway keeps a slightly-fast client clock from suppressing syncs a
// still-valid token would serve; a token that squeaks through near expiry just
// 401s once, which is today's behavior.
const jwtExpiryLeeway = time.Minute

// jwtExpired reports whether the token's exp claim is at least jwtExpiryLeeway
// in the past. It decodes without verifying — the token isn't being trusted,
// only pre-screened. Anything unreadable or exp-less fails open: the server
// stays the authority for everything but a locally provable expiry.
func jwtExpired(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return false
	}
	return time.Unix(claims.Exp, 0).Before(now.Add(-jwtExpiryLeeway))
}
