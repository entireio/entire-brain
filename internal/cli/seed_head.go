package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
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
// files the scan reads -- at most --max-files of them -- paid only when the
// tree is dirty, and only when a reseed is due. A file the scan only stats (a
// denied path, an excluded test, binary media, anything over --max-file-bytes)
// is never written out: its committed size settles its entry, so a tracked
// multi-gigabyte binary or a vendored tree costs nothing here, and a committed
// secret is not copied anywhere. What is written is what the scan reads.
//
// The checkout never touches the repository's real index: read-tree and
// checkout-index run against a scratch GIT_INDEX_FILE, so a concurrent git in
// the same repository (issue #326) sees nothing. Nor does it read the
// worktree's .gitattributes: checkout-index and check-attr take them from the
// scratch index, which holds the commit's tree, so an uncommitted attribute
// change cannot re-encode or re-filter the committed bytes.

// seedHeadTempPrefix names the private checkout directory so a leftover from a
// killed process is recognisable in the system temp directory.
const seedHeadTempPrefix = "entire-brain-seed-head-"

// seedHeadCheckoutArgBytes bounds the paths handed to ONE checkout-index call.
// The runner passes arguments, not stdin, and Windows caps a whole command line
// at 32,767 characters; --max-files paths can be far longer than that.
const seedHeadCheckoutArgBytes = 16 * 1024

// seedHeadLFSPointerMaxBytes is the largest blob Git LFS treats as a pointer.
const seedHeadLFSPointerMaxBytes = 1024

// seedHeadDriverMaxShrink is the shrink a filter driver is allowed before its
// file is treated as too large unread. Smudge drivers in use (decrypting,
// decompressing, stripping) leave a file about its size or grow it. Without a
// cap, an identity driver on a multi-gigabyte blob is run and written out only
// to be dropped. Under it, each driver file is checked out on its own and
// dropped at once when the scan only stats it (populate), so a driver that
// grows a file never leaves more than that one file behind.
const seedHeadDriverMaxShrink = 64

// seedHeadDriverBatchFiles is how many filter-driver files one checkout-index
// call writes before their sizes are checked: few enough that a driver that
// grows its files leaves only that many on disk at once, many enough that a
// filter used across a repository costs a call per batch, not per file.
const seedHeadDriverBatchFiles = 32

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
	// unread holds the committed size of each selected file that was left
	// out of dir because its path and size alone settle its scan entry
	// (populate). The scan builds those entries from the size.
	unread map[string]int64
	// warnings are what the checkout could not do faithfully, for the scan
	// to report.
	warnings []string
	// foldsCase is whether dir's filesystem treats names that differ only in
	// case as one file (seedHeadFoldsCase). Where the repository's filesystem
	// does otherwise and that would change what the scan reads, there is no
	// checkout (seedHeadCaseMismatch).
	foldsCase bool
	// readers lists, for a path checked out on behalf of its case aliases,
	// every selected path whose scan reads its file (readersOf). It is empty
	// on a filesystem that does not fold case.
	readers map[string][]string
	// tempRoot holds dir and the scratch index; remove deletes it.
	tempRoot string
}

func (c *seedHeadCheckout) remove() {
	if c != nil && c.tempRoot != "" {
		_ = os.RemoveAll(c.tempRoot)
	}
}

// unreadSize reports the committed size of a file the checkout left out. It is
// nil-safe: a working-tree scan has no checkout and leaves nothing out.
func (c *seedHeadCheckout) unreadSize(rel string) (int64, bool) {
	if c == nil {
		return 0, false
	}
	size, ok := c.unread[rel]
	return size, ok
}

// seedHeadEntry is one file in the commit's tree, as `ls-tree -l` lists it.
type seedHeadEntry struct {
	mode   string
	kind   string
	object string
	// size is the blob's size in the object store, or -1 for an entry with
	// none (a submodule) or whose size was not read (listSeedHeadTree).
	size int64
	// missing marks a blob this partial clone does not have.
	missing bool
}

// regularFile reports whether the entry is an ordinary file (not a symlink or
// a submodule). Only those can be left out: the scan's answer for the other
// two depends on what checkout-index puts on disk for them.
func (e seedHeadEntry) regularFile() bool {
	return e.ordinaryFile() && e.size >= 0
}

// ordinaryFile reports whether the entry is an ordinary file, whether or not
// its size was read: a partial clone lists none past --max-files.
func (e seedHeadEntry) ordinaryFile() bool {
	return e.mode == "100644" || e.mode == "100755"
}

// seedHeadGitEnv is the environment of every git call that builds the
// checkout. GIT_NO_LAZY_FETCH keeps a partial clone from fetching a missing
// object over the network in the middle of a background seed, and
// GIT_LFS_SKIP_SMUDGE does the same for an LFS smudge filter configured in the
// user's own git config (repository-scoped filter drivers are already blanked by
// repoFilterDriverOverrides). Either way the object is read locally or the
// checkout fails, and a failed checkout falls back to the refusal; an LFS
// object already stored locally is then filled in by settleLFSPointers. Where
// the lfs filter is git-lfs, populate leaves it out of the checkout
// altogether, since its command line can set GIT_LFS_SKIP_SMUDGE itself.
func seedHeadGitEnv(scratchIndex string) map[string]string {
	env := localOnlyGitEnv()
	env["GIT_LFS_SKIP_SMUDGE"] = "1"
	if scratchIndex != "" {
		env["GIT_INDEX_FILE"] = scratchIndex
	}
	return env
}

// checkoutSeedHead checks HEAD's tree out into a private temporary directory:
// of the first --max-files paths in the order the scan reads them, which is
// exactly the set scanSeedRepository will look at, the ones whose bytes the
// scan reads. The caller owns remove().
func checkoutSeedHead(ctx context.Context, runner CommandRunner, repoDir string, opts seedCommandOptions) (*seedHeadCheckout, error) {
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
	paths, entries, err := listSeedHeadTree(ctx, envRunner, repoDir, commit, opts.maxFiles)
	if err != nil {
		return nil, err
	}

	tempRoot, err := os.MkdirTemp("", seedHeadTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("create committed HEAD checkout: %w", err)
	}
	foldsCase := seedHeadFoldsCase(tempRoot)
	if repoFolds, ok := seedRepoFoldsCase(repoDir); ok {
		if err := seedHeadCaseMismatch(repoFolds, foldsCase, seedHeadSelected(paths, opts.maxFiles), paths); err != nil {
			_ = os.RemoveAll(tempRoot)
			return nil, err
		}
	}
	checkout := &seedHeadCheckout{commit: commit, dir: filepath.Join(tempRoot, "tree"), paths: paths, unread: map[string]int64{}, foldsCase: foldsCase, tempRoot: tempRoot}
	if err := checkout.populate(ctx, envRunner, repoDir, entries, opts); err != nil {
		checkout.remove()
		return nil, err
	}
	return checkout, nil
}

// listSeedHeadTree lists commit's tree with each blob's size, which is what
// lets a file the scan only stats stay out of the checkout. `ls-tree -l` gives
// every size in one call, but in a partial clone it dies on the first blob the
// clone never fetched (GIT_NO_LAZY_FETCH keeps it from fetching one), and a
// sparse checkout of a partial clone fetches only the blobs it writes. So when
// that listing fails and the tree has objects missing, the tree is listed
// again without sizes, the missing blobs are marked for populate to leave out,
// and the sizes of the rest of the first maxFiles paths, the ones the scan
// looks at, are listed by path.
func listSeedHeadTree(ctx context.Context, runner EnvironmentCommandRunner, repoDir, commit string, maxFiles int) ([]string, map[string]seedHeadEntry, error) {
	env := seedHeadGitEnv("")
	listing, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "ls-tree", "-r", "-z", "-l", "--full-tree", commit)
	if err == nil {
		paths, entries := parseSeedHeadTree(listing)
		return paths, entries, nil
	}
	listErr := fmt.Errorf("list committed HEAD %s: %w", shortCommitHash(commit), err)
	missing, ok := seedHeadMissingObjects(ctx, runner, repoDir, env, commit)
	if !ok || len(missing) == 0 {
		return nil, nil, listErr
	}
	listing, _, err = runner.RunWithEnv(ctx, repoDir, env, "git", "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, nil, listErr
	}
	paths, entries := parseSeedHeadTree(listing)
	// Every missing blob is marked, not only the selected ones': a case
	// alias past --max-files can decide what a selected path reads.
	for path, entry := range entries {
		if missing[entry.object] {
			entry.missing = true
			entries[path] = entry
		}
	}
	selected := paths
	if maxFiles > 0 && len(selected) > maxFiles {
		selected = selected[:maxFiles]
	}
	if err := listSeedHeadSizes(ctx, runner, repoDir, commit, entries, selected); err != nil {
		return nil, nil, err
	}
	return paths, entries, nil
}

// listSeedHeadSizes fills in the size of each of paths' present blobs whose
// size was not listed yet, one `ls-tree -l` per batch of paths.
func listSeedHeadSizes(ctx context.Context, runner EnvironmentCommandRunner, repoDir, commit string, entries map[string]seedHeadEntry, paths []string) error {
	var unsized []string
	for _, path := range paths {
		if entry := entries[path]; entry.kind == "blob" && !entry.missing && entry.size < 0 {
			unsized = append(unsized, path)
		}
	}
	// Literal pathspecs: a tracked name that starts with ':' or holds a glob
	// character names itself and nothing else.
	env := seedHeadGitEnv("")
	env["GIT_LITERAL_PATHSPECS"] = "1"
	for _, batch := range chunkPathsByBytes(unsized, seedHeadCheckoutArgBytes) {
		args := append([]string{"ls-tree", "-r", "-z", "-l", "--full-tree", commit, "--"}, batch...)
		out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", args...)
		if err != nil {
			return fmt.Errorf("list committed HEAD %s sizes: %w", shortCommitHash(commit), err)
		}
		_, sized := parseSeedHeadTree(out)
		for path, sizedEntry := range sized {
			if entry, ok := entries[path]; ok {
				entry.size = sizedEntry.size
				entries[path] = entry
			}
		}
	}
	return nil
}

