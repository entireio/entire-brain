package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// memory_receipts.go is the projection receipt: the durable, content-free
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

// projectionStateReadState keeps operationally distinct receipt failures
// separate. In particular, a newer schema must never be treated as corrupt
// disposable state: callers can report it and leave it untouched until this
// binary is upgraded.
type projectionStateReadState string

const (
	projectionStateAbsent      projectionStateReadState = "absent"
	projectionStateCurrent     projectionStateReadState = "current"
	projectionStateCorrupt     projectionStateReadState = "corrupt"
	projectionStateStale       projectionStateReadState = "stale"
	projectionStateUnsafe      projectionStateReadState = "unsafe"
	projectionStateUnsupported projectionStateReadState = "unsupported"
)

type projectionStateLoadError struct {
	State   projectionStateReadState
	Code    string
	Path    string
	Version int
	Action  string
	Detail  string
}

func (e *projectionStateLoadError) Error() string {
	if e == nil {
		return ""
	}
	detail := strings.TrimSpace(e.Detail)
	if detail == "" {
		detail = string(e.State)
	}
	return fmt.Sprintf("%s: projection receipt %s: %s", e.Code, e.Path, detail)
}

// projectionStateAction names the command that clears each state.
//
// `memory` is a subcommand of THIS binary, not a verb of the host CLI, so the
// repair command has to carry the brain's own spelling. It was printed as
// `entire memory repair` -- a command neither dispatch mode can resolve: a
// standalone reader has no `entire` at all, and a plugin reader's `entire`
// dispatches `memory` to nothing. The spelling comes from setupCommandPrefix,
// read from the process environment the same way path.go, workspace.go and
// agent_surface.go read it -- these printers sit too deep in the health/error
// path to be handed one.
func projectionStateAction(state projectionStateReadState) string {
	brainCmd := setupCommandPrefix(os.LookupEnv)
	switch state {
	case projectionStateAbsent:
		return "run `" + brainCmd + " memory repair` to rebuild the missing disposable receipt"
	case projectionStateStale:
		return "run `" + brainCmd + " memory repair` to atomically publish a receipt matching current canonical sessions"
	case projectionStateUnsafe:
		return "inspect the manifest receipt path; remove the unsafe pointer before repair"
	case projectionStateUnsupported:
		return "upgrade entire-brain; this newer receipt is left untouched"
	case projectionStateCorrupt:
		return "inspect the reported receipt, then run `" + brainCmd + " memory repair` to rebuild disposable state"
	default:
		return ""
	}
}

func newProjectionStateLoadError(state projectionStateReadState, path, detail string, version int) error {
	code := memoryErrStateCorrupt
	switch state {
	case projectionStateUnsupported:
		code = memoryErrUnsupportedVersion
	case projectionStateStale:
		code = memoryErrSourceStale
	case projectionStateUnsafe:
		code = memoryErrStateUnsafe
	}
	return &projectionStateLoadError{
		State: state, Code: code, Path: path, Version: version,
		Action: projectionStateAction(state), Detail: detail,
	}
}

// sessionTranscriptDigest hashes the exported transcript file (streamed).
func sessionTranscriptDigest(brainDir, rel string) (string, error) {
	data, err := readCanonicalHistoryTranscript(context.Background(), brainDir, rel)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// buildProjectionReceipts derives the receipt set from the canonical manifest
// and the just-built index. A valid empty session succeeds with zero
// exchanges; tombstoned sessions are represented by the tombstone, never a
// receipt. An unreadable transcript simply has no receipt: reconciliation
// keeps its job open.
func buildProjectionReceipts(brainDir string, manifest *exportManifest, index historyIndex, now time.Time) (projectionState, error) {
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return projectionState{}, err
	}
	return buildProjectionReceiptsFromSnapshot(brainDir, manifest, stones, index, now), nil
}

