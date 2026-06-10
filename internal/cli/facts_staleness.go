package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// facts_staleness.go is Phase 2 item 4 (agent-utility plan): the per-fact
// locus-drift signal. Agents are (correctly) instructed to verify recalled
// memory before acting on it, but a fact's provenance anchors point at
// transcripts — the immutable past — so verifying "is this still true?" costs
// a transcript read. Drift binds facts to the present instead: a fact whose
// code locus no longer exists in the worktree is flagged at recall time, so
// the agent knows which facts to trust at face value and which to re-check.
//
// v1 checks unambiguous repo-relative FILE tokens only: a "/" plus a real
// file extension on the last segment ("internal/cli/semantic.go"). Slash
// alone is not enough — live locus data is full of prose constructs that
// contain one ("a/b" testing, "expected/current", "provider/head"), and a
// drift flag that cries wolf trains the agent to ignore it. Bare file names
// ("semantic.go"), directories, and symbol names need a repo walk or a
// semantic-store lookup to resolve honestly; both arrive with the worktree
// overlay seam. Drift is computed for the k facts being SURFACED, never the
// whole corpus, so the read path stays warm.

// factLocusDrift returns the file-path locus tokens of a fact that no longer
// exist in the repo worktree. Empty repoDir (no live repo, e.g. a
// bundle-only brain) reports no drift — absence of evidence, not evidence.
func factLocusDrift(repoDir string, f factRecord) []string {
	if repoDir == "" {
		return nil
	}
	var drift []string
	for _, tok := range factLocusOf(f) {
		if !locusTokenIsFilePath(tok) {
			continue
		}
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(tok))); err != nil {
			drift = append(drift, tok)
		}
	}
	return drift
}

// locusFileExtPattern: the token's last path segment must carry a short
// alphanumeric extension ("semantic.go", "plan.md") for the token to count
// as a checkable file path.
var locusFileExtPattern = regexp.MustCompile(`\.[a-z0-9]{1,6}$`)

func locusTokenIsFilePath(tok string) bool {
	if !strings.Contains(tok, "/") || strings.Contains(tok, "..") || strings.ContainsAny(tok, "*?") {
		return false
	}
	return locusFileExtPattern.MatchString(tok)
}

// factsLocusDrift maps fact id -> drifted locus tokens for the surfaced
// facts, omitting facts with no drift. nil when nothing drifted, so JSON
// surfaces stay clean for the common case.
func factsLocusDrift(repoDir string, facts []factRecord) map[string][]string {
	var out map[string][]string
	for _, f := range facts {
		if drift := factLocusDrift(repoDir, f); len(drift) > 0 {
			if out == nil {
				out = map[string][]string{}
			}
			out[f.ID] = drift
		}
	}
	return out
}
