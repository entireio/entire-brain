package cli

import (
	"path/filepath"
	"strings"
)

// The brain and the entire-graph semantic provider name the same repository
// differently, and neither naming is wrong.
//
//   - entire-graph (internal/sem/provider.go) recognises github.com remotes and
//     nothing else: `gh/<owner>/<name>`, or, failing that, the literal
//     `"local/" + filepath.Base(absRepo)` of the --repo path it was handed.
//   - the brain (internal/cli/env.go) recognises many hosts by slug — gh, gl,
//     bb, et, tg, cs, plus generated slugs — and for a repository with no
//     usable remote derives `local/<basename>-<sha256(abs path)[:12]>`. The
//     twelve-hex suffix is load bearing: without it two checkouts that happen
//     to share a basename would address ONE brain and silently merge their
//     histories.
//
// So the two agree only for a github.com remote. Every other repository — a
// GitLab or Bitbucket origin, and every `git init` repository that has no
// remote at all — produced a key the brain then rejected as "does not match
// current repo", which is a disagreement between two of our own tools reported
// to the user as a fault in their repository.
//
// The brain is the side that can fix this alone: it knows the provider's
// fallback rule, and it knows the exact --repo path it handed the provider, so
// it can recognise the provider's spelling of THIS repository and store the
// snapshot under its own key. Neither of the alternatives survives contact:
// dropping the brain's hash suffix reintroduces the same-basename collision
// above, and moving entire-graph onto the brain's suffix is a second repository
// whose repo key is baked into every symbol ID and every cached index it has
// ever written.
//
// This is not a weakening of the crossover guard. The live path validates the
// snapshot's commit and tree against this repository's HEAD immediately after
// the key (semantic.go, validateLiveSemanticHeader), and the provider was
// invoked by this process on this very directory. Those checks are what
// actually prove the snapshot describes this repository; repo_key equality was
// only ever asserting that two tools spell a name the same way.
//
// The relaxation is deliberately narrow: it applies ONLY to a snapshot the
// brain just produced itself. Bundle import (semanticRepoKeyEqual at the
// manifest and stored-snapshot boundaries) is untouched, because there the key
// is the only provenance there is.

// semanticProviderLocalRepoKey mirrors entire-graph's no-remote fallback for
// the repository directory the provider was handed. It returns "" when there
// is no basename to name — the caller must then treat the provider's key as
// unrecognised rather than accepting an empty match.
func semanticProviderLocalRepoKey(repoDir string) string {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return ""
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		abs = repoDir
	}
	base := filepath.Base(filepath.Clean(abs))
	switch base {
	case "", ".", "..", string(filepath.Separator):
		return ""
	}
	return "local/" + base
}

// semanticProviderRepoKeyAlias reports whether providerKey is the semantic
// provider's own spelling of the repository the brain knows as repoKey, and
// therefore may be stored under repoKey. A provider key that is neither equal
// to repoKey nor the provider's fallback for repoDir is left alone, so a
// provider that indexed some other directory still fails the header check with
// its own key visible in the message.
func semanticProviderRepoKeyAlias(providerKey, repoKey, repoDir string) bool {
	providerKey = strings.TrimSpace(providerKey)
	repoKey = strings.TrimSpace(repoKey)
	if providerKey == "" || repoKey == "" || semanticRepoKeyEqual(providerKey, repoKey) {
		return false
	}
	alias := semanticProviderLocalRepoKey(repoDir)
	return alias != "" && semanticRepoKeyEqual(providerKey, alias)
}
