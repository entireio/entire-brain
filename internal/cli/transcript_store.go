package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// transcript_store.go is COR-2128's single direct-transcript read path.
// Transcript content lives in git checkpoint refs — immutable, already
// stored — so the manifest's transcript paths are VIRTUAL identifiers
// resolved to checkpoint blob reads on demand rather than files duplicated
// under the brain dir.
//
// A materialized file still wins when present: exported bundles (--output)
// are standalone artifacts that carry their transcripts as files, and brains
// written before this change keep working until their cache is swept.
//
// Every direct read goes through the store, because the store is where two
// invariants are enforced:
//
//   - A purged session stays purged. The tombstone gate used to be implicit —
//     the purge deleted the cached file — and with on-demand git reads the
//     gate must live on the read path itself.
//   - Reads never touch the network. Resolution and blob reads run through the
//     same no-egress policy the snapshot export uses; a promisor-absent blob
//     is a read error the caller skips with a warning, never a fetch.

// transcriptLocation addresses one session's transcript inside the snapshot.
// sessionID rides along so the tombstone gate can refuse by SESSION as well as
// by path: the path-keyed gate maps the CURRENT manifest, while byRel can hold
// paths from the manifest as of snapshot resolution — a session re-keyed and
// then purged must not slip through under its old path.
type transcriptLocation struct {
	sessionID  string
	sourceKey  string
	sourcePath string
}

// transcriptStore resolves in two lazy stages: the tombstone gate loads on
// every read (cheap, and the gate must precede every read), while the
// checkpoint snapshot resolves only when a read actually needs git — a bundle
// with materialized files never pays for checkpoint discovery. The snapshot
// scan is amortized by the persisted checkpoint metadata cache (kept warm by
// export); the store loads that cache read-only and never writes it.
type transcriptStore struct {
	runner   CommandRunner
	brainDir string
	// repoDir overrides a legacy manifest worktree during cache migration.
	repoDir string

	// snapMu guards the memoized snapshot. The store outlives its first caller
	// (a long-lived watch loop shares it for the process), so memoization must
	// not freeze the first attempt's outcome:
	//
	//   - a failure caused by the caller's context is never recorded;
	//   - a recorded failure retries once the manifest changes or after a
	//     backoff, so a transient git failure heals;
	//   - a lookup miss re-resolves when the manifest changed since the last
	//     attempt, so sessions exported after the first read become readable.
	snapMu            sync.Mutex
	storageStamp      transcriptManifestStamp
	storageKnown      bool
	checkpointStorage bool
	snapErr           error
	resolved          bool
	lastAttempt       time.Time
	stamp             transcriptManifestStamp
	snapshot          *checkpointSnapshot
	byRel             map[string]transcriptLocation

	// resolve is a test seam; production stores resolve via resolveSnapshot.
	resolve func(context.Context) error
}

// transcriptManifestStamp is the manifest file identity at the last resolve
// attempt; a differing stamp means the brain re-exported since.
type transcriptManifestStamp struct {
	modTime time.Time
	size    int64
}

// transcriptResolveRetryInterval bounds how often a FAILED resolve re-runs
// when the manifest has not changed, so a persistently broken repo costs one
// scan per interval instead of one per read, while a transient failure heals.
const transcriptResolveRetryInterval = time.Minute

func (s *transcriptStore) currentManifestStamp() transcriptManifestStamp {
	info, err := os.Stat(filepath.Join(s.brainDir, bmanifest.FileName))
	if err != nil {
		return transcriptManifestStamp{}
	}
	return transcriptManifestStamp{modTime: info.ModTime(), size: info.Size()}
}

// newTranscriptStore builds a store for one brain. The repository the brain
// belongs to resolves lazily from the manifest's recorded repo root; a
// standalone exported bundle has materialized files and never needs it.
func newTranscriptStore(runner CommandRunner, brainDir string) *transcriptStore {
	return &transcriptStore{runner: runner, brainDir: brainDir}
}

// transcriptStores shares resolution for legacy direct-read helpers. Stores
// revalidate manifest identity and privacy policy across calls, including
// calls made by long-lived processes.
var transcriptStores sync.Map

