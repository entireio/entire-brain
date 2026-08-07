package factgitmeta

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v6/osfs"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

// lockFileName is the advisory lockfile under the bare git dir that serializes
// Advance across processes. It is not a git object/ref, so go-git ignores it.
const lockFileName = "factgitmeta.lock"

// localRepo is a bare, on-disk go-git repository used as the local git-meta
// object + ref store. Its *filesystem.Storage implements BOTH
// storer.EncodedObjectStorer (the git-meta Serialize/BuildCommit target) and
// storer.ReferenceStorer (ref compare-and-swap via CheckAndSetReference), so the
// whole read-merge-CAS loop runs against a plain directory with no network,
// entiredb, or Postgres. This is the deliberate local substitute for
// git-meta-service's gitclient (memory.Storage + smart-HTTP push): same engine,
// no transport.
type localRepo struct {
	store  *filesystem.Storage
	gitDir string // absolute path to the bare repo — backs the advisory lockfile
}

// openLocalRepo opens the bare repository at gitDir, laying down the standard
// git layout (objects/, refs/, HEAD, config) on first use. A missing directory
// is created. Re-opening an already-initialized dir is not an error — that is
// the steady state after the first sync.
func openLocalRepo(gitDir string) (*localRepo, error) {
	if gitDir == "" {
		return nil, errors.New("factgitmeta: git dir is empty")
	}
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		return nil, fmt.Errorf("factgitmeta: create git dir %q: %w", gitDir, err)
	}
	// A nil cache is valid (see git-meta-service client_test) but re-reads every
	// object from disk; a small LRU keeps the tree/commit reads in the CAS loop
	// cheap.
	store := filesystem.NewStorage(osfs.New(gitDir), cache.NewObjectLRUDefault())
	// Init writes HEAD/config and the refs/objects dirs. ErrTargetDirNotEmpty
	// means the repo is already initialized (a re-open), which is expected and
	// not an error; any other failure is real.
	if _, err := git.Init(store); err != nil && !errors.Is(err, git.ErrTargetDirNotEmpty) {
		return nil, fmt.Errorf("factgitmeta: init bare repo %q: %w", gitDir, err)
	}
	return &localRepo{store: store, gitDir: gitDir}, nil
}

// lock takes a cross-process exclusive advisory lock on a lockfile in the
// git dir and returns an unlock func the caller must defer. It serializes the
// whole Advance read-CAS loop across processes: go-git performs the create-path
// ref write (CheckAndSetReference with a nil old ref) UNCONDITIONALLY — it skips
// the absence check when old==nil — so without this, two concurrent first-syncs
// would both "win" and silently drop one member's facts. The update path is
// already ref-CAS-safe under go-git's own flock; this also closes its benign
// read→write TOCTOU. The OS lock is released automatically if the process dies,
// so a crash never leaves a stale lock.
func (r *localRepo) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(r.gitDir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("factgitmeta: open lockfile: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("factgitmeta: acquire lock: %w", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