// seedHeadMissingObjects names the objects of commit's tree the repository
// does not have, which only a partial clone can be missing. ok is false when
// rev-list fails.
func seedHeadMissingObjects(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, commit string) (map[string]bool, bool) {
	// --no-walk: the commit's own tree, not its history. --missing=print
	// lists a missing object as "?<object>" instead of failing on it.
	out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "rev-list", "--objects", "--no-walk", "--no-object-names", "--missing=print", commit)
	if err != nil {
		return nil, false
	}
	missing := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if object, ok := strings.CutPrefix(strings.TrimSpace(line), "?"); ok && object != "" {
			missing[object] = true
		}
	}
	return missing, true
}

func (c *seedHeadCheckout) populate(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, entries map[string]seedHeadEntry, opts seedCommandOptions) error {
	if err := os.Mkdir(c.dir, 0o700); err != nil {
		return fmt.Errorf("create committed HEAD checkout: %w", err)
	}
	env := seedHeadGitEnv(filepath.Join(c.tempRoot, "index"))
	if _, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "read-tree", c.commit); err != nil {
		return fmt.Errorf("read committed HEAD %s into a scratch index: %w", shortCommitHash(c.commit), err)
	}
	// checkout-index looks attributes up in the work tree first and the
	// index second, so it applied the WORKTREE's .gitattributes --
	// uncommitted or edited ones included, even with the commit's own copy in
	// the scratch index -- and an uncommitted working-tree-encoding rule
	// re-encoded committed docs into what the scan rejects as binary.
	// Pointing GIT_WORK_TREE at an empty directory leaves the scratch index,
	// the commit's tree, as the only source (info/attributes and the user's
	// own attributes file apply as they do to a clean checkout, see
	// attributesFileConfig). This works on every supported git, unlike
	// --attr-source (2.40+). Files are written through --prefix, never into
	// this directory.
	emptyWorkTree := filepath.Join(c.tempRoot, "no-worktree")
	if err := os.Mkdir(emptyWorkTree, 0o700); err != nil {
		return fmt.Errorf("create committed HEAD checkout: %w", err)
	}
	env["GIT_WORK_TREE"] = emptyWorkTree
	attrConfig := c.attributesFileConfig(ctx, runner, repoDir, env, entries)
	selected := seedHeadSelected(c.paths, opts.maxFiles)
	// Paths that differ only in case share one file where the filesystem
	// folds case, and a clean checkout leaves it holding the last of them it
	// writes. Only that path is checked out, and its aliases are read through
	// it; elsewhere each path is a file of its own.
	work := selected
	if c.foldsCase {
		work, c.readers = seedHeadCaseWork(selected, seedHeadCaseAliases(c.paths, entries))
		// A partial clone's listing sized only the selected paths; a path
		// past --max-files checked out for its aliases needs its size too.
		if err := listSeedHeadSizes(ctx, runner, repoDir, c.commit, entries, work); err != nil {
			return err
		}
	}
	// The commit's own attributes decide two things below: which blobs are
	// LFS pointers, and which ones a checkout converts.
	var regular []string
	for _, path := range work {
		if entries[path].regularFile() {
			regular = append(regular, path)
		}
	}
	attrs, err := c.attributes(ctx, runner, repoDir, env, attrConfig, regular)
	if err != nil {
		return err
	}
	drivers := seedHeadFilterDrivers(ctx, runner, repoDir, env)
	// Where the caller's lfs filter is git-lfs, seed does itself what git-lfs
	// would (settleLFSPointers), so the checkout leaves git-lfs out, as the
	// hardened runner does for a repository's own filter. Its command line
	// can set GIT_LFS_SKIP_SMUDGE=0 over seedHeadGitEnv's, and it would then
	// download objects in the middle of a background seed. Any other command
	// the lfs filter runs is a driver like any other. git-lfs smudges a
	// pointer whatever filter runs it, so a filter under another name whose
	// command is git-lfs (seedHeadDrivers.lfsCommand) is left out and settled
	// the same way, in its own command's mode.
	lfsModes := map[string]seedHeadLFSMode{}
	var leftOut []string
	for _, path := range regular {
		filter := attrs[path].filter
		if _, done := lfsModes[filter]; done {
			continue
		}
		command, isLFS := drivers.lfsCommand(filter)
		if !isLFS {
			continue
		}
		mode := seedHeadLFSCommandMode(command)
		lfsModes[filter] = mode
		if mode == seedHeadLFSRefused {
			c.warnings = append(c.warnings, fmt.Sprintf("filter %q runs git-lfs with arguments it rejects or answers with its help, so a clean checkout does not write its files' content; seed kept the pointer text", filter))
		}
		if mode != seedHeadLFSUnfiltered {
			drivers.leaveOut(filter)
			leftOut = append(leftOut, filter)
		}
	}
	checkoutEnv := seedHeadLeaveOutFilters(env, leftOut)
	var wanted []string
	var lfs []seedHeadSmallFile
	var absent, convertSkipped, driven []string
	for _, path := range work {
		entry := entries[path]
		if entry.missing {
			// Checking it out would mean fetching it. The partial clone's
			// own sparse checkout leaves the file out of the worktree too,
			// and the scan reports it as it does there.
			absent = append(absent, path)
			continue
		}
		if !entry.regularFile() {
			wanted = append(wanted, path)
			continue
		}
		// The path rules settle an entry whatever the file holds. A path
		// whose own rules settle it is still checked out when a case alias
		// reads its file.
		var reading []string
		for _, reader := range c.readersOf(path) {
			if _, _, pathOK := inspectSeedFilePath(reader, opts); pathOK {
				reading = append(reading, reader)
			} else {
				c.unread[reader] = entry.size
			}
		}
		if len(reading) == 0 {
			continue
		}
		attr := attrs[path]
		if mode, isLFS := lfsModes[attr.filter]; isLFS && !drivers.runs(attr.filter) && entry.size <= seedHeadLFSPointerMaxBytes {
			// The blob is a pointer git-lfs, or no command, would check out:
			// the file's real size is only known once it is read.
			wanted = append(wanted, path)
			lfs = append(lfs, seedHeadSmallFile{path: path, size: entry.size, mode: mode})
			continue
		}
		// Otherwise the size rules need the size of the checked-out file,
		// which conversions make differ from the blob's: an eol conversion
		// grows it (a small media file can cross --max-file-bytes and become
		// "too large"), an encoding, ident or a filter driver can shrink it.
		// So a file is checked out and the scan stats the real one, unless
		// its blob is too large however it converts. A doc the scan reads at
		// any size is always checked out.
		if !slices.ContainsFunc(reading, seedReadsAtAnySize) && entry.size > seedHeadMaxShrink(attr, drivers)*int64(opts.maxFileBytes) {
			for _, reader := range reading {
				c.unread[reader] = entry.size
			}
			if seedHeadConvertsUnbounded(attr, drivers) {
				convertSkipped = append(convertSkipped, path)
			}
			continue
		}
		// Every file a driver converts goes through the driver checkout below,
		// which passes the driver's stderr on, and so does every ident file
		// and every file with a working-tree-encoding: ident shrinks an
		// expanded keyword and grows each "$Id$" by an object id, an encoding
		// can write several characters as one byte (TSCII writes the 12 bytes
		// of UTF-8 for Tamil "sri" as one) and ASCII//TRANSLIT can drop them,
		// so any of them can end up any size. They are
		// checked out in bounded batches and dropped there when they come out
		// too large. A doc the scan reads at any size is never removed there.
		if seedHeadConvertsUnbounded(attr, drivers) {
			driven = append(driven, path)
			continue
		}
		wanted = append(wanted, path)
	}
	if len(convertSkipped) > 0 {
		// Past the cap a driver or ident could still have shrunk the file
		// under --max-file-bytes, where a clean checkout would include it. Say
		// so rather than let the entry read like an ordinary size exclusion.
		c.warnings = append(c.warnings, fmt.Sprintf("%d committed files over %dx --max-file-bytes were not converted by their filter driver, ident attribute or working-tree-encoding and are reported too large at their committed size (first: %s)", len(convertSkipped), seedHeadDriverMaxShrink, convertSkipped[0]))
	}
	if len(absent) > 0 {
		c.warnings = append(c.warnings, fmt.Sprintf("%d committed files were not read because this partial clone has not fetched them (first: %s)", len(absent), absent[0]))
	}
	// git appends each path to the prefix verbatim, so it needs the trailing
	// separator; forward slashes are what git expects on every platform.
	prefix := "--prefix=" + filepath.ToSlash(c.dir) + "/"
	// -f: write over whatever is at a path, so the checkout never fails on
	// one it did not expect there.
	for _, batch := range chunkPathsByBytes(wanted, seedHeadCheckoutArgBytes) {
		args := append(append(slices.Clone(attrConfig), "checkout-index", "-f", prefix, "--"), batch...)
		if _, _, err := runner.RunWithEnv(ctx, repoDir, checkoutEnv, "git", args...); err != nil {
			return fmt.Errorf("check out committed HEAD %s: %w", shortCommitHash(c.commit), err)
		}
	}
	// A driver's output can be any size, so files one converts are checked
	// out seedHeadDriverBatchFiles at a time and stat'd, and those the scan
	// only stats are removed before the next batch is written. No more than a
	// batch of them is on disk at once, each at the size the same driver
	// writes it in the worktree, and the scan gets their real size.
	//
	// A driver that fails is skipped by git and the blob written unconverted,
	// unless the filter is required, when the checkout fails. A driver runs
	// in the empty work tree that keeps the worktree's attributes out, so one
	// whose command names a repository-relative helper fails here although a
	// worktree checkout runs it. Whatever a driver says on stderr is passed on
	// as a seed warning rather than discarded.
	var driverStderr seedHeadDriverStderr
	// git localizes the line that says it gave up on a driver; LANGUAGE
	// keeps it in English so the warning can tell a failure from other
	// output. It changes only the language of messages, for the driver too.
	driverEnv := maps.Clone(checkoutEnv)
	driverEnv["LANGUAGE"] = "en"
	for start := 0; start < len(driven); start += seedHeadDriverBatchFiles {
		group := driven[start:min(start+seedHeadDriverBatchFiles, len(driven))]
		for _, batch := range chunkPathsByBytes(group, seedHeadCheckoutArgBytes) {
			args := append(append(slices.Clone(attrConfig), "checkout-index", "-f", prefix, "--"), batch...)
			_, stderr, err := runner.RunWithEnv(ctx, repoDir, driverEnv, "git", args...)
			if err != nil {
				return fmt.Errorf("check out committed HEAD %s: %w", shortCommitHash(c.commit), err)
			}
			driverStderr.add(stderr)
		}
		for _, path := range group {
			target := filepath.Join(c.dir, filepath.FromSlash(path))
			if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() && !c.needsContent(path, info.Size(), opts) {
				c.dropCheckedOut(path, target, info.Size())
			}
		}
	}
	if warning := driverStderr.warning(); warning != "" {
		c.warnings = append(c.warnings, warning)
	}
	c.settleLFSPointers(ctx, runner, repoDir, lfs, opts)
	return nil
}

