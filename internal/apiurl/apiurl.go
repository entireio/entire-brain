// Package apiurl is the single scheme floor for every hosted-Entire base URL this
// plugin accepts (--api-url / ENTIRE_API_URL).
//
// Everything that flows to that base URL is sensitive: `brain publish` ships the
// whole brain artifact bundle (semantic snapshots, branch overlays, durable facts,
// manifest), `facts sync` ships the merged fact-set, the hosted proposal queue ships
// cross-member proposals, and every one of them puts the member's bearer token in an
// Authorization header. Without a floor, a base URL of "http://evil.example" sends
// all of that — token included — in the clear to whatever that name resolves to.
//
// The rule: https:// only, with two narrow carve-outs.
//
//   - http:// to a LOOPBACK host (127.0.0.0/8, ::1, "localhost") is allowed. The
//     bytes never reach a network interface, so there is nothing to intercept; this
//     is what local dev and every httptest-based test in this repo use.
//   - http:// to any other host is allowed ONLY when ENTIRE_BRAIN_ALLOW_INSECURE_API_URL
//     is explicitly truthy — a documented, deliberate operator override.
//
// Any other scheme (ftp, file, gopher, an empty scheme from a schemeless or relative
// string) is rejected outright: the hosted API is HTTP(S), so anything else is a
// misconfiguration at best and a redirection primitive at worst.
//
// A URL carrying userinfo (https://user:pw@host) is rejected on every scheme. This
// API takes a bearer token, so userinfo authenticates nothing here; what it does do
// is put a cleartext password into a value this plugin prints, and hand net/http a
// credential it will promote to an Authorization: Basic header on any request that
// does not already carry one. See Validate for the full reasoning.
//
// Loopback is decided from the URL's literal host only — no DNS resolution. Resolving
// would be a TOCTOU check (the name can resolve differently at request time), would
// need the network to validate a URL, and would let a hostile DNS answer widen the
// carve-out. A name that happens to resolve to 127.0.0.1 must be spelled as a loopback
// literal or opted in explicitly. The name carve-out is exactly "localhost" (matching
// isLoopbackHTTPURL, the plugin's existing loopback check for the local model server):
// subdomains such as "api.localhost" are RFC 6761 loopback in principle but are still
// resolver-dependent in practice, so they take the explicit opt-in.
package apiurl

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
)

// EnvAllowInsecure is the documented, explicit override that re-enables plaintext
// http:// to a NON-loopback host. It follows the plugin's ENTIRE_BRAIN_* convention.
const EnvAllowInsecure = "ENTIRE_BRAIN_ALLOW_INSECURE_API_URL"

// insecureWarned dedups the unrecognized-value warning so a garbage env value cannot
// flood stderr (Validate runs on every hosted call).
var insecureWarned sync.Map

// Validate checks a hosted API base URL and returns it normalized (trimmed, without a
// trailing slash) or an error. It performs no I/O of any kind — no DNS, no disk, no
// network — so callers can and should run it as their FIRST act after resolving the
// flag/env value, keeping every refusal egress-free.
//
// Errors carry a stable machine-greppable prefix, matching the repo's error style:
// "invalid_api_url:" for a malformed or non-HTTP(S) URL, "insecure_api_url:" for a
// well-formed but plaintext one.
func Validate(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("invalid_api_url: API base URL is empty; want an origin like https://api.entire.io")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid_api_url: %q is not a valid URL: %w", trimmed, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid_api_url: %q is not an absolute URL; want an origin like https://api.entire.io", trimmed)
	}
	switch scheme {
	case "https":
	case "http":
		if !IsLoopbackHost(u.Hostname()) && !allowInsecure() {
			return "", fmt.Errorf(
				"insecure_api_url: refusing to send brain content and the API token in plaintext to %q; nothing was sent. Use https://, or set %s=1 to override for a non-loopback http:// endpoint you control (http:// is always allowed for loopback hosts)",
				trimmed, EnvAllowInsecure)
		}
	default:
		return "", fmt.Errorf("invalid_api_url: unsupported URL scheme %q in %q; the hosted API is HTTP(S) — want https:// (or http:// on loopback)", u.Scheme, trimmed)
	}
	// Credentials must not live in the base URL. Three separate reasons, any one of
	// which is enough:
	//
	//   - The hosted API authenticates with a bearer token (--token /
	//     ENTIRE_API_TOKEN), so userinfo is never the way in. There is nothing to
	//     support here, only a way to get it wrong.
	//   - The base URL is a value this plugin PRINTS: publish names its target,
	//     `facts sync` labels its backend with it, and a transport failure formats
	//     it. net/url guards its own rendering with URL.Redacted(), but every
	//     %s of the plain string defeats that and puts the cleartext password in a
	//     terminal, a CI log, or a pasted bug report.
	//   - net/http promotes userinfo to an `Authorization: Basic` header whenever the
	//     request does not already carry one. The fact-set and hosted-MCP clients set
	//     Authorization only when their token is non-empty, so a tokenless call to a
	//     URL with userinfo silently egresses a DIFFERENT credential than the one the
	//     member configured.
	//
	// The refusal quotes u.Redacted(), not the raw string: an error about a leaked
	// password must not be the thing that leaks it.
	if u.User != nil {
		return "", fmt.Errorf("invalid_api_url: %s carries credentials in the URL; the hosted API authenticates with a bearer token — remove the user:password@ from the base URL and pass the token with --token or ENTIRE_API_TOKEN", u.Redacted())
	}
	return strings.TrimRight(trimmed, "/"), nil
}

// IsLoopbackHost reports whether a URL host (no port, IPv6 brackets already stripped
// by url.URL.Hostname) is a loopback literal or the reserved loopback name. No DNS is
// consulted; see the package doc for why.
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".") // a fully-qualified "localhost." is still localhost
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// allowInsecure reads the plaintext override with OPT-IN fail-closed semantics: only a
// clearly truthy value unlocks plaintext, and anything else — including an
// unrecognized value — leaves the floor in place.
//
// This is deliberately the mirror image of securityToggleEnabled (internal/cli) and
// toggleOn (internal/hostedbrain), which fail an unrecognized value ON: those toggles
// switch a protection on, so a typo must not silently disable the protection. This one
// switches a protection OFF, so the same fail-closed principle requires the opposite
// resolution — a typo'd "ture" must not silently unlock plaintext egress. It matches
// the spelling already used for the ENTIRE_BRAIN_ALLOW_HOSTED publish opt-in. The
// operator is still told their value was not understood.
func allowInsecure() bool {
	raw := strings.TrimSpace(os.Getenv(EnvAllowInsecure))
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	case "", "0", "false", "no", "off", "disable", "disabled":
		return false
	default:
		if _, seen := insecureWarned.LoadOrStore(raw, struct{}{}); !seen {
			fmt.Fprintf(os.Stderr, "warning: %s is not a recognized boolean; treating as disabled (fail-closed: https only)\n", EnvAllowInsecure)
		}
		return false
	}
}
