// Copied verbatim from github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153) — do not edit; de-internalize
// upstream to dedupe (follow-up).

package gitmeta

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// StringVal is a serialized string metadata value.
type StringVal struct {
	Target Target
	Key    string
	Value  string
}

// ListVal is a serialized list metadata value (ordered timestamped entries).
type ListVal struct {
	Target  Target
	Key     string
	Entries []ListEntry
}

// SetVal is a serialized set metadata value (unique members).
type SetVal struct {
	Target  Target
	Key     string
	Members []string
}

// Tombstone records an explicit deletion. Scope selects which form:
// whole-key, a single list entry, or a single set member.
type Tombstone struct {
	Target Target
	Key    string
	// Entry is set for a list-entry tombstone (the "<ts>-<hash>" entry name).
	Entry string
	// Member is set for a set-member tombstone (the member's blob OID).
	Member string
	// Content is the original blob content to re-store under the tombstone
	// (the spec re-uses the deleted value's blob for content comparison).
	Content string
}

// Scope classifies a tombstone for storage: a ("key"|"list_entry"|"set_member")
// scope plus the entry/member identifier (empty for whole-key). This is the
// single source of truth for the tombstone discriminator across packages.
func (t Tombstone) Scope() (scope, ident string) {
	switch {
	case t.Entry != "":
		return "list_entry", t.Entry
	case t.Member != "":
		return "set_member", t.Member
	default:
		return "key", ""
	}
}

// State is the full shareable metadata state for one repo at a point in time.
// It is the in-memory bridge between the Postgres read-model and the Git tree:
// serialize turns a State into a tree; materialize turns a tree into a State.
type State struct {
	Strings    []StringVal
	Lists      []ListVal
	Sets       []SetVal
	Tombstones []Tombstone
}

// CurrentString returns the current string value for (target, key) and whether
// one is present. It is the read half of a value-level compare-and-swap: the
// engine reads it from freshly-materialized state to check an OpCompareAndSet
// precondition before writing.
func (s State) CurrentString(t Target, key string) (string, bool) {
	for _, sv := range s.Strings {
		if sv.Target == t && sv.Key == key {
			return sv.Value, true
		}
	}
	return "", false
}

// HasKey reports whether (target, key) currently holds ANY live value — string,
// list, or set. It is the create-time absence check for a value-level CAS
// (Expected==""): unlike CurrentString, which sees only strings, it also sees a
// list or set, so a "create only if absent" precondition cannot silently pass
// over a non-string value and clobber it. A whole-key tombstone is NOT a value
// (the key was deleted), so it does not count as present.
func (s State) HasKey(t Target, key string) bool {
	for _, sv := range s.Strings {
		if sv.Target == t && sv.Key == key {
			return true
		}
	}
	for _, lv := range s.Lists {
		if lv.Target == t && lv.Key == key && len(lv.Entries) > 0 {
			return true
		}
	}
	for _, sv := range s.Sets {
		if sv.Target == t && sv.Key == key && len(sv.Members) > 0 {
			return true
		}
	}
	return false
}

// blobFile is one terminal blob at a full tree path.
type blobFile struct {
	path    string
	content string
}

// files flattens a State into the full set of (tree path, blob content) pairs,
// deriving every path from the exchange-format encoding.
func (s State) files() ([]blobFile, error) {
	var out []blobFile
	for _, sv := range s.Strings {
		p, err := TreePath(sv.Target, sv.Key)
		if err != nil {
			return nil, err
		}
		out = append(out, blobFile{p, sv.Value})
	}
	for _, lv := range s.Lists {
		dir, err := ListDirPath(lv.Target, lv.Key)
		if err != nil {
			return nil, err
		}
		for _, e := range lv.Entries {
			out = append(out, blobFile{dir + "/" + MakeEntryName(e.Timestamp, e.Value), e.Value})
		}
	}
	for _, st := range s.Sets {
		dir, err := SetDirPath(st.Target, st.Key)
		if err != nil {
			return nil, err
		}
		for _, m := range st.Members {
			out = append(out, blobFile{dir + "/" + SetMemberID(m), m})
		}
	}
	for _, ts := range s.Tombstones {
		var p string
		var err error
		switch {
		case ts.Entry != "":
			p, err = ListEntryTombstonePath(ts.Target, ts.Key, ts.Entry)
		case ts.Member != "":
			p, err = SetMemberTombstonePath(ts.Target, ts.Key, ts.Member)
		default:
			p, err = TombstonePath(ts.Target, ts.Key)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, blobFile{p, ts.Content})
	}
	return out, nil
}

// Serialize builds the metadata tree for a State into the given object storer
// and returns the root tree hash. All intermediate blobs and trees are written
// to the storer so a commit can reference the result and be pushed.
func Serialize(s State, store storer.EncodedObjectStorer) (plumbing.Hash, error) {
	files, err := s.files()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return BuildTree(files, store)
}

// treeNode is a node in the path trie used to build nested trees.
type treeNode struct {
	children map[string]*treeNode // sub-trees / files by component
	content  *string              // non-nil for a leaf blob
}

func newTreeNode() *treeNode { return &treeNode{children: map[string]*treeNode{}} }

// BuildTree writes blobs and nested trees for the given files and returns the
// root tree hash. Paths are "/"-separated; every leaf is a regular-file blob.
func BuildTree(files []blobFile, store storer.EncodedObjectStorer) (plumbing.Hash, error) {
	root := newTreeNode()
	for _, f := range files {
		segs := strings.Split(f.path, "/")
		n := root
		for i, seg := range segs {
			if seg == "" {
				return plumbing.ZeroHash, fmt.Errorf("empty path segment in %q", f.path)
			}
			if i == len(segs)-1 {
				content := f.content
				n.children[seg] = &treeNode{content: &content}
				break
			}
			child, ok := n.children[seg]
			if !ok {
				child = newTreeNode()
				n.children[seg] = child
			}
			n = child
		}
	}
	return writeNode(root, store)
}

// writeNode recursively writes a trie node as a tree object, returning its hash.
func writeNode(n *treeNode, store storer.EncodedObjectStorer) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, 0, len(n.children))
	for name, child := range n.children {
		if child.content != nil {
			h, err := writeBlob(*child.content, store)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
			continue
		}
		h, err := writeNode(child, store)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
	}
	// Git requires canonical entry order (subtrees compared as if they had a
	// trailing slash). TreeEntrySorter implements exactly that rule.
	sort.Sort(object.TreeEntrySorter(entries))

	tree := &object.Tree{Entries: entries}
	obj := store.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree: %w", err)
	}
	return h, nil
}

// writeBlob writes blob content to the storer and returns its hash.
func writeBlob(content string, store storer.EncodedObjectStorer) (plumbing.Hash, error) {
	obj := store.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write([]byte(content)); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store blob: %w", err)
	}
	return h, nil
}
