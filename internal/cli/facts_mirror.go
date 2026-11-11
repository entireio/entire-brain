package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// mirrorCounts reports what one mirror pass removed.
type mirrorCounts struct {
	FactsDeleted    int
	AnchorsStripped int
}

func (c mirrorCounts) empty() bool {
	return c.FactsDeleted == 0 && c.AnchorsStripped == 0
}

// checkpointCatalog is the mirror's membership set: every session the current
// checkpoint refs still back, keyed two ways. It includes the sessions the
// export dropped for want of locally servable bytes — those are still catalog
// members, not deletions.
type checkpointCatalog struct {
	ids  map[string]bool
	rels map[string]bool
}

// catalogFromManifest derives the membership set, or ok=false when this
// manifest cannot speak for the full checkpoint catalog (no session source,
// or a branch-scoped / checkpoint-limited export wrote a partial catalog).
func catalogFromManifest(manifest *exportManifest) (checkpointCatalog, bool) {
	catalog := checkpointCatalog{ids: map[string]bool{}, rels: map[string]bool{}}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return catalog, false
	}
	source := manifest.Sources.Sessions
	if source.CheckpointLimit != 0 || (source.Scope != "" && source.Scope != exportScopeAll) {
		return catalog, false
	}
	for _, session := range source.Sessions {
		if id := strings.TrimSpace(session.SessionID); id != "" {
			catalog.ids[id] = true
		}
		if rel := brainstore.NormalizeTranscriptPath(session.TranscriptPath); rel != "" {
			catalog.rels[rel] = true
		}
	}
	for _, session := range source.UnavailableSessions {
		if id := strings.TrimSpace(session.SessionID); id != "" {
			catalog.ids[id] = true
		}
	}
	return catalog, true
}

// warnLegacyTombstones reports a leftover exclusion policy from the removed
// brain-local privacy layer. The brain now mirrors the checkpoint refs, so a
// session the user excluded or purged re-enters every projection at the next
// refresh while its checkpoints still exist — the user must be told to delete
// those checkpoints instead of silently losing the exclusion.
func warnLegacyTombstones(out io.Writer, brainDir string) {
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(brainstore.HistoryDirName), "tombstones.json"))
	if err != nil {
		return
	}
	var stones struct {
		Excluded map[string]json.RawMessage `json:"excluded"`
	}
	if json.Unmarshal(data, &stones) != nil || len(stones.Excluded) == 0 {
		return
	}
	ids := make([]string, 0, len(stones.Excluded))
	for id := range stones.Excluded {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintf(out, "warning: %s/tombstones.json records %d session exclusion(s) from the removed privacy layer; the brain now mirrors the checkpoint refs, so these sessions re-enter projections while their checkpoints exist.\n"+
		"  Delete their checkpoints (`entire checkpoint delete`) to keep them out, then remove the file. Sessions: %s\n",
		brainstore.HistoryDirName, len(stones.Excluded), strings.Join(ids, ", "))
}

// mirrorBrainToCheckpointCatalog applies the mirror rule to the artifacts a
// session leaves outside the session tree: the brain serves only what the
// checkpoint refs can back, so after a refresh every provenance anchor citing
// a session gone from the catalog is dropped. A distilled fact left with no
// provenance is deleted.
//
// Facts this brain did not distill are never touched. Authored facts
// (`remember`) have no session source to mirror. Anchors without a transcript
// path are dropped only when the same fact proves, via a transcript anchor,
// that their session was distilled here — SanitizeForEgress strips transcript
// anchors on push, so a pulled teammate copy or an import anchor never
// carries one, and its sessions legitimately live outside this catalog.
//
// A partial catalog (branch-scoped or checkpoint-limited export, or no
// session source at all) makes the whole pass a no-op: membership can only be
// judged against the full catalog.
func mirrorBrainToCheckpointCatalog(brainDir string, now time.Time) (mirrorCounts, error) {
	return mirrorBrainToCheckpointCatalogContext(context.Background(), brainDir, now)
}

func mirrorBrainToCheckpointCatalogContext(ctx context.Context, brainDir string, now time.Time) (mirrorCounts, error) {
	var counts mirrorCounts
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return counts, err
	}
	catalog, ok := catalogFromManifest(manifest)
	if !ok {
		return counts, nil
	}
	err = brainstore.WithBrainWriteLockContext(ctx, brainDir, func() error {
		deleted, stripped, err := dropFactsForMissingSessionsLocked(brainDir, catalog, now)
		if err != nil {
			return err
		}
		counts.FactsDeleted, counts.AnchorsStripped = deleted, stripped
		return nil
	})
	return counts, err
}

func dropFactsForMissingSessionsLocked(brainDir string, catalog checkpointCatalog, now time.Time) (int, int, error) {
	anchorPresent := func(anchor factAnchor, localSessions map[string]bool) bool {
		id := strings.TrimSpace(anchor.SessionID)
		rel := brainstore.NormalizeTranscriptPath(anchor.Transcript)
		if rel == "" {
			// No transcript: only an anchor this fact itself attributes to a
			// locally distilled session is subject to the mirror.
			return id == "" || !localSessions[id] || catalog.ids[id]
		}
		return catalog.ids[id] || catalog.rels[rel]
	}
	// Migrate first: this is a write entry point, so legacy branch files
	// (including stragglers an older binary wrote after migration) fold into
	// the flat store before the sweep reads it, and the sweep's one Write
	// covers everything.
	if _, err := factstore.MigrateRepository(brainDir); err != nil {
		return 0, 0, err
	}
	facts, err := factstore.Load(brainDir)
	if err != nil {
		return 0, 0, err
	}
	deletedTotal, strippedTotal := 0, 0
	{
		changed := false
		deletedIDs := map[string]bool{}
		kept := make([]factRecord, 0, len(facts))
		for _, fact := range facts {
			if fact.Origin == factstore.OriginAuthored || !hasLocalProvenance(fact) {
				kept = append(kept, fact)
				continue
			}
			localSessions := map[string]bool{}
			for _, anchor := range fact.Provenance {
				if anchor.Transcript != "" {
					if id := strings.TrimSpace(anchor.SessionID); id != "" {
						localSessions[id] = true
					}
				}
			}
			remaining := fact.Provenance[:0:0]
			stripped := 0
			for _, anchor := range fact.Provenance {
				if anchorPresent(anchor, localSessions) {
					remaining = append(remaining, anchor)
					continue
				}
				stripped++
			}
			if stripped > 0 && len(remaining) == 0 {
				deletedIDs[fact.ID] = true
				deletedTotal++
				changed = true
				continue
			}
			if stripped > 0 {
				fact.Provenance = remaining
				strippedTotal += stripped
				changed = true
			}
			kept = append(kept, fact)
		}
		if !changed {
			return deletedTotal, strippedTotal, nil
		}
		if err := factstore.Write(brainDir, kept); err != nil {
			return deletedTotal, strippedTotal, err
		}
	}
	return deletedTotal, strippedTotal, updateFactSourceManifestLocked(brainDir, now)
}