func transcriptStoreFor(brainDir string) *transcriptStore {
	key := filepath.Clean(brainDir)
	if store, ok := transcriptStores.Load(key); ok {
		return store.(*transcriptStore)
	}
	store, _ := transcriptStores.LoadOrStore(key, newTranscriptStore(ExecRunner{}, brainDir))
	return store.(*transcriptStore)
}

// storageMode caches the manifest's storage discriminator, invalidating it on
// every replacement. Reading the full manifest per transcript would make a
// corpus pass quadratic in the number of sessions.
func (s *transcriptStore) storageMode() (bool, error) {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	stamp := s.currentManifestStamp()
	if s.storageKnown && stamp == s.storageStamp {
		return s.checkpointStorage, nil
	}
	manifest, err := bmanifest.Load(s.brainDir)
	if err != nil {
		return false, err
	}
	s.checkpointStorage = checkpointBackedSessions(manifest)
	s.storageStamp, s.storageKnown = stamp, true
	return s.checkpointStorage, nil
}

// guardNow loads the tombstone gate fresh on every read: a purge that
// tombstones a session mid-process must bind the very next read, so the gate
// is deliberately NOT memoized. The common empty-gate case costs one small
// file read; only tombstoned brains pay the manifest read that maps sessions
// to paths (the same two-step every guarded surface uses — see retrieve.go).
func (s *transcriptStore) guardNow() (sessionReadGuard, error) {
	guard, err := bmanifest.LoadSessionReadGuard(s.brainDir, nil)
	if err != nil {
		// Fail closed: an unreadable tombstone file must not open purged
		// transcripts back up.
		return sessionReadGuard{}, fmt.Errorf("transcript store: %w", err)
	}
	if guard.Empty() {
		return guard, nil
	}
	manifest, err := bmanifest.Load(s.brainDir)
	if err != nil {
		return sessionReadGuard{}, fmt.Errorf("transcript store: tombstones present but manifest unreadable: %w", err)
	}
	guard, err = bmanifest.LoadSessionReadGuard(s.brainDir, manifest)
	if err != nil {
		return sessionReadGuard{}, fmt.Errorf("transcript store: %w", err)
	}
	return guard, nil
}

// lookupLocation resolves one manifest path to its checkpoint location,
// re-resolving the snapshot when the memoized state cannot serve the request
// honestly (see snapMu). It returns the snapshot alongside the location so the
// caller reads from the same resolution the lookup used, race-free.
func (s *transcriptStore) lookupLocation(ctx context.Context, rel string) (transcriptLocation, *checkpointSnapshot, error) {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()

	needResolve := !s.resolved
	if s.resolved {
		if s.snapErr != nil {
			if s.currentManifestStamp() != s.stamp || time.Since(s.lastAttempt) > transcriptResolveRetryInterval {
				needResolve = true
			}
		} else if s.currentManifestStamp() != s.stamp {
			needResolve = true
		}
	}
	if needResolve {
		if err := ctx.Err(); err != nil {
			return transcriptLocation{}, nil, err
		}
		// Stamp BEFORE reading: a manifest written mid-resolve yields an old
		// stamp, so the next miss sees the change and re-resolves again.
		stamp := s.currentManifestStamp()
		resolve := s.resolve
		if resolve == nil {
			resolve = s.resolveSnapshot
		}
		err := resolve(ctx)
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return transcriptLocation{}, nil, err
		}
		s.resolved = true
		s.snapErr = err
		s.stamp = stamp
		s.lastAttempt = time.Now()
	}
	if s.snapErr != nil {
		return transcriptLocation{}, nil, s.snapErr
	}
	location, ok := s.byRel[rel]
	if !ok {
		return transcriptLocation{}, nil, fmt.Errorf("transcript %s has no readable checkpoint source: %w", rel, fs.ErrNotExist)
	}
	return location, s.snapshot, nil
}

