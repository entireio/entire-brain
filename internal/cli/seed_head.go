package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// file is treated as too large unread. A driver's output has no bound of its
// own; smudge drivers in use (decrypting, decompressing, stripping) leave a
// file about its size or grow it. Without a cap, an identity or growing driver
// on a multi-gigabyte blob is written out in full only to be dropped; with it,
// one file writes at most this many times --max-file-bytes.
const seedHeadDriverMaxShrink = 64

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
	mode string
	// size is the blob's size in the object store, or -1 for an entry with
	// none (a submodule).
	size int64
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
	// -l adds each blob's size, which is what lets a file the scan only
	// stats stay out of the checkout.
	listing, _, err := envRunner.RunWithEnv(ctx, repoDir, seedHeadGitEnv(""), "git", "ls-tree", "-r", "-z", "-l", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("list committed HEAD %s: %w", shortCommitHash(commit), err)
	}
	paths, entries := parseSeedHeadTree(listing)

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
	var driverSkipped []string
	for _, path := range selected {
		entry := entries[path]
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
		// "too large"), an encoding or a filter driver can shrink it. So a
		// file is checked out and the scan stats the real one, unless its
		// blob is too large however it converts. A doc the scan reads at any
		// size is always checked out.
		if !seedReadsAtAnySize(path) && entry.size > seedHeadMaxShrink(attr, drivers)*int64(opts.maxFileBytes) {
			c.unread[path] = entry.size
			if drivers.runs(attr.filter) {
				driverSkipped = append(driverSkipped, path)
			}
			continue
		}
		wanted = append(wanted, path)
	}
	if len(driverSkipped) > 0 {
		// Past the cap a driver could still have shrunk the file under
		// --max-file-bytes, where a clean checkout would include it. Say so
		// rather than let the entry read like an ordinary size exclusion.
		c.warnings = append(c.warnings, fmt.Sprintf("%d committed files over %dx --max-file-bytes were not run through their filter driver and are reported too large at their committed size (first: %s)", len(driverSkipped), seedHeadDriverMaxShrink, driverSkipped[0]))
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
	c.settleLFSPointers(ctx, runner, repoDir, lfs, opts)
	return nil
}

// seedHeadAttrs is the commit's checkout-relevant attributes for one path.
type seedHeadAttrs struct {
	filter   string
	encoding string
}

// specifiedAttr reports whether check-attr gave an attribute a value.
func specifiedAttr(value string) bool {
	return value != "" && value != "unspecified" && value != "unset"
}

// seedHeadMaxShrink is how many times smaller than its blob a path's checkout
// can be. With neither an encoding nor a driver, what is left (eol, ident)
// does not shrink a file, so a blob over --max-file-bytes is too large.
func seedHeadMaxShrink(attr seedHeadAttrs, drivers seedHeadDrivers) int64 {
	switch {
	case drivers.runs(attr.filter):
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

// attributes reads the filter and working-tree-encoding attributes of paths
// from the commit: --cached reads .gitattributes from the scratch index only.
func (c *seedHeadCheckout) attributes(ctx context.Context, runner EnvironmentCommandRunner, repoDir string, env map[string]string, paths []string) (map[string]seedHeadAttrs, error) {
	attrs := make(map[string]seedHeadAttrs, len(paths))
	for _, batch := range chunkPathsByBytes(paths, seedHeadCheckoutArgBytes) {
		args := append([]string{"check-attr", "--cached", "-z", "filter", "working-tree-encoding", "--"}, batch...)
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
			if os.Remove(target) == nil {
				c.unread[file.path] = size
			}
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

// parseSeedHeadTree parses `git ls-tree -r -z -l` output into the sorted,
// de-duplicated inventory and each path's entry. NUL separation is what keeps
// a tracked path with a newline or a quote in it intact; the sort matches
// listSeedFiles so the cap selects the same files on either root.
func parseSeedHeadTree(data []byte) ([]string, map[string]seedHeadEntry) {
	entries := map[string]seedHeadEntry{}
	var paths []string
	for _, record := range strings.Split(string(data), "\x00") {
		meta, path, ok := strings.Cut(record, "\t")
		if !ok || path == "" {
			continue
		}
		// <mode> SP <type> SP <object> SP+ <size>, size "-" for a submodule.
		fields := strings.Fields(meta)
		if len(fields) != 4 {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			size = -1
		}
		entries[path] = seedHeadEntry{mode: fields[0], size: size}
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
