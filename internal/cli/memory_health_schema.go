package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// memoryOverlayHealth inspects the disposable short-term projection without
// using the retrieval loader. Health must retain the observed version even
// when it is newer than this binary, and it must not rewrite or clear the
// artifact as a side effect of inspection.
func memoryOverlayHealth(brainDir string, source *historySourceManifest) map[string]any {
	health := map[string]any{
		"path":                         historyShortTermPath,
		"present":                      false,
		"state":                        shortTermStateAbsent,
		"schema_state":                 shortTermStateAbsent,
		"supported_schema_version":     historyShortTermVersion,
		"supported_reconciler_version": historyShortTermReconcilerVersion,
		"action":                       "none",
	}
	data, present, err := readMemoryStateFile(brainDir, historyShortTermPath, "short-term history", defaultMaxReadBytes)
	health["present"] = present
	if err != nil {
		code := memoryErrorCode(err)
		state := shortTermStateCorrupt
		if code == memoryErrStateUnsafe {
			state = shortTermStateUnsafe
		}
		health["state"] = state
		health["schema_state"] = state
		health["error_code"] = code
		health["action"] = "inspect the overlay path; read-only health leaves the exact leaf untouched"
		return health
	}
	if !present {
		return health
	}

	var header struct {
		Version           int `json:"version"`
		ReconcilerVersion int `json:"reconciler_version"`
	}
	if err := jsonUnmarshalSingle(data, &header); err != nil {
		health["state"] = shortTermStateCorrupt
		health["schema_state"] = shortTermStateCorrupt
		health["error_code"] = memoryErrStateCorrupt
		health["action"] = "rebuild the disposable overlay from canonical sessions"
		return health
	}
	health["schema_version"] = header.Version
	health["reconciler_version"] = header.ReconcilerVersion
	switch {
	case header.Version > historyShortTermVersion || header.ReconcilerVersion > historyShortTermReconcilerVersion:
		health["state"] = shortTermStateUnsupported
		health["schema_state"] = shortTermStateUnsupported
		health["error_code"] = memoryErrUnsupportedVersion
		health["action"] = "upgrade entire-brain; this newer overlay remains read-only"
		return health
	case header.Version < historyShortTermVersion || header.ReconcilerVersion < historyShortTermReconcilerVersion:
		health["state"] = memoryErrMigrationRequired
		health["schema_state"] = memoryErrMigrationRequired
		health["error_code"] = memoryErrMigrationRequired
		health["action"] = "rebuild the disposable overlay with `entire brain refresh delta`"
		return health
	}

	var overlay shortTermIndex
	if err := jsonUnmarshalSingle(data, &overlay); err != nil || overlay.Files == nil {
		health["state"] = shortTermStateCorrupt
		health["schema_state"] = shortTermStateCorrupt
		health["error_code"] = memoryErrStateCorrupt
		health["action"] = "rebuild the disposable overlay from canonical sessions"
		return health
	}
	health["schema_state"] = shortTermStateCurrent
	health["generated_at"] = overlay.GeneratedAt
	health["files"] = len(overlay.Files)
	health["complete"] = overlay.complete()
	health["truncated"] = overlay.Truncated
	health["failed_files"] = len(overlay.FailedFiles)
	health["scan_warnings"] = len(overlay.ScanWarnings)
	if source == nil || !overlay.BaseGeneratedAt.Equal(source.GeneratedAt) {
		health["state"] = shortTermStateStale
		health["error_code"] = memoryErrSourceStale
		health["action"] = "rebuild the overlay against the current canonical history source"
		return health
	}
	health["state"] = shortTermStateCurrent
	return health
}

// shortTermStateUnsafe is health-only. Retrieval has historically folded an
// unsafe overlay path into "corrupt"; the administrative surface keeps the
// stronger classification so operators do not mistake an alias for ordinary
// disposable bytes.
const shortTermStateUnsafe = "unsafe"