func (s *transcriptStore) resolveSnapshot(ctx context.Context) error {
	manifest, err := bmanifest.Load(s.brainDir)
	if err != nil {
		return fmt.Errorf("transcript store: %w", err)
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return fmt.Errorf("transcript store: brain has no session source manifest: %w", fs.ErrNotExist)
	}
	repoDir := strings.TrimSpace(s.repoDir)
	if repoDir == "" {
		repoDir = strings.TrimSpace(manifest.RepoRoot)
	}
	if repoDir == "" {
		return fmt.Errorf("transcript store: brain manifest records no repo root to resolve checkpoints from: %w", fs.ErrNotExist)
	}
	cache := loadCheckpointMetadataCache(filepath.Join(s.brainDir, filepath.FromSlash(checkpointMetadataCachePath)))
	mode := strings.TrimSpace(manifest.Sources.Sessions.TranscriptMode)
	// Branch destinations only affect which BRANCH a session is filed under;
	// resolution here matches by (session, checkpoint, index) identity, so the
	// zero value serves, exactly as verify's use of the same helper.
	var snapshot *checkpointSnapshot
	if s.repoDir == "" && manifest.CheckpointGitDir != "" {
		snapshot, _, err = loadLocalCheckpointUnionSnapshot(ctx, s.runner, manifest.CheckpointGitDir, manifest.CheckpointPrimary, mode == "raw", 0, checkpointBranchDestinations{}, nil, cache, true)
	} else {
		snapshot, _, err = loadLocalCheckpointSnapshotLocalOnly(ctx, s.runner, repoDir, mode == "raw", checkpointBranchDestinations{}, cache)
	}
	if err != nil {
		return fmt.Errorf("transcript store: resolve checkpoint snapshot: %w", err)
	}
	s.snapshot = snapshot

	// Index EVERY (session, checkpoint) pair the snapshot read — not just each
	// session's newest checkpoint. The manifest records the checkpoint as of
	// the LAST EXPORT; a session that gained checkpoints since must still
	// resolve by the id the manifest wrote.
	type sessionIdentity struct {
		sessionID    string
		checkpointID string
		sessionIndex int
	}
	byIdentity := make(map[sessionIdentity]sessionCheckpoint, len(snapshot.SessionCheckpoints))
	for _, member := range snapshot.SessionCheckpoints {
		byIdentity[sessionIdentity{member.SessionID, member.CheckpointID, member.SessionIndex}] = member
	}

	s.byRel = make(map[string]transcriptLocation, len(manifest.Sources.Sessions.Sessions))
	for _, session := range manifest.Sources.Sessions.Sessions {
		rel := brainstore.NormalizePrivacyTranscriptPath(session.TranscriptPath)
		if rel == "" {
			continue
		}
		member, ok := byIdentity[sessionIdentity{session.SessionID, session.LatestCheckpoint, session.SessionIndex}]
		if !ok {
			// Checkpoint no longer resolvable locally: the session stays listed
			// but unreadable, and Read reports it when asked.
			continue
		}
		location, ok := memberTranscriptLocation(member, mode, snapshot.TranscriptFileName)
		if !ok {
			continue
		}
		location.sessionID = session.SessionID
		s.byRel[rel] = location
	}
	return nil
}

// memberTranscriptLocation picks which stored bytes a manifest session reads.
// The manifest's recorded mode decides: this brain's derived state (distill
// fingerprints, evidence hashes, line anchors) was built against that mode's
// bytes. The snapshot re-decides its own mode across CURRENT members, and
// following that re-decision would serve different bytes than the export
// materialized — so the choice is pinned per member, and a member without the
// recorded mode's pointer stays unreadable rather than silently switching.
func memberTranscriptLocation(member sessionCheckpoint, mode, snapshotFileName string) (transcriptLocation, bool) {
	sourcePath := member.TranscriptPath
	if mode == "compact" {
		sourcePath = member.CompactTranscriptPath
	} else if sourcePath == "" {
		sourcePath = snapshotTranscriptPath(member.CheckpointID, member.SessionIndex, snapshotFileName)
	}
	if sourcePath == "" {
		return transcriptLocation{}, false
	}
	return transcriptLocation{sourceKey: member.SourceKey, sourcePath: sourcePath}, true
}

