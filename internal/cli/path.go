package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type pathCommandOptions struct {
	checkpointLimit int
	entireBinary    string
	rawTranscript   bool
	scope           string
	// ensure opts INTO building a missing brain. It is off by default because
	// `path` is a getter: it answers "where does this repository's brain live".
	// It used to refresh a missing brain unconditionally, so a single read-only
	// question against an empty store wrote a seed corpus, a docs FTS index, a
	// pattern corpus, a manifest and a lock directory -- and in a directory git
	// could not answer for, it failed inside that build with a raw
	// `fatal: not a git repository` from a subprocess the caller never asked for.
	ensure bool
}

func newPathCommand(opts Options) *cobra.Command {
	pathOpts := pathCommandOptions{
		checkpointLimit: defaultCheckpointLimit,
		entireBinary:    "entire",
		scope:           exportScopeAll,
	}

	cmd := &cobra.Command{
		Use:   "path [path-or-repo-url]",
		Short: "Print the persistent brain path for a repo path or URL",
		Long: `Path prints the persistent brain export directory for a repository.

Path is a getter: by default it writes nothing. It resolves the target -- an
existing local path to its containing git worktree, a repo URL to its
deterministic brain directory -- and prints where that repository's brain
lives. The directory is printed whether or not the brain has been built yet;
build it with "setup" or "refresh", or pass --ensure to build a missing brain
here.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runPath(cmd.Context(), cmd, opts, pathOpts, target)
		},
	}

	cmd.Flags().BoolVar(&pathOpts.ensure, "ensure", false, "Build the brain when it is missing instead of only printing where it would live")
	cmd.Flags().IntVar(&pathOpts.checkpointLimit, "checkpoint-limit", defaultCheckpointLimit, "Maximum checkpoints to inspect when building with --ensure (0 means all)")
	cmd.Flags().StringVar(&pathOpts.entireBinary, "entire-binary", "entire", "Entire CLI binary to invoke when building with --ensure")
	cmd.Flags().BoolVar(&pathOpts.rawTranscript, "raw", false, "Export raw agent transcripts instead of normalized compact transcripts when building with --ensure")
	cmd.Flags().StringVar(&pathOpts.scope, "scope", exportScopeAll, "Checkpoint discovery scope when building with --ensure: all or branch")

	return cmd
}

func runPath(ctx context.Context, cmd *cobra.Command, opts Options, pathOpts pathCommandOptions, target string) error {
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		return err
	}

	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		if looksLikeWindowsDriveTargetPath(target) {
			return err
		}
		key, ok, keyErr := repoKeyFromRemote(dirs.Config, target)
		if keyErr != nil {
			return keyErr
		}
		if !ok {
			return err
		}
		brainDir, dirErr := brainDirForKey(opts.Env, key)
		if dirErr != nil {
			return dirErr
		}
		fmt.Fprintln(cmd.OutOrStdout(), brainDir)
		return nil
	}
	if local {
		storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
		if err != nil {
			return err
		}
		if err := checkPathTargetIsAddressable(ctx, opts, storage, repoDir); err != nil {
			return err
		}
		if pathOpts.ensure && !brainExportExists(storage.BrainDir) {
			if err := pathOpts.validateForEnsure(); err != nil {
				return err
			}
			if err := runPathRefresh(ctx, opts, pathOpts, repoDir); err != nil {
				return err
			}
		}
		fmt.Fprintln(cmd.OutOrStdout(), storage.BrainDir)
		return nil
	}

	if looksLikeWindowsDriveTargetPath(target) {
		return fmt.Errorf("target is neither an existing path nor a supported repo URL: %s", target)
	}
	key, ok, err := repoKeyFromRemote(dirs.Config, target)
	if err != nil {
		return err
	}
	if ok {
		// brainDirForKey rather than a raw join: `brain path` must print the
		// location every other command would actually use, which means it has to
		// answer to the same reserved-segment and symlink refusals. A path this
		// helper refuses is one no other verb will read or write, so printing it
		// hands the caller a location the tool does not honour.
		brainDir, dirErr := brainDirForKey(opts.Env, key)
		if dirErr != nil {
			return dirErr
		}
		fmt.Fprintln(cmd.OutOrStdout(), brainDir)
		return nil
	}
	return fmt.Errorf("target is neither an existing path nor a supported repo URL: %s", target)
}

// checkPathTargetIsAddressable refuses a local target git cannot answer for,
// UNLESS a brain is already stored for it.
//
// resolveLocalTargetRepoDir deliberately tolerates a non-repository, so without
// this check `path` printed a brain directory derived from a path hash for any
// directory at all -- a location `refresh` can never populate, because every
// source the brain is built from is read out of git. Printing it made a
// `BRAIN=$(entire brain path)` caller believe it had an answer. The existing-brain
// exemption keeps a brain reachable after its worktree loses .git, which is
// exactly when a reader needs to find it.
func checkPathTargetIsAddressable(ctx context.Context, opts Options, storage repoStorage, repoDir string) error {
	if _, ok := gitWorkTreeRoot(ctx, opts.Runner, repoDir); ok {
		return nil
	}
	stored, err := repoStorageContainsState(storage)
	if err != nil {
		return err
	}
	if stored {
		return nil
	}
	brainCmd := setupCommandPrefix(os.LookupEnv)
	return fmt.Errorf("not a git repository: %s\n"+
		"%[2]s path reports where a repository's brain lives, and a repository's identity -- like every source its brain is built from -- comes from git.\n"+
		"run `git init` here, or name a repository: %[2]s path <path-or-repo-url>", repoDir, brainCmd)
}

// validateForEnsure checks the build flags, which are only read when --ensure
// actually builds something.
func (o pathCommandOptions) validateForEnsure() error {
	if o.checkpointLimit < 0 {
		return fmt.Errorf("--checkpoint-limit must be greater than or equal to zero")
	}
	if strings.TrimSpace(o.entireBinary) == "" {
		return fmt.Errorf("--entire-binary must not be empty")
	}
	if o.scope != exportScopeAll && o.scope != exportScopeBranch {
		return fmt.Errorf("--scope must be either all or branch")
	}
	return nil
}

func looksLikeWindowsDrivePath(target string) bool {
	return len(target) >= 2 &&
		((target[0] >= 'A' && target[0] <= 'Z') || (target[0] >= 'a' && target[0] <= 'z')) &&
		target[1] == ':'
}

func looksLikeWindowsDriveTargetPath(target string) bool {
	if !looksLikeWindowsDrivePath(target) {
		return false
	}
	if match := repoRemoteSCPRegex.FindStringSubmatch(target); len(match) == 3 && !looksLikeWindowsDriveRemotePath(target, match[2]) {
		return false
	}
	return true
}

func resolveLocalTargetRepoDir(ctx context.Context, runner CommandRunner, target string) (string, bool, error) {
	info, err := os.Stat(target)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("stat target path: %w", err)
	}

	dir := target
	if !info.IsDir() {
		dir = filepath.Dir(target)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", true, fmt.Errorf("resolve target path: %w", err)
	}

	if runner != nil {
		stdout, _, err := runner.Run(ctx, abs, "git", "rev-parse", "--show-toplevel")
		if err == nil {
			root := filepath.Clean(strings.TrimSpace(string(stdout)))
			if root != "" {
				if !filepath.IsAbs(root) {
					root = filepath.Join(abs, root)
				}
				rootAbs, absErr := filepath.Abs(root)
				if absErr != nil {
					return "", true, fmt.Errorf("resolve git root: %w", absErr)
				}
				// Git may report an ancestor's physical spelling even though the
				// requested path used an established logical spelling (notably
				// macOS /var versus /private/var). Prefer the matching lexical
				// ancestor so path-hashed local identities remain stable.
				// A relative target carries no lexical root spelling of its own.
				// On macOS, filepath.Abs after Chdir may expand /var to
				// /private/var; in that case Git's absolute root is the only
				// caller-provided spelling and must win. Preserve a matching
				// lexical ancestor only for an explicitly absolute target.
				if filepath.IsAbs(target) {
					if lexicalRoot, ok := matchingLexicalAncestor(abs, rootAbs); ok {
						abs = lexicalRoot
					} else {
						abs = rootAbs
					}
				} else {
					abs = rootAbs
				}
			}
		}
	}

	return abs, true, nil
}

// gitWorkTreeRoot reports the work-tree root git resolves for dir, and whether
// dir is inside a git work tree at all.
//
// resolveLocalTargetRepoDir deliberately TOLERATES a non-repository (a path is
// still a path, and workspace members keep path hints for repos that are not
// present yet), so the callers that REQUIRE a repository have to ask for
// themselves. Without this, `setup` in a plain directory discovered the answer
// one component at a time, as four separate raw `fatal: not a git repository`
// subprocess failures, after it had already registered a workspace and
// installed a background watcher.
func gitWorkTreeRoot(ctx context.Context, runner CommandRunner, dir string) (string, bool) {
	if runner == nil || strings.TrimSpace(dir) == "" {
		return "", false
	}
	stdout, _, err := runner.Run(ctx, dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(stdout))
	if root == "" {
		return "", false
	}
	return filepath.Clean(root), true
}

// firstErrorLine is an error reduced to its opening line, for the one-line
// slots -- progress notes, status axes -- that a multi-line named condition
// would otherwise wrap into nonsense.
func firstErrorLine(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		return text[:idx]
	}
	return text
}

// nameDegenerateRepoFailure replaces a raw git failure with the repository
// condition that explains it.
//
// A brain command run against a repository with no commits exited 1 -- the right
// code -- while printing whichever git subprocess happened to notice first,
// verbatim, including the hardening flags this binary adds and the caller never
// typed:
//
//	resolve HEAD for semantic index: git [-c core.fsmonitor=false rev-parse HEAD]:
//	exit status 128: fatal: ambiguous argument 'HEAD': unknown revision or path
//	not in the working tree.
//
// Only the conditions one further probe settles are named: no work tree at all,
// a bare repository, and an unborn HEAD. A detached HEAD and a shallow clone are
// deliberately absent -- both build a brain, and the surfaces that care already
// describe them (branch_tip=stale, and the shallow-boundary warning entityindex
// raises) -- and every other failure keeps git's own text, which is worth more
// than a guess.
//
// The probes run only on a path that has already failed, so a healthy repo pays
// nothing for them.
func nameDegenerateRepoFailure(ctx context.Context, runner CommandRunner, repoDir string, err error) error {
	if err == nil || runner == nil {
		return err
	}
	brainCmd := setupCommandPrefix(os.LookupEnv)
	if _, ok := gitWorkTreeRoot(ctx, runner, repoDir); !ok {
		if bare, bareErr := gitScalar(ctx, runner, repoDir, "rev-parse", "--is-bare-repository"); bareErr == nil && strings.TrimSpace(bare) == "true" {
			return fmt.Errorf("bare repository has no working tree: %s\n"+
				"%[2]s reads seed, docs and the semantic index out of a working tree, and a bare repository has none.\n"+
				"point it at a clone with a checkout: %[2]s refresh <path>", repoDir, brainCmd)
		}
		return fmt.Errorf("not a git repository: %s\n"+
			"%[2]s builds every source it has -- sessions, seed, docs, semantic index -- from git history, so it needs one.\n"+
			"run `git init` here, or name a repository: %[2]s refresh <path>", repoDir, brainCmd)
	}
	// An unborn HEAD has to be shown, not inferred from the failure that got us
	// here: `git rev-parse HEAD` also fails when git itself is unavailable, and
	// answering that with "no commits yet" would be a confident wrong diagnosis.
	//
	// symbolic-ref SUCCEEDING is the positive evidence -- it proves git ran and
	// the repo is readable, and it returns the branch HEAD points at even when
	// that branch has no commit. If that branch then does not resolve, the only
	// thing left is that nothing has been committed to it. A detached HEAD has
	// no symbolic ref and falls through here untouched, which is correct: it is
	// not a condition this names.
	ref, refErr := gitScalar(ctx, runner, repoDir, "symbolic-ref", "--quiet", "HEAD")
	if refErr != nil || strings.TrimSpace(ref) == "" {
		return err
	}
	if _, resolved := gitScalar(ctx, runner, repoDir, "rev-parse", "--verify", "--quiet", strings.TrimSpace(ref)); resolved == nil {
		return err
	}
	return fmt.Errorf("this repository has no commits yet: %s\n"+
		"%[2]s pins every source it builds to a commit, and an unborn branch has none.\n"+
		"make at least one commit, then run `%[2]s refresh` again", repoDir, brainCmd)
}

func matchingLexicalAncestor(path, target string) (string, bool) {
	targetInfo, err := os.Stat(target)
	if err != nil {
		return "", false
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil && os.SameFile(info, targetInfo) {
			return current, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
	}
}

func brainExportExists(brainDir string) bool {
	info, err := os.Stat(filepath.Join(brainDir, exportManifestFileName))
	return err == nil && !info.IsDir()
}

func runPathRefresh(ctx context.Context, opts Options, pathOpts pathCommandOptions, repoDir string) error {
	refreshCmd := &cobra.Command{Use: "refresh"}
	refreshCmd.SetOut(io.Discard)
	refreshCmd.SetErr(io.Discard)
	refreshEnv := opts.Env
	refreshEnv.RepoRoot = repoDir
	return runRefresh(ctx, refreshCmd, Options{
		Version: opts.Version,
		Env:     refreshEnv,
		Runner:  opts.Runner,
		Now:     opts.Now,
	}, refreshCommandOptions{
		checkpointLimit: pathOpts.checkpointLimit,
		entireBinary:    pathOpts.entireBinary,
		rawTranscript:   pathOpts.rawTranscript,
		scope:           pathOpts.scope,
		seed: seedCommandOptions{
			includeTests:       true,
			maxFileBytes:       defaultSeedMaxFileBytes,
			maxFiles:           defaultSeedMaxFiles,
			format:             "markdown+json",
			agent:              "none",
			agentTimeoutAction: "keep-quick",
			agentMaxInputBytes: defaultAgentMaxInput,
		},
	})
}
