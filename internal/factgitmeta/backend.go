// Package factgitmeta is a fully-local implementation of the factsync.Server
// seam (internal/factsync): it stores a repo/branch fact-set head pointer and
// its NDJSON blob as git-meta project records inside a bare, on-disk git
// repository, and compare-and-swaps the head through the git-meta engine plus a
// filesystem ref CAS. It is the offline sibling of factsync.HTTPServer: identical
// content-addressed refs and CAS semantics (ErrConflict on a lost swap,
// ErrNoChange on an identical blob), but no entire-api, entiredb, Postgres, or
// mTLS — everything lives under a cache directory.
//
// Mapping (all records hang off the git-meta `project` target, repo-wide):
//
//	head  key = "brain:facts:" + hex(repoKey) + ":" + hex(branch) + ":head"
//	      val = "<version>|<contentRef>"        (moved with OpCompareAndSet)
//	blob  key = "brain:facts:" + hex(repoKey) + ":" + hex(branch) + ":blob"
//	      val = <full NDJSON fact-set plaintext> (OpSetString; a SINGLE per-branch
//	            key overwritten each advance — the live tree holds only the current
//	            blob, prior versions stay in git history, keeping live state O(1))
//
// repoKey and branch are hex-encoded into single key segments so any slug/branch
// is a valid git-meta key (git-meta rejects '/', '.', '..', NUL in key segments
// and target values) and tenants stay isolated.
package factgitmeta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
	"github.com/ashtom/entire-brain/internal/factsync"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	gitstorage "github.com/go-git/go-git/v6/storage"
)

// metaRef is the single git ref the local git-meta head lives on. The upstream
// `git meta` CLI serializes to refs/meta/<remote>/main (golden_test resolves
// refs/meta/local/main); we reuse that local ref name to mirror the reference
// layout while staying unmistakably local-only.
const metaRef = "refs/meta/local/main"

// factsKeyPrefix namespaces every brain fact record inside the shared project
// git-meta state, so facts never collide with other metadata on the same target.
const factsKeyPrefix = "brain:facts:"

// maxRefCASRetries bounds the ref-level compare-and-swap retry loop. A lost ref
// race (another writer advanced refs/meta between our read and write) is
// mechanical and retried; exhaustion is surfaced as ErrConflict for Sync to
// re-read and re-merge — never a silent drop.
const maxRefCASRetries = 5

// projectTarget is the git-meta target for all brain fact records: repo-wide,
// carries no value (so ValidateTargetValue never applies) and never collides
// with commit/branch/path metadata.
var projectTarget = gitmeta.Target{Type: gitmeta.TargetProject}

// Backend is a factsync.Server backed entirely by a local, bare git repository
// whose refs/meta/local/main history stores the fact-set head pointer and blob
// as git-meta project records. It implements the exact CAS semantics of the
// hosted HTTPServer / entire-api FactSetStore with no network dependency, so the
// factsync.Sync read-merge-CAS driver runs unchanged against it.
type Backend struct {
	repo    *localRepo
	repoKey string
	now     func() time.Time
}

// compile-time proof Backend satisfies the seam it is written against.
var _ factsync.Server = (*Backend)(nil)

// NewLocalBackend opens (initializing if needed) the bare git-meta repository at
// gitDir and returns a Server keyed for repoKey. repoKey is hex-encoded into the
// git-meta record keys so any repo slug is a valid key and tenants stay isolated
// even if two repos ever shared one git dir. now is injected for deterministic
// commit timestamps; nil defaults to time.Now.
//
// The Backend is a per-repo handle: it keys every record off repoKey, so the
// repoID argument that factsync.Sync threads through Current/Advance is
// redundant here and intentionally ignored (the CLI passes the same value for
// both).
func NewLocalBackend(gitDir, repoKey string, now func() time.Time) (*Backend, error) {
	if repoKey == "" {
		return nil, errors.New("factgitmeta: repoKey is required")
	}
	if now == nil {
		now = time.Now
	}
	repo, err := openLocalRepo(gitDir)
	if err != nil {
		return nil, err
	}
	return &Backend{repo: repo, repoKey: repoKey, now: now}, nil
}

// FactSetRef is the content ref for a fact-set plaintext: "facts-" +
// hex(sha256(plaintext)). It matches the hosted store's content-addressing
// (factsync fakeServer.contentRef / entire-api FactSetStore), so the SAME bytes
// yield the SAME ref across the local and hosted backends.
func FactSetRef(plaintext []byte) string {
	sum := sha256.Sum256(plaintext)
	return "facts-" + hex.EncodeToString(sum[:])
}