// Read returns one session transcript's verbatim bytes by its manifest path.
// A materialized file wins when present; otherwise the content reads from the
// session's checkpoint blob. Both branches sit behind the tombstone gate.
func (s *transcriptStore) Read(ctx context.Context, rel string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reader, finish, err := s.Open(ctx, rel)
	if err != nil {
		return nil, err
	}
	data, readErr := base.SafeReadAll(contextCheckingReader{ctx: ctx, r: reader}, base.MaxDocumentTranscriptBytes, "canonical transcript "+rel)
	finishErr := finish()
	if readErr != nil || finishErr != nil {
		return nil, classifyTranscriptReadError(errors.Join(readErr, finishErr))
	}
	return data, nil
}

// Open supplies a streaming reader for bundle files and a bounded checkpoint
// reader otherwise. finish must run after consumption to validate file identity
// and the current privacy gate before publishing content.
func (s *transcriptStore) Open(ctx context.Context, rel string) (io.Reader, func() error, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("transcript store is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	clean, err := base.CleanBrainRelativePath(rel)
	if err != nil || filepath.ToSlash(clean) != rel || !strings.HasPrefix(rel, "sessions/") {
		return nil, nil, &historySessionInventoryError{Path: rel, Reason: "canonical transcript path is unsafe", Err: err}
	}
	guard, err := s.guardNow()
	if err != nil {
		return nil, nil, err
	}
	if _, purged := guard.Paths[rel]; purged {
		return nil, nil, fmt.Errorf("%s: transcript %s is tombstoned", brainstore.StateUnsafeCode, rel)
	}
	checkpointStorage, err := s.storageMode()
	if err != nil {
		return nil, nil, err
	}
	if !checkpointStorage {
		file, finish, fileErr := openCanonicalHistoryTranscript(ctx, s.brainDir, rel)
		if fileErr == nil {
			return file, func() error {
				finishErr := finish()
				guard, err := s.guardNow()
				if err == nil {
					if _, blocked := guard.Paths[rel]; blocked {
						err = fmt.Errorf("%s: transcript %s is tombstoned", brainstore.StateUnsafeCode, rel)
					}
				}
				return errors.Join(finishErr, err)
			}, nil
		}
		if !errors.Is(fileErr, fs.ErrNotExist) {
			return nil, nil, fileErr
		}
	}
	location, snapshot, err := s.lookupLocation(ctx, rel)
	if err != nil {
		return nil, nil, err
	}
	if guard.BlocksSession(location.sessionID) {
		return nil, nil, fmt.Errorf("%s: transcript %s is tombstoned", brainstore.StateUnsafeCode, rel)
	}
	data, err := readSnapshotTranscriptFromSource(ctx, s.runner, snapshot, location.sourceKey, location.sourcePath)
	if err != nil {
		return nil, nil, classifyTranscriptReadError(fmt.Errorf("read transcript %s: %w", rel, err))
	}
	return bytes.NewReader(data), func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		guard, err := s.guardNow()
		if err != nil {
			return err
		}
		_, blocked := guard.Paths[rel]
		if blocked || guard.BlocksSession(location.sessionID) {
			return fmt.Errorf("%s: transcript %s is tombstoned", brainstore.StateUnsafeCode, rel)
		}
		return nil
	}, nil
}

// classifyTranscriptReadError labels size refusals with the error code
// consumers degrade on (history treats brainstore.InputTooLargeCode as skippable),
// identically for the file and checkpoint-blob branches.
//
// It wraps errHistoryTranscriptTooLarge rather than formatting the code as a
// string, so the three readers of the transcript inventory can classify with
// errors.Is instead of matching text. This branch is the one that matters: a
// fileless, checkpoint-backed brain reads every transcript through the store,
// so a sentinel raised only on the on-disk path would classify nothing in the
// configuration the whole-index refusal was actually observed in. The sentinel
// carries the bare code as its message, so the wording here is unchanged and
// the substring check in history_delta.go still matches.
func classifyTranscriptReadError(err error) error {
	var bound *readBoundExceededError
	if !errors.As(err, &bound) {
		return err
	}
	// The size breach must be the ONLY fault, the same rule
	// oversizeIsTheSoleFault and oversizeMayBeAttributedToTheFile enforce on
	// the two local read paths.
	//
	// errors.As walks a join, so a bound breach ANYWHERE in the chain used to
	// label the whole error skippable. Read hands this
	// errors.Join(readErr, finishErr) and finish() validates file identity and
	// the current privacy gate -- refusals that must never be swallowed by a
	// per-file skip, because every inventory reader skips on
	// errors.Is(errHistoryTranscriptTooLarge).
	if !everyFaultIsABoundBreach(err) {
		return err
	}
	return fmt.Errorf("%w: %w", errHistoryTranscriptTooLarge, err)
}

