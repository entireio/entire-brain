// Copied verbatim from github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153); do not edit; de-internalize
// upstream to dedupe (follow-up).

package gitmeta

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/object"
)

// ErrMalformedTree marks a metadata tree that cannot be parsed back into a State
// because it is structurally invalid (an unrecognized layout, a bad leaf path,
// unreadable blob content, etc.). It is a PERMANENT failure: re-materializing the
// same tree will always fail the same way, so a consumer must drop (Term) the
// event rather than redeliver it forever. Wrapped with %w so callers can classify
// via errors.Is.
var ErrMalformedTree = errors.New("gitmeta: malformed metadata tree")

// Materialize walks a metadata tree and reconstructs the State it represents.
// It is the inverse of Serialize: every leaf path is parsed back into a
// (target, key) record by the exchange-format rules, then grouped by value type.
func Materialize(root *object.Tree) (State, error) {
	files, err := flatten(root)
	if err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrMalformedTree, err)
	}

	type listKey struct {
		target Target
		key    string
	}
	strings_ := []StringVal{}
	lists := map[listKey][]ListEntry{}
	listOrder := []listKey{}
	sets := map[listKey][]string{}
	setOrder := []listKey{}
	var tombstones []Tombstone

	for _, f := range files {
		rec, err := parseLeafPath(f.path)
		if err != nil {
			return State{}, fmt.Errorf("%w: parse %q: %v", ErrMalformedTree, f.path, err)
		}
		switch rec.kind {
		case kindString:
			strings_ = append(strings_, StringVal{rec.target, rec.key, f.content})
		case kindListEntry:
			lk := listKey{rec.target, rec.key}
			if _, ok := lists[lk]; !ok {
				listOrder = append(listOrder, lk)
			}
			ts, _ := ParseTimestampFromEntryName(rec.entry)
			lists[lk] = append(lists[lk], ListEntry{Value: f.content, Timestamp: ts})
		case kindSetMember:
			sk := listKey{rec.target, rec.key}
			if _, ok := sets[sk]; !ok {
				setOrder = append(setOrder, sk)
			}
			sets[sk] = append(sets[sk], f.content)
		case kindKeyTombstone:
			tombstones = append(tombstones, Tombstone{Target: rec.target, Key: rec.key, Content: f.content})
		case kindListEntryTombstone:
			tombstones = append(tombstones, Tombstone{Target: rec.target, Key: rec.key, Entry: rec.entry, Content: f.content})
		case kindSetMemberTombstone:
			tombstones = append(tombstones, Tombstone{Target: rec.target, Key: rec.key, Member: rec.member, Content: f.content})
		}
	}

	st := State{Strings: strings_, Tombstones: tombstones}
	for _, lk := range listOrder {
		entries := lists[lk]
		SortListEntries(entries)
		st.Lists = append(st.Lists, ListVal{Target: lk.target, Key: lk.key, Entries: entries})
	}
	for _, sk := range setOrder {
		members := sets[sk]
		slices.Sort(members)
		st.Sets = append(st.Sets, SetVal{Target: sk.target, Key: sk.key, Members: members})
	}
	return st, nil
}

// flatten returns every leaf blob in the tree as (full path, content), like
// `git ls-tree -r`, using go-git's recursive file iterator.
func flatten(t *object.Tree) ([]blobFile, error) {
	var out []blobFile
	err := t.Files().ForEach(func(f *object.File) error {
		content, err := f.Contents()
		if err != nil {
			return err
		}
		out = append(out, blobFile{path: f.Name, content: content})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type leafKind int

const (
	kindString leafKind = iota
	kindListEntry
	kindSetMember
	kindKeyTombstone
	kindListEntryTombstone
	kindSetMemberTombstone
)

type leafRecord struct {
	target Target
	key    string
	kind   leafKind
	entry  string // list entry name
	member string // set member id
}

// parseLeafPath reverses the exchange-format path encoding for one leaf.
func parseLeafPath(path string) (leafRecord, error) {
	comps := strings.Split(path, "/")
	if len(comps) == 0 {
		return leafRecord{}, fmt.Errorf("empty path")
	}
	tt, err := ParseTargetType(comps[0])
	if err != nil {
		return leafRecord{}, err
	}

	var target Target
	var keyStart int
	switch tt {
	case TargetProject:
		target = Target{Type: TargetProject}
		keyStart = 1
	case TargetPath:
		sepIdx := slices.Index(comps, PathTargetSeparator)
		if sepIdx < 2 {
			return leafRecord{}, fmt.Errorf("path target missing %s", PathTargetSeparator)
		}
		target = Target{Type: TargetPath, Value: decodePathTargetSegments(comps[1:sepIdx])}
		keyStart = sepIdx + 1
	default: // commit, branch, change-id: <type>/<shard>/<value>/<key...>
		if len(comps) < 4 {
			return leafRecord{}, fmt.Errorf("target path too short")
		}
		target = Target{Type: tt, Value: comps[2]}
		keyStart = 3
	}

	kc := comps[keyStart:]
	if len(kc) == 0 {
		return leafRecord{}, fmt.Errorf("no key components")
	}

	// Whole-key tombstone: __tombstones/<key segs>/__deleted right after base.
	if kc[0] == TombstoneRoot {
		if len(kc) < 3 || kc[len(kc)-1] != TombstoneBlob {
			return leafRecord{}, fmt.Errorf("malformed key tombstone")
		}
		return leafRecord{target: target, key: decodeKey(kc[1 : len(kc)-1]), kind: kindKeyTombstone}, nil
	}

	// Find the first reserved structural component within the key path.
	resIdx := -1
	for i, c := range kc {
		if strings.HasPrefix(c, "__") {
			resIdx = i
			break
		}
	}
	if resIdx <= 0 {
		return leafRecord{}, fmt.Errorf("no structural marker in key path %v", kc)
	}
	key := decodeKey(kc[:resIdx])
	marker := kc[resIdx]
	rest := kc[resIdx+1:]

	switch marker {
	case StringValueBlob:
		return leafRecord{target: target, key: key, kind: kindString}, nil
	case ListValueDir:
		if len(rest) >= 2 && rest[0] == TombstoneRoot {
			return leafRecord{target: target, key: key, kind: kindListEntryTombstone, entry: rest[1]}, nil
		}
		if len(rest) >= 1 {
			return leafRecord{target: target, key: key, kind: kindListEntry, entry: rest[0]}, nil
		}
	case SetValueDir:
		if len(rest) >= 1 {
			return leafRecord{target: target, key: key, kind: kindSetMember, member: rest[0]}, nil
		}
	case TombstoneRoot:
		// Set-member tombstone: <key>/__tombstones/<member>.
		if len(rest) >= 1 {
			return leafRecord{target: target, key: key, kind: kindSetMemberTombstone, member: rest[0]}, nil
		}
	}
	return leafRecord{}, fmt.Errorf("unrecognized structure %q in %q", marker, path)
}

func decodeKey(segs []string) string { return strings.Join(segs, ":") }