// seedHeadFoldsCase reports whether the filesystem holding dir treats names
// that differ only in case as one file. A probe that cannot be written counts
// as folding; the checkout cannot write there either, and fails.
func seedHeadFoldsCase(dir string) bool {
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return true
	}
	defer os.Remove(probe)
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// seedRepoFoldsCase reports whether the filesystem holding repoDir treats
// names that differ only in case as one file, which is what decides how a
// clean checkout there stores them. It reads repoDir's listing and looks one
// of its entries up under another case, and writes nothing; ok is false when
// no entry could tell.
func seedRepoFoldsCase(repoDir string) (folds, ok bool) {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return false, false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return seedFoldsCaseAmong(names, func(name string) (fs.FileInfo, error) {
		return os.Lstat(filepath.Join(repoDir, name))
	})
}

// seedFoldsCaseAmong decides seedRepoFoldsCase from a directory's entry names
// and a lookup of a name in that directory. A listing with two entries whose
// names differ only in case comes from a filesystem that keeps them apart,
// even when both are hard links to one file, which the lookup alone would
// take for folding. Otherwise an entry found under another case as the same
// file shows folding, and one not found shows that case is kept.
func seedFoldsCaseAmong(names []string, lstat func(name string) (fs.FileInfo, error)) (folds, ok bool) {
	spellings := make(map[string]bool, len(names))
	for _, name := range names {
		key := seedFoldCase(name)
		if spellings[key] {
			return false, true
		}
		spellings[key] = true
	}
	for _, name := range names {
		other := seedSwapCase(name)
		if other == name {
			continue
		}
		have, err := lstat(name)
		if err != nil {
			continue
		}
		got, err := lstat(other)
		if errors.Is(err, fs.ErrNotExist) {
			return false, true
		}
		if err != nil {
			continue
		}
		return os.SameFile(have, got), true
	}
	return false, false
}

// seedFoldCase is the key under which names that differ only in case match:
// each letter becomes the least of its Unicode simple case folding orbit, so
// K, k and the Kelvin sign match while the dotted I (İ) and i stay apart, as
// case-insensitive filesystems keep them. strings.ToLower would merge those.
func seedFoldCase(name string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, name)
}

// seedSwapCase swaps the case of every cased letter in name.
func seedSwapCase(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsUpper(r) {
			return unicode.ToLower(r)
		}
		return unicode.ToUpper(r)
	}, name)
}

// seedHeadCaseMismatch is why a checkout on a temporary filesystem that folds
// case (tempFolds) unlike the repository's (repoFolds) cannot read selected the
// way a clean checkout of the repository does, or nil. It matters only for a
// selected path that differs only in case from another path of the commit: one
// filesystem keeps the two as separate files and the other as one, so the
// checkout would read other bytes for it than a clean checkout does.
func seedHeadCaseMismatch(repoFolds, tempFolds bool, selected, paths []string) error {
	if repoFolds == tempFolds {
		return nil
	}
	spellings := map[string]int{}
	for _, path := range paths {
		spellings[seedFoldCase(path)]++
	}
	for _, path := range selected {
		if spellings[seedFoldCase(path)] > 1 {
			repoRule, tempRule := "separate files", "one file"
			if repoFolds {
				repoRule, tempRule = tempRule, repoRule
			}
			return fmt.Errorf("%s differs only in case from another committed path; the repository's filesystem stores such paths as %s and the temporary directory's (%s) as %s, so a checkout there would not read them as a clean checkout does (point TMPDIR, or TMP on Windows, at a filesystem that treats case like the repository's)", path, repoRule, os.TempDir(), tempRule)
		}
	}
	return nil
}

// seedHeadSelected is the paths the scan reads: the first maxFiles of paths.
func seedHeadSelected(paths []string, maxFiles int) []string {
	if maxFiles > 0 && len(paths) > maxFiles {
		return paths[:maxFiles]
	}
	return paths
}

// seedHeadCaseAliases maps each path that shares its file with another on a
// case-insensitive filesystem to the one whose content that file holds. A
// clean checkout writes every path in tree order, so the file holds the last
// one it writes: the last of them whose blob is present, since a partial
// clone's sparse checkout does not write a blob it has not fetched. A missing
// alias of a present path is read through that path's file too. paths is the
// whole sorted inventory, because the path a clean checkout writes last can
// lie past --max-files.
func seedHeadCaseAliases(paths []string, entries map[string]seedHeadEntry) map[string]string {
	groups := map[string][]string{}
	for _, path := range paths {
		folded := seedFoldCase(path)
		groups[folded] = append(groups[folded], path)
	}
	aliases := map[string]string{}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		owner := ""
		for _, path := range group {
			if !entries[path].missing {
				owner = path
			}
		}
		if owner == "" {
			continue
		}
		for _, path := range group {
			if path != owner {
				aliases[path] = owner
			}
		}
	}
	return aliases
}

// seedHeadCaseWork is the paths populate checks out, given the selected ones
// and their case aliases: each selected path that is no alias, and the path
// whose file each alias is read through, in tree order. readers lists, for
// each such path, the selected paths that read its file.
func seedHeadCaseWork(selected []string, aliases map[string]string) ([]string, map[string][]string) {
	readers := map[string][]string{}
	work := make([]string, 0, len(selected))
	for _, path := range selected {
		if owner, ok := aliases[path]; ok {
			readers[owner] = append(readers[owner], path)
		} else {
			work = append(work, path)
		}
	}
	scanned := map[string]bool{}
	for _, path := range work {
		if _, ok := readers[path]; ok {
			scanned[path] = true
		}
	}
	for owner, aliased := range readers {
		if scanned[owner] {
			readers[owner] = append([]string{owner}, aliased...)
		} else {
			// Past --max-files: checked out for its aliases, never scanned.
			work = append(work, owner)
		}
	}
	sort.Strings(work)
	return work, readers
}

// readersOf lists the selected paths whose scan reads path's file: path
// itself, and where the filesystem folds case, its case aliases.
func (c *seedHeadCheckout) readersOf(path string) []string {
	if readers, ok := c.readers[path]; ok {
		return readers
	}
	return []string{path}
}

// needsContent reports whether the scan reads the bytes of path's file, at
// size, for any path that reads it.
func (c *seedHeadCheckout) needsContent(path string, size int64, opts seedCommandOptions) bool {
	return slices.ContainsFunc(c.readersOf(path), func(reader string) bool {
		return seedFileNeedsContent(reader, size, opts)
	})
}

// seedHeadDriverStderr collects what the driver checkout calls print on
// stderr. A driver that only prints progress is not reported as failing:
// git gives up on one with "error: external filter '<command>' failed",
// and only that line makes the warning say files are seeded unconverted.
// Otherwise the warning quotes the last line printed, which is git's own
// when a failure was not recognized (a git that ignores LANGUAGE).
type seedHeadDriverStderr struct {
	calls   int
	last    string
	failure string
}

func (r *seedHeadDriverStderr) add(stderr []byte) {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		return
	}
	r.calls++
	lines := strings.Split(text, "\n")
	r.last = truncateAgentWarning(strings.TrimSpace(lines[len(lines)-1]))
	if r.failure != "" {
		return
	}
	for _, line := range lines {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "error: external filter ") {
			r.failure = truncateAgentWarning(line)
			return
		}
	}
}

func (r seedHeadDriverStderr) warning() string {
	switch {
	case r.failure != "":
		return fmt.Sprintf("a filter driver failed checking out committed HEAD (%d of its checkout calls printed to stderr), so some of its files are seeded unconverted: %s", r.calls, r.failure)
	case r.calls > 0:
		return fmt.Sprintf("a filter driver printed to stderr checking out committed HEAD (%d of its checkout calls), last: %s", r.calls, r.last)
	default:
		return ""
	}
}

// dropCheckedOut records a checked-out file the scan only stats as unread at
// size, for every path that reads it, which is what their entries are built
// from, and removes it. A file that cannot be removed stays; the scan never
// reads it either way.
func (c *seedHeadCheckout) dropCheckedOut(path, target string, size int64) {
	for _, reader := range c.readersOf(path) {
		c.unread[reader] = size
	}
	_ = os.Remove(target)
}

// seedHeadAttrs is the commit's checkout-relevant attributes for one path.
type seedHeadAttrs struct {
	filter   string
	encoding string
	ident    bool
}

// specifiedAttr reports whether check-attr gave an attribute a value.
func specifiedAttr(value string) bool {
	return value != "" && value != "unspecified" && value != "unset"
}

// seedHeadMaxShrink is how many times smaller than its blob a path's checkout
// can be. A driver can shrink a file without bound, and so can ident: checkout
// rewrites an expanded "$Id: ... $" committed as it is to "$Id: <object> $",
// however long the line. So can a working-tree-encoding. One byte of an
// encoding can stand for several characters: TSCII writes Tamil "sri", four
// characters and 12 bytes of UTF-8, as one byte, and which encodings there are
// depends on the platform's iconv. One that transliterates or skips what it
// cannot convert (ASCII//TRANSLIT, //IGNORE) can drop characters. All of them
// get the driver cap. eol, all that is left, does not shrink a file, so a
// blob over --max-file-bytes is too large.
func seedHeadMaxShrink(attr seedHeadAttrs, drivers seedHeadDrivers) int64 {
	if seedHeadConvertsUnbounded(attr, drivers) {
		return seedHeadDriverMaxShrink
	}
	return 1
}

// seedHeadConvertsUnbounded reports whether a path's checkout can come out
// any size relative to its blob: a filter driver runs on it, it has ident, or
// it has a working-tree-encoding.
func seedHeadConvertsUnbounded(attr seedHeadAttrs, drivers seedHeadDrivers) bool {
	return drivers.runs(attr.filter) || attr.ident || specifiedAttr(attr.encoding)
}

