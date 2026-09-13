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
