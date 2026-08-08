package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// memory_receipts.go is the C3 projection receipt: the durable, content-free
// proof of which canonical sessions the last full consolidation represented,
// at which input digests. Jobs are operational history; a deterministic job
// is complete only when its identity appears here (or its session is
// tombstoned). The receipt is published through the Brain manifest, whose
// atomic replacement is the commit point.

const (
	projectionStateFileName = "projection-state-v1.json"
	projectionStateRel      = historyDirName + "/" + projectionStateFileName
	projectionSchemaVersion = 1
	// projectionReconcilerVersion mirrors the overlay's reconciliation rule
	// version: receipts from a different rule are stale.
	projectionReconcilerVersion = historyShortTermReconcilerVersion
)

type projectionReceipt struct {
	SessionRef    string    `json:"session_ref"`
	SessionID     string    `json:"session_id"`
	Branch        string    `json:"branch,omitempty"`
	InputDigest   string    `json:"input_digest"`
	ExchangeCount int       `json:"exchange_count"`
	CompletedAt   time.Time `json:"completed_at"`
}

type projectionState struct {
	SchemaVersion     int                 `json:"schema_version"`
	ReconcilerVersion int                 `json:"reconciler_version"`
	GeneratedAt       time.Time           `json:"generated_at"`
	Sessions          []projectionReceipt `json:"sessions"`
}

// sessionTranscriptDigest hashes the exported transcript file (streamed).
func sessionTranscriptDigest(brainDir, rel string) (string, error) {
	f, err := os.Open(filepath.Join(brainDir, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// buildProjectionReceipts derives the receipt set from the canonical manifest
// and the just-built index. A valid empty session succeeds with zero
// exchanges; tombstoned sessions are represented by the tombstone, never a
// receipt. An unreadable transcript simply has no receipt: reconciliation
// keeps its job open.
func buildProjectionReceipts(brainDir string, manifest *exportManifest, index historyIndex, now time.Time) projectionState {
	state := projectionState{SchemaVersion: projectionSchemaVersion, ReconcilerVersion: projectionReconcilerVersion, GeneratedAt: now}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return state
	}
	stones := loadSessionTombstones(brainDir)
	exchangesByPath := map[string]int{}
	for _, record := range index.Records {
		if record.Kind == conversationKind {
			exchangesByPath[record.Path]++
		}
	}
	seen := map[string]bool{}
	for _, session := range manifest.Sources.Sessions.Sessions {
		id := strings.TrimSpace(session.SessionID)
		if _, excluded := stones.Excluded[id]; excluded {
			continue
		}
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		digest, err := sessionTranscriptDigest(brainDir, rel)
		if err != nil {
			continue // no receipt: the job for this session stays open
		}
		branch := strings.TrimSpace(session.Branch)
		if branch == "" {
			branch = strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
		}
		ref, _ := conversationSessionRef(manifest.RepoKey, branch, id, digest)
		if seen[ref] {
			continue // re-export duplicates resolve to one receipt (newest-first manifest order not guaranteed; digest ties break by first)
		}
		seen[ref] = true
		state.Sessions = append(state.Sessions, projectionReceipt{
			SessionRef: ref, SessionID: id, Branch: branch,
			InputDigest: digest, ExchangeCount: exchangesByPath[rel], CompletedAt: now,
		})
	}
	sort.Slice(state.Sessions, func(i, j int) bool { return state.Sessions[i].SessionRef < state.Sessions[j].SessionRef })
	return state
}

func writeProjectionState(brainDir string, state projectionState) (digest string, err error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := writeBrainRelativeFileAtomic(brainDir, projectionStateRel, data, 0o600); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// loadProjectionState returns the receipt set when it is valid and matches
// the digest the manifest published; anything else reads as absent (the jobs
// re-enqueue, nothing is lost).
func loadProjectionState(brainDir string, source *historySourceManifest) (projectionState, bool) {
	if source == nil || source.ProjectionStateDigest == "" {
		return projectionState{}, false
	}
	data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(projectionStateRel)), maxManifestBytes)
	if err != nil {
		return projectionState{}, false
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != source.ProjectionStateDigest {
		return projectionState{}, false
	}
	var state projectionState
	if json.Unmarshal(data, &state) != nil || state.SchemaVersion != projectionSchemaVersion || state.ReconcilerVersion != projectionReconcilerVersion {
		return projectionState{}, false
	}
	// Duplicate references or conflicting digests invalidate the file.
	seen := map[string]string{}
	for _, receipt := range state.Sessions {
		if prev, ok := seen[receipt.SessionRef]; ok && prev != receipt.InputDigest {
			return projectionState{}, false
		} else if ok {
			return projectionState{}, false
		}
		seen[receipt.SessionRef] = receipt.InputDigest
	}
	return state, true
}

// receiptFor finds one session's receipt.
func (s projectionState) receiptFor(sessionRef string) (projectionReceipt, bool) {
	for _, receipt := range s.Sessions {
		if receipt.SessionRef == sessionRef {
			return receipt, true
		}
	}
	return projectionReceipt{}, false
}

func projectionStateError(kind, detail string) error {
	return fmt.Errorf("%s: %s", kind, detail)
}