// seedHeadDrivers is the set of filter drivers that run a command on checkout:
// a smudge or process command set in the configuration the hardened runner
// leaves in force. Repository-scoped drivers are blanked there
// (git_harden_filters.go), so a filter=x with only a repository driver, or
// none at all, writes the blob unchanged. Where a filter's command is git-lfs,
// LFS is handled on its own (settleLFSPointers); another command named lfs is
// a driver like any other.
type seedHeadDrivers struct {
	names map[string]bool
	// commands: each filter's command as a git command the caller runs reads
	// it (the process command where one is set, the smudge command
	// otherwise), trimmed, with the hardened runner's blanking left out.
	commands map[string]string
	// gitLFS: the filters the checkout leaves out because they are git-lfs.
	gitLFS map[string]bool
	// unknown: the configuration could not be read, so every filter is
	// taken to run one, which only costs checking more files out.
	unknown bool
}

func (d seedHeadDrivers) runs(filter string) bool {
	if !specifiedAttr(filter) || d.gitLFS[filter] {
		return false
	}
	return d.unknown || d.names[filter]
}

// lfsCommand reports whether seed settles filter's files as Git LFS
// pointers, and the command whose mode it settles them in
// (seedHeadLFSCommandMode): lfs whatever its command, which counts as none
// unless it is git-lfs, and any other filter whose command is git-lfs, since
// git-lfs smudges a pointer whatever filter runs it. The command is the
// caller's: a repository's own driver is blanked in seed's git commands, not
// in the caller's, and reading it as text runs nothing.
func (d seedHeadDrivers) lfsCommand(filter string) (string, bool) {
	if !specifiedAttr(filter) {
		return "", false
	}
	command := d.commands[filter]
	if filter == "lfs" {
		return command, true
	}
	_, _, ok := seedHeadGitLFSCommand(command)
	return command, ok
}

// leaveOut records that the checkout leaves filter out as git-lfs.
func (d *seedHeadDrivers) leaveOut(filter string) {
	if d.gitLFS == nil {
		d.gitLFS = map[string]bool{}
	}
	d.gitLFS[filter] = true
}

// seedHeadFilterDrivers reads every driver command in one git call, however
// many filters the commit names: both the configuration seed's git commands
// read and, for seedHeadDrivers.commands, the one the caller's do.
func seedHeadFilterDrivers(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string) seedHeadDrivers {
	out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "config", "-z", "--show-scope", "--show-origin", "--get-regexp", `^filter\..*\.(clean|smudge|process|required)$`)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 {
			return seedHeadDrivers{} // no driver is configured
		}
		return seedHeadDrivers{unknown: true}
	}
	// -z with --show-scope and --show-origin: <scope> NUL <origin> NUL <key> LF
	// <value> NUL per value, in the order git reads them: the configuration
	// files, then GIT_CONFIG_COUNT's entries, then GIT_CONFIG_PARAMETERS
	// (git -c), so a key's last value is the one in force. Keys come
	// lowercased except for the subsection. A file included from the command
	// scope keeps that scope and names its own origin; values set there
	// directly have the origin "command line:".
	//
	// Where the repository's own configuration sets any key of a filter, the
	// hardened runner blanks its commands with GIT_CONFIG_COUNT entries
	// appended after the caller's (git_harden_filters.go): one more value set
	// directly for each key, which seed's commands read and the caller's do
	// not.
	type configValue struct {
		direct bool
		value  string
	}
	values := map[string][]configValue{}
	repositoryScoped := map[string]bool{}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		scope, origin := fields[i], fields[i+1]
		key, value, _ := strings.Cut(fields[i+2], "\n")
		name, ok := filterDriverFromConfigKey(key)
		if !ok {
			continue
		}
		if scope == "local" || scope == "worktree" {
			repositoryScoped[name] = true
		}
		values[key] = append(values[key], configValue{direct: scope == "command" && origin == "command line:", value: value})
	}
	// command is the value of key in force, trimmed: for seed, or with the
	// hardened runner's blank dropped, for the caller. The blank follows the
	// caller's own GIT_CONFIG_COUNT entries for the key, which come first
	// among the values set directly.
	command := func(name, key string, caller bool) string {
		all := values[key]
		if caller && repositoryScoped[name] {
			skip := seedHeadCallerConfigCountEntries(key)
			for i, v := range all {
				if !v.direct {
					continue
				}
				if skip == 0 {
					all = slices.Delete(slices.Clone(all), i, i+1)
					break
				}
				skip--
			}
		}
		if len(all) == 0 {
			return ""
		}
		return strings.TrimSpace(all[len(all)-1].value)
	}
	names, commands := map[string]bool{}, map[string]string{}
	for key := range values {
		name, _ := filterDriverFromConfigKey(key)
		if _, done := commands[name]; done {
			continue
		}
		process, smudge := "filter."+name+".process", "filter."+name+".smudge"
		if command(name, process, false) != "" || command(name, smudge, false) != "" {
			names[name] = true
		}
		commands[name] = command(name, process, true)
		if commands[name] == "" {
			commands[name] = command(name, smudge, true)
		}
	}
	return seedHeadDrivers{names: names, commands: commands}
}

// seedHeadLeaveOutFilters returns env with the filters named left out of a
// checkout: their commands blank and required false. They go after
// everything the caller set, appended to GIT_CONFIG_PARAMETERS as git -c
// itself appends them, key and value each single-quoted, so they win over any
// value but a later -c, and a filter name with "=" is left out too, which a
// -c argument cannot express (git_harden_filters.go). The checkout's own -c
// arguments set no filter.
func seedHeadLeaveOutFilters(env map[string]string, filters []string) map[string]string {
	if len(filters) == 0 {
		return env
	}
	quote := strings.NewReplacer("'", `'\''`, "!", `'\!'`)
	var params []string
	if caller := strings.TrimSpace(os.Getenv("GIT_CONFIG_PARAMETERS")); caller != "" {
		params = append(params, caller)
	}
	for _, filter := range filters {
		for _, setting := range [][2]string{{"process", ""}, {"smudge", ""}, {"required", "false"}} {
			key := "filter." + filter + "." + setting[0]
			params = append(params, "'"+quote.Replace(key)+"'='"+quote.Replace(setting[1])+"'")
		}
	}
	out := maps.Clone(env)
	out["GIT_CONFIG_PARAMETERS"] = strings.Join(params, " ")
	return out
}

// seedHeadAttributesFileMaxBytes bounds the committed attributes file seed
// reads (git itself ignores one over 100 MiB).
const seedHeadAttributesFileMaxBytes = 1 << 20

// attributesFileConfig keeps the user's own attributes file
// (core.attributesFile) in force for the checkout, as a clean checkout of
// HEAD has it. git reads a relative value from the top of the work tree,
// which the checkout points at an empty directory, so such a value is read
// from the real one (repoDir) instead (seedHeadAttributesPath).
// seedHeadTrackedPath then reads the path as a clean checkout of HEAD
// resolves it. A file HEAD tracks there is part of the commit and, like
// .gitattributes, is read as committed: its blob is written to the scratch
// area as it is, with no filter or conversion run (git reads LF and CRLF
// rules alike), and that copy is named; settleAttributesFile then hydrates
// one HEAD stores in Git LFS. One that cannot be read from HEAD is left out,
// with a warning, rather than read with its uncommitted edits. A file HEAD
// does not track is named where the walk found it, and a path that names no
// file in a clean checkout gets an empty one, so no rules apply, as git
// applies none. --path expands ~ and %(prefix). With the value unset, git
// reads its default file (seedDefaultAttributesFile), which a work tree can
// track like any other (a dotfiles checkout at $HOME), so that path is read
// the same way; an empty value names none.
func (c *seedHeadCheckout) attributesFileConfig(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, entries map[string]seedHeadEntry) []string {
	out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "config", "-z", "--path", "--get", "core.attributesFile")
	value, named := strings.TrimSuffix(string(out), "\x00"), "the attributes file core.attributesFile names"
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || len(out) != 0 {
			return nil // unreadable, which the checkout reports itself
		}
		value, named = seedDefaultAttributesFile(env), "the attributes file git reads by default"
	}
	if value == "" {
		return nil
	}
	root := repoDir
	if !filepath.IsAbs(root) {
		if root, err = filepath.Abs(root); err != nil {
			return nil
		}
	}
	folds, ok := seedRepoFoldsCase(root)
	if !ok {
		folds = c.foldsCase
	}
	tree := newSeedHeadTree(entries, folds)
	path, kind, err := seedHeadTrackedPath(root, seedHeadAttributesPath(root, value), tree, seedHeadLinkReader(ctx, runner, repoDir, env))
	named = fmt.Sprintf("%s (%s)", named, path)
	if err == nil && kind == seedHeadPathUntracked {
		return []string{"-c", "core.attributesFile=" + path}
	}
	committed := filepath.Join(c.tempRoot, "attributes")
	if err == nil && kind == seedHeadPathTracked {
		entry := entries[path]
		if err = writeSeedHeadBlob(ctx, runner, repoDir, env, entry, committed); err == nil {
			if entry.mode != seedHeadSymlinkMode { // git converts no symlink
				c.settleAttributesFile(ctx, runner, repoDir, env, named, path, committed, filepath.Join(root, filepath.FromSlash(path)))
			}
			return []string{"-c", "core.attributesFile=" + committed}
		}
	}
	if err != nil {
		c.warnings = append(c.warnings, fmt.Sprintf("%s could not be read from HEAD (%v); its rules were not applied", named, err))
	}
	if os.WriteFile(committed, nil, 0o600) != nil {
		return nil
	}
	return []string{"-c", "core.attributesFile=" + committed}
}

// seedDefaultAttributesFile is the attributes file git reads with
// core.attributesFile unset, in the environment seed runs git with:
// $XDG_CONFIG_HOME/git/attributes, or $HOME/.config/git/attributes where
// XDG_CONFIG_HOME is unset or empty. It is "" with neither set, as git then
// reads none.
func seedDefaultAttributesFile(env map[string]string) string {
	lookup := func(key string) (string, bool) {
		if value, ok := env[key]; ok {
			return value, true
		}
		return os.LookupEnv(key)
	}
	if config, _ := lookup("XDG_CONFIG_HOME"); config != "" {
		return config + "/git/attributes"
	}
	if home, ok := seedGitHome(lookup); ok {
		return home + "/.config/git/attributes"
	}
	return ""
}

