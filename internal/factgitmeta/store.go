package factgitmeta

import (
	"errors"
	"fmt"
	"time"

	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	gitstorage "github.com/go-git/go-git/v6/storage"
)

// ErrNoUpdate is returned by a MetaStore.Update mutate func that decided there
// is nothing to write. Update then leaves refs/meta/local/main untouched and
// reports the unchanged tip, so a no-op incremental pass costs no commit.
var ErrNoUpdate = errors.New("factgitmeta: no update")

// ErrLockBusy is returned by TryUpdate/TryLock when another writer holds the
// store's advisory lock. It is a normal outcome for a best-effort pass, not a
// failure: the caller skips this round and tries again on the next one.
var ErrLockBusy = errors.New("factgitmeta: git-meta store is locked by another writer")

// MetaStore is the general-purpose local git-meta record store: the same bare
// repository, advisory lock, ref (metaRef) and read-modify-CAS discipline the
// fact-set Backend uses, exposed for other record namespaces (the entity ->
// checkpoint index) so every local git-meta writer serializes against the SAME
// lockfile and compare-and-swaps the SAME ref. A second, independent opener
// would give the create path (go-git writes a nil-old-ref unconditionally) two
// unsynchronized writers and silently drop one namespace's first commit.
//
// Key namespacing is the caller's responsibility; Backend already reserves
// "brain:facts:".
//
// NOTE — deliberate duplication. Backend.Advance still runs its OWN copy of the
// lock + read-materialize-CAS-retry loop (see backend.go); it was not
// refactored onto MetaStore. What is genuinely shared is the lockfile and the
// ref, which is what makes the two writers safe against each other; the loop
// body is duplicated. Collapsing Backend onto MetaStore is a follow-up.
//
// Because the two loops are separate, they must never NEST: both acquire the
// same advisory lock through a FRESH descriptor, and flock(2) is held per open
// file description, so a MetaStore.Update called from inside a Backend.Advance
// (or vice versa) would deadlock against itself in-process. Every caller today
// takes exactly one of them at a time.
type MetaStore struct {
	repo *localRepo
	now  func() time.Time
}

// OpenMetaStore opens (initializing if needed) the bare git-meta repository at
// gitDir. now is injected for deterministic commit timestamps; nil defaults to
// time.Now.
func OpenMetaStore(gitDir string, now func() time.Time) (*MetaStore, error) {
	if now == nil {
		now = time.Now
	}
	repo, err := openLocalRepo(gitDir)
	if err != nil {
		return nil, err
	}
	return &MetaStore{repo: repo, now: now}, nil
}

// Ref is the git ref the local git-meta state lives on. Exposed so callers can
// name it in diagnostics.
func (s *MetaStore) Ref() string { return metaRef }

// Tip returns the current metaRef commit hash in hex, or "" when the ref does
// not exist yet. It is the cheap cache-invalidation token for any derived,
// rebuildable index built out of this store: the tip changes on every write.
func (s *MetaStore) Tip() (string, error) {
	ref, err := s.repo.store.Reference(plumbing.ReferenceName(metaRef))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("factgitmeta: read %s: %w", metaRef, err)
	}
	return ref.Hash().String(), nil
}

// State materializes the full current git-meta state. A missing ref yields an
// empty State, never an error, so a first read merges into nothing.
func (s *MetaStore) State() (gitmeta.State, error) {
	st, _, err := s.state()
	return st, err
}

// ReadString reads ONE string record straight out of the git-meta tree, without
// materializing the whole state. Materialize is O(every record in the store);
// a caller that only needs its own cursor (the entity index's per-branch
// window) must not pay that on every poll, which is what turned a bounded
// freshness tick into a full-index read per tick.
//
// It sees string values only: a key holding a list or a set, or one that was
// tombstoned, reports absent. That is exactly right for the single-typed
// cursor keys this exists for.
func (s *MetaStore) ReadString(target gitmeta.Target, key string) (string, bool, error) {
	path, err := gitmeta.TreePath(target, key)
	if err != nil {
		return "", false, fmt.Errorf("factgitmeta: tree path for %s: %w", key, err)
	}
	ref, err := s.repo.store.Reference(plumbing.ReferenceName(metaRef))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("factgitmeta: read %s: %w", metaRef, err)
	}
	commit, err := object.GetCommit(s.repo.store, ref.Hash())
	if err != nil {
		return "", false, fmt.Errorf("factgitmeta: read commit %s: %w", ref.Hash(), err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false, fmt.Errorf("factgitmeta: read tree %s: %w", commit.TreeHash, err)
	}
	file, err := tree.File(path)
	if errors.Is(err, object.ErrFileNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("factgitmeta: read %s: %w", path, err)
	}
	contents, err := file.Contents()
	if err != nil {
		return "", false, fmt.Errorf("factgitmeta: read %s: %w", path, err)
	}
	return contents, true, nil
}

// HasString reports whether a string record exists, without materializing the
// whole state. See ReadString for the scope of "exists".
func (s *MetaStore) HasString(target gitmeta.Target, key string) (bool, error) {
	_, ok, err := s.ReadString(target, key)
	return ok, err
}

