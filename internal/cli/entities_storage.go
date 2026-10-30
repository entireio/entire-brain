package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/entities"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// gitmetaDirForKey resolves the local git-meta store path for a repo key
// (<cache>/brain/<key>/gitmeta.git), mirroring brainDirForKey's key handling but
// rooted in the plugin CACHE tree. The store holds the entity index's meta
// records (and, historically, the retired local-gitmeta fact-set heads —
// COR-2236).
func gitmetaDirForKey(env EntireEnv, key string) (string, error) {
	if first, _, _ := strings.Cut(key, "/"); first == base.WorkspacesDirName {
		return "", fmt.Errorf("repo key uses the reserved %q segment: %s", base.WorkspacesDirName, key)
	}
	dirs, err := base.ResolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dirs.Cache, "brain")
	if err := base.RejectBrainRootPathSymlinks(root, filepath.FromSlash(key)); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(key), "gitmeta.git"), nil
}

// sessionIDsByCheckpoint maps a checkpoint id to the sessions it belongs to.
//
// Two sources, in this order:
//
//  1. The LOCAL CHECKPOINT STORE, which knows the session behind EVERY
//     checkpoint. This is the one that matters: the brain manifest records only
//     each session's LATEST checkpoint, and "the session's last checkpoint" is
//     exactly the coarse answer this index exists to sharpen. Without it a
//     mid-session checkpoint carries no session at all and provenance could
//     never move off the last one.
//  2. The brain manifest, which still covers sessions whose checkpoints are no
//     longer readable locally.
//
// The checkpoint store is best-effort: an unreadable or absent one leaves the
// manifest join exactly as it was.
// Session tombstones are honored here rather than only at the manifest, because
// the checkpoint-store half of this join reads the repository's checkpoint refs
// directly and never consults the brain manifest at all. Without the guard an
// excluded or purged session reappeared in the index on the next
// `entities backfill`, and `entities history` answered with it — a derived
// projection quietly restoring the session linkage the user asked to erase.
// Unreadable tombstone state fails the join closed for the same reason every
// other privacy gate does.
func sessionIDsByCheckpoint(ctx context.Context, opts Options, repoDir, brainDir string) (map[string][]string, error) {
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, nil)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	add := func(checkpoint, id string) {
		checkpoint, id = strings.TrimSpace(checkpoint), strings.TrimSpace(id)
		if checkpoint == "" || id == "" || guard.BlocksSession(id) {
			return
		}
		if !base.ContainsString(out[checkpoint], id) {
			out[checkpoint] = append(out[checkpoint], id)
		}
	}
	// Read through the export-maintained metadata cache, like the transcript
	// store does: without it this join reads every checkpoint's metadata blob
	// with its own git cat-file, which dominated distill wall time (COR-2182).
	// Loaded read-only; export remains the only writer.
	metadataCache := loadCheckpointMetadataCache(filepath.Join(brainDir, filepath.FromSlash(checkpointMetadataCachePath)))
	if snapshot, _, err := loadLocalCheckpointSnapshotLocalOnly(ctx, opts.Runner, repoDir, true, checkpointBranchDestinations{}, metadataCache); err == nil && snapshot != nil {
		for _, member := range snapshot.SessionCheckpoints {
			add(member.CheckpointID, member.SessionID)
		}
	}
	if manifest, err := bmanifest.Load(brainDir); err == nil && manifest != nil {
		sessions := manifest.Sessions
		if manifest.Sources != nil && manifest.Sources.Sessions != nil && len(manifest.Sources.Sessions.Sessions) > 0 {
			sessions = manifest.Sources.Sessions.Sessions
		}
		for _, session := range sessions {
			add(session.LatestCheckpoint, session.SessionID)
		}
	}
	for key := range out {
		sort.Strings(out[key])
	}
	return out, nil
}

func saveEntityIndexCache(brainDir string, cache entities.IndexCache) error {
	if strings.TrimSpace(brainDir) == "" {
		return errors.New("entity index cache: brain dir is empty")
	}
	if err := os.MkdirAll(filepath.Join(brainDir, entities.DirName), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return brainstore.WithBrainWriteLock(brainDir, func() error {
		return brainstore.WriteBrainRelativeFileAtomic(brainDir, entities.CachePath, append(data, '\n'), 0o600)
	})
}
