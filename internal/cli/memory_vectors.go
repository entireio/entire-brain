package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	memoryVectorProgressRel  = memoryWorkDirRel + "/vector-progress.json"
	memoryVectorSchema       = 2
	memoryVectorBatchRecords = 64
	memoryVectorCallTimeout  = 15 * time.Second
)

type memoryVectorProgress struct {
	SchemaVersion        int       `json:"schema_version"`
	ModelID              string    `json:"model_id"`
	SourceDigest         string    `json:"source_digest,omitempty"`
	CompleteSourceDigest string    `json:"complete_source_digest,omitempty"`
	ResetComplete        bool      `json:"reset_complete"`
	HistoryAdded         int       `json:"history_added"`
	ConversationAdded    int       `json:"conversation_added"`
	Pending              bool      `json:"pending"`
	UpdatedAt            time.Time `json:"updated_at"`
	ErrorCode            string    `json:"error_code,omitempty"`
}

type memoryVectorSyncPass struct {
	HistoryAdded      int
	HistoryDropped    int
	HistoryTotal      int
	ConversationAdded int
	ConversationDrop  int
	ConversationTotal int
}

type memoryVectorEpoch struct {
	manifestIdentity  string
	tombstoneIdentity string
}

// memoryVectorProgressOwnership is the exact progress leaf observed by one
// vector-sync pass. Both vector stores share the same pass-local token. A
// successful progress write advances it to the newly written bytes; a store
// mutation never does. This lets one worker publish its ordinary incremental
// progress while making any intervening writer a detectable ownership change.
type memoryVectorProgressOwnership struct {
	present bool
	data    []byte
}

type memoryVectorProgressReadState string

const (
	memoryVectorProgressCorrupt     memoryVectorProgressReadState = "corrupt"
	memoryVectorProgressUnsafe      memoryVectorProgressReadState = "unsafe"
	memoryVectorProgressUnsupported memoryVectorProgressReadState = "unsupported"
)

// memoryVectorProgressLoadError keeps a late forward-version takeover typed
// all the way through a guarded SQLite mutation. Error still begins with the
// established memory code for CLI/MCP compatibility.
type memoryVectorProgressLoadError struct {
	Code    string
	State   memoryVectorProgressReadState
	Version int
	Err     error
}

func (e *memoryVectorProgressLoadError) Error() string {
	detail := "vector progress state is invalid"
	if e.Err != nil {
		detail = e.Err.Error()
	}
	if strings.HasPrefix(detail, e.Code+":") {
		return detail
	}
	return e.Code + ": " + detail
}

func (e *memoryVectorProgressLoadError) Unwrap() error { return e.Err }

// memoryVectorProgressBeforeCommitCheck is a deterministic test seam for the
// interval after expensive vector work/epoch validation and before the exact
// progress leaf is classified at the commit point.
var memoryVectorProgressBeforeCommitCheck = func(string) {}

// memoryGuardedVectorStoreBeforeMutation is a deterministic test seam for the
// interval after epoch validation and immediately before the exact progress
// ownership check that guards a reset/drop or add/upsert.
var memoryGuardedVectorStoreBeforeMutation = func(string, map[string][]float32, []string) {}

func captureMemoryVectorEpoch(brainDir string) (memoryVectorEpoch, *exportManifest, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return memoryVectorEpoch{}, nil, err
	}
	manifestIdentity, err := memoryManifestIdentity(manifest)
	if err != nil {
		return memoryVectorEpoch{}, nil, err
	}
	_, tombstoneState, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return memoryVectorEpoch{}, nil, err
	}
	return memoryVectorEpoch{manifestIdentity: manifestIdentity, tombstoneIdentity: tombstoneState.Identity}, manifest, nil
}

func validateMemoryVectorEpochLocked(brainDir string, expected memoryVectorEpoch) error {
	actual, _, err := captureMemoryVectorEpoch(brainDir)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("%s: projection generation or privacy epoch changed during vector sync", memoryErrSourceStale)
	}
	return nil
}

type memoryGuardedVectorStore struct {
	brainDir  string
	epoch     memoryVectorEpoch
	ownership *memoryVectorProgressOwnership
	store     historyVectorStore
}