func buildProjectionReceiptsFromSnapshot(brainDir string, manifest *exportManifest, stones sessionTombstones, index historyIndex, now time.Time) projectionState {
	state := projectionState{SchemaVersion: projectionSchemaVersion, ReconcilerVersion: projectionReconcilerVersion, GeneratedAt: now}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return state
	}
	exchangesByPath := map[string]int{}
	for _, record := range index.Records {
		if record.Kind == conversationKind {
			exchangesByPath[record.Path]++
		}
	}
	seen := map[string]bool{}
	for _, session := range manifest.Sources.Sessions.Sessions {
		id := strings.TrimSpace(session.SessionID)
		if id == "" {
			continue // no canonical receipt identity; reconciliation keeps it open
		}
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
	if existing, present, readErr := readMemoryStateFile(brainDir, projectionStateRel, "projection receipt", maxManifestBytes); readErr != nil {
		return "", readErr
	} else if present {
		_, readState, decodeErr := decodeProjectionStateDocument(existing, projectionStateRel)
		if readState != projectionStateCurrent && readState != projectionStateStale {
			return "", decodeErr
		}
	}
	data, digest, err := encodeProjectionState(state)
	if err != nil {
		return "", err
	}
	if err := writeBrainRelativeFileAtomic(brainDir, projectionStateRel, data, 0o600); err != nil {
		return "", err
	}
	return digest, nil
}

func encodeProjectionState(state projectionState) (data []byte, digest string, err error) {
	data, err = json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, "", err
	}
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// loadProjectionStateChecked returns a typed state for every failure mode.
// It is strictly read-only and never rewrites or removes an unknown file.
func loadProjectionStateChecked(brainDir string, source *historySourceManifest) (projectionState, projectionStateReadState, error) {
	state, readState, err := loadProjectionStateCheckedOnce(brainDir, source)
	if readState == projectionStateCurrent || source == nil || !validHistoryGenerationArtifactPath(strings.TrimSpace(source.ProjectionStatePath), projectionStateFileName) {
		return state, readState, err
	}
	// Match the history-index race contract: a caller may hold manifest A while
	// a writer commits B and prunes A. Retry once only when the current manifest
	// names a different receipt identity; never mask damage to the current one.
	manifest, manifestErr := loadBrainManifest(brainDir)
	if manifestErr != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		return state, readState, err
	}
	current := manifest.Sources.History
	if strings.TrimSpace(current.ProjectionStatePath) == strings.TrimSpace(source.ProjectionStatePath) &&
		current.ProjectionStateDigest == source.ProjectionStateDigest {
		return state, readState, err
	}
	return loadProjectionStateCheckedOnce(brainDir, current)
}

func loadProjectionStateCheckedOnce(brainDir string, source *historySourceManifest) (projectionState, projectionStateReadState, error) {
	if source == nil || source.ProjectionStateDigest == "" {
		return projectionState{}, projectionStateAbsent, nil
	}
	rel := strings.TrimSpace(source.ProjectionStatePath)
	if rel == "" {
		rel = projectionStateRel
	}
	clean, err := validateProjectionStatePath(rel)
	if err != nil {
		return projectionState{}, projectionStateUnsafe, newProjectionStateLoadError(projectionStateUnsafe, rel, err.Error(), 0)
	}
	if !validSHA256Identity(source.ProjectionStateDigest) {
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "manifest content digest is invalid", 0)
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return projectionState{}, projectionStateUnsafe, newProjectionStateLoadError(projectionStateUnsafe, rel, "path contains a symlink", 0)
	}
	path := filepath.Join(brainDir, clean)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return projectionState{}, projectionStateAbsent, nil
		}
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "cannot inspect the receipt file", 0)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return projectionState{}, projectionStateUnsafe, newProjectionStateLoadError(projectionStateUnsafe, rel, "receipt must be a regular file and must not be a symlink", 0)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "cannot open the receipt without following links", 0)
	}
	defer f.Close()
	if err := rejectOpenFileAlias(path, f, "projection receipt"); err != nil {
		return projectionState{}, projectionStateUnsafe, newProjectionStateLoadError(projectionStateUnsafe, rel, err.Error(), 0)
	}
	data, err := safeReadAll(f, maxManifestBytes, path)
	if err != nil {
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "cannot read a bounded regular file", 0)
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != source.ProjectionStateDigest {
		return projectionState{}, projectionStateStale, newProjectionStateLoadError(projectionStateStale, rel, "content digest does not match the manifest commit", 0)
	}
	state, documentState, documentErr := decodeProjectionStateDocument(data, rel)
	if documentState != projectionStateCurrent {
		return projectionState{}, documentState, documentErr
	}
	if source.SessionsFingerprint == "" || source.SessionsFingerprint != brainSessionsFingerprint(brainDir) {
		return projectionState{}, projectionStateStale, newProjectionStateLoadError(projectionStateStale, rel, "canonical session fingerprint changed", state.SchemaVersion)
	}
	return state, projectionStateCurrent, nil
}

