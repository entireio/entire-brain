package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// git resolves several configuration keys to *commands it executes*, and it
// honours them from the per-repository .git/config — plus the in-tree
// .gitattributes — of whatever repository it is pointed at. A repository is
// data, not a trust boundary: fingerprinting or indexing one must not run code
// that repository carries. The vectors below were each confirmed against
// git 2.54.0 by arming them in a scratch repository and observing the payload
// run:
//
//   - diff.external (and GIT_EXTERNAL_DIFF) runs for `git diff`, worktree and
//     --cached alike. It also *replaces* the diff, so an armed repository makes
//     `git diff --binary HEAD` return nothing at all — a silent corruption of
//     any fingerprint computed from that output. `git log -p` and `git show`
//     were verified NOT to honour it unless --ext-diff is passed.
//   - diff.<driver>.textconv, selected by an in-tree .gitattributes entry, runs
//     for every command that renders patch text: `git diff` in all its forms
//     and `git log -p`.
//   - core.fsmonitor runs for `git status`, every `git diff` form, `git
//     ls-files` (with and without --others) and `git diff-tree`.
//   - filter.<driver>.{clean,smudge,process}, selected by an in-tree
//     .gitattributes entry, runs when git converts between worktree and stored
//     content -- which `git diff HEAD` must do to compare them. There is no
//     per-command flag for it, so it is neutralized in the environment instead;
//     see git_harden_filters.go, which is the other half of this hardening
//     point.
//   - the hook directory needs no config key at all: an executable
//     .git/hooks/post-index-change runs for any command that REWRITES THE
//     INDEX, which `git status` does for a tracked file whose stat data went
//     stale while its content did not. That makes it the cheapest vector of the
//     five to arm -- a file and a chmod, nothing in .git/config -- and
//     core.hooksPath relocates the directory for an attacker who prefers a
//     config key. Confirmed on git 2.54.0: a first `refresh --worktree` ran the
//     payload 3x and `status` and `overview` 1x each, every one of them while
//     the command reported success. Only post-index-change is reachable while
//     this binary merely reads, but the hook directory belongs to the
//     repository, so any hook it ships is one future write-side git command
//     away from running.
//
// The flags below cover the first three vectors and the hooks. The filter
// drivers have no flag equivalent and are handled by repoFilterDriverOverrides,
// applied at the same single spawn point in runner.go.
//
// Neutralizing the diff drivers with `-c diff.external=` does NOT work: git
// still takes the external-diff path and aborts with "external diff died,
// stopping at <path>", losing the output. The per-command --no-ext-diff and
// --no-textconv flags are the only neutralizers that both stop the execution
// and preserve correct output, so they are applied as flags rather than config.
//
// core.fsmonitor is a pure performance cache, so disabling it is behaviour
// preserving: the commands return identical results, just without the
// filesystem-monitor shortcut.
// gitHooksDisabledPath is the value core.hooksPath is pinned to. git never
// creates or writes this path -- it only probes <path>/<hook-name> for an
// executable -- so any value that cannot resolve to a directory disables every
// hook at once, which is exactly the blunt instrument wanted here: this binary
// reads repositories and has no use for a hook of any name.
//
// The value must be ABSOLUTE. git resolves a relative core.hooksPath against
// the worktree, and the worktree is the attacker's, so a relative value is one
// `mkdir` away from naming an attacker-supplied hook directory -- it would hand
// over the vector it is meant to close.
//
// /dev/null is a character device, so /dev/null/<anything> is ENOTDIR by
// construction: `mkdir -p` on it fails even for root, a guarantee a merely
// not-yet-existing directory cannot make. The trailing segment carries the
// intent into `ps` output and keeps the value inert on Windows too, where there
// is no /dev/null and the path simply does not resolve. Verified on git 2.54.0
// to add neither stderr nor a non-zero exit to status, rev-parse, diff, log,
// ls-files, worktree, gc, clone or commit.
const gitHooksDisabledPath = "/dev/null/entire-brain-hooks-disabled"

