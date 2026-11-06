package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Seeding a dirty worktree from committed HEAD (issue #327).
//
// The default seed is a snapshot of committed state: the manifest records the
// commit, and the freshness axis compares that commit against HEAD. The scan
// itself, though, reads the WORKING TREE, which is only the committed state
// while the tree is clean. So a dirty tree used to be refused outright -- the
// one honest answer available to a scan that cannot tell committed bytes from
// uncommitted ones.
//
// The refusal was the right call and the wrong outcome. A tree with someone
// working in it is the normal case, not the exception, and while it stays dirty
// nothing reseeds: commit, and the seed's commit falls behind HEAD, the axis
// reads `stale`, freshness reads `unsafe`, and `context --include-content` is
// refused, for as long as anyone keeps editing. The code index already stopped
// doing this by following HEAD through Graph's committed-tree store; the seed
// was the half left behind.
//
// So a dirty tree is now seeded from a private checkout of HEAD's tree instead:
// the scan runs exactly as it does on a clean tree, pointed at a directory that
// holds only committed bytes. Uncommitted content is still kept out -- the
// invariant the refusal existed for -- and the brain keeps up with HEAD.
//
// Why a checkout and not reading blobs in place: every read the scan makes goes
// through SafeOpenRegularFile, RejectSymlinkPathComponents and the bounded
// prefix readers, and pointing the same code at a real directory keeps all of
// them, unchanged, instead of growing a second, object-store flavour of each.
// Committed objects are also immutable, so an edit made while the scan runs
// cannot leak into a seed attributed to HEAD. The cost is disk and I/O for the
// files the scan will look at -- at most --max-files of them -- paid only when
// the tree is dirty, and only when a reseed is due.
//
// The checkout never touches the repository's real index: read-tree and
// checkout-index run against a scratch GIT_INDEX_FILE, so a concurrent git in
// the same repository (issue #326) sees nothing.

// seedHeadTempPrefix names the private checkout directory so a leftover from a
// killed process is recognisable in the system temp directory.
const seedHeadTempPrefix = "entire-brain-seed-head-"

// seedHeadCheckoutArgBytes bounds the paths handed to ONE checkout-index call.
// The runner passes arguments, not stdin, and Windows caps a whole command line
// at 32,767 characters; --max-files paths can be far longer than that.
const seedHeadCheckoutArgBytes = 16 * 1024

// errSeedNoCommittedHead is the one checkout failure that is not a failure: a
// repository whose branch has no commit has no committed state to seed from,
// so the dirty-tree refusal -- "commit or stash them" -- is still the answer.
var errSeedNoCommittedHead = errors.New("no committed HEAD to seed from")

// seedHeadCheckout is a private, read-only-by-convention checkout of one
// commit's tree, used as the scan root in place of a dirty working tree.
type seedHeadCheckout struct {
	// commit is the commit the checkout holds, resolved once so a commit made
	// while the seed runs cannot split the inventory from the content.
	commit string
	// dir is the root of the checked-out files.
	dir string
	// paths is the commit's whole sorted file inventory, not only the files
	// checked out, so the --max-files warning still counts what was skipped.
	paths []string
	// tempRoot holds dir and the scratch index; remove deletes it.
	tempRoot string
}

func (c *seedHeadCheckout) remove() {
	if c != nil && c.tempRoot != "" {
		_ = os.RemoveAll(c.tempRoot)
	}
}

// seedHeadGitEnv is the environment of every git call that builds the
// checkout. GIT_NO_LAZY_FETCH keeps a partial clone from fetching a missing
// object over the network in the middle of a background seed, and
// GIT_LFS_SKIP_SMUDGE does the same for an LFS smudge filter configured in the
// user's own git config (repository-scoped filter drivers are already blanked by
// repoFilterDriverOverrides). Either way the object is read locally or the
// checkout fails, and a failed checkout falls back to the refusal.
func seedHeadGitEnv(scratchIndex string) map[string]string {
	env := localOnlyGitEnv()
	env["GIT_LFS_SKIP_SMUDGE"] = "1"
	if scratchIndex != "" {
		env["GIT_INDEX_FILE"] = scratchIndex
	}
	return env
}

// checkoutSeedHead checks HEAD's tree out into a private temporary directory:
// the first maxFiles paths in the order the scan reads them, which is exactly
// the set scanSeedRepository will look at. The caller owns remove().
func checkoutSeedHead(ctx context.Context, runner CommandRunner, repoDir string, maxFiles int) (*seedHeadCheckout, error) {
	envRunner, ok := runner.(EnvironmentCommandRunner)
	if !ok {
		return nil, errors.New("command runner cannot run git against a scratch index")
	}
	// --verify --quiet answers an unborn branch with exit 1 and no output.
	// Any failure here means there is no commit to fall back on.
	stdout, _, err := runner.Run(ctx, repoDir, "git", "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	commit := strings.TrimSpace(string(stdout))
	if err != nil || commit == "" {
		return nil, errSeedNoCommittedHead
	}
	listing, _, err := envRunner.RunWithEnv(ctx, repoDir, seedHeadGitEnv(""), "git", "ls-tree", "-r", "-z", "--full-tree", "--name-only", commit)
	if err != nil {
		return nil, fmt.Errorf("list committed HEAD %s: %w", shortCommitHash(commit), err)
	}
	paths := splitNULPaths(listing)

	tempRoot, err := os.MkdirTemp("", seedHeadTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("create committed HEAD checkout: %w", err)
	}
	checkout := &seedHeadCheckout{commit: commit, dir: filepath.Join(tempRoot, "tree"), paths: paths, tempRoot: tempRoot}
	if err := checkout.populate(ctx, envRunner, repoDir, maxFiles); err != nil {
		checkout.remove()
		return nil, err
	}
	return checkout, nil
}

func (c *seedHeadCheckout) populate(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, maxFiles int) error {
	if err := os.Mkdir(c.dir, 0o700); err != nil {
		return fmt.Errorf("create committed HEAD checkout: %w", err)
	}
	env := seedHeadGitEnv(filepath.Join(c.tempRoot, "index"))
	if _, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "read-tree", c.commit); err != nil {
		return fmt.Errorf("read committed HEAD %s into a scratch index: %w", shortCommitHash(c.commit), err)
	}
	selected := c.paths
	if maxFiles > 0 && len(selected) > maxFiles {
		selected = selected[:maxFiles]
	}
	// git appends each path to the prefix verbatim, so it needs the trailing
	// separator; forward slashes are what git expects on every platform.
	prefix := "--prefix=" + filepath.ToSlash(c.dir) + "/"
	// -f: two tracked paths that differ only in case land on one file on a
	// case-insensitive filesystem. A clean checkout there keeps the last one
	// written, and so does this; without -f the second is an error and the
	// whole seed falls back to refusing.
	for _, batch := range chunkPathsByBytes(selected, seedHeadCheckoutArgBytes) {
		args := append([]string{"checkout-index", "-f", prefix, "--"}, batch...)
		if _, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", args...); err != nil {
			return fmt.Errorf("check out committed HEAD %s: %w", shortCommitHash(c.commit), err)
		}
	}
	return nil
}

