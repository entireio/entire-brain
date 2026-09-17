package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// repo-identity is the recovery path for repo_identity_conflict.
//
// A local repository's brain is stored under a key derived from its root path.
// Older builds derived that key from the path STRING the caller happened to
// supply, so one repository reached through a symlink -- /tmp on macOS is a
// symlink to /private/tmp, so this is the default, not an edge case -- could
// end up with two stores. resolveRepoStorageSet refuses to guess between them,
// which means every repo-scoped command refuses, INCLUDING the deletes that
// could clear the duplicate. The repository becomes unusable with no way out
// through the tool.
//
// This command is the way out. It is deliberately the one verb that reads the
// storage set WITHOUT the refusal, because a repair tool that is blocked by the
// condition it repairs is not a repair tool.
//
// It never deletes. --keep RENAMES the stores it is not keeping to a
// timestamped sibling, so a user who picks the wrong one can pick again by
// renaming it back. Merging two brains is not something a command can do
// safely -- they are two independent export histories -- so the choice stays
// with the person who knows which one they have been using.

// repoIdentityConflictSuffix dates the set-aside copy. Two repairs in the same
// second would otherwise collide, and a collision that silently overwrote the
// first set-aside store would destroy the history this command exists to
// preserve.
func repoIdentityConflictSuffix(now time.Time) string {
	return ".conflict-" + now.UTC().Format("20060102T150405Z")
}

type repoIdentityOptions struct {
	keep string
}

