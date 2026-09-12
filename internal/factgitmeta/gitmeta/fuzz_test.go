package gitmeta

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// These harnesses assert the robustness invariant every record-encoding surface
// owes once the git-meta store is shared (P1): arbitrary target values, keys and
// tree layouts must produce an ERROR, never a panic, and never a silently
// corrupting round trip. Run one with:
//
//	go test ./internal/factgitmeta/gitmeta -run xxx -fuzz FuzzName

// FuzzTreePath drives the whole path-encoding surface with an arbitrary target
// type, target value and key. Every entry point validates before encoding, so no
// input may panic: a value the encoder cannot express must come back as an error.
func FuzzTreePath(f *testing.F) {
	f.Add("commit", "abc123", "brain:facts")
	f.Add("branch", "main", "brain:entities")
	f.Add("project", "", "brain:entity:6162")
	f.Add("path", "a/b/c", "k")
	f.Add("change-id", "I1", "k")
	f.Add("commit", "a", "k") // one-char shard candidate
	f.Add("commit", "", "k")  // empty value
	f.Add("branch", "é", "k")
	f.Add("commit", "ab\x00cd", "k")

	f.Fuzz(func(t *testing.T, typ, value, key string) {
		tt, err := ParseTargetType(typ)
		if err != nil {
			return
		}
		target := Target{Type: tt, Value: value}
		for _, fn := range []func() (string, error){
			func() (string, error) { return TreePath(target, key) },
			func() (string, error) { return ListDirPath(target, key) },
			func() (string, error) { return SetDirPath(target, key) },
			func() (string, error) { return TombstonePath(target, key) },
			func() (string, error) { return ListEntryTombstonePath(target, key, "1-abcde") },
			func() (string, error) { return SetMemberTombstonePath(target, key, "member") },
		} {
			path, err := fn()
			if err != nil {
				continue
			}
			// An accepted path must be usable as a tree path: no empty
			// components, or BuildTree rejects what the encoder produced.
			for _, comp := range strings.Split(path, "/") {
				if comp == "" {
					t.Fatalf("encoded path %q has an empty component (type=%q value=%q key=%q)", path, typ, value, key)
				}
			}
		}
	})
}

// FuzzParseTarget asserts the wire parser never panics and that anything it
// accepts survives String() -> ParseTarget unchanged.
func FuzzParseTarget(f *testing.F) {
	f.Add("commit:abc123")
	f.Add("project")
	f.Add("branch:main")
	f.Add("path:a/b")
	f.Add("commit:")
	f.Add(":::")
	f.Fuzz(func(t *testing.T, s string) {
		target, err := ParseTarget(s)
		if err != nil {
			return
		}
		again, err := ParseTarget(target.String())
		if err != nil {
			t.Fatalf("re-parsing an accepted target failed: %v (target=%#v)", err, target)
		}
		if again != target {
			t.Fatalf("target round trip changed the value: %#v -> %#v", target, again)
		}
	})
}

// FuzzMaterializeRoundTrip is the security-relevant property for a SHARED
// git-meta ref: a tree another member wrote is untrusted input. Materializing it
// and re-serializing the result — exactly what MetaStore.Update does on every
// write — must never panic, whatever leaf paths the tree carries.
func FuzzMaterializeRoundTrip(f *testing.F) {
	f.Add("commit/ab/abc123/brain:facts/__value", "v")
	f.Add("project/brain:entity:6162/__list/1-abcde", "sha")
	f.Add("branch/aa/main/k/__set/deadbeef", "m")
	f.Add("commit/ab/abc123/__tombstones/k/__deleted", "")
	f.Add("path/a/b/__target__/k/__value", "v")
	f.Add("commit/ab/a/k/__value", "v") // one-char target value
	f.Add("commit/ab//k/__value", "v")  // empty target value

	f.Fuzz(func(t *testing.T, path, content string) {
		store := memory.NewStorage()
		root, err := BuildTree([]blobFile{{path: path, content: content}}, store)
		if err != nil {
			return // the encoder refused to build it; nothing to materialize
		}
		tree, err := object.GetTree(store, root)
		if err != nil {
			t.Fatalf("reading back a tree we just wrote: %v", err)
		}
		state, err := Materialize(tree)
		if err != nil {
			return // a malformed tree is an error by contract
		}
		// The write half of the same loop MetaStore.updateLocked runs.
		out := memory.NewStorage()
		if _, err := Serialize(state, out); err != nil {
			return // refusing to re-encode is fine; panicking is not
		}
	})
}

// FuzzApplyMutation drives the state machine the sync path runs between
// materialize and serialize.
func FuzzApplyMutation(f *testing.F) {
	f.Add("set", "commit", "abc123", "k", "v", int64(0))
	f.Add("list:push", "project", "", "brain:entity:61", "sha", int64(1))
	f.Add("set:add", "branch", "main", "k", "m", int64(2))
	f.Add("rm", "commit", "abc", "k", "", int64(3))

	f.Fuzz(func(t *testing.T, op, typ, value, key, val string, nowMS int64) {
		tt, err := ParseTargetType(typ)
		if err != nil {
			return
		}
		state := State{}.Apply(Mutation{
			Op:     MutationOp(op),
			Target: Target{Type: tt, Value: value},
			Key:    key,
			Value:  val,
			NowMS:  nowMS,
		})
		store := memory.NewStorage()
		_, _ = Serialize(state, store)
	})
}