// realGitHooksDir resolves the hooks directory git would use WITHOUT the pin
// above. It lives here, beside the pin, because it is the sanctioned way to
// read around it: `rev-parse --git-path hooks` and plain config reads through
// the hardened runner always answer the sentinel, and the next caller must
// not have to rediscover that. `git config --get-all` lists values in
// ascending precedence with the pinned entry guaranteed last (command-line
// config wins), so the last value that is not the sentinel is the user's
// effective core.hooksPath — whatever mechanism injected it. None means the
// default, <git-common-dir>/hooks; the common dir so linked worktrees share
// one hooks directory, exactly as git does. A relative value resolves against
// the worktree, matching git (see the pin's comment on why that matters).
// The second return reports whether the resolved directory belongs to THIS
// repository (under its worktree or its git common dir). A shared
// core.hooksPath — a global ~/.githooks, say — resolves for every repository
// at once, and a caller that installs or removes files there acts on repos it
// was never asked about.
func realGitHooksDir(ctx context.Context, runner CommandRunner, repoDir string) (string, bool, error) {
	commonOut, err := gitScalar(ctx, runner, repoDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", false, fmt.Errorf("resolve git dir: %w", err)
	}
	commonDir := strings.TrimSpace(commonOut)
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repoDir, commonDir)
	}

	dir := filepath.Join(commonDir, "hooks")
	// The pin guarantees at least one value, so a failure here is a real
	// error, never "key unset".
	valuesOut, err := gitScalar(ctx, runner, repoDir, "config", "--get-all", "core.hooksPath")
	if err != nil {
		return "", false, fmt.Errorf("resolve core.hooksPath: %w", err)
	}
	for _, value := range strings.Split(valuesOut, "\n") {
		value = strings.TrimSpace(value)
		if value == "" || value == gitHooksDisabledPath {
			continue
		}
		resolved, resolveErr := resolveHooksPathValue(value, repoDir)
		if resolveErr != nil {
			return "", false, resolveErr
		}
		dir = resolved
	}
	inRepo := dirWithin(dir, repoDir) || dirWithin(dir, commonDir)
	return dir, inRepo, nil
}

// dirWithin reports whether path sits at or below root, lexically.
func dirWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveHooksPathValue resolves one core.hooksPath value the way git does:
// a leading ~ expands to the home directory, an absolute path stands, and a
// relative path resolves against the worktree. A ~user form is refused rather
// than misresolved — installing a hook into the wrong directory is worse than
// an install that reports why it did nothing.
func resolveHooksPathValue(value, repoDir string) (string, error) {
	switch {
	case value == "~" || strings.HasPrefix(value, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand core.hooksPath %q: %w", value, err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(value, "~"), "/")), nil
	case strings.HasPrefix(value, "~"):
		return "", fmt.Errorf("core.hooksPath %q uses ~user expansion, which this plugin does not resolve", value)
	case filepath.IsAbs(value):
		return value, nil
	default:
		return filepath.Join(repoDir, value), nil
	}
}

var (
	// gitHardenConfig applies to every git invocation. Verified accepted by
	// every subcommand this binary runs, including clone.
	gitHardenConfig = []string{
		"--no-optional-locks",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + gitHooksDisabledPath,
	}

	// gitHardenDiffFlags apply only to the subcommands that accept them.
	// `git status` and `git ls-files` reject both flags outright.
	gitHardenDiffFlags = []string{"--no-ext-diff", "--no-textconv"}

	// gitHardenDiffFamily lists the subcommands verified to accept
	// gitHardenDiffFlags on git 2.54.0. whatchanged is deliberately absent: it
	// is deprecated and refuses to run at all.
	gitHardenDiffFamily = map[string]bool{
		"diff":         true,
		"diff-files":   true,
		"diff-index":   true,
		"diff-tree":    true,
		"format-patch": true,
		"log":          true,
		"range-diff":   true,
		"rev-list":     true,
		"show":         true,
	}
)

// hardenedGitArgs rewrites a git argument list so repo-local configuration
// cannot turn a read into an execution. It is applied at the single point where
// this binary actually spawns git (see runner.go), which is what keeps it
// unbypassable: every call site in every package — including
// internal/entityindex, which receives ExecRunner straight through — is covered
// without each one having to remember.
//
// Flags are inserted immediately after the subcommand so they land before any
// `--` pathspec separator, and are skipped when the caller already passed them.
func hardenedGitArgs(args ...string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args)+len(gitHardenConfig)+len(gitHardenDiffFlags))
	out = append(out, gitHardenConfig...)

	sub := gitSubcommandIndex(args)
	if sub < 0 {
		return append(out, args...)
	}
	out = append(out, args[:sub+1]...)
	if gitHardenDiffFamily[args[sub]] {
		existing := gitOptionsBeforePathspecs(args[sub+1:])
		for _, flag := range gitHardenDiffFlags {
			if !slices.Contains(existing, flag) {
				out = append(out, flag)
			}
		}
	}
	return append(out, args[sub+1:]...)
}

// gitOptionsBeforePathspecs returns the leading run of a subcommand's arguments
// that git still parses as options, i.e. everything before the "--"
// end-of-options separator.
//
// Only that region may be consulted when deciding whether a flag is already
// present. Pathspecs after "--" are partly repository-controlled — .brainignore
// feeds exclusion pathspecs into gitDiffBinary — and a repository that shipped
// a pattern rendering as the literal "--no-ext-diff" would otherwise suppress
// the very flag that neutralizes it.
func gitOptionsBeforePathspecs(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[:i]
		}
	}
	return args
}

// gitSubcommandIndex finds the subcommand, stepping over the global options
// that may precede it (callers already pass `-c core.quotepath=false`). It
// returns -1 when the list is nothing but global options.
func gitSubcommandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "" || arg[0] != '-' {
			return i
		}
		switch arg {
		case "-c", "-C", "--git-dir", "--work-tree", "--namespace", "--exec-path":
			// These take their value as a separate argument.
			i++
		}
	}
	return -1
}