// Current reads the branch's fact-set head from the local git-meta state. When
// no head record exists yet it returns found=false and a nil blob (merge into
// empty), exactly like the hosted GET returning {found:false}.
func (b *Backend) Current(_ context.Context, _, branch string) (string, []byte, bool, error) {
	st, _, err := b.state()
	if err != nil {
		return "", nil, false, err
	}
	headVal, ok := st.CurrentString(projectTarget, b.headKey(branch))
	if !ok {
		return "", nil, false, nil
	}
	_, contentRef, err := parseHead(headVal)
	if err != nil {
		return "", nil, false, fmt.Errorf("factgitmeta: %w", err)
	}
	blob, ok := st.CurrentString(projectTarget, b.blobKey(branch))
	if !ok {
		return "", nil, false, fmt.Errorf("factgitmeta: head %q points at missing blob", headVal)
	}
	// Integrity: the single per-branch blob must be exactly the content the head
	// names. head + blob are written in one commit, so a mismatch is on-disk
	// corruption, never a normal state.
	if got := FactSetRef([]byte(blob)); got != contentRef {
		return "", nil, false, fmt.Errorf("factgitmeta: blob content ref %s != head %s (corrupt store)", got, contentRef)
	}
	return contentRef, []byte(blob), true, nil
}

// Advance writes the blob and compare-and-swaps the head onto plaintext's
// content ref. It short-circuits ErrNoChange when the new ref equals oldRef
// (checked first, before any store write — matching FactSetStore.Advance), and
// returns ErrConflict when the head the caller advanced from is no longer
// current (a concurrent member advanced first) or when ref contention exhausts
// the retry budget.
func (b *Backend) Advance(_ context.Context, _, branch, oldRef string, plaintext []byte) (string, error) {
	if len(plaintext) == 0 {
		// A head always points at content; the hosted store hard-rejects empty
		// data (400). Sync's empty-merge guard prevents reaching here, but reject
		// defensively so an empty blob can never be committed.
		return "", errors.New("factgitmeta: empty plaintext (a fact-set head always points at content)")
	}
	newRef := FactSetRef(plaintext)
	if newRef == oldRef {
		return "", factsync.ErrNoChange
	}

	// Serialize the whole read-CAS loop across processes. go-git's create-path ref
	// write is UNCONDITIONAL (CheckAndSetReference with a nil old ref skips the
	// absence check), so without this two concurrent first-syncs both succeed and
	// silently drop one member's facts. Released on return.
	unlock, err := b.repo.lock()
	if err != nil {
		return "", err
	}
	defer unlock()

	headKey := b.headKey(branch)
	blobKey := b.blobKey(branch)

	for attempt := 0; attempt < maxRefCASRetries; attempt++ {
		st, headRefObj, err := b.state()
		if err != nil {
			return "", err
		}

		// Value-level precondition (the fact-set CAS): the head the caller
		// advanced from must still be current. A lost value-CAS is TERMINAL
		// ErrConflict — the fact-set moved, so Sync must re-read and re-merge,
		// not blindly retry the same merged bytes.
		cur, mismatch := b.expectedHead(st, headKey, oldRef)
		if mismatch {
			return "", factsync.ErrConflict
		}

		// Apply both records onto the freshly-materialized state:
		//   blob — content-addressed key, immutable ⇒ a plain set.
		//   head — value-level compare-and-swap onto "<ver+1>|<newRef>". Apply's
		//          OpCompareAndSet is an unconditional set; the precondition was
		//          already enforced by expectedHead against this same state.
		newHeadVal := formatHead(cur.version+1, newRef)
		st = st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: blobKey, Value: string(plaintext)})
		st = st.Apply(gitmeta.Mutation{Op: gitmeta.OpCompareAndSet, Target: projectTarget, Key: headKey, Value: newHeadVal, Expected: cur.value})

		commitHash, err := b.commitState(st, headRefObj)
		if err != nil {
			return "", err
		}

		newRefObj := plumbing.NewHashReference(plumbing.ReferenceName(metaRef), commitHash)
		switch err := b.repo.store.CheckAndSetReference(newRefObj, headRefObj); {
		case err == nil:
			return newRef, nil
		case errors.Is(err, gitstorage.ErrReferenceHasChanged):
			continue // the ref advanced between our read and write — re-read and retry
		default:
			return "", fmt.Errorf("factgitmeta: set %s: %w", metaRef, err)
		}
	}
	// Sustained ref contention: report as a conflict so Sync re-reads and
	// re-merges, mirroring the hosted 412 path. Never a silent drop.
	return "", factsync.ErrConflict
}

// headState is the head record's stored value and parsed version at read time —
// the swap-from side of the value-level CAS.
type headState struct {
	value   string // exact "<version>|<contentRef>" currently stored, "" when absent
	version int    // current version, 0 when absent
}