// seedGitHome is HOME as git reads it. On Windows git sets an unset HOME as
// it starts: to HOMEDRIVE and HOMEPATH where they name a directory, and to
// USERPROFILE otherwise.
func seedGitHome(lookup func(string) (string, bool)) (string, bool) {
	if home, ok := lookup("HOME"); ok || runtime.GOOS != "windows" {
		return home, ok
	}
	if drive, ok := lookup("HOMEDRIVE"); ok {
		if dir, ok := lookup("HOMEPATH"); ok {
			if info, err := os.Stat(drive + dir); err == nil && info.IsDir() {
				return drive + dir, true
			}
		}
	}
	return lookup("USERPROFILE")
}

// seedHeadSymlinkMode is the mode git records for a symlink.
const seedHeadSymlinkMode = "120000"

// seedHeadAttributesPath is the absolute path git opens for value, a
// core.attributesFile value, from the top of the work tree (root), where git
// runs in the repository. A relative value is joined to root as git joins it,
// without cleaning. On Windows a value rooted without a drive (\attrs) is on
// root's drive, and a drive-relative one (C:attrs) is read from root on
// root's own drive and from that drive's current directory on another.
func seedHeadAttributesPath(root, value string) string {
	sep := string(filepath.Separator)
	volume := filepath.VolumeName(value)
	switch {
	case !seedHeadGitAbsolutePath(value):
		return root + sep + value
	case filepath.IsAbs(value):
		return value
	case volume == "":
		return filepath.VolumeName(root) + value
	case strings.EqualFold(volume, filepath.VolumeName(root)):
		return root + sep + value[len(volume):]
	}
	if abs, err := filepath.Abs(value); err == nil {
		return abs
	}
	return value
}

// seedHeadLinkMaxBytes bounds a symlink target seed reads from HEAD (Linux's
// PATH_MAX).
const seedHeadLinkMaxBytes = 4096

// seedHeadLinkReader reads the target of a symlink HEAD tracks, as a clean
// checkout writes it. With core.symlinks false, git checks a symlink out as a
// plain file holding its target, and the reader says so (ok false). The
// setting is read once, at the first symlink.
func seedHeadLinkReader(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string) seedHeadLinks {
	symlinks := -1
	return func(entry seedHeadEntry) (string, bool, error) {
		if symlinks < 0 {
			symlinks = 1 // git's default; unset or unreadable keeps it
			out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "config", "--type=bool", "--get", "core.symlinks")
			if err == nil && strings.TrimSpace(string(out)) == "false" {
				symlinks = 0
			}
		}
		if symlinks == 0 {
			return "", false, nil
		}
		target, err := readSeedHeadBlob(ctx, runner, repoDir, env, entry, seedHeadLinkMaxBytes)
		return string(target), true, err
	}
}

// writeSeedHeadBlob writes entry's blob to target as it is, up to
// seedHeadAttributesFileMaxBytes.
func writeSeedHeadBlob(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, entry seedHeadEntry, target string) error {
	data, err := readSeedHeadBlob(ctx, runner, repoDir, env, entry, seedHeadAttributesFileMaxBytes)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

// readSeedHeadBlob reads entry's blob as it is, up to limit bytes.
func readSeedHeadBlob(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, entry seedHeadEntry, limit int64) ([]byte, error) {
	if entry.missing {
		return nil, errors.New("this partial clone has not fetched it")
	}
	size := entry.size
	if size < 0 {
		out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "cat-file", "-s", entry.object)
		if err != nil {
			return nil, err
		}
		if size, err = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err != nil {
			return nil, err
		}
	}
	if size > limit {
		return nil, fmt.Errorf("it is over %d bytes", limit)
	}
	data, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "cat-file", "blob", entry.object)
	return data, err
}

// settleAttributesFile makes seed's copy (committed) of the tracked attributes
// file at path what a clean checkout of HEAD writes, as far as seed can. Its
// own attributes are the ones a fresh checkout reads before that file exists:
// HEAD's .gitattributes and info/attributes. Where they give it a filter
// whose command is the caller's git-lfs and that smudges
// (seedHeadLFSCommandMode), lfsAttributesFile hydrates it. Any other filter driver, or a working-tree-encoding, is not
// run for it, since running the file's own filters could fetch or fail
// unreported; seed reads its committed bytes and says so. named is how
// warnings name the file.
func (c *seedHeadCheckout) settleAttributesFile(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, named, path, committed, worktree string) {
	none := filepath.Join(c.tempRoot, "no-attributes")
	if os.WriteFile(none, nil, 0o600) != nil {
		return
	}
	attrs, err := c.attributes(ctx, runner, repoDir, env, []string{"-c", "core.attributesFile=" + none}, []string{path})
	if err != nil {
		return
	}
	attr := attrs[path]
	lfsMode := seedHeadLFSUnfiltered
	var drivers seedHeadDrivers
	if specifiedAttr(attr.filter) {
		drivers = seedHeadFilterDrivers(ctx, runner, repoDir, env)
		if command, isLFS := drivers.lfsCommand(attr.filter); isLFS {
			lfsMode = seedHeadLFSCommandMode(command)
		}
	}
	switch {
	case lfsMode == seedHeadLFSSmudges:
		c.lfsAttributesFile(ctx, runner, repoDir, named, committed, worktree)
	case specifiedAttr(attr.encoding), lfsMode == seedHeadLFSRefused,
		lfsMode == seedHeadLFSUnfiltered && drivers.runs(attr.filter):
		c.warnings = append(c.warnings, fmt.Sprintf("%s has a filter or working-tree-encoding, which seed does not run for it; seed read the rules in its committed bytes", named))
	}
}

// lfsAttributesFile hydrates seed's copy (committed) of a tracked attributes
// file that HEAD stores as a Git LFS pointer and git-lfs smudges, always in
// that copy, so nothing the work tree holds is read after it was checked. Its
// object is read from the local store, as settleLFSPointers reads one, or
// from the worktree's file where that is exactly the object (both copied and
// verified as they are read). Otherwise the pointer is kept, with a warning
// unless the worktree shows the pointer too.
func (c *seedHeadCheckout) lfsAttributesFile(ctx context.Context, runner EnvironmentCommandRunner, repoDir, named, committed, worktree string) {
	pointer, err := clibase.SafeReadFile(committed, seedHeadLFSPointerMaxBytes)
	if err != nil {
		return
	}
	oid, size, extended, ok := parseLFSPointer(pointer)
	if !ok {
		return
	}
	keep := func(why string) {
		c.warnings = append(c.warnings, fmt.Sprintf("%s is stored in Git LFS and %s; seed read the rules in its committed pointer text", named, why))
	}
	switch {
	case size == 0:
		// git-lfs writes the empty object without reading the store, as in
		// settleLFSPointers.
		if os.WriteFile(committed, nil, 0o600) != nil {
			keep("seed could not write its empty object")
		}
		return
	case extended:
		keep("its pointer names an LFS extension, which seed does not run")
		return
	case size > seedHeadAttributesFileMaxBytes:
		keep(fmt.Sprintf("its object is over %d bytes", seedHeadAttributesFileMaxBytes))
		return
	case seedHeadLFSFetchFiltered(ctx, runner, repoDir, c.commit):
		keep("Git LFS fetch include/exclude patterns are configured, which seed does not evaluate")
		return
	}
	if objectsDir := seedHeadLFSObjectsDir(ctx, runner, repoDir); objectsDir != "" {
		if copyLFSObject(filepath.Join(objectsDir, oid[0:2], oid[2:4], oid), committed, oid, size) == nil {
			return
		}
	}
	if copyLFSObject(worktree, committed, oid, size) == nil {
		return
	}
	if data, err := clibase.SafeReadFile(worktree, seedHeadLFSPointerMaxBytes); err != nil || !bytes.Equal(data, pointer) {
		keep("its object is not stored locally while the work tree's copy is edited")
	}
}

// seedHeadPathKind is what a path names in a clean checkout of HEAD.
type seedHeadPathKind int

const (
	// seedHeadPathNone is no file a clean checkout could open: a directory,
	// a path through a file, or a name nothing has.
	seedHeadPathNone seedHeadPathKind = iota
	// seedHeadPathTracked is a file HEAD tracks, named in git's form.
	seedHeadPathTracked
	// seedHeadPathUntracked is a file HEAD does not track, named by its real
	// path.
	seedHeadPathUntracked
)

// seedHeadTree is HEAD's layout as a clean checkout of the repository has
// it. Where the repository's filesystem treats names that differ only in case
// as one (folds), a name is any spelling of a committed path, and one shared
// by several files holds the last of them that checkout writes, as
// seedHeadCaseAliases has it.
type seedHeadTree struct {
	entries map[string]seedHeadEntry
	folds   bool
	files   map[string]string // folded path (seedFoldCase): the path whose content is there
	dirs    map[string]bool   // folded directories
}

func newSeedHeadTree(entries map[string]seedHeadEntry, folds bool) seedHeadTree {
	tree := seedHeadTree{entries: entries, folds: folds}
	if !folds {
		return tree
	}
	tree.files, tree.dirs = map[string]string{}, map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(entries)) {
		key := seedFoldCase(path)
		if owner, ok := tree.files[key]; !ok || entries[owner].missing || !entries[path].missing {
			tree.files[key] = path
		}
		for i := strings.LastIndexByte(key, '/'); i > 0; i = strings.LastIndexByte(key[:i], '/') {
			tree.dirs[key[:i]] = true
		}
	}
	return tree
}

// lookup returns the committed path a clean checkout reads at path, and its
// entry.
func (t seedHeadTree) lookup(path string) (string, seedHeadEntry, bool) {
	if t.folds {
		if owner, ok := t.files[seedFoldCase(path)]; ok {
			path = owner
		}
	}
	entry, ok := t.entries[path]
	return path, entry, ok
}

// hasDir reports whether HEAD tracks anything under dir.
func (t seedHeadTree) hasDir(dir string) bool {
	if t.folds {
		return t.dirs[seedFoldCase(dir)]
	}
	return seedHeadTreeHasDir(t.entries, dir)
}

// seedHeadLinks reads the target of a symlink HEAD tracks
// (seedHeadLinkReader).
type seedHeadLinks func(entry seedHeadEntry) (target string, ok bool, err error)

// seedHeadMaxSymlinks is how many symlinks a path may pass through, as
// Linux's MAXSYMLINKS; past that an open fails with ELOOP.
const seedHeadMaxSymlinks = 40

