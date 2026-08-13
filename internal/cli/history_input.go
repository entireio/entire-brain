package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const historySessionInventoryErrorCode = memoryErrSessionInventory

// historySessionInventoryMaxEntries bounds the complete sessions-tree
// inventory, including directories and files with unsupported extensions. A
// limit over the whole tree prevents a hostile collection of irrelevant names
// from turning projection preparation into unbounded work. It is a variable so
// adversarial tests can exercise the refusal without creating thousands of
// files.
var historySessionInventoryMaxEntries = 10_000

// beforeHistorySessionFileOpen is a deterministic race seam for tests. It runs
// after descriptor-rooted enumeration and immediately before the enumerated
// leaf is rebound to an open file descriptor.
var beforeHistorySessionFileOpen = func(string) {}

// beforeHistorySessionDescriptorOpen lands after the final Lstat and before
// OpenFile. Unix tests replace the leaf with a FIFO here to prove O_NONBLOCK
// plus the post-open fstat rejects it without hanging.
var beforeHistorySessionDescriptorOpen = func(string) {}

// beforeCanonicalHistoryTranscriptFinish is a deterministic race seam for
// tests. It runs after a direct transcript has been consumed and immediately
// before the opened descriptor, its relative components, and the Brain root's
// current pathname identity are revalidated.
var beforeCanonicalHistoryTranscriptFinish = func() {}

// errHistorySessionInventoryDegraded is the stable typed refusal returned when
// the canonical transcript inventory cannot be proven complete and safe.
var errHistorySessionInventoryDegraded = errors.New(memoryErrSessionInventory)

// errHistorySessionsRootMissing reports that the Brain has no sessions/
// directory at all: a fresh Brain, or an export that selected zero sessions
// (ensureExportDirectories only creates branch directories). That is a NORMAL
// state, not a degraded inventory, so callers take their empty-Brain branch.
// It needs its own error because historySessionInventoryError deliberately
// does not unwrap (its Is matches only the degraded sentinel), so an ENOENT
// wrapped in one can never satisfy an fs.ErrNotExist test. Callers must use
// errors.Is, not os.IsNotExist, which does not unwrap either.
var errHistorySessionsRootMissing = fmt.Errorf("history sessions directory: %w", fs.ErrNotExist)

type historySessionInventoryError struct {
	Path   string
	Reason string
	Limit  int
	Err    error
}

func (e *historySessionInventoryError) Error() string {
	detail := strings.TrimSpace(e.Reason)
	if detail == "" {
		detail = "canonical transcript inventory is unsafe or incomplete"
	}
	if e.Path != "" {
		detail += ": " + filepath.ToSlash(e.Path)
	}
	if e.Limit > 0 {
		detail += fmt.Sprintf(" (limit %d)", e.Limit)
	}
	if e.Err != nil {
		detail += ": " + e.Err.Error()
	}
	return memoryErrSessionInventory + ": " + memoryErrStateUnsafe + ": " + detail
}

func (e *historySessionInventoryError) Unwrap() error { return e.Err }

func (e *historySessionInventoryError) Is(target error) bool {
	return target == errHistorySessionInventoryDegraded
}

type historySessionInventory struct {
	brainDir string
	root     *os.Root
	files    []historySessionFile
	entries  []historySessionTreeEntry
	rootInfo os.FileInfo
}

type historySessionTreeEntry struct {
	Rel         string
	Mode        os.FileMode
	Size        int64
	ModUnixNano int64
	info        os.FileInfo
}

func (i *historySessionInventory) Close() error {
	if i == nil || i.root == nil {
		return nil
	}
	err := i.root.Close()
	i.root = nil
	return err
}