func newRepoIdentityCommand(opts Options) *cobra.Command {
	identityOpts := repoIdentityOptions{}
	cmd := &cobra.Command{
		Use:   "repo-identity [path]",
		Short: "Find and resolve conflicting brain stores",
		Long: `Repo-identity reports every brain store a local repository resolves to.

A repository normally has exactly one. It has more than one when an older build
keyed a brain on the path spelling it was addressed by -- a repository reached
through a symlinked path (on macOS, anything under /tmp) could acquire a second
store that way. While two stores exist, every other command refuses with
repo_identity_conflict rather than guess which history is the real one.

Run it with no flags to see the stores. Pass --keep <repo-key> to keep one and
move the rest aside: the others are RENAMED, not deleted, to a timestamped
sibling directory, so nothing is lost and the choice can be undone with mv.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runRepoIdentity(cmd.Context(), cmd, opts, identityOpts, target)
		},
	}
	cmd.Flags().StringVar(&identityOpts.keep, "keep", "",
		"Repo key of the store to keep; every other store for this repository is moved aside")
	return cmd
}

func runRepoIdentity(ctx context.Context, cmd *cobra.Command, opts Options, identityOpts repoIdentityOptions, target string) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("repo-identity requires a local repository path: %s", target)
	}
	set, err := resolveRepoStorageSet(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	keep := strings.TrimSpace(identityOpts.keep)
	if keep == "" {
		return reportRepoIdentity(cmd, set)
	}
	return resolveRepoIdentityConflict(cmd, opts, set, keep)
}

func reportRepoIdentity(cmd *cobra.Command, set repoStorageSet) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "canonical repo key: %s\n", set.Canonical.Key)
	fmt.Fprintf(out, "canonical brain:    %s\n", set.Canonical.BrainDir)
	switch len(set.Populated) {
	case 0:
		fmt.Fprintln(out, "stores with state:  none (this repository has no brain yet)")
	case 1:
		fmt.Fprintf(out, "stores with state:  1 (%s), no conflict\n", set.Populated[0].Key)
	default:
		fmt.Fprintf(out, "stores with state:  %d, reported as %s\n", len(set.Populated), localRepoIdentityConflictCode)
		for _, storage := range set.Populated {
			fmt.Fprintf(out, "  %s\n    brain: %s\n    head:  %s\n", storage.Key, storage.BrainDir, storage.HeadPath)
		}
		fmt.Fprintf(out, "keep one with: %s repo-identity --keep <repo-key>\n", setupCommandPrefix(os.LookupEnv))
	}
	return nil
}

// resolveRepoIdentityConflict keeps one store and renames the others aside.
//
// Renaming rather than deleting is the whole point: the caller is choosing
// between two real histories with only a key to tell them apart, and a wrong
// choice must be recoverable with `mv`. os.Rename is also atomic within a
// filesystem, so an interrupted repair leaves each store either where it was or
// where it was moved to, never half-copied.
func resolveRepoIdentityConflict(cmd *cobra.Command, opts Options, set repoStorageSet, keep string) error {
	keys := make([]string, 0, len(set.Populated))
	for _, storage := range set.Populated {
		keys = append(keys, storage.Key)
	}
	if !slices.Contains(keys, keep) {
		sort.Strings(keys)
		known := "none (this repository has no stored brain)"
		if len(keys) > 0 {
			known = strings.Join(keys, ", ")
		}
		return fmt.Errorf("--keep %q names no store for this repository; stores holding state: %s", keep, known)
	}
	if len(set.Populated) == 1 {
		fmt.Fprintf(cmd.OutOrStdout(), "%s is already the only store for this repository; nothing to move aside\n", keep)
		return nil
	}

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	suffix := repoIdentityConflictSuffix(now())
	out := cmd.OutOrStdout()
	var kept repoStorage
	for _, storage := range set.Populated {
		if storage.Key == keep {
			kept = storage
			continue
		}
		if err := moveRepoStorageAside(out, storage, suffix); err != nil {
			return err
		}
	}
	// Leave the repository in the ONE state where every route agrees: the kept
	// history living at the canonical key.
	//
	// Stopping here would only half-repair it. An adopted non-canonical store is
	// reachable through the spelling that created it and invisible to every
	// other -- a `cd`-and-run would find the canonical key empty and start a
	// fresh brain beside the one just rescued, and the conflict would be back.
	if kept.Key != set.Canonical.Key {
		promoted, err := promoteRepoStorage(out, kept, set.Canonical, suffix)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "kept: %s, now stored under the canonical key %s\n", kept.Key, promoted.Key)
		return nil
	}
	fmt.Fprintf(out, "kept: %s\n", kept.Key)
	return nil
}

// moveRepoStorageAside retires both halves of one store.
//
// The head store is a per-key DIRECTORY (it holds the export cursor alongside
// the setup record and the watch cursor), so it moves with its brain. Leaving
// the head behind would leave the conflict standing -- repoStorageContainsState
// counts either half, so a retired brain with a live head is still a second
// identity.
func moveRepoStorageAside(out io.Writer, storage repoStorage, suffix string) error {
	for _, dir := range []string{storage.BrainDir, filepath.Dir(storage.HeadPath)} {
		moved, err := moveRepoStoreAside(dir, suffix)
		if err != nil {
			return err
		}
		if moved != "" {
			fmt.Fprintf(out, "moved aside: %s -> %s\n", dir, moved)
		}
	}
	return nil
}

// promoteRepoStorage renames a kept store onto the canonical key.
//
// Each half is one os.Rename within a single filesystem, so each is atomic:
// nothing is ever half-copied. The two renames are not atomic TOGETHER, and
// that is the residual risk. A crash between them leaves the brain at the
// canonical key and the head still at the old one -- which reads as two
// identities again, so the next command reports the same conflict and this same
// command repairs it. Nothing is lost either way, which is why this moves
// rather than copies and never removes.
func promoteRepoStorage(out io.Writer, kept, canonical repoStorage, suffix string) (repoStorage, error) {
	// Whatever is sitting at the canonical paths is not the history being kept
	// (the caller chose against it, or it is an empty shell left by a command
	// that resolved the key without writing). Retire it rather than write over
	// it.
	if err := moveRepoStorageAside(out, canonical, suffix); err != nil {
		return repoStorage{}, err
	}
	moves := [][2]string{
		{kept.BrainDir, canonical.BrainDir},
		{filepath.Dir(kept.HeadPath), filepath.Dir(canonical.HeadPath)},
	}
	for _, move := range moves {
		from, to := move[0], move[1]
		if _, err := os.Lstat(from); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return repoStorage{}, fmt.Errorf("inspect %s: %w", from, err)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return repoStorage{}, fmt.Errorf("create %s: %w", filepath.Dir(to), err)
		}
		if err := os.Rename(from, to); err != nil {
			return repoStorage{}, fmt.Errorf("move %s to the canonical key: %w", from, err)
		}
		fmt.Fprintf(out, "promoted: %s -> %s\n", from, to)
	}
	return canonical, nil
}

// moveRepoStoreAside renames dir to a timestamped sibling and reports the new
// name, or "" when there was nothing there. It refuses to overwrite an existing
// set-aside store rather than destroy a previous repair's copy.
func moveRepoStoreAside(dir, suffix string) (string, error) {
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("inspect %s: %w", dir, err)
	}
	target := dir + suffix
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("cannot move %s aside: %s already exists", dir, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect %s: %w", target, err)
	}
	if err := os.Rename(dir, target); err != nil {
		return "", fmt.Errorf("move %s aside: %w", dir, err)
	}
	return target, nil
}
