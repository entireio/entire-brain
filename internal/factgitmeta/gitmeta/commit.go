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
	"fmt"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// CommitInfo carries the identity and message for a serialized metadata commit.
type CommitInfo struct {
	AuthorName  string
	AuthorEmail string
	When        time.Time
	Message     string
	Parents     []plumbing.Hash // current refs/meta/main head, if any
}

// BuildCommit writes a commit object pointing at treeHash and returns its hash.
// The same identity is used for author and committer (metadata history has no
// separate committer concept).
func BuildCommit(treeHash plumbing.Hash, info CommitInfo, store storer.EncodedObjectStorer) (plumbing.Hash, error) {
	sig := object.Signature{Name: info.AuthorName, Email: info.AuthorEmail, When: info.When}
	c := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      info.Message,
		TreeHash:     treeHash,
		ParentHashes: info.Parents,
	}
	obj := store.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode commit: %w", err)
	}
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store commit: %w", err)
	}
	return h, nil
}

// SerializeMessage formats a serialize commit message in the git-meta convention
// ("git-meta: serialize (N changes)"). The detailed change lines are optional
// and omitted here; consumers fall back to tree diffing.
func SerializeMessage(changes int) string {
	return fmt.Sprintf("git-meta: serialize (%d changes)", changes)
}
