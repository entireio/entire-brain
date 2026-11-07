package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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

// seedHeadEncodingMaxShrink bounds how much smaller a working-tree-encoding
// checkout can be than its blob: the blob is UTF-8, at most 4 bytes a
// character, and no encoding writes a character in under one byte.
const seedHeadEncodingMaxShrink = 4

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
	// folded counts the checked-out paths under each case-folded spelling,
	// so a file two paths share on a case-insensitive filesystem is never
	// removed from under the other one (dropCheckedOut).
	folded map[string]int
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
	return (e.mode == "100644" || e.mode == "100755") && e.size >= 0
}

// seedHeadGitEnv is the environment of every git call that builds the
// checkout. GIT_NO_LAZY_FETCH keeps a partial clone from fetching a missing
// object over the network in the middle of a background seed, and
// GIT_LFS_SKIP_SMUDGE does the same for an LFS smudge filter configured in the
// user's own git config (repository-scoped filter drivers are already blanked by
// repoFilterDriverOverrides). Either way the object is read locally or the
// checkout fails, and a failed checkout falls back to the refusal; an LFS
// object already stored locally is then filled in by settleSmallFiles.
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
	checkout := &seedHeadCheckout{commit: commit, dir: filepath.Join(tempRoot, "tree"), paths: paths, unread: map[string]int64{}, tempRoot: tempRoot}
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
	selected := paths
	if maxFiles > 0 && len(selected) > maxFiles {
		selected = selected[:maxFiles]
	}
	var present []string
	for _, path := range selected {
		entry := entries[path]
		switch {
		case missing[entry.object]:
			entry.missing = true
			entries[path] = entry
		case entry.kind == "blob":
			present = append(present, path)
		}
	}
	// Literal pathspecs: a tracked name that starts with ':' or holds a glob
	// character names itself and nothing else.
	sizeEnv := maps.Clone(env)
	sizeEnv["GIT_LITERAL_PATHSPECS"] = "1"
	for _, batch := range chunkPathsByBytes(present, seedHeadCheckoutArgBytes) {
		args := append([]string{"ls-tree", "-r", "-z", "-l", "--full-tree", commit, "--"}, batch...)
		out, _, err := runner.RunWithEnv(ctx, repoDir, sizeEnv, "git", args...)
		if err != nil {
			return nil, nil, fmt.Errorf("list committed HEAD %s sizes: %w", shortCommitHash(commit), err)
		}
		_, sized := parseSeedHeadTree(out)
		for path, sizedEntry := range sized {
			if entry, ok := entries[path]; ok {
				entry.size = sizedEntry.size
				entries[path] = entry
			}
		}
	}
	return paths, entries, nil
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
	// global file apply as they do to a clean checkout). This works on every
	// supported git, unlike --attr-source (2.40+). Files are written through
	// --prefix, never into this directory.
	emptyWorkTree := filepath.Join(c.tempRoot, "no-worktree")
	if err := os.Mkdir(emptyWorkTree, 0o700); err != nil {
		return fmt.Errorf("create committed HEAD checkout: %w", err)
	}
	env["GIT_WORK_TREE"] = emptyWorkTree
	selected := c.paths
	if opts.maxFiles > 0 && len(selected) > opts.maxFiles {
		selected = selected[:opts.maxFiles]
	}
	// The commit's own attributes decide two things below: which blobs are
	// LFS pointers, and which ones a checkout converts.
	var regular []string
	for _, path := range selected {
		if entries[path].regularFile() {
			regular = append(regular, path)
		}
	}
	attrs, err := c.attributes(ctx, runner, repoDir, env, regular)
	if err != nil {
		return err
	}
	drivers := seedHeadFilterDrivers(ctx, runner, repoDir, env)
	var wanted []string
	var lfs []seedHeadSmallFile
	var absent, convertSkipped, driven []string
	for _, path := range selected {
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
		// The path rules settle an entry whatever the file holds.
		if _, _, pathOK := inspectSeedFilePath(path, opts); !pathOK {
			c.unread[path] = entry.size
			continue
		}
		attr := attrs[path]
		if attr.filter == "lfs" && entry.size <= seedHeadLFSPointerMaxBytes {
			// The blob is a pointer: the file's real size is only known once
			// it is read.
			wanted = append(wanted, path)
			lfs = append(lfs, seedHeadSmallFile{path: path, size: entry.size})
			continue
		}
		// Otherwise the size rules need the size of the checked-out file,
		// which conversions make differ from the blob's: an eol conversion
		// grows it (a small media file can cross --max-file-bytes and become
		// "too large"), an encoding, ident or a filter driver can shrink it.
		// So a file is checked out and the scan stats the real one, unless
		// its blob is too large however it converts. A doc the scan reads at
		// any size is always checked out.
		if !seedReadsAtAnySize(path) && entry.size > seedHeadMaxShrink(attr, drivers)*int64(opts.maxFileBytes) {
			c.unread[path] = entry.size
			if drivers.runs(attr.filter) || attr.ident {
				convertSkipped = append(convertSkipped, path)
			}
			continue
		}
		// Every file a driver converts goes through the driver checkout below,
		// which passes the driver's stderr on, and so does every ident file:
		// ident shrinks an expanded keyword and grows each "$Id$" by an object
		// id, so either can end up any size. Both are checked out in bounded
		// batches and dropped there when they come out too large. A doc the
		// scan reads at any size is never removed there.
		if drivers.runs(attr.filter) || attr.ident {
			driven = append(driven, path)
			continue
		}
		wanted = append(wanted, path)
	}
	wanted, driven = seedHeadOrderCaseAliases(selected, wanted, driven)
	if len(convertSkipped) > 0 {
		// Past the cap a driver or ident could still have shrunk the file
		// under --max-file-bytes, where a clean checkout would include it. Say
		// so rather than let the entry read like an ordinary size exclusion.
		c.warnings = append(c.warnings, fmt.Sprintf("%d committed files over %dx --max-file-bytes were not converted by their filter driver or ident attribute and are reported too large at their committed size (first: %s)", len(convertSkipped), seedHeadDriverMaxShrink, convertSkipped[0]))
	}
	if len(absent) > 0 {
		c.warnings = append(c.warnings, fmt.Sprintf("%d committed files were not read because this partial clone has not fetched them (first: %s)", len(absent), absent[0]))
	}
	c.folded = map[string]int{}
	for _, paths := range [][]string{wanted, driven} {
		for _, path := range paths {
			c.folded[strings.ToLower(path)]++
		}
	}
	// git appends each path to the prefix verbatim, so it needs the trailing
	// separator; forward slashes are what git expects on every platform.
	prefix := "--prefix=" + filepath.ToSlash(c.dir) + "/"
	// -f: two tracked paths that differ only in case land on one file on a
	// case-insensitive filesystem. A clean checkout there keeps the last one
	// written, and so does this; without -f the second is an error and the
	// whole seed falls back to refusing.
	for _, batch := range chunkPathsByBytes(wanted, seedHeadCheckoutArgBytes) {
		args := append([]string{"checkout-index", "-f", prefix, "--"}, batch...)
		if _, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", args...); err != nil {
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
	driverEnv := maps.Clone(env)
	driverEnv["LANGUAGE"] = "en"
	for start := 0; start < len(driven); start += seedHeadDriverBatchFiles {
		group := driven[start:min(start+seedHeadDriverBatchFiles, len(driven))]
		for _, batch := range chunkPathsByBytes(group, seedHeadCheckoutArgBytes) {
			args := append([]string{"checkout-index", "-f", prefix, "--"}, batch...)
			_, stderr, err := runner.RunWithEnv(ctx, repoDir, driverEnv, "git", args...)
			if err != nil {
				return fmt.Errorf("check out committed HEAD %s: %w", shortCommitHash(c.commit), err)
			}
			driverStderr.add(stderr)
		}
		for _, path := range group {
			target := filepath.Join(c.dir, filepath.FromSlash(path))
			if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() && !seedFileNeedsContent(path, info.Size(), opts) {
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

// seedHeadOrderCaseAliases keeps the checkout's last write the one a clean
// checkout makes. Two paths that differ only in case are one file on a
// case-insensitive filesystem, where a clean checkout leaves whichever path
// comes last in tree order. The driven files are checked out after the
// others, so a path that folds to a driven one moves in with them, and the
// driven list is put back in tree order.
func seedHeadOrderCaseAliases(selected, wanted, driven []string) ([]string, []string) {
	if len(driven) == 0 {
		return wanted, driven
	}
	drivenFolds := make(map[string]bool, len(driven))
	for _, path := range driven {
		drivenFolds[strings.ToLower(path)] = true
	}
	late := map[string]bool{}
	var kept []string
	for _, path := range wanted {
		if drivenFolds[strings.ToLower(path)] {
			late[path] = true
		} else {
			kept = append(kept, path)
		}
	}
	if len(late) == 0 {
		return wanted, driven
	}
	for _, path := range driven {
		late[path] = true
	}
	ordered := make([]string, 0, len(late))
	for _, path := range selected {
		if late[path] {
			ordered = append(ordered, path)
		}
	}
	return kept, ordered
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
// size, which is what its entry is built from, and removes it. Two selected
// paths that differ only in case share one file on a case-insensitive
// filesystem, so a file another checked-out path folds to is left on disk:
// removing it would take the other path's file with it. A file that cannot be
// removed stays too; the scan never reads it either way.
func (c *seedHeadCheckout) dropCheckedOut(path, target string, size int64) {
	c.unread[path] = size
	if c.folded[strings.ToLower(path)] > 1 {
		return
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
// however long the line, so both get the driver cap. An encoding shrinks a
// file at most seedHeadEncodingMaxShrink times. eol, all that is left, does
// not shrink one, so a blob over --max-file-bytes is too large.
func seedHeadMaxShrink(attr seedHeadAttrs, drivers seedHeadDrivers) int64 {
	switch {
	case drivers.runs(attr.filter) || attr.ident:
		return seedHeadDriverMaxShrink
	case specifiedAttr(attr.encoding):
		return seedHeadEncodingMaxShrink
	}
	return 1
}

// seedHeadDrivers is the set of filter drivers that run a command on checkout:
// a smudge or process command set in the configuration the hardened runner
// leaves in force. Repository-scoped drivers are blanked there
// (git_harden_filters.go), so a filter=x with only a repository driver, or
// none at all, writes the blob unchanged. LFS is handled on its own
// (settleLFSPointers).
type seedHeadDrivers struct {
	names map[string]bool
	// unknown: the configuration could not be read, so every filter is
	// taken to run one, which only costs checking more files out.
	unknown bool
}

func (d seedHeadDrivers) runs(filter string) bool {
	if !specifiedAttr(filter) || filter == "lfs" {
		return false
	}
	return d.unknown || d.names[filter]
}

// seedHeadFilterDrivers reads every driver command in one git call, however
// many filters the commit names.
func seedHeadFilterDrivers(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string) seedHeadDrivers {
	out, _, err := runner.RunWithEnv(ctx, repoDir, env, "git", "config", "-z", "--get-regexp", `^filter\..*\.(smudge|process)$`)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 {
			return seedHeadDrivers{} // no driver is configured
		}
		return seedHeadDrivers{unknown: true}
	}
	// -z: <key> LF <value> NUL per entry, in the order git reads its
	// configuration, so a key's last value is the one in force; the hardened
	// runner's blanking comes last of all.
	values := map[string]string{}
	for _, record := range strings.Split(string(out), "\x00") {
		if key, value, _ := strings.Cut(record, "\n"); key != "" {
			values[key] = value
		}
	}
	names := map[string]bool{}
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if name, ok := filterDriverFromConfigKey(key); ok {
			names[name] = true
		}
	}
	return seedHeadDrivers{names: names}
}

// attributes reads the filter, working-tree-encoding and ident attributes of
// paths from the commit: --cached reads .gitattributes from the scratch index
// only.
func (c *seedHeadCheckout) attributes(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, paths []string) (map[string]seedHeadAttrs, error) {
	attrs := make(map[string]seedHeadAttrs, len(paths))
	for _, batch := range chunkPathsByBytes(paths, seedHeadCheckoutArgBytes) {
		args := append([]string{"check-attr", "--cached", "-z", "filter", "working-tree-encoding", "ident", "--"}, batch...)
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

// seedHeadSmallFile is a checked-out blob small enough to be a Git LFS pointer.
type seedHeadSmallFile struct {
	path string
	size int64
}

// settleLFSPointers finishes the blobs the commit tracks with filter=lfs. The
// checkout skips the smudge filter so a background seed never downloads
// anything (seedHeadGitEnv), which on its own seeded every LFS document as its
// pointer text. A pointer whose object is already in the local LFS store is
// replaced with that object, which is what a working tree with the content
// fetched holds; one whose object is not stored locally stays the pointer, the
// same text a working tree without the content shows. Only the default store
// (<git-common-dir>/lfs) is consulted. An object that is stored but cannot be
// copied is reported in the seed's warnings rather than passed off as read.
//
// Each file is then held to the same rule as every other blob, at its real
// size: one the scan would only stat leaves the checkout and is recorded
// unread, so a large LFS binary is never copied here either.
func (c *seedHeadCheckout) settleLFSPointers(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, files []seedHeadSmallFile, opts seedCommandOptions) {
	objectsDir, objectsResolved := "", false
	for _, file := range files {
		target := filepath.Join(c.dir, filepath.FromSlash(file.path))
		data, err := clibase.SafeReadFile(target, seedHeadLFSPointerMaxBytes)
		if err != nil {
			continue
		}
		oid, size, ok := parseLFSPointer(data)
		if !ok {
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
		if !seedFileNeedsContent(file.path, size, opts) {
			c.dropCheckedOut(file.path, target, size)
			continue
		}
		if err := copyLFSObject(object, target, oid, size); err != nil {
			c.warnings = append(c.warnings, fmt.Sprintf("LFS object for %s could not be read (%v); seeded the pointer text", file.path, err))
		}
	}
}

// seedHeadLFSObjectsDir is the repository's default Git LFS object store, or ""
// when the git directory cannot be resolved.
func seedHeadLFSObjectsDir(ctx context.Context, runner EnvironmentCommandRunner, repoDir string) string {
	common, _, err := runner.RunWithEnv(ctx, repoDir, seedHeadGitEnv(""), "git", "rev-parse", "--git-common-dir")
	commonDir := strings.TrimSpace(string(common))
	if err != nil || commonDir == "" {
		return ""
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repoDir, commonDir)
	}
	return filepath.Join(commonDir, "lfs", "objects")
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

// parseLFSPointer recognises a Git LFS pointer file (spec v1) and returns its
// sha256 object id and size.
func parseLFSPointer(data []byte) (string, int64, bool) {
	if len(data) > seedHeadLFSPointerMaxBytes || !bytes.HasPrefix(data, []byte("version https://git-lfs.github.com/spec/v1\n")) {
		return "", 0, false
	}
	oid, size := "", int64(-1)
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "oid sha256:"); ok {
			oid = value
		} else if value, ok := strings.CutPrefix(line, "size "); ok {
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
				size = n
			}
		}
	}
	if len(oid) != sha256.Size*2 || size < 0 {
		return "", 0, false
	}
	if _, err := hex.DecodeString(oid); err != nil || strings.ToLower(oid) != oid {
		return "", 0, false
	}
	return oid, size, true
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
