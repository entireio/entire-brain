package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// THE BRAIN <-> ENTIRE-GRAPH repo_key CONTRACT
//
// Two different keys name one repository across this seam, and they are NOT
// interchangeable:
//
//   - Brain's STORAGE key (localRepoKey / repoKeyFromRemote in env.go) addresses
//     the on-disk brain. It must be unique per checkout path, so a local repo
//     gets a path hash suffix, and it maps unknown forge hosts through a slug
//     table persisted in Brain's own config. Neither input is visible to an
//     external binary, so no other process can reproduce it.
//
//   - entire-graph's PROVIDER key is the symbol-ID namespace stamped into every
//     record of an NDJSON snapshot. It is derived from the repository alone so
//     that snapshots are reproducible: a github.com `origin` yields
//     gh/<owner>/<name>, and everything else yields local/<basename>.
//
// The two rules coincide for exactly one repository shape — a github.com origin
// — which is why every pre-existing semantic fixture used one and why the seam
// looked healthy. For a repo with no remote, or a GitLab/Bitbucket/self-hosted
// remote, they diverge and an equality check rejects a perfectly valid snapshot.
//
// providerSemanticRepoKey below is the executable statement of the provider
// rule. entire-graph's repoKey() (internal/sem/provider.go) is the other half;
// both repos assert the same golden vectors, so a change to either rule breaks
// a test on both sides rather than silently splitting the seam.
//
// Snapshot identity is not carried by repo_key alone: the header's commit and
// tree are validated against the live repository in the same pass, so accepting
// either spelling of the key does not weaken the "is this snapshot mine" gate.

// semanticProviderGitHubRemotePatterns mirrors entire-graph's githubRepoKey.
var semanticProviderGitHubRemotePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^git@github\.com:([^/]+)/(.+)$`),
	regexp.MustCompile(`^https://github\.com/([^/]+)/(.+)$`),
	regexp.MustCompile(`^http://github\.com/([^/]+)/(.+)$`),
	regexp.MustCompile(`^ssh://git@github\.com/([^/]+)/(.+)$`),
}

// providerSemanticRepoKey returns the repo_key entire-graph emits for repoDir
// with the given `origin` remote (empty when the repository has none).
func providerSemanticRepoKey(repoDir, originRemote string) string {
	if key, ok := providerSemanticGitHubRepoKey(originRemote); ok {
		return key
	}
	base := filepath.Base(filepath.Clean(repoDir))
	return "local/" + base
}

func providerSemanticGitHubRepoKey(remoteURL string) (string, bool) {
	remoteURL = strings.TrimSpace(remoteURL)
	remoteURL = strings.TrimRight(remoteURL, "/")
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	for _, pattern := range semanticProviderGitHubRemotePatterns {
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

// semanticRepoIdentity is the set of repo_key spellings that legitimately name
// one repository across the seam, plus the context needed to explain a
// mismatch to an operator.
type semanticRepoIdentity struct {
	// StorageKey is Brain's own key: what Brain writes into headers it
	// synthesizes (the --skip-graph path) and what older snapshots carry.
	StorageKey string
	// ProviderKey is what the provider contract says entire-graph emits.
	ProviderKey string
	RepoDir     string
	GraphBinary string
}

// newSemanticRepoIdentity resolves both spellings for repoDir. A failing
// `git remote get-url origin` (no remote configured) is the no-remote case, not
// an error: the provider rule falls back to local/<basename> exactly as
// entire-graph does.
func newSemanticRepoIdentity(ctx context.Context, runner CommandRunner, repoDir, storageKey, graphBinary string) semanticRepoIdentity {
	remote := ""
	if runner != nil {
		if stdout, _, err := runner.Run(ctx, repoDir, "git", "remote", "get-url", "origin"); err == nil {
			remote = strings.TrimSpace(string(stdout))
		}
	}
	return semanticRepoIdentity{
		StorageKey:  storageKey,
		ProviderKey: providerSemanticRepoKey(repoDir, remote),
		RepoDir:     repoDir,
		GraphBinary: strings.TrimSpace(graphBinary),
	}
}

// expected lists the accepted spellings, storage key first.
func (id semanticRepoIdentity) expected() []string {
	out := make([]string, 0, 2)
	if id.StorageKey != "" {
		out = append(out, id.StorageKey)
	}
	if id.ProviderKey != "" && !semanticRepoKeyEqual(id.ProviderKey, id.StorageKey) {
		out = append(out, id.ProviderKey)
	}
	return out
}

func (id semanticRepoIdentity) accepts(key string) bool {
	for _, candidate := range id.expected() {
		if semanticRepoKeyEqual(key, candidate) {
			return true
		}
	}
	return false
}

// mismatchError explains a repo_key that matches neither spelling. A skewed
// installed entire-graph can still reach here, so the message must name the key
// that arrived, both keys that were expected, where each came from, and the
// remedy — never a bare "does not match".
func (id semanticRepoIdentity) mismatchError(subject, found string) error {
	binary := id.GraphBinary
	if binary == "" {
		binary = "entire"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s repo_key %q does not name this repository", subject, found)
	if id.RepoDir != "" {
		fmt.Fprintf(&b, " (%s)", id.RepoDir)
	}
	b.WriteString("; expected ")
	if id.ProviderKey != "" {
		fmt.Fprintf(&b, "%q from the entire-graph provider rule (github origin -> gh/<owner>/<name>, otherwise local/<basename>)", id.ProviderKey)
		if id.StorageKey != "" && !semanticRepoKeyEqual(id.StorageKey, id.ProviderKey) {
			fmt.Fprintf(&b, " or %q, this brain's storage key", id.StorageKey)
		}
	} else {
		fmt.Fprintf(&b, "%q, this brain's storage key", id.StorageKey)
	}
	fmt.Fprintf(&b,
		". The installed provider most likely derives repo_key differently: check `%s graph version` and the first line of `%s graph snapshot --repo %s --format ndjson`, upgrade the Entire CLI, or point --graph-binary at a build that matches this brain.",
		binary, binary, quotedRepoDirForHint(id.RepoDir))
	return fmt.Errorf("%s", b.String())
}

func quotedRepoDirForHint(repoDir string) string {
	if repoDir == "" {
		return "."
	}
	if strings.ContainsAny(repoDir, " \t\"'") {
		return "'" + strings.ReplaceAll(repoDir, "'", `'\''`) + "'"
	}
	return repoDir
}
