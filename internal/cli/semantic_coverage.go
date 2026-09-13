package cli

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// semantic_coverage.go answers a question nothing else in the index path asks:
// did the semantic provider actually LOOK at every source file in the tree?
//
// The provider is the only witness to its own coverage. Its summary reports
// files, parsed_files, warnings and partial_failures, and the brain relays all
// four -- but a file the provider never considered appears in none of them. It
// is not a parse error, not a partial failure, not a warning: it is simply
// absent, and absence is indistinguishable from "this repository has no such
// file" once the snapshot is the only thing left to read.
//
// That gap is not hypothetical. entire-graph applies Go build constraints for
// the HOST platform, so on darwin every //go:build linux file, every _linux.go
// and every _windows.go file is dropped before parsing -- and the summary still
// says parsed_files == files, partial_failures == 0, completeness_level "ok".
// A cross-platform Go repository indexes with a clean bill of health while the
// whole non-host half of it is invisible to search, impact and dead-code.
//
// The reconciliation below is deliberately CONSERVATIVE, because the brain does
// not and should not reimplement the provider's file-selection policy. It flags
// a tracked file only when the provider's own behaviour in this same repository
// says it should have been parsed:
//
//   - the provider parsed at least one file of the SAME extension in the SAME
//     directory here, and
//   - it was not reported as a warning or a partial failure, and
//   - it is not filtered by .brainignore.
//
// The first filter is one key, not two. Keeping the directory set and the
// extension set independent takes their cross product, and the cross product is
// wrong in exactly the case that has no extension: `mise-tasks/lint/go` is a
// parsed file whose extension is "", and the repository root holds parsed .go
// and .md files, so an independent pair of sets flags the root LICENSE as a
// silently skipped source file. Requiring the (directory, extension) PAIR to
// have been parsed makes the evidence local to the file being judged.
//
// A vendored tree contributes no parsed file, so nothing in it is ever flagged.
// A go.mod has no parsed .mod sibling, so it is not flagged. A file type the
// provider does not handle at all never enters the set. What survives is a file
// whose immediate neighbours of its own type the provider parsed, which it
// skipped, and about which it said nothing -- exactly the silent skip this
// warning exists to name.
//
// The bias is deliberately toward silence: a lone unparsed file in a directory
// with no parsed sibling of its type goes unreported rather than risk telling a
// reader their repository is broken when it is not.

// semanticUnreportedSkipCode is the warning code for a tracked source file the
// provider neither parsed nor reported.
const semanticUnreportedSkipCode = "provider_unreported_skip"

// semanticUnreportedSkipLimit bounds how many per-file warnings one index may
// emit. A repository that trips this in bulk needs the count, not ten thousand
// manifest entries; the overflow warning carries the total.
const semanticUnreportedSkipLimit = 25

// semanticUnreportedSkips returns the tracked paths that the provider parsed
// the neighbours of but never emitted, and never reported as skipped.
//
// Pure over its inputs so the policy is testable without a provider, a git
// repository, or a filesystem.
func semanticUnreportedSkips(tracked []string, parsed map[string]struct{}, reported []semanticWarning, ignore brainIgnore) []string {
	if len(tracked) == 0 || len(parsed) == 0 {
		return nil
	}
	// Normalize the provider's spelling to the same form the tracked list is
	// compared in, so "./a/b.go" and "a/b.go" are one file rather than two.
	kinds := make(map[string]struct{}, len(parsed))
	seen := make(map[string]struct{}, len(parsed))
	for raw := range parsed {
		p := path.Clean(strings.TrimSpace(raw))
		if p == "" || p == "." {
			continue
		}
		seen[p] = struct{}{}
		kinds[semanticFileKind(p)] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	// A file the provider DID say something about is accounted for, whatever it
	// said. Only silence is a finding.
	spoken := make(map[string]struct{}, len(reported))
	for _, warning := range reported {
		if p := strings.TrimSpace(warning.Path); p != "" {
			spoken[path.Clean(p)] = struct{}{}
		}
	}
	var missing []string
	for _, raw := range tracked {
		p := path.Clean(strings.TrimSpace(raw))
		if p == "" || p == "." {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		if _, ok := spoken[p]; ok {
			continue
		}
		if _, ok := kinds[semanticFileKind(p)]; !ok {
			continue
		}
		if ignore.Ignored(p) {
			continue
		}
		missing = append(missing, p)
	}
	sort.Strings(missing)
	return missing
}

// semanticFileKind is the (directory, lowercased extension) pair that decides
// whether the provider has shown, in this repository, that it parses a file of
// this type in this place. It is the single key the narrowness of the whole
// check rests on; see the file comment for why it is not two.
func semanticFileKind(p string) string {
	return path.Dir(p) + "\x00" + strings.ToLower(path.Ext(p))
}

// semanticUnreportedSkipWarnings turns the missing paths into warnings, one per
// file up to the limit, plus a single overflow warning carrying the true total.
//
// Per-file (rather than one aggregate) is what lets `status` group them the way
// it groups every other per-file provider finding, and what lets `doctor` list
// the paths. The effect text says what is actually lost, because a reader who
// sees only a count cannot tell whether it matters.
func semanticUnreportedSkipWarnings(missing []string) []semanticWarning {
	if len(missing) == 0 {
		return nil
	}
	const effect = "file was not parsed; its symbols and relations are absent from the index"
	shown := missing
	if len(shown) > semanticUnreportedSkipLimit {
		shown = shown[:semanticUnreportedSkipLimit]
	}
	warnings := make([]semanticWarning, 0, len(shown)+1)
	for _, p := range shown {
		warnings = append(warnings, semanticWarning{
			Code:     semanticUnreportedSkipCode,
			Severity: "warning",
			Path:     p,
			Effect:   effect,
			Detail:   "semantic provider parsed neighbouring files of the same type but never emitted this one, and reported no failure for it",
		})
	}
	if len(missing) > len(shown) {
		warnings = append(warnings, semanticWarning{
			Code:     semanticUnreportedSkipCode,
			Severity: "warning",
			Effect:   effect,
			Detail: fmt.Sprintf(
				"%d tracked source file(s) were not parsed and not reported by the semantic provider; %d listed above, %d omitted",
				len(missing), len(shown), len(missing)-len(shown)),
		})
	}
	return warnings
}

// semanticTrackedFiles lists the repository paths the index was built from:
// the committed tree for a HEAD index, the worktree's tracked set for a
// --worktree index. Both spellings are git's own, so they match the paths the
// provider reports.
func semanticTrackedFiles(ctx context.Context, runner CommandRunner, repoDir, treeish string, worktree bool) ([]string, error) {
	args := []string{"ls-tree", "-r", "--name-only", "-z", treeish}
	if worktree {
		args = []string{"ls-files", "-z"}
	}
	stdout, _, err := runner.Run(ctx, repoDir, "git", args...)
	if err != nil {
		return nil, err
	}
	// -z keeps paths with newlines, quotes or non-UTF8 bytes intact; without it
	// git quotes them and the spelling stops matching the provider's.
	fields := strings.Split(string(stdout), "\x00")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out, nil
}
