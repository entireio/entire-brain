package cli

import "slices"

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
//
// The flags below cover the first three vectors. The filter drivers have no
// flag equivalent and are handled by repoFilterDriverOverrides, applied at the
// same single spawn point in runner.go.
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
var (
	// gitHardenConfig applies to every git invocation. Verified accepted by
	// every subcommand this binary runs, including clone.
	gitHardenConfig = []string{"-c", "core.fsmonitor=false"}

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