func (s *memoryGuardedVectorStore) ids() (map[string]struct{}, bool) { return s.store.ids() }
func (s *memoryGuardedVectorStore) knnCos(vector []float32, limit int) (map[string]float64, bool) {
	return s.store.knnCos(vector, limit)
}
func (s *memoryGuardedVectorStore) upsert(add map[string][]float32, drop []string) error {
	return withBrainWriteLock(s.brainDir, func() error {
		if err := validateMemoryVectorEpochLocked(s.brainDir, s.epoch); err != nil {
			return err
		}
		memoryGuardedVectorStoreBeforeMutation(s.brainDir, add, drop)
		if err := validateMemoryVectorProgressOwnershipLocked(s.brainDir, s.ownership); err != nil {
			return err
		}
		if !s.ownership.present {
			return fmt.Errorf("%s: vector store mutation has no current progress ownership", memoryErrSourceStale)
		}
		return s.store.upsert(add, drop)
	})
}

func clearMemoryVectorStore(ctx context.Context, store historyVectorStore) (int, error) {
	ids, _ := store.ids()
	if len(ids) == 0 {
		return 0, nil
	}
	drop := make([]string, 0, len(ids))
	for id := range ids {
		drop = append(drop, id)
	}
	sort.Strings(drop)
	for len(drop) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		count := 1024
		if len(drop) < count {
			count = len(drop)
		}
		if err := store.upsert(nil, drop[:count]); err != nil {
			return 0, err
		}
		drop = drop[count:]
	}
	return len(ids), nil
}

func saveMemoryVectorProgressAtEpoch(brainDir string, epoch memoryVectorEpoch, progress memoryVectorProgress) error {
	_, _, ownership, err := loadMemoryVectorProgressWithOwnership(brainDir)
	if err != nil {
		return err
	}
	return saveMemoryVectorProgressAtEpochOwned(brainDir, epoch, ownership, progress)
}

func saveMemoryVectorProgressAtEpochOwned(brainDir string, epoch memoryVectorEpoch, ownership *memoryVectorProgressOwnership, progress memoryVectorProgress) error {
	return withBrainWriteLock(brainDir, func() error {
		if err := validateMemoryVectorEpochLocked(brainDir, epoch); err != nil {
			return err
		}
		memoryVectorProgressBeforeCommitCheck(brainDir)
		return saveMemoryVectorProgressOwnedLocked(brainDir, ownership, progress)
	})
}

type contextualDocumentEmbedder interface {
	EmbedContext(context.Context, string) []float32
}

func embedDocumentWithContext(ctx context.Context, e Embedder, text string) []float32 {
	if err := ctx.Err(); err != nil {
		return nil
	}
	if contextual, ok := e.(contextualDocumentEmbedder); ok {
		return contextual.EmbedContext(ctx, text)
	}
	return e.Embed(text)
}

func vectorEligibleRecordIDs(index historyIndex, hiddenKind func(string) bool) (map[string]struct{}, map[string]int) {
	counts := make(map[string]int)
	for _, record := range index.Records {
		if !hiddenKind(record.Kind) {
			counts[record.ID]++
		}
	}
	want := make(map[string]struct{}, len(counts))
	for id, count := range counts {
		if id != "" && count == 1 {
			want[id] = struct{}{}
		}
	}
	return want, counts
}

