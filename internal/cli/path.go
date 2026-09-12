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

When the target is an existing local path, it is resolved to the containing git
worktree when possible. If that brain has not been exported yet, path creates
the persistent export before printing the directory. Repo URLs are resolved to
their deterministic brain directory without exporting.`,
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

	cmd.Flags().IntVar(&pathOpts.checkpointLimit, "checkpoint-limit", defaultCheckpointLimit, "Maximum checkpoints to inspect when exporting (0 means all)")
	cmd.Flags().StringVar(&pathOpts.entireBinary, "entire-binary", "entire", "Entire CLI binary to invoke when exporting")
	cmd.Flags().BoolVar(&pathOpts.rawTranscript, "raw", false, "Export raw agent transcripts instead of normalized compact transcripts")
	cmd.Flags().StringVar(&pathOpts.scope, "scope", exportScopeAll, "Checkpoint discovery scope when exporting: all or branch")

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
		if !brainExportExists(storage.BrainDir) {
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

func runPathExport(ctx context.Context, opts Options, pathOpts pathCommandOptions, repoDir string) error {
	if pathOpts.checkpointLimit < 0 {
		return fmt.Errorf("--checkpoint-limit must be greater than or equal to zero")
	}
	if strings.TrimSpace(pathOpts.entireBinary) == "" {
		return fmt.Errorf("--entire-binary must not be empty")
	}
	if pathOpts.scope != exportScopeAll && pathOpts.scope != exportScopeBranch {
		return fmt.Errorf("--scope must be either all or branch")
	}

	exportEnv := opts.Env
	exportEnv.RepoRoot = repoDir
	exportCmd := &cobra.Command{Use: "export"}
	exportCmd.SetOut(io.Discard)
	exportCmd.SetErr(io.Discard)
	exportOpts := exportCommandOptions{
		outputDir:       defaultExportDir,
		checkpointLimit: pathOpts.checkpointLimit,
		entireBinary:    pathOpts.entireBinary,
		rawTranscript:   pathOpts.rawTranscript,
		scope:           pathOpts.scope,
	}
	return runExport(ctx, exportCmd, Options{
		Version: opts.Version,
		Env:     exportEnv,
		Runner:  opts.Runner,
		Now:     opts.Now,
	}, exportOpts)
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