// seedHeadTrackedPath reads file, an absolute path, as a clean checkout of
// HEAD resolves it: one part at a time, following each symlink to its target.
// Inside the work tree a name HEAD tracks (tree, which knows its spellings
// where the filesystem folds case) is what HEAD has there, whatever the
// worktree made of it: a directory, a file (so a path through it names
// nothing), or a symlink, whose committed target is followed (links). A name
// HEAD does not track is what the filesystem has, looked for only in a
// directory the worktree still has as HEAD does, and so is everything outside
// the work tree; a path that comes back in, under any spelling, is read
// through HEAD's layout again. A path that ends in a separator, "." or ".." names a directory,
// never the file, as git's open of it finds. It returns the path HEAD tracks,
// or the real path of a file it does not, and which it is. An error is a
// symlink HEAD tracks, at the path returned, whose target could not be read.
func seedHeadTrackedPath(root, file string, tree seedHeadTree, links seedHeadLinks) (string, seedHeadPathKind, error) {
	w := &seedHeadWalk{realRoot: root, tree: tree, links: links}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		w.realRoot = real
	}
	w.root, _ = os.Stat(w.realRoot)
	volume := filepath.VolumeName(file)
	w.move(volume + string(filepath.Separator))
	todo := seedHeadPathParts(file[len(volume):])
	for hops := 0; len(todo) > 0; {
		part, final := todo[0], len(todo) == 1
		todo = todo[1:]
		var end *seedHeadPathEnd
		target, follow, inHEAD := "", false, false
		switch {
		case part == "" || part == ".":
			continue
		case part == "..":
			w.up()
			continue
		case w.outside == "":
			target, follow, end, inHEAD = w.headStep(part, final)
		}
		if !inHEAD {
			target, follow, end = w.fsStep(part, final)
		}
		if end != nil {
			return end.path, end.kind, end.err
		}
		if !follow {
			continue
		}
		if hops++; hops > seedHeadMaxSymlinks || target == "" {
			return "", seedHeadPathNone, nil
		}
		todo = append(w.jump(target), todo...)
	}
	return "", seedHeadPathNone, nil // the path ends at a directory
}

// seedHeadWalk is seedHeadTrackedPath's position: in the work tree, in HEAD's
// layout, or outside it, as a real path.
type seedHeadWalk struct {
	realRoot string
	root     fs.FileInfo // realRoot's, to know it under any spelling
	tree     seedHeadTree
	links    seedHeadLinks
	inside   []string
	outside  string
}

// seedHeadPathEnd is the answer a walk ends with.
type seedHeadPathEnd struct {
	path string
	kind seedHeadPathKind
	err  error
}

var seedHeadPathNothing = &seedHeadPathEnd{kind: seedHeadPathNone}

// move goes to real, a real path, inside the work tree or outside it.
func (w *seedHeadWalk) move(real string) {
	rel, err := filepath.Rel(w.realRoot, real)
	if rel = filepath.ToSlash(rel); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		w.inside, w.outside = nil, real
		return
	}
	w.inside, w.outside = nil, ""
	if rel != "." {
		w.inside = strings.Split(rel, "/")
	}
}

// up goes to the parent directory.
func (w *seedHeadWalk) up() {
	switch {
	case w.outside != "":
		w.move(filepath.Dir(w.outside))
	case len(w.inside) == 0:
		w.move(filepath.Dir(w.realRoot))
	default:
		w.inside = w.inside[:len(w.inside)-1]
	}
}

// jump returns the parts of target, a symlink's, to read in its place, from
// the root when it is absolute and from the link's directory otherwise.
func (w *seedHeadWalk) jump(target string) []string {
	volume := filepath.VolumeName(target)
	if volume != "" || os.IsPathSeparator(target[0]) {
		if volume == "" {
			volume = filepath.VolumeName(w.realRoot)
		}
		w.move(volume + string(filepath.Separator))
		target = target[len(filepath.VolumeName(target)):]
	}
	return seedHeadPathParts(target)
}

// headStep reads part, inside the work tree, as HEAD has it: entering a
// directory, returning a symlink's target to follow, or ending the walk at a
// file. It reports false when HEAD has nothing there.
func (w *seedHeadWalk) headStep(part string, final bool) (string, bool, *seedHeadPathEnd, bool) {
	path, entry, tracked := w.tree.lookup(strings.Join(append(slices.Clone(w.inside), part), "/"))
	switch {
	case tracked && entry.mode == seedHeadSymlinkMode:
		target, ok, err := w.links(entry)
		if err != nil {
			return "", false, &seedHeadPathEnd{path: path, kind: seedHeadPathTracked, err: err}, true
		}
		if ok {
			return target, true, nil, true
		}
		// checked out as a plain file holding its target
	case tracked && !entry.ordinaryFile(), !tracked && w.tree.hasDir(path):
		// a directory, or a submodule's
		w.inside = append(w.inside, part)
		return "", false, nil, true
	case !tracked:
		return "", false, nil, false
	}
	if final {
		return "", false, &seedHeadPathEnd{path: path, kind: seedHeadPathTracked}, true
	}
	return "", false, seedHeadPathNothing, true // a path through a file
}

// fsStep reads part as the filesystem has it: entering a directory,
// returning a symlink's target to follow, or ending the walk.
func (w *seedHeadWalk) fsStep(part string, final bool) (string, bool, *seedHeadPathEnd) {
	dir := w.outside
	if dir == "" {
		// A directory HEAD tracks that the worktree replaced or moved holds
		// only what HEAD tracks in a clean checkout.
		if !seedHeadRealDirs(w.realRoot, w.inside) {
			return "", false, seedHeadPathNothing
		}
		dir = filepath.Join(append([]string{w.realRoot}, w.inside...)...)
	}
	name := filepath.Join(dir, part)
	info, err := os.Lstat(name)
	switch {
	case err != nil:
		return "", false, seedHeadPathNothing
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(name)
		if err != nil {
			return "", false, seedHeadPathNothing
		}
		return target, true, nil
	case info.IsDir():
		switch {
		case w.outside == "":
			w.inside = append(w.inside, part)
		case os.SameFile(info, w.root): // the work tree, however it is spelled
			w.inside, w.outside = nil, ""
		default:
			w.outside = name
		}
		return "", false, nil
	case final:
		return "", false, &seedHeadPathEnd{path: name, kind: seedHeadPathUntracked}
	case info.Mode().IsRegular():
		return "", false, seedHeadPathNothing // a path through a file
	}
	// Anything else, such as a Windows junction, is resolved as the OS does.
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", false, seedHeadPathNothing
	}
	w.move(real)
	return "", false, nil
}

