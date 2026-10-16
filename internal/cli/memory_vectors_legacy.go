package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Semantic retrieval was removed (trail #278), and with it every vector
// writer. Brains refreshed before the removal still hold vector artifacts on
// disk — per-branch embeddings/vectors.bin files and the memory worker's
// vector-progress leaf — so the health, admin, and privacy-audit surfaces keep
// the read-side definitions needed to recognize, report, and clean those
// legacy files. Nothing in this file embeds or writes vectors.

const (
	memoryVectorProgressRel = memoryWorkDirRel + "/vector-progress.json"
	memoryVectorSchema      = 2

	embedStoreFileName = "vectors.bin"
	embedStoreDirName  = "embeddings"
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

type memoryVectorProgressReadState string

const (
	memoryVectorProgressUnsupported memoryVectorProgressReadState = "unsupported"
)

// memoryVectorProgressLoadError keeps a forward-version or corrupt legacy
// progress leaf typed for the health surface. Error still begins with the
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

func newMemoryVectorProgressLoadError(err error, version int) *memoryVectorProgressLoadError {
	return &memoryVectorProgressLoadError{Code: memoryErrStateCorrupt, Version: version, Err: err}
}

func loadMemoryVectorProgress(brainDir string) (memoryVectorProgress, bool, error) {
	data, present, err := readMemoryStateFile(brainDir, memoryVectorProgressRel, "vector progress", maxManifestBytes)
	if err != nil {
		return memoryVectorProgress{}, present, newMemoryVectorProgressLoadError(err, 0)
	}
	if !present {
		return memoryVectorProgress{}, false, nil
	}
	progress, err := decodeMemoryVectorProgress(data)
	if err != nil {
		return memoryVectorProgress{}, true, err
	}
	return progress, true, nil
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
	if _, err := decodeVersionedJSONBody(data, &progress, false); err != nil {
		if errors.Is(err, errTrailingJSONData) {
			return memoryVectorProgress{}, newMemoryVectorProgressLoadError(fmt.Errorf("vector progress has trailing JSON data"), header.SchemaVersion)
		}
		return memoryVectorProgress{}, newMemoryVectorProgressLoadError(err, header.SchemaVersion)
	}
	// Identity/bounds validation carried over from the deleted writer: a legacy
	// leaf that fails these is corrupt and must classify as such in health
	// output, not report as a healthy pending/current state.
	if progress.SchemaVersion != memoryVectorSchema || progress.ModelID == "" || progress.UpdatedAt.IsZero() ||
		progress.HistoryAdded < 0 || progress.ConversationAdded < 0 ||
		!validSHA256Identity(progress.SourceDigest) ||
		(progress.CompleteSourceDigest != "" && !validSHA256Identity(progress.CompleteSourceDigest)) ||
		(!progress.Pending && (!progress.ResetComplete || progress.CompleteSourceDigest != progress.SourceDigest)) ||
		(progress.ErrorCode != "" && progress.ErrorCode != "memory_vector_sync_failed") {
		return memoryVectorProgress{}, newMemoryVectorProgressLoadError(fmt.Errorf("vector progress has invalid identity or bounds"), header.SchemaVersion)
	}
	return progress, nil
}

// saveMemoryVectorProgress exists for tests and migration tooling that
// fabricate or repair a legacy progress leaf; no production path writes one
// anymore.
func saveMemoryVectorProgress(brainDir string, progress memoryVectorProgress) error {
	return withBrainWriteLock(brainDir, func() error {
		if _, _, err := loadMemoryVectorProgress(brainDir); err != nil {
			return err
		}
		progress.SchemaVersion = memoryVectorSchema
		data, err := json.Marshal(progress)
		if err != nil {
			return err
		}
		return writeBrainRelativeFileAtomic(brainDir, memoryVectorProgressRel, data, 0o600)
	})
}