// everyFaultIsABoundBreach reports whether every distinct fault in err is a
// read-bound breach, walking both unwrap forms: errors.Join's Unwrap() []error
// for several faults, and a single Unwrap() error for one fault carrying
// context. A join with no leaves answers false rather than vacuously true.
func everyFaultIsABoundBreach(err error) bool {
	switch unwrapper := err.(type) {
	case interface{ Unwrap() []error }:
		leaves := unwrapper.Unwrap()
		if len(leaves) == 0 {
			return false
		}
		for _, leaf := range leaves {
			if leaf != nil && !everyFaultIsABoundBreach(leaf) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if inner := unwrapper.Unwrap(); inner != nil {
			return everyFaultIsABoundBreach(inner)
		}
	}
	var bound *readBoundExceededError
	return errors.As(err, &bound)
}

// validateLocalTranscriptSessions checks the exact selected identities and every
// transcript chunk. Tree entries alone do not establish object availability in
// partial clones. Object type checks avoid reading transcript bytes or fetching
// missing promisor blobs; extra local sessions do not expand discovery scope.
// validateLocalTranscriptSessions keeps the original all-or-nothing contract
// for callers that want it: the first unavailable session is an error.
//
// It returns that diagnostic AS the error rather than re-wording it, so
// errors.Is still reaches errGitObjectMissing underneath. Formatting it into a
// fresh errors.New would leave the message intact and the identity gone --
// which is the same conflation the partition exists to remove, moved one layer
// up.
func validateLocalTranscriptSessions(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot, sessions []exportSession, mode string) error {
	_, unavailable, err := partitionLocalTranscriptSessions(ctx, runner, snapshot, sessions, mode)
	if err != nil {
		return err
	}
	if len(unavailable) > 0 {
		return unavailable[0]
	}
	return nil
}

// partitionLocalTranscriptSessions splits the selection into sessions whose
// transcripts this brain can actually serve and those it cannot, instead of
// failing on the first one it cannot.
//
// The guard's purpose is that a brain never RECORDS a session whose bytes it
// cannot serve, and skipping such a session satisfies that purpose exactly.
// Refusing the whole export is a separate choice, and on a repository using
// squash merges it is a permanent one: a manual-commit checkpoint names a
// commit on a feature branch, and deleting that branch destroys the commit, so
// the checkpoint references bytes that no longer exist anywhere. Measured on a
// real repository: 0 of 39 sampled commit-style checkpoint ids still resolved
// to a local object, out of 252 such checkpoints in its catalog. Fetching
// cannot recover them. Refusing therefore preserves nothing and additionally
// discards every session that IS available -- 2,561 found against 2,375
// published, frozen for ten days behind one unavailable session.
//
// Only two outcomes are reported as unavailability, and each of them only
// where it is definite: this snapshot records no transcript for the session at
// all, and git's own "object missing" answer for a chunk (errGitObjectMissing,
// which both probe paths in localObjectType wrap). Everything else is returned
// as an error and keeps the export fatal.
//
// NEITHER of those two is definite on its own, and the first one used to be
// treated as if it were. "No snapshot entry" is what a destroyed commit looks
// like AND what an unfetched checkpoint ref looks like; a tree listing that a
// partial clone could not complete produces the same shape. So both outcomes
// are gated on the same question -- can the sources this snapshot was built
// from still fetch what they do not hold? -- and report unavailability only
// when the answer is a definite no.
//
// That line matters more than it looks. The caller is fatal only when NOTHING
// is servable, so classifying a failure to RUN git -- a cancelled context, an
// expired deadline, a failed spawn, an unopenable repository -- as "the bytes
// are gone" would let a transient fault silently republish a shrunken corpus.
// That is strictly worse than the all-or-nothing refusal this replaced, which
// at least said so.
//
// The skipped sessions come back as errors rather than as formatted strings
// for the same reason: a diagnostic flattened with %v keeps its message and
// loses its identity, so a caller -- or the strict wrapper above -- can no
// longer tell a cancellation from a vanished object either. Every error this
// returns wraps the cause it had.
// errTranscriptNotInSnapshot is the cause behind "this snapshot names no
// transcript for that session", carried by both the per-session diagnostic and
// the refusal so a caller can tell this absence from a probed object miss
// without matching on message text.
var errTranscriptNotInSnapshot = errors.New("transcript not in local checkpoint snapshot")

// snapshotSourcesMayLazyFetch reports whether ANY source this snapshot was
// built from could still obtain objects it does not currently hold. A
// snapshot's sources are the local checkpoint refs it scanned, so one
// fetchable source is enough to make "not in the snapshot" an unproven
// absence rather than a destroyed one.
//
// It reuses gitDirHasPromisorRemote rather than detecting partial clones a
// second way: that helper already distinguishes git's definitive exit 1 from a
// probe that could not be answered, caches only the definitive answers, and
// matches the promisor key by suffix across every remote, including the
// URL-named remote the cli checkpoint repository actually uses. Keys are
// visited in sorted order so the git calls a test observes are deterministic.
func snapshotSourcesMayLazyFetch(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot) bool {
	if snapshot == nil {
		return false
	}
	dirs := make([]string, 0, len(snapshot.Sources))
	for key := range snapshot.Sources {
		if dir := snapshot.Sources[key].GitDir; dir != "" {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	for _, dir := range slices.Compact(dirs) {
		if gitDirHasPromisorRemote(ctx, runner, dir) {
			return true
		}
	}
	return false
}

func partitionLocalTranscriptSessions(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot, sessions []exportSession, mode string) ([]exportSession, []error, error) {
	type identity struct {
		session, checkpoint string
		index               int
	}
	available := make(map[identity]transcriptLocation, len(snapshot.SessionCheckpoints))
	for _, member := range snapshot.SessionCheckpoints {
		location, ok := memberTranscriptLocation(member, mode, snapshot.TranscriptFileName)
		if ok {
			available[identity{member.SessionID, member.CheckpointID, member.SessionIndex}] = location
		}
	}
	type objectKey struct{ gitDir, ref, path string }
	checked := make(map[objectKey]bool)
	availableSessions := make([]exportSession, 0, len(sessions))
	var unavailable []error
	// Probed at most once, and only if a session actually turns up missing
	// from the snapshot, so the common path costs no extra git call. The
	// probe's own answer is asymmetric by design (see gitDirHasPromisorRemote:
	// an unanswered probe reports "may fetch" and is not cached), which is
	// exactly the direction this guard needs.
	lazyFetchKnown, lazyFetch := false, false
	mayLazyFetch := func() bool {
		if !lazyFetchKnown {
			lazyFetchKnown = true
			lazyFetch = snapshotSourcesMayLazyFetch(ctx, runner, snapshot)
		}
		return lazyFetch
	}
	for _, session := range sessions {
		usable := true
		location, ok := available[identity{session.SessionID, session.LatestCheckpoint, session.SessionIndex}]
		source := snapshot.Sources[location.sourceKey]
		chunks := snapshotTranscriptChunks(location.sourcePath, source.TreePaths)
		if !ok || len(chunks) == 0 {
			// "This snapshot names no transcript for the session" is NOT proof
			// that the bytes were destroyed. It is equally what an unfetched
			// checkpoint ref looks like, so it needs the same gate the
			// object-missing answer below gets: when the sources this snapshot
			// was built from can still fetch what they do not hold, dropping
			// the session would republish a corpus smaller than the published
			// one over bytes a sync recovers. Refuse there and skip only where
			// the absence is definitive, which is #489's case.
			if mayLazyFetch() {
				return nil, nil, fmt.Errorf("persistent transcript storage: no local checkpoint snapshot entry for session %s checkpoint %s index %d, and this repository can still fetch objects it does not hold, so an unsynced checkpoint ref cannot be told apart from a destroyed one here; sync checkpoints and retry: %w", session.SessionID, session.LatestCheckpoint, session.SessionIndex, errTranscriptNotInSnapshot)
			}
			unavailable = append(unavailable, fmt.Errorf("persistent transcript storage requires local checkpoint refs for session %s checkpoint %s index %d; sync checkpoints and retry, or use --output for a standalone bundle: %w", session.SessionID, session.LatestCheckpoint, session.SessionIndex, errTranscriptNotInSnapshot))
			continue
		}
		for _, chunk := range chunks {
			actualPath, err := source.actualPath(chunk)
			if err != nil {
				// A malformed source path is a fault in this brain's own
				// snapshot, not a statement about one session's availability.
				return nil, nil, err
			}
			key := objectKey{source.GitDir, source.Ref, actualPath}
			if checked[key] {
				continue
			}
			kind, err := localObjectType(ctx, runner, source.GitDir, source.Ref+":"+actualPath)
			switch {
			case errors.Is(err, errGitObjectMissing) && gitDirHasPromisorRemote(ctx, runner, source.GitDir):
				// git answered "not here", but in a PARTIAL clone that answer
				// does not distinguish an unfetched blob from a destroyed one:
				// both exit 1 silently under the local-only policy. Dropping
				// the session would republish a corpus smaller than the one
				// already published, over bytes a sync would recover. So this
				// is an error, like any other thing we could not establish.
				//
				// It does not weaken what this partition exists for: a full
				// clone's confirmed-missing object still costs only its own
				// session, which is what #489 restored.
				//
				// What it does NOT establish is the claim this comment used to
				// make -- that a destroyed commit "never reaches this probe
				// because it has no snapshot entry, and is classified from
				// data above". Having no snapshot entry is not evidence of
				// destruction: an unsynced checkpoint ref has no entry either.
				// That reasoning was used to argue this gate cost nothing, and
				// it is why the no-entry case above went ungated. Both cases
				// now ask the same question.
				return nil, nil, fmt.Errorf("persistent transcript storage: transcript bytes for session %s checkpoint %s index %d chunk %s are not in this partial clone, and an unfetched object cannot be told apart from a deleted one here; sync checkpoints and retry: %w", session.SessionID, session.LatestCheckpoint, session.SessionIndex, chunk, err)
			case errors.Is(err, errGitObjectMissing):
				// git ANSWERED, and the answer is that these bytes are not in
				// this repository. That is the only probe outcome that costs
				// one session rather than the export.
				unavailable = append(unavailable, fmt.Errorf("persistent transcript storage: unreadable local transcript for session %s checkpoint %s index %d chunk %s; sync checkpoints and retry: %w", session.SessionID, session.LatestCheckpoint, session.SessionIndex, chunk, err))
				usable = false
			case err != nil:
				// Every other failure is a failure to ASK git, not an answer
				// from it: a cancelled context, an expired deadline, a spawn
				// that never happened, a repository that could not be opened.
				// Recording those as unavailability is what review caught --
				// the export is fatal only when nothing at all is servable, so
				// a transient git fault would silently republish a shrunken
				// corpus, which is worse than the refusal this partition
				// replaced.
				return nil, nil, fmt.Errorf("persistent transcript storage: cannot establish local availability for session %s checkpoint %s index %d chunk %s: %w", session.SessionID, session.LatestCheckpoint, session.SessionIndex, chunk, err)
			case kind != "blob":
				// Also an answer, and a definite one: the path this snapshot
				// recorded for a transcript is not a blob in the ref it
				// recorded it under. A fault in this brain's own snapshot, not
				// a statement about one session's availability.
				return nil, nil, fmt.Errorf("persistent transcript storage: snapshot path for session %s checkpoint %s index %d chunk %s is not a transcript blob: expected blob, got %q", session.SessionID, session.LatestCheckpoint, session.SessionIndex, chunk, kind)
			}
			if !usable {
				break
			}
			checked[key] = true
		}
		if usable {
			availableSessions = append(availableSessions, session)
		}
	}
	return availableSessions, unavailable, nil
}