// decodeProjectionStateDocument classifies the schema header before strict
// supported decoding, so additive fields from a vNext producer cannot turn a
// read-only receipt into disposable "corruption".
func decodeProjectionStateDocument(data []byte, rel string) (projectionState, projectionStateReadState, error) {
	version, err := checkedVersionedJSONHeader(data, projectionSchemaVersion, "projection receipt")
	if err != nil {
		if version > projectionSchemaVersion {
			return projectionState{}, projectionStateUnsupported, newProjectionStateLoadError(projectionStateUnsupported, rel, "schema is newer than this binary", version)
		}
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "invalid JSON", version)
	}
	var state projectionState
	if _, err := decodeStrictVersionedJSON(data, &state, projectionSchemaVersion, "projection receipt"); err != nil {
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "invalid JSON or unknown fields", version)
	}
	if state.SchemaVersion != projectionSchemaVersion {
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "invalid schema version", state.SchemaVersion)
	}
	if state.GeneratedAt.IsZero() {
		return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "generated_at is missing", state.SchemaVersion)
	}
	previousRef := ""
	for _, receipt := range state.Sessions {
		if !strings.HasPrefix(receipt.SessionRef, conversationSessionIDPrefix) || strings.TrimSpace(receipt.SessionID) == "" ||
			!validSHA256Identity(receipt.InputDigest) || receipt.ExchangeCount < 0 || receipt.CompletedAt.IsZero() || receipt.CompletedAt.After(state.GeneratedAt) {
			return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "receipt has invalid identity or timestamp", state.SchemaVersion)
		}
		if previousRef != "" && receipt.SessionRef <= previousRef {
			return projectionState{}, projectionStateCorrupt, newProjectionStateLoadError(projectionStateCorrupt, rel, "session receipts are not strictly sorted", state.SchemaVersion)
		}
		previousRef = receipt.SessionRef
	}
	if state.ReconcilerVersion != projectionReconcilerVersion {
		return projectionState{}, projectionStateStale, newProjectionStateLoadError(projectionStateStale, rel, "reconciliation rules changed", state.ReconcilerVersion)
	}
	return state, projectionStateCurrent, nil
}

// loadProjectionState is the legacy convenience wrapper for reconciliation
// paths where every non-current result safely means "no completion proof".
// Status, repair, and migration use the checked loader so they preserve exact
// failure classification.
func loadProjectionState(brainDir string, source *historySourceManifest) (projectionState, bool) {
	state, readState, _ := loadProjectionStateChecked(brainDir, source)
	return state, readState == projectionStateCurrent
}

func validateProjectionStatePath(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	cleanSlash := filepath.ToSlash(clean)
	if cleanSlash != rel {
		return "", fmt.Errorf("projection_state_path must be canonical: %s", rel)
	}
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		(cleanSlash != projectionStateRel && !validHistoryGenerationArtifactPath(cleanSlash, projectionStateFileName)) {
		return "", fmt.Errorf("projection_state_path is unsafe: %s", rel)
	}
	return clean, nil
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