func syncVectorsForKindsContextBatch(ctx context.Context, store historyVectorStore, index historyIndex, e Embedder, hiddenKind func(string) bool, embedText func(historyRecord) string, budget int) (added, dropped, total int, pending bool, err error) {
	existing, _ := store.ids()
	want, counts := vectorEligibleRecordIDs(index, hiddenKind)
	var drop []string
	// A store is keyed only by record ID. If two independently scoped records
	// collide, one vector could otherwise score both records. The helper leaves
	// every ambiguous ID to exact/lexical retrieval instead.
	for id := range existing {
		if _, ok := want[id]; !ok {
			drop = append(drop, id)
		}
	}
	if len(drop) > 0 {
		if err := store.upsert(nil, drop); err != nil {
			return 0, 0, len(want), false, err
		}
	}
	batch := make(map[string][]float32)
	byText := make(map[string][]float32)
	for _, record := range index.Records {
		if hiddenKind(record.Kind) || counts[record.ID] != 1 || record.ID == "" {
			continue
		}
		if _, ok := existing[record.ID]; ok {
			continue
		}
		if added >= budget {
			pending = true
			break
		}
		if err := ctx.Err(); err != nil {
			pending = true
			break
		}
		text := embedText(record)
		vector, ok := byText[text]
		if !ok {
			callCtx, cancel := context.WithTimeout(ctx, memoryVectorCallTimeout)
			vector = embedDocumentWithContext(callCtx, e, text)
			callErr := callCtx.Err()
			cancel()
			if callErr != nil || len(vector) != e.Dim() {
				if len(batch) > 0 {
					if flushErr := store.upsert(batch, nil); flushErr != nil {
						return added, len(drop), len(want), true, flushErr
					}
				}
				return added, len(drop), len(want), true, fmt.Errorf("memory_vector_sync_failed: embedding call did not complete")
			}
			byText[text] = vector
		}
		batch[record.ID] = vector
		added++
		if len(batch) >= 16 {
			if err := store.upsert(batch, nil); err != nil {
				return added - len(batch), len(drop), len(want), true, err
			}
			batch = make(map[string][]float32)
		}
	}
	if len(batch) > 0 {
		if err := store.upsert(batch, nil); err != nil {
			return added - len(batch), len(drop), len(want), true, err
		}
	}
	return added, len(drop), len(want), pending, nil
}

func saveMemoryVectorProgress(brainDir string, progress memoryVectorProgress) error {
	return withBrainWriteLock(brainDir, func() error {
		return saveMemoryVectorProgressLocked(brainDir, progress)
	})
}

// saveMemoryVectorProgressLocked classifies the exact leaf immediately before
// replacement. The caller holds the Brain write lock, so every cooperative
// progress writer observes one linear order and can never down-convert a
// vNext, corrupt, additive-current, or unsafe leaf that arrived after an
// earlier worker read.
func saveMemoryVectorProgressLocked(brainDir string, progress memoryVectorProgress) error {
	if _, _, err := loadMemoryVectorProgress(brainDir); err != nil {
		return err
	}
	data, err := encodeMemoryVectorProgress(progress)
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, memoryVectorProgressRel, data, 0o600)
}

func saveMemoryVectorProgressOwnedLocked(brainDir string, ownership *memoryVectorProgressOwnership, progress memoryVectorProgress) error {
	if err := validateMemoryVectorProgressOwnershipLocked(brainDir, ownership); err != nil {
		return err
	}
	data, err := encodeMemoryVectorProgress(progress)
	if err != nil {
		return err
	}
	if err := writeBrainRelativeFileAtomic(brainDir, memoryVectorProgressRel, data, 0o600); err != nil {
		return err
	}
	ownership.present = true
	ownership.data = append(ownership.data[:0], data...)
	return nil
}

func encodeMemoryVectorProgress(progress memoryVectorProgress) ([]byte, error) {
	progress.SchemaVersion = memoryVectorSchema
	data, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func loadMemoryVectorProgress(brainDir string) (memoryVectorProgress, bool, error) {
	progress, present, _, err := loadMemoryVectorProgressWithOwnership(brainDir)
	return progress, present, err
}

func loadMemoryVectorProgressWithOwnership(brainDir string) (memoryVectorProgress, bool, *memoryVectorProgressOwnership, error) {
	data, present, err := readMemoryStateFile(brainDir, memoryVectorProgressRel, "vector progress", maxManifestBytes)
	if err != nil {
		return memoryVectorProgress{}, present, nil, newMemoryVectorProgressLoadError(err, 0)
	}
	ownership := &memoryVectorProgressOwnership{present: present}
	if !present {
		return memoryVectorProgress{}, false, ownership, nil
	}
	progress, err := decodeMemoryVectorProgress(data)
	if err != nil {
		return memoryVectorProgress{}, true, nil, err
	}
	ownership.data = append([]byte(nil), data...)
	return progress, true, ownership, nil
}

func decodeMemoryVectorProgress(data []byte) (memoryVectorProgress, error) {
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return memoryVectorProgress{}, newMemoryVectorProgressLoadError(fmt.Errorf("vector progress cannot be parsed"), 0)
	}
	if header.SchemaVersion > memoryVectorSchema {
		return memoryVectorProgress{}, &memoryVectorProgressLoadError{
			Code: memoryErrUnsupportedVersion, State: memoryVectorProgressUnsupported, Version: header.SchemaVersion,
			Err: fmt.Errorf("vector progress schema version %d is newer than supported version %d", header.SchemaVersion, memoryVectorSchema),
		}
	}
	var progress memoryVectorProgress
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&progress); err != nil {
		return memoryVectorProgress{}, newMemoryVectorProgressLoadError(fmt.Errorf("vector progress cannot be parsed"), header.SchemaVersion)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return memoryVectorProgress{}, newMemoryVectorProgressLoadError(fmt.Errorf("vector progress has trailing JSON data"), header.SchemaVersion)
	}
	if progress.SchemaVersion != memoryVectorSchema || progress.ModelID == "" || progress.UpdatedAt.IsZero() || progress.HistoryAdded < 0 || progress.ConversationAdded < 0 || !validSHA256Identity(progress.SourceDigest) || (progress.CompleteSourceDigest != "" && !validSHA256Identity(progress.CompleteSourceDigest)) || (!progress.Pending && (!progress.ResetComplete || progress.CompleteSourceDigest != progress.SourceDigest)) || (progress.ErrorCode != "" && progress.ErrorCode != "memory_vector_sync_failed") {
		return memoryVectorProgress{}, newMemoryVectorProgressLoadError(fmt.Errorf("vector progress has invalid identity or bounds"), header.SchemaVersion)
	}
	return progress, nil
}