// expectedHead validates the fact-set CAS precondition against the current state
// and returns the head the new commit must swap from. mismatch=true means the
// caller's oldRef no longer matches the stored head (a concurrent advance), which
// Advance maps to ErrConflict.
func (b *Backend) expectedHead(st gitmeta.State, headKey, oldRef string) (headState, bool) {
	curVal, ok := st.CurrentString(projectTarget, headKey)
	if oldRef == "" {
		// Create: the head must currently be absent. HasKey (not CurrentString)
		// so a non-string value ever written under the key also blocks the create
		// instead of being silently clobbered.
		if st.HasKey(projectTarget, headKey) {
			return headState{}, true
		}
		return headState{value: "", version: 0}, false
	}
	// Update: the head must exist and its contentRef must equal oldRef.
	if !ok {
		return headState{}, true
	}
	curVer, curRef, err := parseHead(curVal)
	if err != nil || curRef != oldRef {
		return headState{}, true
	}
	return headState{value: curVal, version: curVer}, false
}

// commitState serializes st into a new git-meta tree, writes a commit parented
// on the current head (when present), and returns the new commit hash. Every
// intermediate blob/tree/commit object is written into the local store so the
// subsequent CheckAndSetReference can point the ref at a fully-reachable commit.
func (b *Backend) commitState(st gitmeta.State, parent *plumbing.Reference) (plumbing.Hash, error) {
	treeHash, err := gitmeta.Serialize(st, b.repo.store)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("factgitmeta: serialize: %w", err)
	}
	info := gitmeta.CommitInfo{
		AuthorName:  "entire-brain",
		AuthorEmail: "brain@entire.local",
		When:        b.now().UTC(),
		Message:     gitmeta.SerializeMessage(2), // blob + head
	}
	if parent != nil {
		info.Parents = []plumbing.Hash{parent.Hash()}
	}
	commitHash, err := gitmeta.BuildCommit(treeHash, info, b.repo.store)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("factgitmeta: build commit: %w", err)
	}
	return commitHash, nil
}

// state fetches the current metadata State from metaRef along with the ref it
// was read from (nil when the ref does not exist yet — the create case). A
// missing ref yields an empty State, so a first sync merges into nothing.
func (b *Backend) state() (gitmeta.State, *plumbing.Reference, error) {
	ref, err := b.repo.store.Reference(plumbing.ReferenceName(metaRef))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return gitmeta.State{}, nil, nil
	}
	if err != nil {
		return gitmeta.State{}, nil, fmt.Errorf("factgitmeta: read %s: %w", metaRef, err)
	}
	commit, err := object.GetCommit(b.repo.store, ref.Hash())
	if err != nil {
		return gitmeta.State{}, nil, fmt.Errorf("factgitmeta: read commit %s: %w", ref.Hash(), err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return gitmeta.State{}, nil, fmt.Errorf("factgitmeta: read tree %s: %w", commit.TreeHash, err)
	}
	st, err := gitmeta.Materialize(tree)
	if err != nil {
		return gitmeta.State{}, nil, fmt.Errorf("factgitmeta: materialize %s: %w", metaRef, err)
	}
	return st, ref, nil
}

// headKey / blobKey build the two git-meta project keys for one (repo, branch).
// repoKey and branch are hex-encoded so any slug/branch is a single, valid
// git-meta key segment.
func (b *Backend) headKey(branch string) string {
	return factsKeyPrefix + hexSeg(b.repoKey) + ":" + hexSeg(branch) + ":head"
}

// blobKey is a SINGLE per-branch key, overwritten each Advance — the live tree
// holds only the current fact-set blob (prior versions remain in git history for
// audit). A per-contentRef key would accumulate every version in the live state,
// making Materialize/Serialize grow O(versions) per sync (cumulative O(n^2)).
func (b *Backend) blobKey(branch string) string {
	return factsKeyPrefix + hexSeg(b.repoKey) + ":" + hexSeg(branch) + ":blob"
}

func hexSeg(s string) string { return hex.EncodeToString([]byte(s)) }

// formatHead renders a head record value "<version>|<contentRef>".
func formatHead(version int, contentRef string) string {
	return strconv.Itoa(version) + "|" + contentRef
}

// parseHead splits a head record value "<version>|<contentRef>" into its parts.
func parseHead(headVal string) (version int, contentRef string, err error) {
	verStr, ref, ok := strings.Cut(headVal, "|")
	if !ok {
		return 0, "", fmt.Errorf("malformed head %q (want \"<version>|<contentRef>\")", headVal)
	}
	version, err = strconv.Atoi(verStr)
	if err != nil {
		return 0, "", fmt.Errorf("malformed head version %q: %w", verStr, err)
	}
	if ref == "" {
		return 0, "", fmt.Errorf("malformed head %q (empty contentRef)", headVal)
	}
	return version, ref, nil
}
