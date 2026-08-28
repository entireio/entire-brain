package hostedbrain

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrInvalidRepoID is returned before any request is built when a repo id is not
// exactly one safe path segment. Callers can match it with errors.Is to tell a
// malformed target apart from a transport or authorization failure.
var ErrInvalidRepoID = errors.New("hostedbrain: invalid repo id")

// validRepoID enforces the only rule that makes "/api/v1/repos/" + id + "/brain/mcp"
// safe to assemble by concatenation: the id must be EXACTLY ONE path segment that
// survives the trip unchanged.
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
func validRepoID(repoID string) error {
	if repoID == "" {
		return fmt.Errorf("%w: repo id is required", ErrInvalidRepoID)
	}
	if strings.TrimSpace(repoID) != repoID {
		return fmt.Errorf("%w: %q has leading or trailing whitespace", ErrInvalidRepoID, repoID)
	}
	if strings.Trim(repoID, ".") == "" {
		// ".", "..", "..." — a dot segment, or something a normalizer may treat as one.
		return fmt.Errorf("%w: %q is a path segment, not a repo id", ErrInvalidRepoID, repoID)
	}
	if strings.ContainsAny(repoID, `/\`) {
		return fmt.Errorf("%w: %q contains a path separator, so it is more than one path segment", ErrInvalidRepoID, repoID)
	}
	if escaped := url.PathEscape(repoID); escaped != repoID {
		return fmt.Errorf("%w: %q is not a bare path segment (it would be rewritten to %q)", ErrInvalidRepoID, repoID, escaped)
	}
	return nil
}