// collectHistorySessionInventory establishes one Brain-root descriptor and
// performs a complete, bounded walk beneath sessions/. Symlinks and special
// files are rejected rather than ignored, because a partial inventory must
// never be published as a complete projection.
func collectHistorySessionInventory(ctx context.Context, brainDir string) (*historySessionInventory, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(brainDir)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, &historySessionInventoryError{Path: brainDir, Reason: "Brain root is not a regular directory"}
	}
	root, err := os.OpenRoot(brainDir)
	if err != nil {
		return nil, &historySessionInventoryError{Path: brainDir, Reason: "open Brain root", Err: err}
	}
	inventory := &historySessionInventory{brainDir: brainDir, root: root, rootInfo: before}
	keep := false
	defer func() {
		if !keep {
			_ = inventory.Close()
		}
	}()
	if err := inventory.validateBrainRoot(before); err != nil {
		return nil, err
	}
	// An absent sessions/ ROOT is an empty Brain, not a degraded inventory.
	// Probe it separately so that case gets a plain not-exist error; a
	// directory that vanishes DEEPER in the walk stays degraded, because that
	// is a real integrity signal rather than an empty Brain.
	if _, err := root.Lstat(exportSessionsDirectory); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errHistorySessionsRootMissing
		}
		return nil, &historySessionInventoryError{Path: exportSessionsDirectory, Reason: "inspect transcript directory", Err: err}
	}
	count := 0
	if err := inventory.walkDirectory(ctx, exportSessionsDirectory, nil, &count); err != nil {
		return nil, err
	}
	sort.Slice(inventory.files, func(a, b int) bool {
		if !inventory.files[a].SortTime.Equal(inventory.files[b].SortTime) {
			return inventory.files[a].SortTime.After(inventory.files[b].SortTime)
		}
		return inventory.files[a].Rel < inventory.files[b].Rel
	})
	keep = true
	return inventory, nil
}

func (i *historySessionInventory) validateBrainRoot(expected os.FileInfo) error {
	handle, err := i.root.Open(".")
	if err != nil {
		return &historySessionInventoryError{Path: i.brainDir, Reason: "validate Brain root", Err: err}
	}
	defer handle.Close()
	opened, err := handle.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(expected, opened) {
		return &historySessionInventoryError{Path: i.brainDir, Reason: "Brain root changed while opening", Err: err}
	}
	after, err := os.Lstat(i.brainDir)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return &historySessionInventoryError{Path: i.brainDir, Reason: "Brain root changed or became unsafe", Err: err}
	}
	return nil
}

func (i *historySessionInventory) walkDirectory(ctx context.Context, rel string, expected os.FileInfo, count *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := i.root.Lstat(rel)
	if err != nil {
		return &historySessionInventoryError{Path: rel, Reason: "inspect transcript directory", Err: err}
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return &historySessionInventoryError{Path: rel, Reason: "transcript path component is not a directory"}
	}
	if expected != nil && !os.SameFile(expected, before) {
		return &historySessionInventoryError{Path: rel, Reason: "transcript directory changed after enumeration"}
	}
	if expected != nil && !sameHistoryTreeEntryMetadata(expected, before) {
		return fmt.Errorf("%s: transcript directory changed after enumeration: %s", memoryErrSourceStale, rel)
	}
	i.entries = append(i.entries, newHistorySessionTreeEntry(rel, before))
	dir, err := i.root.OpenFile(rel, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return &historySessionInventoryError{Path: rel, Reason: "open transcript directory", Err: err}
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		return &historySessionInventoryError{Path: rel, Reason: "transcript directory changed while opening", Err: err}
	}
	entries, err := readHistoryDirectoryEntries(ctx, dir, count)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		childRel := path.Join(filepath.ToSlash(rel), entry.Name())
		info, err := i.root.Lstat(childRel)
		if err != nil {
			return &historySessionInventoryError{Path: childRel, Reason: "inspect transcript entry", Err: err}
		}
		if entry.Type()&os.ModeSymlink != 0 || info.Mode()&os.ModeSymlink != 0 {
			return &historySessionInventoryError{Path: childRel, Reason: "symlinked transcript entry is forbidden"}
		}
		if info.IsDir() {
			if err := i.walkDirectory(ctx, childRel, info, count); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return &historySessionInventoryError{Path: childRel, Reason: "transcript entry is not a regular file"}
		}
		i.entries = append(i.entries, newHistorySessionTreeEntry(childRel, info))
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		switch ext {
		case ".jsonl", ".json", ".md", ".txt":
		default:
			continue
		}
		i.files = append(i.files, historySessionFile{
			Path:        filepath.Join(i.brainDir, filepath.FromSlash(childRel)),
			Rel:         childRel,
			SortTime:    historySessionSortTime(entry.Name(), info.ModTime()),
			Size:        info.Size(),
			ModUnixNano: info.ModTime().UnixNano(),
			info:        info,
		})
	}
	after, err := i.root.Lstat(rel)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(opened, after) {
		return &historySessionInventoryError{Path: rel, Reason: "transcript directory changed during enumeration", Err: err}
	}
	if !sameHistoryTreeEntryMetadata(before, after) {
		return fmt.Errorf("%s: transcript directory membership changed during enumeration: %s", memoryErrSourceStale, rel)
	}
	return nil
}

