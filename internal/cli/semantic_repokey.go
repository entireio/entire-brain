package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Repo-key contract between entire-brain and the entire-graph semantic provider.
//
// Two INDEPENDENT derivations of "repository identity" exist, and they are not
// the same rule:
//
//	brain storage key (resolveRepoStorageIdentity, env.go)
//	    the `origin` remote, through repoKeyFromRemote, for ANY host, folded to
//	    lower case: "gh/owner/name", "gl/owner/name", "<generated-slug>/…".
//	    With no `origin`: "local/<sanitized base>-<sha256(abs repo dir)[:12]>".
//	    The hash suffix is load-bearing — the key IS the on-disk directory name
//	    under <data>/repos/, so two checkouts sharing a basename must not
//	    collide onto one brain.
//
//	provider label (entire-graph internal/sem/provider.go repoKey())
//	    the first remote URL (origin first) matching a github.com pattern, case
//	    preserved: "gh/<owner>/<name>". Otherwise "local/<basename>", with no
//	    hash. The value is also embedded in every emitted symbol id, so the
//	    provider cannot adopt the brain's spelling without churning every id.
//
// The two coincide only for a lower-case github.com `origin`. They diverge for
// a repository with no remote at all, for any non-GitHub forge, for a GitHub
// remote that is not named `origin`, and for mixed-case paths. Requiring
// equality therefore broke `entire-brain refresh index` on a plain local
// repository — the first thing a new user has.
//
// The brain is the side that must change: it owns the comparison, and it is
// the side that conflated a storage key with a provenance label. This file
// makes the contract explicit — it mirrors the provider rule so the brain can
// compute the key the provider is expected to emit, accepts either spelling,
// normalizes what it persists to its own canonical storage key, and produces
// an actionable error (naming both keys and a remedy) when a provider emits
// something outside the contract.
//
// providerRepoKeyFromRemotes is kept behaviourally identical to the upstream
// rule by TestProviderRepoKeyMatchesInstalledEntireGraph, which diffs it
// against the installed provider on a real repository.

// providerGitHubRepoKeyPatterns mirrors entire-graph's githubRepoKey patterns
// exactly, including their order.
var providerGitHubRepoKeyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^git@github\.com:([^/]+)/(.+)$`),
	regexp.MustCompile(`^https://github\.com/([^/]+)/(.+)$`),
	regexp.MustCompile(`^http://github\.com/([^/]+)/(.+)$`),
	regexp.MustCompile(`^ssh://git@github\.com/([^/]+)/(.+)$`),
}

// providerGitHubRepoKey mirrors entire-graph's githubRepoKey.
func providerGitHubRepoKey(remoteURL string) (string, bool) {
	remoteURL = strings.TrimSpace(remoteURL)
	remoteURL = strings.TrimRight(remoteURL, "/")
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	for _, pattern := range providerGitHubRepoKeyPatterns {
		matches := pattern.FindStringSubmatch(remoteURL)
		if len(matches) != 3 {
			continue
		}
		owner := strings.TrimSpace(matches[1])
		name := strings.TrimSpace(matches[2])
		if owner == "" || name == "" || strings.Contains(name, "/") {
			continue
		}
		return "gh/" + owner + "/" + name, true
	}
	return "", false
}

// providerRepoKeyFromRemotes mirrors entire-graph's repoKey(): the first
// GitHub-shaped remote wins, otherwise the repository basename under "local/".
// remotes must be ordered the way entire-graph orders them (origin first).
func providerRepoKeyFromRemotes(remotes []string, repoDir string) string {
	for _, remote := range remotes {
		if key, ok := providerGitHubRepoKey(remote); ok {
			return key
		}
	}
	return "local/" + filepath.Base(filepath.Clean(repoDir))
}

// providerRemoteURLs reproduces entire-graph's gitutil.RemoteURLs: every
// configured remote URL, with origin hoisted to the front. A repository with no
// remotes (or an unreadable config) yields none, which is exactly the input the
// provider itself sees.
func providerRemoteURLs(ctx context.Context, runner CommandRunner, repoDir string) []string {
	if runner == nil {
		return nil
	}
	stdout, _, err := runner.Run(ctx, repoDir, "git", "config", "--get-regexp", `^remote\..*\.url$`)
	if err != nil {
		return nil
	}
	origin := ""
	var urls []string
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "remote.origin.url" {
			origin = fields[1]
			continue
		}
		urls = append(urls, fields[1])
	}
	if origin != "" {
		urls = append([]string{origin}, urls...)
	}
	return urls
}