// jsonUnmarshalSingle centralizes the no-trailing-data rule used by the
// lightweight health header readers. Unknown fields remain allowed here so a
// newer version can be classified by its header before strict interpretation.
func jsonUnmarshalSingle(data []byte, out any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

// memoryHistoryFTSHealth reads only the bounded metadata table through an
// immutable, read-only SQLite connection. It never opens the ordinary FTS
// path, whose freshness helper may create a database, WAL, or shared-memory
// sidecar and may rebuild under the Brain write lock.
func memoryHistoryFTSHealth(brainDir string, source *historySourceManifest) map[string]any {
	rel := historyFTSDBRelPath()
	health := map[string]any{
		"path":                     rel,
		"present":                  false,
		"state":                    "absent",
		"schema_state":             "absent",
		"supported_schema_version": historyFTSSchema,
		"action":                   "none",
	}
	clean, err := cleanBrainRelativePath(rel)
	if err != nil || rejectSymlinkedBrainRoot(brainDir) != nil || rejectExistingSymlinkPathComponents(brainDir, clean) != nil {
		health["state"] = "unsafe"
		health["schema_state"] = "unsafe"
		health["error_code"] = memoryErrStateUnsafe
		health["action"] = "inspect the FTS path; status never follows aliases or rebuilds the store"
		return health
	}
	path := filepath.Join(brainDir, clean)
	before, err := memoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return health
		}
		health["state"] = "corrupt"
		health["schema_state"] = "corrupt"
		health["error_code"] = memoryErrStateCorrupt
		health["action"] = "inspect the FTS path and access policy"
		return health
	}
	health["present"] = true
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		health["state"] = "unsafe"
		health["schema_state"] = "unsafe"
		health["error_code"] = memoryErrStateUnsafe
		health["action"] = "replace the unsafe FTS leaf with a regular derived store"
		return health
	}
	guard, err := memoryStateOpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		health["state"] = "unsafe"
		health["schema_state"] = "unsafe"
		health["error_code"] = memoryErrStateUnsafe
		health["action"] = "inspect the FTS path and access policy"
		return health
	}
	defer guard.Close()
	opened, statErr := guard.Stat()
	afterOpen, lstatErr := memoryStateLstat(path)
	if statErr != nil || lstatErr != nil || !opened.Mode().IsRegular() || afterOpen.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, afterOpen) || rejectOpenFileAlias(path, guard, "history FTS") != nil {
		health["state"] = "unsafe"
		health["schema_state"] = "unsafe"
		health["error_code"] = memoryErrStateUnsafe
		health["action"] = "retry after the FTS leaf stops changing"
		return health
	}

	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(path))
	if err != nil {
		return finishHistoryFTSReadError(health)
	}
	db.SetMaxOpenConns(1)
	var schema, fingerprint string
	if err := db.QueryRow(`SELECT value FROM history_fts_meta WHERE key = 'schema'`).Scan(&schema); err != nil {
		_ = db.Close()
		return finishHistoryFTSReadError(health)
	}
	// The identity key is only meaningful for a schema this build understands.
	// Requiring it unconditionally reported an unknown-NEWER store as corrupt,
	// which is exactly the misclassification the schema ladder below exists to
	// avoid: a newer store must stay read-only, not be declared damaged.
	if schema == historyFTSSchema {
		if err := db.QueryRow(`SELECT value FROM history_fts_meta WHERE key = 'records_fingerprint'`).Scan(&fingerprint); err != nil {
			_ = db.Close()
			return finishHistoryFTSReadError(health)
		}
	}
	if err := db.Close(); err != nil {
		return finishHistoryFTSReadError(health)
	}
	after, err := memoryStateLstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		health["state"] = "unsafe"
		health["schema_state"] = "unsafe"
		health["error_code"] = memoryErrStateUnsafe
		health["action"] = "retry after the FTS leaf stops changing"
		return health
	}
	health["schema_version"] = schema
	health["fingerprint"] = fingerprint
	if schema != historyFTSSchema {
		observed, observedErr := strconv.Atoi(schema)
		supported, supportedErr := strconv.Atoi(historyFTSSchema)
		if observedErr == nil && supportedErr == nil && observed < supported {
			health["state"] = memoryErrMigrationRequired
			health["schema_state"] = memoryErrMigrationRequired
			health["error_code"] = memoryErrMigrationRequired
			health["action"] = "rebuild the disposable FTS store from the current history index"
			return health
		}
		health["state"] = "unsupported"
		health["schema_state"] = "unsupported"
		health["error_code"] = memoryErrUnsupportedVersion
		health["action"] = "upgrade entire-brain; this newer or unknown FTS schema remains read-only"
		return health
	}
	health["schema_state"] = "current"
	if source == nil {
		health["state"] = "stale"
		health["error_code"] = memoryErrSourceStale
		health["action"] = "publish a current canonical history source, then rebuild FTS"
		return health
	}
	index, err := loadBrainHistoryIndex(brainDir, source)
	if err != nil {
		health["state"] = "stale"
		health["error_code"] = memoryErrSourceStale
		health["action"] = "repair the canonical history source before validating or rebuilding FTS"
		return health
	}
	if fingerprint != historyFTSIdentityFromIndex(index).RecordsFingerprint {
		health["state"] = "stale"
		health["error_code"] = memoryErrSourceStale
		health["action"] = "rebuild the disposable FTS store from the current history index"
		return health
	}
	health["state"] = "current"
	return health
}

func finishHistoryFTSReadError(health map[string]any) map[string]any {
	health["state"] = "corrupt"
	health["schema_state"] = "corrupt"
	health["error_code"] = memoryErrStateCorrupt
	health["action"] = "delete and lazily rebuild the disposable FTS store after validating canonical history"
	return health
}

