// Copied verbatim from github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153); do not edit; de-internalize
// upstream to dedupe (follow-up).

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
