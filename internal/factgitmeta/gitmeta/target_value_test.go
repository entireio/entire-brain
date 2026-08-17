// Vendored git-meta exchange engine. Originally copied verbatim from
// github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153) — do not edit here.
//
// PROVENANCE HAS MOVED: git-meta-service was a proof of concept, and the shipping
// implementation now lives in github.com/entirehq/entire-api internal/gitmeta. Re-vendor
// from THERE, not from the PoC repo, whose PR was superseded rather than merged.
//
// The two copies have since diverged in both directions — entire-api grew path-target
// support (PathTargetSubtree, ValidatePathTargetValue) and typed accessors, while this
// copy carries helpers of its own and the older ValidateTargetValue name. That is inert
// today because this engine only ever drives the BARE LOCAL store at
// refs/meta/local/main: no remote, no push or fetch, and no ref that entire-api also
// writes, so no record crosses between the two implementations. It stops being inert the
// moment brain exchanges git-meta records with entire-api over a shared ref, which is
// what makes deduplicating this the right follow-up: both are internal packages, so
// neither can import the other and a real fix means extracting the engine into a shared
// module.

package gitmeta

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// H1 regression: a branch/change-id target value containing '/' silently
// corrupts the serialize<->materialize round-trip (value "feature/foo" + key "k"
// comes back as target "feature", key "foo:k") and can collide in the read-model
// PK. We chose REJECT: such values must be refused at the API boundary
// (ParseTarget) and can never reach the tree (Serialize). This whole test FAILS
// on the pre-fix code, where ParseTarget/Serialize accept the slash and the
// round-trip mis-parses.
func TestBranchChangeIDTargetValueRejectsSlash(t *testing.T) {
	// (1) API boundary: ParseTarget rejects slash-bearing branch/change-id values.
	for _, s := range []string{"branch:feature/foo", "branch:release/1.2", "change-id:abc/def"} {
		if _, err := ParseTarget(s); err == nil {
			t.Errorf("ParseTarget(%q) = nil err; want rejection of '/' in target value", s)
		}
	}
	// Dash/dotted branch values (no '/') still parse — we only reject '/'/'.'/NUL.
	if _, err := ParseTarget("branch:feature-x"); err != nil {
		t.Errorf("ParseTarget(branch:feature-x) unexpected err: %v", err)
	}
	// Path targets legitimately contain '/': must NOT be rejected.
	if _, err := ParseTarget("path:src/main.rs"); err != nil {
		t.Errorf("ParseTarget(path:src/main.rs) rejected a legal path value: %v", err)
	}

	// (2) Serialize boundary (defense in depth): a directly-constructed branch
	// target with a '/' value cannot be serialized into a corrupting tree.
	badState := State{Strings: []StringVal{{
		Target: Target{Type: TargetBranch, Value: "feature/foo"}, Key: "k", Value: "v",
	}}}
	if _, err := Serialize(badState, memory.NewStorage()); err == nil {
		t.Fatal("Serialize accepted a slash-bearing branch target value; want error")
	}
}

// H1 PK-collision guard: without the fix, `branch:feature/foo`+key "k" and
// `branch:feature`+key "foo:k" both serialize under .../feature/foo/k/ and
// materialize to the IDENTICAL record (target "feature", key "foo:k"), a
// read-model primary-key collision that fails the whole COPY transaction. With
// the fix, the slash-bearing entry can never be serialized, so the collision is
// impossible. Pre-fix, Serialize succeeds and the two records collapse into one.
func TestBranchSlashValuePKCollisionPrevented(t *testing.T) {
	colliding := State{Strings: []StringVal{
		{Target: Target{Type: TargetBranch, Value: "feature/foo"}, Key: "k", Value: "v1"},
		{Target: Target{Type: TargetBranch, Value: "feature"}, Key: "foo:k", Value: "v2"},
	}}
	if _, err := Serialize(colliding, memory.NewStorage()); err == nil {
		t.Fatal("Serialize accepted a state that would collide two records onto one read-model PK")
	}

	// Sanity: the legitimate `branch:feature`+`foo:k` record on its own round-trips
	// cleanly (proving the rejection targets only the corrupting slash value).
	ok := mustTarget(t, "branch:feature")
	good := State{}.Apply(Mutation{Op: OpSetString, Target: ok, Key: "foo:k", Value: "v2"})
	store := memory.NewStorage()
	h, err := Serialize(good, store)
	if err != nil {
		t.Fatalf("Serialize(branch:feature foo:k): %v", err)
	}
	tree, err := object.GetTree(store, h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Materialize(tree)
	if err != nil {
		t.Fatal(err)
	}
	if v := findString(got, "branch:feature", "foo:k"); v != "v2" {
		t.Fatalf("branch:feature foo:k round-trip = %q; want v2", v)
	}
	if !strings.HasPrefix(TreeBasePath(ok), "branch/") {
		t.Fatalf("unexpected base path %q", TreeBasePath(ok))
	}
}