func readHistoryDirectoryEntries(ctx context.Context, dir *os.File, count *int) ([]os.DirEntry, error) {
	var entries []os.DirEntry
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := dir.ReadDir(128)
		for _, entry := range batch {
			(*count)++
			if *count > historySessionInventoryMaxEntries {
				return nil, &historySessionInventoryError{
					Reason: "canonical transcript inventory exceeds its entry ceiling",
					Limit:  historySessionInventoryMaxEntries,
				}
			}
			entries = append(entries, entry)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &historySessionInventoryError{Reason: "read transcript directory", Err: err}
		}
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	return entries, nil
}

// open binds a transcript descriptor to the exact leaf enumerated above. The
// nonblocking flag prevents a regular-file-to-FIFO/socket swap from hanging on
// Unix; it is zero on Windows. The Root keeps path resolution beneath Brain.
func (i *historySessionInventory) open(ctx context.Context, file historySessionFile) (*os.File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	beforeHistorySessionFileOpen(file.Rel)
	before, err := i.root.Lstat(file.Rel)
	if err != nil {
		return nil, &historySessionInventoryError{Path: file.Rel, Reason: "inspect transcript before open", Err: err}
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || file.info == nil || !os.SameFile(file.info, before) {
		return nil, &historySessionInventoryError{Path: file.Rel, Reason: "transcript changed or became unsafe after enumeration"}
	}
	if !sameHistoryFileMetadata(file, before) {
		return nil, fmt.Errorf("%s: transcript changed after inventory: %s", memoryErrSourceStale, file.Rel)
	}
	beforeHistorySessionDescriptorOpen(file.Rel)
	f, err := i.root.OpenFile(file.Rel, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		current, currentErr := i.root.Lstat(file.Rel)
		if currentErr == nil && current.Mode().IsRegular() && current.Mode()&os.ModeSymlink == 0 && file.info != nil && os.SameFile(file.info, current) && sameHistoryFileMetadata(file, current) {
			return nil, fmt.Errorf("read transcript %s: %w", file.Rel, err)
		}
		return nil, &historySessionInventoryError{Path: file.Rel, Reason: "open transcript without following unsafe aliases", Err: errors.Join(err, currentErr)}
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, &historySessionInventoryError{Path: file.Rel, Reason: "transcript changed or became unsafe while opening", Err: err}
	}
	if !sameHistoryFileMetadata(file, opened) {
		_ = f.Close()
		return nil, fmt.Errorf("%s: transcript changed while opening: %s", memoryErrSourceStale, file.Rel)
	}
	after, err := i.root.Lstat(file.Rel)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		_ = f.Close()
		return nil, &historySessionInventoryError{Path: file.Rel, Reason: "transcript path changed while opening", Err: err}
	}
	if !sameHistoryFileMetadata(file, after) {
		_ = f.Close()
		return nil, fmt.Errorf("%s: transcript changed while opening: %s", memoryErrSourceStale, file.Rel)
	}
	return f, nil
}