func newMemoryVectorProgressLoadError(err error, version int) error {
	code := memoryErrorCode(err)
	state := memoryVectorProgressCorrupt
	if code == memoryErrStateUnsafe {
		state = memoryVectorProgressUnsafe
	} else {
		code = memoryErrStateCorrupt
	}
	return &memoryVectorProgressLoadError{Code: code, State: state, Version: version, Err: err}
}

// validateMemoryVectorProgressOwnershipLocked reclassifies the live leaf before
// comparing it with the exact bytes that authorize this pass. Classification
// comes first so a forward-version takeover remains a typed unsupported error,
// rather than being flattened into a generic ownership mismatch. The caller
// holds the Brain write lock through the subsequent store mutation.
func validateMemoryVectorProgressOwnershipLocked(brainDir string, expected *memoryVectorProgressOwnership) error {
	if expected == nil {
		return fmt.Errorf("%s: vector progress ownership is missing", memoryErrSourceStale)
	}
	_, present, actual, err := loadMemoryVectorProgressWithOwnership(brainDir)
	if err != nil {
		return err
	}
	if present != expected.present || (present && !bytes.Equal(actual.data, expected.data)) {
		return fmt.Errorf("%s: vector progress ownership changed during vector sync", memoryErrSourceStale)
	}
	return nil
}

func memoryVectorProgressNeedsReset(progress memoryVectorProgress, present bool, modelID, sourceDigest string) bool {
	return !present || progress.ModelID != modelID || progress.SourceDigest != sourceDigest || !progress.ResetComplete
}

func memoryProjectionVectorsCurrent(brainDir string, e Embedder) bool {
	return memoryProjectionVectorState(brainDir, e) == "current"
}

func memoryProjectionVectorState(brainDir string, e Embedder) string {
	if e == nil {
		return "degraded"
	}
	epoch, manifest, err := captureMemoryVectorEpoch(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		if err != nil && strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
			return "unsupported"
		}
		return "degraded"
	}
	source := manifest.Sources.History
	if source.PrivacyIdentity != epoch.tombstoneIdentity && !(source.PrivacyIdentity == "" && epoch.tombstoneIdentity == "absent") {
		return "stale"
	}
	progress, present, err := loadMemoryVectorProgress(brainDir)
	if err != nil {
		if strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
			return "unsupported"
		}
		return "degraded"
	}
	if !present {
		return "absent"
	}
	if progress.ModelID != e.ID() || progress.SourceDigest != source.IndexDigest {
		return "stale"
	}
	if progress.Pending {
		return "pending"
	}
	if progress.ResetComplete && progress.CompleteSourceDigest == source.IndexDigest {
		return "current"
	}
	return "degraded"
}

func vectorStoreHasMissing(store historyVectorStore, index historyIndex, hiddenKind func(string) bool) bool {
	existing, _ := store.ids()
	wanted, _ := vectorEligibleRecordIDs(index, hiddenKind)
	for id := range wanted {
		if _, ok := existing[id]; !ok {
			return true
		}
	}
	return false
}