// expectedProviderRepoKey is the key entire-graph is expected to stamp on a
// snapshot of repoDir.
func expectedProviderRepoKey(ctx context.Context, runner CommandRunner, repoDir string) string {
	return providerRepoKeyFromRemotes(providerRemoteURLs(ctx, runner, repoDir), repoDir)
}

// validateSemanticProviderRepoKey enforces the contract above. providerKey is
// the repo_key the provider stamped on the snapshot header, before the brain
// normalizes it to its own storage key. doctorKey is the key the provider
// reported for this repository from `graph doctor --json`, or "" if the
// installed provider predates that handshake.
//
// Three sources of truth, in descending authority:
//
//  1. the brain's own storage key — a snapshot the brain wrote itself;
//  2. doctorKey — the provider's own answer about its own rule. When the
//     provider tells us what it will stamp, the mirror is not consulted at all:
//     a mirror that disagreed with the provider would only ever be wrong, and
//     silently accepting a third spelling would defeat the guard;
//  3. the mirror (providerRepoKeyFromRemotes) — the fallback for a provider too
//     old to answer.
func validateSemanticProviderRepoKey(ctx context.Context, runner CommandRunner, repoDir, storageKey, providerKey, doctorKey, graphBinary string) error {
	if strings.TrimSpace(providerKey) == "" {
		return fmt.Errorf("semantic snapshot header missing repo_key (provider %q, repo %s)", graphBinary, repoDir)
	}
	// Fast path: the provider and the brain agree outright (a lower-case
	// github.com origin, or a snapshot the brain wrote itself). No git call.
	if semanticRepoKeyEqual(providerKey, storageKey) {
		return nil
	}
	if doctorKey = strings.TrimSpace(doctorKey); doctorKey != "" {
		if semanticRepoKeyEqual(providerKey, doctorKey) {
			return nil
		}
		return semanticProviderRepoKeyMismatchError(providerKey, storageKey, doctorKey, repoDir, graphBinary, true)
	}
	expected := expectedProviderRepoKey(ctx, runner, repoDir)
	if semanticRepoKeyEqual(providerKey, expected) {
		return nil
	}
	return semanticProviderRepoKeyMismatchError(providerKey, storageKey, expected, repoDir, graphBinary, false)
}

// semanticProviderRepoKeyMismatchError names BOTH keys, the key the provider
// was expected to emit and where that expectation came from, the repository and
// provider binary involved, and what to do about it. A mid-phase failure with no
// remedy is what made the original bug so expensive to diagnose.
func semanticProviderRepoKeyMismatchError(providerKey, storageKey, expectedKey, repoDir, graphBinary string, fromDoctor bool) error {
	binary := strings.TrimSpace(graphBinary)
	if binary == "" {
		binary = "entire"
	}
	source := "is expected to report"
	cause := fmt.Sprintf("or %q is a build whose repo-key rule this brain does not know", binary)
	if fromDoctor {
		source = "reports"
		cause = fmt.Sprintf("since `%s graph doctor --json` reports that key for this repository", binary)
	}
	return fmt.Errorf(
		"semantic provider reported repo_key %q, which does not match repository %s: the brain knows this repository as %q, "+
			"and %q %s %q for it. The snapshot most likely belongs to a different repository, %s. Remedy: update the Entire "+
			"CLI so %q matches this brain, or point the brain at a matching build with --graph-binary <path>; if the snapshot "+
			"really does describe another repository, re-run the index inside that repository (%s)",
		providerKey, repoDir, storageKey, binary, source, expectedKey, cause, binary, shellQuotedRepoDir(repoDir),
	)
}

// shellQuotedRepoDir makes the remedy copy-pasteable when the repository path
// contains spaces or quotes.
func shellQuotedRepoDir(repoDir string) string {
	if strings.TrimSpace(repoDir) == "" {
		return "."
	}
	if strings.ContainsAny(repoDir, " \t\"'") {
		return "'" + strings.ReplaceAll(repoDir, "'", `'\''`) + "'"
	}
	return repoDir
}