// seedHeadRealDirs reports whether each directory on the way to inside is a
// directory in the work tree itself, not a symlink or a file.
func seedHeadRealDirs(realRoot string, inside []string) bool {
	dir := realRoot
	for _, part := range inside {
		dir = filepath.Join(dir, part)
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// seedHeadPathParts splits path at every separator, keeping empty and "."
// components.
func seedHeadPathParts(path string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || os.IsPathSeparator(path[i]) || path[i] == '/' {
			parts = append(parts, path[start:i])
			start = i + 1
		}
	}
	return parts
}

// seedHeadTreeHasDir reports whether HEAD tracks anything under dir.
func seedHeadTreeHasDir(entries map[string]seedHeadEntry, dir string) bool {
	prefix := dir + "/"
	for path := range entries {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// seedHeadGitAbsolutePath reports whether git takes a path as absolute: on
// Windows a drive prefix or a leading separator of either kind is enough.
func seedHeadGitAbsolutePath(value string) bool {
	return filepath.IsAbs(value) || filepath.VolumeName(value) != "" || strings.HasPrefix(filepath.ToSlash(value), "/")
}

// attributes reads the filter, working-tree-encoding and ident attributes of
// paths from the commit: --cached reads .gitattributes from the scratch index
// only. config carries attributesFileConfig.
func (c *seedHeadCheckout) attributes(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, config, paths []string) (map[string]seedHeadAttrs, error) {
	attrs := make(map[string]seedHeadAttrs, len(paths))
	for _, batch := range chunkPathsByBytes(paths, seedHeadCheckoutArgBytes) {
		args := append(append(slices.Clone(config), "check-attr", "--cached", "-z", "filter", "working-tree-encoding", "ident", "--"), batch...)
		out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", args...)
		if err != nil {
			return nil, fmt.Errorf("read committed HEAD %s attributes: %w", shortCommitHash(c.commit), err)
		}
		// -z: <path> NUL <attribute> NUL <value> NUL, repeated.
		fields := strings.Split(string(out), "\x00")
		for i := 0; i+2 < len(fields); i += 3 {
			attr := attrs[fields[i]]
			switch fields[i+1] {
			case "filter":
				attr.filter = fields[i+2]
			case "working-tree-encoding":
				attr.encoding = fields[i+2]
			case "ident":
				// git converts only for a set ident, not ident=<value>.
				attr.ident = fields[i+2] == "set"
			}
			attrs[fields[i]] = attr
		}
	}
	return attrs, nil
}

// seedHeadSmallFile is a checked-out blob small enough to be a Git LFS
// pointer, with what a clean checkout's filter does with one (mode).
type seedHeadSmallFile struct {
	path string
	size int64
	mode seedHeadLFSMode
}

// settleLFSPointers finishes the blobs the commit tracks with filter=lfs. The
// checkout never runs git-lfs, so a background seed never downloads anything
// (populate, seedHeadGitEnv), which on its own seeded every LFS document as
// its pointer text. A pointer whose object is already in the local LFS store is
// replaced with that object, which is what a working tree with the content
// fetched holds; one whose object is not stored locally stays the pointer, the
// same text a working tree without the content shows. The store is the one
// git-lfs reads (seedHeadLFSObjectsDir). An object that is stored but cannot be
// copied is reported in the seed's warnings rather than passed off as read.
// The caller's Git LFS settings come first (each file's mode, from
// seedHeadLFSCommandMode, for lfs or another filter that runs git-lfs): with
// no lfs filter, or one that is not git-lfs, a file stays as the checkout
// wrote it, and where the smudge is skipped, its pointer is written the way
// git-lfs writes it.
//
// Each file is then held to the same rule as every other blob, at its real
// size: one the scan would only stat leaves the checkout and is recorded
// unread, so a large LFS binary is never copied here either.
func (c *seedHeadCheckout) settleLFSPointers(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, files []seedHeadSmallFile, opts seedCommandOptions) {
	objectsDir, objectsResolved := "", false
	fetchFiltered, filterResolved := false, false
	var filteredPaths []string
	defer func() {
		if len(filteredPaths) > 0 {
			c.warnings = append(c.warnings, fmt.Sprintf("Git LFS fetch include/exclude patterns are configured, which seed does not evaluate; seeded the pointer text of %d files whose LFS objects are stored locally (first: %s)", len(filteredPaths), filteredPaths[0]))
		}
	}()
	for _, file := range files {
		if file.mode == seedHeadLFSUnfiltered || file.mode == seedHeadLFSRefused {
			continue
		}
		target := filepath.Join(c.dir, filepath.FromSlash(file.path))
		data, err := clibase.SafeReadFile(target, seedHeadLFSPointerMaxBytes)
		if err != nil {
			continue
		}
		oid, size, extended, ok := parseLFSPointer(data)
		if !ok {
			continue
		}
		// git-lfs writes a pointer it skips in its own encoding, and the
		// empty object as an empty file whether or not the store has it
		// (checked against git-lfs 3.4.1).
		if file.mode == seedHeadLFSSkips || size == 0 {
			c.encodeLFSPointer(file.path, target, data, oid, size, extended)
			continue
		}
		if !objectsResolved {
			objectsResolved = true
			objectsDir = seedHeadLFSObjectsDir(ctx, runner, repoDir)
		}
		if objectsDir == "" {
			continue
		}
		object := filepath.Join(objectsDir, oid[0:2], oid[2:4], oid)
		info, err := os.Lstat(object)
		if err != nil {
			// Not fetched: the pointer is what the worktree shows too.
			continue
		}
		if !info.Mode().IsRegular() || info.Size() != size {
			c.warnings = append(c.warnings, fmt.Sprintf("LFS object for %s does not match its pointer; seeded the pointer text", file.path))
			continue
		}
		if extended {
			// An LFS extension transforms the stored object on smudge, and
			// seed runs no smudge, so the object is not what a worktree with
			// the content holds.
			c.warnings = append(c.warnings, fmt.Sprintf("LFS pointer for %s names an LFS extension, which seed does not run; seeded the pointer text", file.path))
			continue
		}
		if !filterResolved {
			filterResolved = true
			fetchFiltered = seedHeadLFSFetchFiltered(ctx, runner, repoDir, c.commit)
		}
		if fetchFiltered {
			// A clean checkout keeps the pointer of a path the patterns leave
			// out, although its object is stored locally, and smudges the
			// rest. Telling which is which would mean matching paths the way
			// Git LFS does, so every pointer is kept and the warning says so.
			filteredPaths = append(filteredPaths, file.path)
			continue
		}
		if !c.needsContent(file.path, size, opts) {
			c.dropCheckedOut(file.path, target, size)
			continue
		}
		if err := copyLFSObject(object, target, oid, size); err != nil {
			c.warnings = append(c.warnings, fmt.Sprintf("LFS object for %s could not be read (%v); seeded the pointer text", file.path, err))
		}
	}
}

// seedHeadLFSMode is what a clean checkout by the caller does with an LFS
// pointer.
type seedHeadLFSMode int

const (
	// seedHeadLFSUnfiltered: no lfs filter is set up, so git writes the
	// pointer as committed.
	seedHeadLFSUnfiltered seedHeadLFSMode = iota
	// seedHeadLFSSkips: git-lfs runs but skips the smudge, and writes the
	// pointer in its own encoding.
	seedHeadLFSSkips
	// seedHeadLFSSmudges: git-lfs replaces the pointer with its object,
	// where it can get it.
	seedHeadLFSSmudges
	// seedHeadLFSRefused: git-lfs does not smudge with the command's
	// arguments. It exits with an error, so git writes the pointer as
	// committed or, where the filter is required, fails the checkout; for -h
	// it writes its help instead. Seed keeps the pointer, and says so.
	seedHeadLFSRefused
)

// seedHeadLFSCommandMode is what a filter's command, the one the caller's git
// runs (seedHeadDrivers.lfsCommand), does with an LFS pointer on checkout. A
// command that is not git-lfs (seedHeadGitLFSCommand), or none, leaves it
// unfiltered. Git LFS skips the smudge when its --skip flag is true
// (seedHeadLFSSkipFlag), as `git lfs install --skip-smudge` sets it up, or
// when GIT_LFS_SKIP_SMUDGE is true in its environment: the caller's, unless
// the command line sets it. Seed's own git commands always skip it
// (seedHeadGitEnv), so only the caller's settings count. Checked against
// git-lfs 3.4.1, which reads 1, t, true, yes and on in any case as true there.
func seedHeadLFSCommandMode(command string) seedHeadLFSMode {
	args, assignments, ok := seedHeadGitLFSCommand(command)
	if !ok {
		return seedHeadLFSUnfiltered
	}
	skip, ok := seedHeadLFSSkipFlag(args)
	if !ok {
		return seedHeadLFSRefused
	}
	skipSmudge := os.Getenv("GIT_LFS_SKIP_SMUDGE")
	for _, assignment := range assignments {
		if value, found := strings.CutPrefix(assignment, "GIT_LFS_SKIP_SMUDGE="); found {
			skipSmudge = value
		}
	}
	switch strings.ToLower(skipSmudge) {
	case "1", "t", "true", "yes", "on":
		return seedHeadLFSSkips
	}
	if skip {
		return seedHeadLFSSkips
	}
	return seedHeadLFSSmudges
}

// seedHeadLFSSkipFlag reads the --skip flag from the arguments git-lfs gets,
// its subcommand first, as git-lfs's flag parser does (checked against
// git-lfs 3.4.1): anywhere after the subcommand and before "--", as --skip or
// -s, in a cluster of shorthands (-ss), or with a value after "=" that
// strconv.ParseBool reads, the parser git-lfs uses. The last one wins. ok is
// false where git-lfs refuses the arguments: no subcommand, a flag before it,
// another flag (--skip is the only one smudge and filter-process take), or a
// value ParseBool rejects. So is -h or --help, for which git-lfs prints its
// help instead.
func seedHeadLFSSkipFlag(args []string) (skip, ok bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false, false
	}
	value := func(text string) bool {
		v, err := strconv.ParseBool(text)
		skip, ok = v, err == nil
		return ok
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		if long, isLong := strings.CutPrefix(arg, "--"); isLong {
			name, text, hasValue := strings.Cut(long, "=")
			switch {
			case name != "skip":
				return false, false
			case !hasValue:
				skip = true
			case !value(text):
				return false, false
			}
			continue
		}
		for rest := arg[1:]; rest != ""; rest = rest[1:] {
			if rest[0] != 's' {
				return false, false
			}
			if text, hasValue := strings.CutPrefix(rest[1:], "="); hasValue {
				if !value(text) {
					return false, false
				}
				break
			}
			skip = true
		}
	}
	return skip, true
}

// seedHeadGitLFSCommand reads a filter command as the shell git runs it
// through does, far enough to tell whether it is one git-lfs command. It
// returns the arguments git-lfs gets and the NAME=value settings the command
// line gives its environment, in order. The program counts as git-lfs from
// any directory, with .exe on Windows, or as `git lfs`, after any
// assignments, behind `env` with its own, or as the script of `sh -c`. That
// covers the commands `git lfs install` writes and a wrapper such as
// `env LC_ALL=C git-lfs smudge -- %f`, with stderr sent to /dev/null or not.
// Anything else is not modelled and ok is false: another program (one that
// only names git-lfs as an argument too), a list or pipeline, any other
// redirection, or an expansion. Seed runs no other
// program, so what the checkout wrote is left for it, as for no filter.
func seedHeadGitLFSCommand(command string) (args, assignments []string, ok bool) {
	words, ok := seedHeadShellWords(command)
	if !ok {
		return nil, nil, false
	}
	for len(words) > 0 && seedHeadIsAssignment(words[0].raw) {
		assignments = append(assignments, words[0].text)
		words = words[1:]
	}
	if len(words) > 0 && seedHeadProgram(words[0].text) == "env" {
		// env takes NAME=value up to its program; an option is not modelled,
		// and leaves an option as the program.
		for words = words[1:]; len(words) > 0 && strings.Contains(words[0].text, "=") && !strings.HasPrefix(words[0].text, "-"); words = words[1:] {
			assignments = append(assignments, words[0].text)
		}
	}
	if len(words) == 0 {
		return nil, nil, false
	}
	var rest []seedHeadShellWord
	program := seedHeadProgram(words[0].text)
	switch {
	case program == "git-lfs":
		rest = words[1:]
	case program == "git" && len(words) > 1 && words[1].text == "lfs":
		rest = words[2:]
	case (program == "sh" || program == "bash" || program == "dash") && len(words) > 2 && words[1].text == "-c":
		scriptArgs, scriptAssignments, ok := seedHeadGitLFSCommand(words[2].text)
		return scriptArgs, append(assignments, scriptAssignments...), ok
	default:
		return nil, nil, false
	}
	for _, word := range rest {
		args = append(args, word.text)
	}
	return args, assignments, true
}

// seedHeadProgram names the program a command word runs: its last path
// element, either separator, lowercased, without .exe.
func seedHeadProgram(word string) string {
	return strings.TrimSuffix(strings.ToLower(path.Base(strings.ReplaceAll(word, `\`, "/"))), ".exe")
}

// seedHeadIsAssignment reports whether a word, as written, is a shell
// assignment: an unquoted name, then =.
func seedHeadIsAssignment(raw string) bool {
	name, _, found := strings.Cut(raw, "=")
	if !found || name == "" {
		return false
	}
	for i, c := range name {
		letter := c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// seedHeadShellWord is one word of a filter command: raw as written, text as
// the program gets it once sh removes the quoting.
type seedHeadShellWord struct{ raw, text string }

// seedHeadShellWords splits a filter command into words as sh does for one
// simple command, and stops at a comment. Sending stderr or a higher
// descriptor to /dev/null (2>/dev/null, 2>>/dev/null) only drops what the
// program writes there, and opening /dev/null does not fail, so it and its
// target are dropped. A file target is not: if the shell cannot open it, the
// command never runs. ok is false at anything else it does not model: an
// unquoted operator, another redirection, an expansion or pattern, a
// backslash at a line end, or an unclosed quote.
func seedHeadShellWords(command string) (words []seedHeadShellWord, ok bool) {
	var text strings.Builder
	start := -1
	// redirected: the word being read, or the next, is a 2> target.
	redirected, otherTarget := false, false
	end := func(i int) {
		if start < 0 {
			return
		}
		if redirected {
			redirected = false
			otherTarget = otherTarget || text.String() != "/dev/null"
		} else {
			words = append(words, seedHeadShellWord{raw: command[start:i], text: text.String()})
		}
		text.Reset()
		start = -1
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == ' ' || c == '\t' || c == '#' && start < 0 {
			end(i)
			if c == '#' {
				break
			}
			continue
		}
		if c == '>' && !redirected && start >= 0 && seedHeadStderrFD(command[start:i]) {
			text.Reset()
			start = -1
			redirected = true
			if i+1 < len(command) && command[i+1] == '>' {
				i++
			}
			continue
		}
		if strings.IndexByte("|&;<>()$`*?[\n", c) >= 0 {
			return nil, false
		}
		if start < 0 {
			start = i
		}
		switch c {
		case '\'':
			closing := strings.IndexByte(command[i+1:], '\'')
			if closing < 0 {
				return nil, false
			}
			text.WriteString(command[i+1 : i+1+closing])
			i += 1 + closing
		case '"':
			n, closed := seedHeadDoubleQuoted(command[i+1:], &text)
			if !closed {
				return nil, false
			}
			i += n
		case '\\':
			if i+1 == len(command) || command[i+1] == '\n' {
				return nil, false
			}
			i++
			text.WriteByte(command[i])
		default:
			text.WriteByte(c)
		}
	}
	end(len(command))
	if redirected || otherTarget {
		// A 2> with no target, or one that is not /dev/null.
		return nil, false
	}
	return words, true
}

// seedHeadStderrFD reports whether a word written right before > names a
// descriptor above stdout: unquoted digits, 2 or more.
func seedHeadStderrFD(raw string) bool {
	fd, err := strconv.Atoi(raw)
	return err == nil && fd >= 2 && strings.Trim(raw, "0123456789") == ""
}

// seedHeadDoubleQuoted reads the inside of a double-quoted string, writes
// what sh makes of it, and returns how many bytes it took, the closing quote
// included. A backslash escapes only $, `, ", \ and a newline there. closed
// is false at an expansion or with no closing quote.
func seedHeadDoubleQuoted(s string, text *strings.Builder) (n int, closed bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			return i + 1, true
		case '$', '`':
			return 0, false
		case '\\':
			if i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
				i++
				if s[i] != '\n' {
					text.WriteByte(s[i])
				}
				continue
			}
		}
		text.WriteByte(s[i])
	}
	return 0, false
}

// seedHeadCallerConfigCountEntries counts the GIT_CONFIG_COUNT entries of the
// caller's environment that set key, compared as git compares keys: the
// section and variable names ignore case, a subsection does not.
func seedHeadCallerConfigCountEntries(key string) int {
	count, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GIT_CONFIG_COUNT")))
	if err != nil {
		return 0
	}
	n := 0
	for i := range count {
		name := os.Getenv("GIT_CONFIG_KEY_" + strconv.Itoa(i))
		first, last := strings.Index(name, "."), strings.LastIndex(name, ".")
		if first >= 0 && strings.ToLower(name[:first])+name[first:last]+strings.ToLower(name[last:]) == key {
			n++
		}
	}
	return n
}

// encodeLFSPointer writes the pointer at target the way git-lfs writes one
// whose smudge it skips: the current version line, LF line ends, nothing for
// an empty object. An extended pointer is left as committed.
func (c *seedHeadCheckout) encodeLFSPointer(path, target string, data []byte, oid string, size int64, extended bool) {
	encoded := ""
	if size > 0 {
		encoded = fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)
	}
	if extended || string(data) == encoded {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".lfs-pointer-*")
	if err == nil {
		_, err = tmp.WriteString(encoded)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), target)
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}
	if err != nil {
		c.warnings = append(c.warnings, fmt.Sprintf("LFS pointer for %s could not be written as Git LFS writes it (%v); seeded the committed pointer text", path, err))
	}
}