func (i *historySessionInventory) validateAfterRead(file historySessionFile, f *os.File) error {
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || file.info == nil || !os.SameFile(file.info, opened) {
		return &historySessionInventoryError{Path: file.Rel, Reason: "transcript changed while reading", Err: err}
	}
	if !sameHistoryFileMetadata(file, opened) {
		return fmt.Errorf("%s: transcript changed while reading: %s", memoryErrSourceStale, file.Rel)
	}
	after, err := i.root.Lstat(file.Rel)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return &historySessionInventoryError{Path: file.Rel, Reason: "transcript path changed while reading", Err: err}
	}
	if !sameHistoryFileMetadata(file, after) {
		return fmt.Errorf("%s: transcript changed while reading: %s", memoryErrSourceStale, file.Rel)
	}
	return nil
}

func (i *historySessionInventory) validateAll(ctx context.Context) error {
	for _, file := range i.files {
		if err := i.validateFileMembership(ctx, file); err != nil {
			return err
		}
	}
	return nil
}

func (i *historySessionInventory) validateFileMembership(ctx context.Context, file historySessionFile) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := i.root.Lstat(file.Rel)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || file.info == nil || !os.SameFile(file.info, current) {
		return &historySessionInventoryError{Path: file.Rel, Reason: "transcript membership changed or became unsafe", Err: err}
	}
	if !sameHistoryFileMetadata(file, current) {
		return fmt.Errorf("%s: transcript metadata changed after inventory: %s", memoryErrSourceStale, file.Rel)
	}
	return nil
}

func sameHistoryFileMetadata(expected historySessionFile, current os.FileInfo) bool {
	return current.Size() == expected.Size && current.ModTime().UnixNano() == expected.ModUnixNano
}

func newHistorySessionTreeEntry(rel string, info os.FileInfo) historySessionTreeEntry {
	return historySessionTreeEntry{
		Rel: rel, Mode: info.Mode(), Size: info.Size(), ModUnixNano: info.ModTime().UnixNano(), info: info,
	}
}

func sameHistoryTreeEntryMetadata(a, b os.FileInfo) bool {
	return a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().UnixNano() == b.ModTime().UnixNano()
}

func (i *historySessionInventory) sameMembership(other *historySessionInventory) bool {
	if i == nil || other == nil {
		return false
	}
	return sameHistorySessionMembership(i.rootInfo, i.entries, other.rootInfo, other.entries)
}

func sameHistorySessionMembership(leftRoot os.FileInfo, leftEntries []historySessionTreeEntry, rightRoot os.FileInfo, rightEntries []historySessionTreeEntry) bool {
	if leftRoot == nil || rightRoot == nil || !os.SameFile(leftRoot, rightRoot) || len(leftEntries) != len(rightEntries) {
		return false
	}
	for index := range leftEntries {
		left, right := leftEntries[index], rightEntries[index]
		if left.Rel != right.Rel || left.Mode != right.Mode || left.Size != right.Size || left.ModUnixNano != right.ModUnixNano || left.info == nil || right.info == nil || !os.SameFile(left.info, right.info) {
			return false
		}
	}
	return true
}

type contextCheckingReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextCheckingReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// openCanonicalHistoryTranscript is the shared direct-read contract used by
// digest and conversation-expansion paths that do not begin from an inventory.
// It performs a one-file inventory under the Brain root and returns an opened,
// descriptor-bound regular file plus a closure that must be called after the
// complete read to prove the leaf did not change.
func openCanonicalHistoryTranscript(ctx context.Context, brainDir, rel string) (*os.File, func() error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	clean, err := cleanBrainRelativePath(rel)
	if err != nil || filepath.ToSlash(clean) != rel || !strings.HasPrefix(rel, exportSessionsDirectory+"/") {
		return nil, nil, &historySessionInventoryError{Path: rel, Reason: "canonical transcript path is unsafe", Err: err}
	}
	beforeRoot, err := os.Lstat(brainDir)
	if err != nil {
		return nil, nil, err
	}
	if beforeRoot.Mode()&os.ModeSymlink != 0 || !beforeRoot.IsDir() {
		return nil, nil, &historySessionInventoryError{Path: brainDir, Reason: "Brain root is not a regular directory"}
	}
	root, err := os.OpenRoot(brainDir)
	if err != nil {
		return nil, nil, &historySessionInventoryError{Path: brainDir, Reason: "open Brain root", Err: err}
	}
	inventory := &historySessionInventory{brainDir: brainDir, root: root, rootInfo: beforeRoot}
	if err := inventory.validateBrainRoot(beforeRoot); err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	components := strings.Split(rel, "/")
	expectedComponents := make(map[string]os.FileInfo, len(components))
	for index := range components {
		componentRel := path.Join(components[:index+1]...)
		info, statErr := root.Lstat(componentRel)
		if statErr != nil {
			_ = root.Close()
			if os.IsNotExist(statErr) {
				return nil, nil, os.ErrNotExist
			}
			return nil, nil, &historySessionInventoryError{Path: componentRel, Reason: "inspect canonical transcript component", Err: statErr}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			_ = root.Close()
			return nil, nil, &historySessionInventoryError{Path: componentRel, Reason: "symlinked canonical transcript component is forbidden"}
		}
		if index < len(components)-1 && !info.IsDir() {
			_ = root.Close()
			return nil, nil, &historySessionInventoryError{Path: componentRel, Reason: "canonical transcript component is not a directory"}
		}
		expectedComponents[componentRel] = info
	}
	leaf := expectedComponents[rel]
	if leaf == nil || !leaf.Mode().IsRegular() {
		_ = root.Close()
		return nil, nil, &historySessionInventoryError{Path: rel, Reason: "canonical transcript is not a regular file"}
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		_ = root.Close()
		return nil, nil, &historySessionInventoryError{Path: rel, Reason: "open canonical transcript", Err: err}
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(leaf, opened) {
		_ = f.Close()
		_ = root.Close()
		return nil, nil, &historySessionInventoryError{Path: rel, Reason: "canonical transcript changed or became unsafe while opening", Err: err}
	}
	validate := func() error {
		currentRoot, rootErr := os.Lstat(brainDir)
		if rootErr != nil || currentRoot.Mode()&os.ModeSymlink != 0 || !currentRoot.IsDir() || !os.SameFile(beforeRoot, currentRoot) {
			return &historySessionInventoryError{Path: brainDir, Reason: "Brain root changed or became unsafe while reading", Err: rootErr}
		}
		openedAfter, statErr := f.Stat()
		if statErr != nil || !openedAfter.Mode().IsRegular() || !os.SameFile(opened, openedAfter) || openedAfter.Size() != opened.Size() || !openedAfter.ModTime().Equal(opened.ModTime()) {
			return &historySessionInventoryError{Path: rel, Reason: "canonical transcript changed while reading", Err: statErr}
		}
		for index := range components {
			componentRel := path.Join(components[:index+1]...)
			current, componentErr := root.Lstat(componentRel)
			expected := expectedComponents[componentRel]
			if componentErr != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
				return &historySessionInventoryError{Path: componentRel, Reason: "canonical transcript component changed while reading", Err: componentErr}
			}
		}
		return nil
	}
	finish := func() error {
		defer root.Close()
		defer f.Close()
		beforeCanonicalHistoryTranscriptFinish()
		return validate()
	}
	return f, finish, nil
}

// readCanonicalHistoryTranscript is the complete direct-transcript read
// contract. It binds the read to the canonical Brain descriptor tree, caps the
// bytes retained in memory, checks cancellation while reading, and withholds
// all bytes unless the mandatory finish revalidation succeeds.
func readCanonicalHistoryTranscript(ctx context.Context, brainDir, rel string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f, finish, err := openCanonicalHistoryTranscript(ctx, brainDir, rel)
	if err != nil {
		return nil, err
	}
	data, readErr := safeReadAll(contextCheckingReader{ctx: ctx, r: f}, maxDocumentTranscriptBytes, "canonical transcript "+filepath.ToSlash(rel))
	finishErr := finish()
	if readErr != nil || finishErr != nil {
		combined := errors.Join(readErr, finishErr)
		if readErr != nil && strings.Contains(readErr.Error(), "exceeds maximum size") {
			return nil, fmt.Errorf("%s: %w", memoryErrInputTooLarge, combined)
		}
		return nil, combined
	}
	return data, nil
}
