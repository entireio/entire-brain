// Package repoid holds the one rule that makes a hosted-API path safe to assemble
// by concatenation: a repo id must be EXACTLY ONE path segment that survives the
// trip unchanged.
//
// # Why this is a package and not a helper in one client
//
// Three packages build "/api/v1/repos/{repoID}/…" targets and carry a bearer token
// on them:
//
//	internal/hostedbrain  the brain MCP client        /brain/mcp
//	internal/factsync     the fact-set + proposal transport
//	                      /brain/facts, /brain/facts/advance, /brain/facts/proposals…
//	internal/cli          brain publish               /brain/artifacts
//
// They cannot share the rule through each other. internal/hostedbrain already
// imports internal/factsync (hostedbrain.Client drives the proposal endpoints over
// factsync.HTTPServer), so factsync importing hostedbrain back is an import cycle,
// not merely bad layering — the rule physically cannot live in either client. And
// internal/cli sits above both, so putting it there is upside down.
//
// A rule about the wire format shared by clients at three different layers belongs
// in a leaf below all of them, which is what this package is: standard library only,
// no dependency on any client. internal/httpx is the same shape for the outbound
// HTTP policy those same three callers share.
package repoid

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrInvalid is returned before any request is built when a repo id is not exactly
// one safe path segment. Callers can match it with errors.Is to tell a malformed
// target apart from a transport or authorization failure.
var ErrInvalid = errors.New("invalid repo id")

// Validate enforces the only rule that makes "/api/v1/repos/" + id + "/…" safe to
// assemble by concatenation: the id must be EXACTLY ONE path segment that survives
// the trip unchanged.
//
// Escaping alone does not give that. url.PathEscape is defined over RFC 3986
// unreserved characters, and "." is unreserved — so PathEscape(".") == "." and
// PathEscape("..") == "..". A repo id of ".." therefore escapes to itself and yields
//
//	/api/v1/repos/../brain/mcp
//
// which any dot-segment-normalizing hop (net/http's ServeMux, nginx, a CDN, RFC 3986
// §5.2.4 reference resolution) collapses to /api/v1/brain/mcp — a different route,
// reached with the caller's bearer token attached. The escape function cannot catch
// this, and neither can a test that asserts the escape against itself.
//
// So this is a rule about the id, checked before the URL exists:
//
//  1. non-empty after trimming, so the segment cannot vanish and let the surrounding
//     slashes fuse ("/api/v1/repos//brain/mcp" -> "/api/v1/repos/brain/mcp");
//  2. no leading/trailing whitespace, since a trimmed and an untrimmed id must not
//     address the same repo;
//  3. not "." or ".." or any all-dots run — the dot-segment family above;
//  4. no path separator, "/" or "\", so it cannot add segments (a backslash is a
//     separator on Windows and is normalized by some proxies);
//  5. identical to url.PathEscape(id) — the catch-all. Anything PathEscape would
//     rewrite ("?", "#", "%", space, control bytes, a pre-encoded "%2f") is rejected
//     rather than silently re-encoded, so what the caller wrote is what the server
//     routes on, and the assembled path is already in normal form.
//
// Rule 5 makes the accepted set exactly the RFC 3986 unreserved and sub-delims
// characters, which covers every id this API issues (repo ids are ULIDs) with room
// for "-", "_", ".", "~" inside a longer id.
//
// Because rule 5 already guarantees escape-identity, a caller that has run Validate
// may concatenate the id into the path raw: url.PathEscape would be a no-op, and
// leaving it out keeps it obvious that the check, not the escape, is what makes the
// path safe.
func Validate(repoID string) error {
	if repoID == "" {
		return fmt.Errorf("%w: repo id is required", ErrInvalid)
	}
	if strings.TrimSpace(repoID) != repoID {
		return fmt.Errorf("%w: %q has leading or trailing whitespace", ErrInvalid, repoID)
	}
	if strings.Trim(repoID, ".") == "" {
		// ".", "..", "..." — a dot segment, or something a normalizer may treat as one.
		return fmt.Errorf("%w: %q is a path segment, not a repo id", ErrInvalid, repoID)
	}
	if strings.ContainsAny(repoID, `/\`) {
		return fmt.Errorf("%w: %q contains a path separator, so it is more than one path segment", ErrInvalid, repoID)
	}
	if escaped := url.PathEscape(repoID); escaped != repoID {
		return fmt.Errorf("%w: %q is not a bare path segment (it would be rewritten to %q)", ErrInvalid, repoID, escaped)
	}
	return nil
}