// seedHeadLFSFetchFiltered reports whether Git LFS is set to smudge only some
// paths: lfs.fetchinclude or lfs.fetchexclude holds a pattern. Git LFS reads
// the .lfsconfig of the checked-out commit and then the git configuration, and
// the last value of a key wins, so a key the git configuration sets (to
// nothing, too) overrides the commit's and only the last value counts.
func seedHeadLFSFetchFiltered(ctx context.Context, runner EnvironmentCommandRunner, repoDir, commit string) bool {
	env := seedHeadGitEnv("")
	for _, key := range []string{"lfs.fetchinclude", "lfs.fetchexclude"} {
		value, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "config", "--get", key)
		if err != nil {
			value, _, err = runner.RunWithEnv(ctx, repoDir, env, "git", "config", "--blob", commit+":.lfsconfig", "--get", key)
		}
		// The value is a comma-separated pattern list, which Git LFS ignores
		// when it holds no pattern.
		if err == nil && strings.Trim(string(value), " ,\t\r\n") != "" {
			return true
		}
	}
	return false
}

// seedHeadLFSObjectsDir is the object directory of the repository's Git LFS
// store, as git-lfs finds it: the directory lfs.storage names, read from the
// git configuration (git-lfs takes no lfs.storage from .lfsconfig), relative
// to the git common directory, which holds lfs where the value is unset or
// empty. It is "" when the git directory cannot be resolved. A path from
// configuration is safe to read: copyLFSObject takes only a regular file
// whose size and sha256 match the committed pointer.
func seedHeadLFSObjectsDir(ctx context.Context, runner EnvironmentCommandRunner, repoDir string) string {
	common, _, err := runner.RunWithEnv(ctx, repoDir, seedHeadGitEnv(""), "git", "rev-parse", "--git-common-dir")
	commonDir := strings.TrimSpace(string(common))
	if err != nil || commonDir == "" {
		return ""
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repoDir, commonDir)
	}
	storage := "lfs"
	if out, _, err := runner.RunWithEnv(ctx, repoDir, seedHeadGitEnv(""), "git", "config", "-z", "--get", "lfs.storage"); err == nil {
		if value := strings.TrimSuffix(string(out), "\x00"); value != "" {
			storage = value
		}
	}
	if !filepath.IsAbs(storage) {
		storage = filepath.Join(commonDir, storage)
	}
	return filepath.Join(storage, "objects")
}

// copyLFSObject replaces target with the LFS object, verifying its size and
// sha256 on the way; on any mismatch target keeps the pointer.
func copyLFSObject(object, target, oid string, size int64) error {
	src, err := clibase.SafeOpenRegularFile(object)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".lfs-object-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(src, size+1))
	closeErr := tmp.Close()
	switch {
	case copyErr != nil:
		err = copyErr
	case closeErr != nil:
		err = closeErr
	case written != size || hex.EncodeToString(hash.Sum(nil)) != oid:
		err = fmt.Errorf("LFS object %s does not match its pointer", oid)
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// lfsPointerVersions are the version lines git-lfs reads as a pointer's.
var lfsPointerVersions = map[string]bool{
	"https://git-lfs.github.com/spec/v1": true,
	"https://hawser.github.com/spec/v1":  true,
	"http://git-media.io/v/2":            true,
}

// lfsPointerExtensionKey matches an extension line's key, ext-<n>-<name>.
var lfsPointerExtensionKey = regexp.MustCompile(`^ext-\d-\w+`)

// parseLFSPointer reads data the way git-lfs's smudge does (its non-strict
// pointer decoder) and returns the sha256 object id and size, and whether the
// pointer names an extension. With surrounding white space trimmed, a pointer
// is non-empty "key value" lines: version, oid and size in that order,
// extension lines only before size, and nothing after it. A file with any
// other line, such as a document that quotes a pointer and goes on, is not
// one: smudge leaves it as committed, so it is never replaced with an object.
// An eol conversion runs before the smudge filter, so a checked-out pointer
// can have CRLF line ends, which git-lfs reads as LF.
func parseLFSPointer(data []byte) (string, int64, bool, bool) {
	if len(data) > seedHeadLFSPointerMaxBytes {
		return "", 0, false, false
	}
	keys := [...]string{"version", "oid", "size"}
	var values [len(keys)]string
	next := 0
	extensions := map[byte]string{} // priority -> key
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, " ")
		if !found || next == len(keys) {
			return "", 0, false, false
		}
		if key == keys[next] {
			values[next] = value
			next++
			continue
		}
		// Only an extension can stand in another key's place. Its priority is
		// the digit after "ext-", and two extensions may not share one.
		if !lfsPointerExtensionKey.MatchString(key) {
			return "", 0, false, false
		}
		if _, ok := lfsPointerObjectID(value); !ok {
			return "", 0, false, false
		}
		if other, ok := extensions[key[4]]; ok && other != key {
			return "", 0, false, false
		}
		extensions[key[4]] = key
	}
	if next != len(keys) || !lfsPointerVersions[values[0]] {
		return "", 0, false, false
	}
	oid, ok := lfsPointerObjectID(values[1])
	if !ok {
		return "", 0, false, false
	}
	size, err := strconv.ParseInt(values[2], 10, 64)
	if err != nil || size < 0 {
		return "", 0, false, false
	}
	return oid, size, len(extensions) > 0, true
}

// lfsPointerObjectID returns the object id in an LFS pointer's
// "sha256:<64 lowercase hex>" value.
func lfsPointerObjectID(value string) (string, bool) {
	oid, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(oid) != sha256.Size*2 || strings.ToLower(oid) != oid {
		return "", false
	}
	if _, err := hex.DecodeString(oid); err != nil {
		return "", false
	}
	return oid, true
}

// parseSeedHeadTree parses `git ls-tree -r -z` output, with or without -l,
// into the sorted, de-duplicated inventory and each path's entry. NUL
// separation is what keeps a tracked path with a newline or a quote in it
// intact; the sort matches listSeedFiles so the cap selects the same files on
// either root.
func parseSeedHeadTree(data []byte) ([]string, map[string]seedHeadEntry) {
	entries := map[string]seedHeadEntry{}
	var paths []string
	for _, record := range strings.Split(string(data), "\x00") {
		meta, path, ok := strings.Cut(record, "\t")
		if !ok || path == "" {
			continue
		}
		// <mode> SP <type> SP <object> [SP+ <size>], size "-" for a
		// submodule and absent without -l.
		fields := strings.Fields(meta)
		if len(fields) != 3 && len(fields) != 4 {
			continue
		}
		size := int64(-1)
		if len(fields) == 4 {
			if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
				size = n
			}
		}
		entries[path] = seedHeadEntry{mode: fields[0], kind: fields[1], object: fields[2], size: size}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return compactSortedStrings(paths), entries
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