// TryLock takes the store's cross-process advisory lock WITHOUT blocking and
// returns the release func. ErrLockBusy means another writer holds it; that is
// a normal outcome for a best-effort caller, not a failure.
func (s *MetaStore) TryLock() (func(), error) {
	unlock, ok, err := s.repo.tryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLockBusy
	}
	return unlock, nil
}

// Lock takes the store's cross-process advisory lock, WAITING for it, and
// returns the release func. It exists for a caller whose pass has to hold the
// lock across work it does BEFORE the mutation — the entity index's builder
// runs one `entire graph diff` subprocess per new commit, and taking the lock
// only at the write would let a whole contended pass do that work and then
// throw it away. Such a caller pairs it with UpdateLocked.
//
// It WAITS, and flock(2) cannot be interrupted by a Go context: a caller under
// a deadline uses TryLock instead.
func (s *MetaStore) Lock() (func(), error) {
	return s.repo.lock()
}

// UpdateLocked is Update for a caller that ALREADY holds the store lock via
// Lock or TryLock. It runs the same read-materialize-CAS-retry loop and must
// never be called without the lock held; calling Update instead would deadlock
// against the caller's own lock (see the type comment).
func (s *MetaStore) UpdateLocked(records int, mutate func(gitmeta.State) (gitmeta.State, error)) (string, error) {
	if mutate == nil {
		return "", errors.New("factgitmeta: mutate is required")
	}
	return s.updateLocked(records, mutate)
}

// Update applies mutate to the freshly-materialized state under the
// cross-process advisory lock and commits the result onto metaRef with a
// ref-level compare-and-swap retry. mutate may be called more than once (once
// per retry) and MUST be a pure function of the state it is handed. Returning
// ErrNoUpdate leaves the ref untouched. records is the change count stamped in
// the git-meta commit message.
//
// It WAITS for the lock. A caller that must not wait (a background freshness
// pass under a deadline) uses TryUpdate instead: flock(2) is not interruptible
// by a Go context, so a contended Update outlives any timeout around it.
func (s *MetaStore) Update(records int, mutate func(gitmeta.State) (gitmeta.State, error)) (string, error) {
	if mutate == nil {
		return "", errors.New("factgitmeta: mutate is required")
	}
	unlock, err := s.repo.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	return s.updateLocked(records, mutate)
}

// TryUpdate is Update with a non-blocking lock acquisition: it returns
// ErrLockBusy immediately when another writer holds the lock, writing nothing.
func (s *MetaStore) TryUpdate(records int, mutate func(gitmeta.State) (gitmeta.State, error)) (string, error) {
	if mutate == nil {
		return "", errors.New("factgitmeta: mutate is required")
	}
	unlock, err := s.TryLock()
	if err != nil {
		return "", err
	}
	defer unlock()
	return s.updateLocked(records, mutate)
}

// updateLocked is the read-materialize-CAS-retry loop both Update and TryUpdate
// run. The caller must already hold the advisory lock.
func (s *MetaStore) updateLocked(records int, mutate func(gitmeta.State) (gitmeta.State, error)) (string, error) {
	for attempt := 0; attempt < maxRefCASRetries; attempt++ {
		st, headRefObj, err := s.state()
		if err != nil {
			return "", err
		}
		next, err := mutate(st)
		if errors.Is(err, ErrNoUpdate) {
			if headRefObj == nil {
				return "", nil
			}
			return headRefObj.Hash().String(), nil
		}
		if err != nil {
			return "", err
		}
		treeHash, err := gitmeta.Serialize(next, s.repo.store)
		if err != nil {
			return "", fmt.Errorf("factgitmeta: serialize: %w", err)
		}
		info := gitmeta.CommitInfo{
			AuthorName:  "entire-brain",
			AuthorEmail: "brain@entire.local",
			When:        s.now().UTC(),
			Message:     gitmeta.SerializeMessage(records),
		}
		if headRefObj != nil {
			info.Parents = []plumbing.Hash{headRefObj.Hash()}
		}
		commitHash, err := gitmeta.BuildCommit(treeHash, info, s.repo.store)
		if err != nil {
			return "", fmt.Errorf("factgitmeta: build commit: %w", err)
		}
		newRefObj := plumbing.NewHashReference(plumbing.ReferenceName(metaRef), commitHash)
		switch err := s.repo.store.CheckAndSetReference(newRefObj, headRefObj); {
		case err == nil:
			return commitHash.String(), nil
		case errors.Is(err, gitstorage.ErrReferenceHasChanged):
			continue // the ref advanced between our read and write; re-read and retry
		default:
			return "", fmt.Errorf("factgitmeta: set %s: %w", metaRef, err)
		}
	}
	return "", fmt.Errorf("factgitmeta: %s contended after %d attempts", metaRef, maxRefCASRetries)
}

// state fetches the current State from metaRef along with the ref it was read
// from (nil when the ref does not exist yet).
func (s *MetaStore) state() (gitmeta.State, *plumbing.Reference, error) {
	ref, err := s.repo.store.Reference(plumbing.ReferenceName(metaRef))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return gitmeta.State{}, nil, nil
	}
	if err != nil {
		return gitmeta.State{}, nil, fmt.Errorf("factgitmeta: read %s: %w", metaRef, err)
	}
	commit, err := object.GetCommit(s.repo.store, ref.Hash())
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
