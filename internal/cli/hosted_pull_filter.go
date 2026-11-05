package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/entireio/entire-brain/internal/factsync"
)

// newCommitReachability answers "is this commit an ancestor of HEAD?" with
// one memoized `git merge-base --is-ancestor` per distinct commit. Any
// failure — including a commit this clone has never seen (squash merge, gc,
// shallow clone) — is quietly "unreachable": absence of history is a normal
// state here, not an error worth a line per fact.
func newCommitReachability(ctx context.Context, runner CommandRunner, repoDir string) func(commit string) bool {
	memo := make(map[string]bool)
	return func(commit string) bool {
		if commit == "" {
			return false
		}
		if known, ok := memo[commit]; ok {
			return known
		}
		_, _, err := runner.Run(ctx, repoDir, "git", "merge-base", "--is-ancestor", commit, "HEAD")
		memo[commit] = err == nil
		return memo[commit]
	}
}

// keepHostedFact is the local pull filter from the COR-2110 v3 design: the
// server ships the whole repo feed and the client keeps what its own git
// graph and identity justify. First match wins:
//
//  1. A merged trail id — stamped by brainstore only at trail merge — marks
//     the fact main-visible even when its anchor commits never became
//     ancestors (squash merges rewrite them).
//  2. The member's own facts follow them to every machine. Checked before
//     reachability: it is free, while the anchor loop shells out to git.
//     A member's own commit-anchored facts must not each cost a git call.
//  3. Any anchor commit reachable from HEAD.
//  4. No session/commit anchors at all: the repo pool, kept by everyone.
//
// ponytail: rule 2 keys on the fact's server-stamped creator, not per-anchor
// session ownership — a teammate-created fact that unions in one of this
// member's session anchors is dropped until its commits merge. Upgrade path:
// match anchor session ids against the local session manifest as well.
func keepHostedFact(item factsync.FeedItem, memberAccountID string, reachable func(string) bool) bool {
	if item.TrailID != "" {
		return true
	}
	if memberAccountID != "" && item.CreatedBy == memberAccountID {
		return true
	}
	anchored := false
	for _, anchor := range item.Record.Provenance {
		if anchor.Commit != "" && reachable(anchor.Commit) {
			return true
		}
		if anchor.Commit != "" || anchor.SessionID != "" {
			anchored = true
		}
	}
	return !anchored
}

// hasLocalProvenance reports whether a record carries an anchor only this
// machine's distill could have written: the brain-relative transcript path,
// which SanitizeForEgress strips before any record leaves.
func hasLocalProvenance(rec factRecord) bool {
	for _, anchor := range rec.Provenance {
		if anchor.Transcript != "" {
			return true
		}
	}
	return false
}

// jwtSubClaim extracts the sub claim from a JWT without verifying it. The
// token comes from our own host CLI's login and the claim is used only to
// recognize this member's facts in the pull filter — never as an authz
// input. Any malformed input is "", which simply disables the member rule.
func jwtSubClaim(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}
