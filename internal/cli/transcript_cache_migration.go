package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/entire-brain/internal/tui"
)

// planTranscriptCacheSweep proves each live cached transcript can be recovered
// locally before the export switches storage modes. A missing/promisor-only
// source must never turn migration into loss of the only readable copy.
// Unlisted files are stale exports. Exclusion is reversible, so excluded
// sessions need the same recovery proof as every other legacy cached session.
func planTranscriptCacheSweep(ctx context.Context, runner CommandRunner, brainDir, repoDir string) (*historySessionInventory, error) {
	if _, err := os.Lstat(brainDir); os.IsNotExist(err) {
		return nil, nil
	}
	inventory, err := collectHistorySessionInventoryMode(ctx, brainDir, true)
	if errors.Is(err, errHistorySessionsRootMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = inventory.Close()
		}
	}()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	sessions := historySessionByTranscriptPath(manifest)
	if _, err := loadSessionReadGuard(brainDir, manifest); err != nil {
		return nil, err
	}
	store := newTranscriptStore(runner, brainDir)
	// Persistent brains are shared by worktrees; the previous exporter's
	// recorded worktree may have been removed since the cache was written.
	store.repoDir = repoDir
	backed := checkpointBackedSessions(manifest)
	var retained map[string]bool
	retain := func(rel, reason string) {
		tui.Warnf(os.Stderr, "retain transcript cache: %s for %s; keeping the cached copy", reason, rel)
		if retained == nil {
			retained = make(map[string]bool)
		}
		retained[rel] = true
	}
	for _, file := range inventory.files {
		if _, live := sessions[file.Rel]; !live {
			continue
		}
		location, snapshot, err := store.lookupLocation(ctx, file.Rel)
		if err != nil {
			if backed {
				retain(file.Rel, "checkpoint copy is unavailable")
				continue
			}
			return nil, fmt.Errorf("retain transcript cache: %w", err)
		}
		data, err := readSnapshotTranscriptFromSource(ctx, runner, snapshot, location.sourceKey, location.sourcePath)
		if err != nil {
			if backed {
				retain(file.Rel, "checkpoint copy is unreadable")
				continue
			}
			return nil, fmt.Errorf("retain transcript cache for %s: %w", file.Rel, err)
		}
		cached, _, err := readHistorySessionInventoryFile(ctx, inventory, file)
		if err != nil {
			return nil, err
		}
		switch {
		case bytes.HasPrefix(data, cached):
			// The checkpoint holds every cached byte (equal, or the
			// append-only transcript ran ahead of the last disk export), so
			// the file is recoverable and safe to sweep.
		case bytes.HasPrefix(cached, data):
			// The disk export ran ahead of the last checkpoint. Keep the
			// file out of the sweep instead of blocking every future export;
			// a later checkpoint reconciles it and the next sweep collects
			// it. Checkpoint-backed exports repeat this proof so the file is
			// never deleted while it still holds the only copy of the tail.
			retain(file.Rel, "cached transcript is newer than its checkpoint")
		case backed:
			// A divergent override of a checkpoint-backed session is an
			// untrusted stray; sweeping it is the established cleanup.
		default:
			// Divergent, and nothing proves which copy is whole: neither is a
			// prefix of the other, and this brain is not checkpoint-backed, so
			// the cached file may hold bytes the checkpoint never saw.
			//
			// This used to return an error, which aborted the WHOLE delta
			// export over one stray file -- and never self-healed, because
			// nothing in the export path reconciles a divergence. Observed:
			// one session file froze a real brain's manifest at 2026-10-11 for
			// thirteen days, and because the source never advanced, the memory
			// worker's receipt digest never matched its job digest, so it
			// failed `memory_source_stale` and relaunched on backoff forever.
			// Every symptom was two layers away from this line.
			//
			// Retaining is both safer and unblocking: the file is kept either
			// way (the error path deleted nothing), so refusing to sweep it
			// loses no bytes, while the export goes on and every other session
			// gets through. It is exactly what the newer-than-its-checkpoint
			// case above already does, for the reason stated there -- "instead
			// of blocking every future export".
			retain(file.Rel, "checkpoint bytes differ")
		}
	}
	if len(retained) > 0 {
		kept := inventory.files[:0]
		for _, file := range inventory.files {
			if !retained[file.Rel] {
				kept = append(kept, file)
			}
		}
		inventory.files = kept
	}
	success = true
	return inventory, nil
}

// sweepTranscriptCache removes only the descriptor-bound files validated by the
// migration preflight. The caller holds the brain write lock and has published
// the checkpoint-backed manifest. Directories are removed only when empty.
func sweepTranscriptCache(ctx context.Context, inventory *historySessionInventory) error {
	if inventory == nil {
		return nil
	}
	current, err := collectHistorySessionInventoryMode(ctx, inventory.brainDir, true)
	if err != nil {
		return err
	}
	unchanged := inventory.sameMembership(current)
	_ = current.Close()
	if !unchanged {
		return fmt.Errorf("%s: transcript cache changed before migration sweep", memoryErrSourceStale)
	}
	if err := inventory.validateAll(ctx); err != nil {
		return err
	}
	for _, file := range inventory.files {
		if err := inventory.validateFileMembership(ctx, file); err != nil {
			return err
		}
		if err := inventory.root.Remove(file.Rel); err != nil {
			return err
		}
	}
	for i := len(inventory.entries) - 1; i >= 0; i-- {
		if inventory.entries[i].Mode.IsDir() {
			// A nonempty directory may contain non-transcript artifacts.
			_ = inventory.root.Remove(inventory.entries[i].Rel)
		}
	}
	if len(inventory.files) > 0 {
		fmt.Fprintf(os.Stderr, "transcript cache: removed %d cached transcript file(s); originals remain recoverable from checkpoints\n", len(inventory.files))
	}
	return nil
}