func memoryJobInventoryHealth(inventory memoryJobInventory) map[string]any {
	health := map[string]any{
		"path":                     memoryJobsDirRel,
		"present":                  inventory.EntriesObserved > 0,
		"state":                    "absent",
		"schema_state":             "absent",
		"supported_schema_version": memoryJobSchemaVersion,
		"jobs":                     len(inventory.Jobs),
		"entries_observed":         inventory.EntriesObserved,
		"entries_scanned":          inventory.EntriesScanned,
		"scan_complete":            inventory.ScanComplete,
		"scan_truncated":           inventory.Truncated,
		"degraded":                 inventory.Degraded,
		"action":                   "none",
	}
	issues := append(append([]memoryStateIssue{}, inventory.Issues...), inventory.Migrations...)
	if len(inventory.Jobs) > 0 {
		health["state"] = "current"
		health["schema_state"] = "current"
	}
	versions := map[int]bool{}
	if len(inventory.Jobs) > 0 {
		versions[memoryJobSchemaVersion] = true
	}
	for _, issue := range issues {
		if issue.Version > 0 {
			versions[issue.Version] = true
		}
	}
	observedVersions := make([]int, 0, len(versions))
	for version := range versions {
		observedVersions = append(observedVersions, version)
	}
	sort.Ints(observedVersions)
	if len(observedVersions) > 0 {
		health["schema_versions_observed"] = observedVersions
		health["schema_version"] = observedVersions[len(observedVersions)-1]
	}
	if len(issues) == 0 {
		return health
	}
	health["issue_count"] = len(issues)
	health["issues"] = memoryHealthIssues(issues)
	state, code := memoryInventoryIssueState(issues)
	health["state"] = state
	health["schema_state"] = state
	health["error_code"] = code
	health["mixed"] = len(inventory.Jobs) > 0 || len(observedVersions) > 1
	switch state {
	case "unsafe":
		health["action"] = "remove aliases or special entries from the content-free job inventory"
	case "unsupported":
		health["action"] = "upgrade entire-brain; newer job records remain read-only"
	case "corrupt":
		health["action"] = "repair the named content-free job records before queue mutation"
	case memoryErrMigrationRequired:
		health["action"] = "run `entire memory migrate` to rewrite previous job schemas"
	}
	return health
}

func memoryObservedSchemaVersions(current []int, issues []memoryStateIssue) []int {
	versions := map[int]bool{}
	for _, version := range current {
		if version > 0 {
			versions[version] = true
		}
	}
	for _, issue := range issues {
		if issue.Version > 0 {
			versions[issue.Version] = true
		}
	}
	observed := make([]int, 0, len(versions))
	for version := range versions {
		observed = append(observed, version)
	}
	sort.Ints(observed)
	return observed
}

func memoryInventoryIssueState(issues []memoryStateIssue) (string, string) {
	priority := map[string]int{
		memoryErrMigrationRequired:  1,
		memoryErrStateCorrupt:       2,
		memoryErrUnsupportedVersion: 3,
		memoryErrStateUnsafe:        4,
	}
	selected, selectedPriority := memoryErrStateCorrupt, -1
	for _, issue := range issues {
		if p := priority[issue.Code]; p > selectedPriority {
			selected, selectedPriority = issue.Code, p
		}
	}
	switch selected {
	case memoryErrMigrationRequired:
		return memoryErrMigrationRequired, selected
	case memoryErrUnsupportedVersion:
		return "unsupported", selected
	case memoryErrStateUnsafe:
		return "unsafe", selected
	default:
		return "corrupt", selected
	}
}

func memoryVectorProgressHealth(brainDir string, source *historySourceManifest) map[string]any {
	health := map[string]any{
		"path":                     memoryVectorProgressRel,
		"present":                  false,
		"state":                    "absent",
		"schema_state":             "absent",
		"supported_schema_version": memoryVectorSchema,
		"action":                   "none",
	}
	progress, present, err := loadMemoryVectorProgress(brainDir)
	health["present"] = present
	if err != nil {
		code := memoryErrorCode(err)
		state := "corrupt"
		var loadErr *memoryVectorProgressLoadError
		if errors.As(err, &loadErr) {
			if loadErr.Version > 0 {
				health["schema_version"] = loadErr.Version
			}
			switch loadErr.State {
			case memoryVectorProgressUnsupported:
				state = "unsupported"
			case memoryVectorProgressUnsafe:
				state = "unsafe"
			}
		}
		health["state"] = state
		health["schema_state"] = state
		health["error_code"] = code
		health["action"] = "inspect the content-free vector progress state; unknown newer bytes remain untouched"
		return health
	}
	if !present {
		return health
	}
	health["schema_version"] = progress.SchemaVersion
	health["schema_state"] = "current"
	health["state"] = "pending"
	health["model_id"] = progress.ModelID
	health["source_digest"] = progress.SourceDigest
	health["complete_source_digest"] = progress.CompleteSourceDigest
	health["pending"] = progress.Pending
	health["reset_complete"] = progress.ResetComplete
	health["updated_at"] = progress.UpdatedAt
	if source != nil && source.IndexDigest != "" && progress.CompleteSourceDigest == source.IndexDigest && progress.ResetComplete && !progress.Pending {
		health["state"] = "current"
	} else if source != nil && source.IndexDigest != "" && progress.SourceDigest != source.IndexDigest {
		health["state"] = "stale"
		health["error_code"] = memoryErrSourceStale
		health["action"] = "resume vector synchronization against the current history generation"
	}
	if progress.ErrorCode != "" {
		health["state"] = "degraded"
		health["error_code"] = progress.ErrorCode
		health["action"] = "retry vector synchronization; lexical retrieval remains available"
	}
	return health
}