// splitNULPaths splits `git ... -z` output into sorted, de-duplicated,
// slash-separated paths. NUL separation is what keeps a tracked path with a
// newline or a quote in it intact; the sort matches listSeedFiles so the cap
// selects the same files on either root.
func splitNULPaths(data []byte) []string {
	var paths []string
	for _, path := range strings.Split(string(data), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return compactSortedStrings(paths)
}

// chunkPathsByBytes groups paths so the arguments of each group stay under
// limit bytes. A single path longer than limit still gets a group of its own.
func chunkPathsByBytes(paths []string, limit int) [][]string {
	var batches [][]string
	var current []string
	size := 0
	for _, path := range paths {
		if len(current) > 0 && size+len(path)+1 > limit {
			batches = append(batches, current)
			current, size = nil, 0
		}
		current = append(current, path)
		size += len(path) + 1
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// seedHeadRefusal is what a dirty tree gets when it cannot be seeded from
// HEAD: the original refusal, unchanged in shape so setup's hint and watch's
// deferral still key on its code, plus the reason the fallback was not taken
// when there is one worth naming.
func seedHeadRefusal(checkoutErr error, dirtyPaths []string) error {
	refusal := dirtyWorktreeRefusal("build repository context from", dirtyPaths)
	if errors.Is(checkoutErr, errSeedNoCommittedHead) {
		return refusal
	}
	return fmt.Errorf("%w; committed HEAD could not be checked out on its own to seed from instead: %v", refusal, checkoutErr)
}
