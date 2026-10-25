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
	"strings"
	"sync"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
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
	// (a long-lived MCP server shares it for the process), so memoization must
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
	info, err := os.Stat(filepath.Join(s.brainDir, exportManifestFileName))
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
// calls made by long-lived MCP processes.
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
	manifest, err := loadBrainManifest(s.brainDir)
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
	guard, err := loadSessionReadGuard(s.brainDir, nil)
	if err != nil {
		// Fail closed: an unreadable tombstone file must not open purged
		// transcripts back up.
		return sessionReadGuard{}, fmt.Errorf("transcript store: %w", err)
	}
	if guard.empty() {
		return guard, nil
	}
	manifest, err := loadBrainManifest(s.brainDir)
	if err != nil {
		return sessionReadGuard{}, fmt.Errorf("transcript store: tombstones present but manifest unreadable: %w", err)
	}
	guard, err = loadSessionReadGuard(s.brainDir, manifest)
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
	manifest, err := loadBrainManifest(s.brainDir)
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
		rel := normalizePrivacyTranscriptPath(session.TranscriptPath)
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
	if _, purged := guard.paths[rel]; purged {
		return nil, nil, fmt.Errorf("%s: transcript %s is tombstoned", memoryErrStateUnsafe, rel)
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
					if _, blocked := guard.paths[rel]; blocked {
						err = fmt.Errorf("%s: transcript %s is tombstoned", memoryErrStateUnsafe, rel)
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
	if guard.blocksSession(location.sessionID) {
		return nil, nil, fmt.Errorf("%s: transcript %s is tombstoned", memoryErrStateUnsafe, rel)
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
		_, blocked := guard.paths[rel]
		if blocked || guard.blocksSession(location.sessionID) {
			return fmt.Errorf("%s: transcript %s is tombstoned", memoryErrStateUnsafe, rel)
		}
		return nil
	}, nil
}

// classifyTranscriptReadError labels size refusals with the error code
// consumers degrade on (history treats memoryErrInputTooLarge as skippable),
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
	if errors.As(err, &bound) {
		return fmt.Errorf("%w: %w", errHistoryTranscriptTooLarge, err)
	}
	return err
}

// validateLocalTranscriptSessions checks the exact selected identities and every
// transcript chunk. Tree entries alone do not establish object availability in
// partial clones. Object type checks avoid reading transcript bytes or fetching
// missing promisor blobs; extra local sessions do not expand discovery scope.
func validateLocalTranscriptSessions(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot, sessions []exportSession, mode string) error {
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
	for _, session := range sessions {
		location, ok := available[identity{session.SessionID, session.LatestCheckpoint, session.SessionIndex}]
		source := snapshot.Sources[location.sourceKey]
		chunks := snapshotTranscriptChunks(location.sourcePath, source.TreePaths)
		if !ok || len(chunks) == 0 {
			return fmt.Errorf("persistent transcript storage requires local checkpoint refs for session %s checkpoint %s index %d; sync checkpoints and retry, or use --output for a standalone bundle", session.SessionID, session.LatestCheckpoint, session.SessionIndex)
		}
		for _, chunk := range chunks {
			actualPath, err := source.actualPath(chunk)
			if err != nil {
				return err
			}
			key := objectKey{source.GitDir, source.Ref, actualPath}
			if checked[key] {
				continue
			}
			kind, err := localObjectType(ctx, runner, source.GitDir, source.Ref+":"+actualPath)
			if err != nil || kind != "blob" {
				if err == nil {
					err = fmt.Errorf("expected blob, got %q", kind)
				}
				return fmt.Errorf("persistent transcript storage: unreadable local transcript for session %s checkpoint %s index %d chunk %s; sync checkpoints and retry: %w", session.SessionID, session.LatestCheckpoint, session.SessionIndex, chunk, err)
			}
			checked[key] = true
		}
	}
	return nil
}
