package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// memory_state.go is the durable work record: content-free lifecycle hint
// files and per-job state files under history/work/v1/. Jobs are operational
// history; the projection receipt (memory_receipts.go) is the durable proof
// of currentness, and reconciliation against the canonical manifest remains
// the correctness authority. Losing every file here loses no memory.

const (
	memoryWorkDirRel  = historyDirName + "/work/v1"
	memoryHintsDirRel = memoryWorkDirRel + "/hints"
	memoryJobsDirRel  = memoryWorkDirRel + "/jobs"

	memoryHintSchemaVersion = 1
	memoryJobSchemaVersion  = 2

	memoryJobKindProjection      = "deterministic_projection"
	memoryJobKindSessionAbstract = "session_abstract"

	memoryJobStatePending    = "pending"
	memoryJobStateRunning    = "running"
	memoryJobStateComplete   = "complete"
	memoryJobStateRetryable  = "retryable_error"
	memoryJobStateInvalid    = "invalid"
	memoryJobStateExcluded   = "excluded"
	memoryJobStateSuperseded = "superseded"
	memoryJobStateCancelled  = "cancelled"

	memoryJobErrorMaxBytes = 512
	memoryStateIssueLimit  = 100
	// memoryJobMaxAttempts: past it the job stays retryable_error for manual
	// retry and never occupies the runnable head.
	memoryJobMaxAttempts = 5
)

// memoryJobRetryDelays indexes by completed attempt count (1-based attempt
// one .. four); attempt five and beyond is manual-only.
var memoryJobRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// memoryLifecycleHint is a content-free host lifecycle event, coalesced per
// canonical session scope.
type memoryLifecycleHint struct {
	SchemaVersion int       `json:"schema_version"`
	RepoKey       string    `json:"repo_key"`
	SessionID     string    `json:"session_id"`
	Branch        string    `json:"branch,omitempty"`
	LastEvent     string    `json:"last_event"`
	ObservedAt    time.Time `json:"observed_at"`
	Generation    int       `json:"generation"`
}

var memoryHintEvents = map[string]bool{"session_start": true, "checkpoint": true, "session_end": true}

func memoryHintRel(repoKey, sessionID, branch string) string {
	h := sha256.New()
	h.Write([]byte(repoKey))
	h.Write([]byte{0})
	h.Write([]byte(branch))
	h.Write([]byte{0})
	h.Write([]byte(sessionID))
	return filepath.ToSlash(filepath.Join(memoryHintsDirRel, hex.EncodeToString(h.Sum(nil))[:32]+".json"))
}

// writeMemoryHint records or coalesces one lifecycle hint. Repeated events
// update last_event/observed_at and monotonically bump generation.
func writeMemoryHint(brainDir, repoKey, sessionID, branch, event string, now time.Time) (memoryLifecycleHint, error) {
	if !memoryHintEvents[event] {
		return memoryLifecycleHint{}, fmt.Errorf("event must be session_start, checkpoint, or session_end (got %q)", event)
	}
	if strings.TrimSpace(sessionID) == "" {
		return memoryLifecycleHint{}, fmt.Errorf("a non-empty session id is required; sessions without one are discovered by reconciliation")
	}
	if manifest, err := loadBrainManifest(brainDir); err != nil {
		return memoryLifecycleHint{}, err
	} else if strings.TrimSpace(manifest.RepoKey) != "" && repoKey != manifest.RepoKey {
		return memoryLifecycleHint{}, fmt.Errorf("%s: lifecycle hint repository identity does not match this Brain", memoryErrStateUnsafe)
	}
	rel := memoryHintRel(repoKey, sessionID, branch)
	hint := memoryLifecycleHint{SchemaVersion: memoryHintSchemaVersion, RepoKey: repoKey, SessionID: sessionID, Branch: branch}
	if data, present, err := readMemoryStateFile(brainDir, rel, "lifecycle hint", maxManifestBytes); err == nil && present {
		var prev memoryLifecycleHint
		if err := json.Unmarshal(data, &prev); err != nil {
			return memoryLifecycleHint{}, fmt.Errorf("memory_state_corrupt: lifecycle hint cannot be parsed")
		}
		if prev.SchemaVersion != memoryHintSchemaVersion {
			return memoryLifecycleHint{}, fmt.Errorf("memory_unsupported_version: lifecycle hint schema %d", prev.SchemaVersion)
		}
		if prev.RepoKey != repoKey || prev.SessionID != sessionID || prev.Branch != branch || filepath.Base(rel) != filepath.Base(memoryHintRel(prev.RepoKey, prev.SessionID, prev.Branch)) {
			return memoryLifecycleHint{}, fmt.Errorf("%s: lifecycle hint identity does not match its canonical path", memoryErrStateUnsafe)
		}
		hint.Generation = prev.Generation
	} else if err != nil {
		return memoryLifecycleHint{}, err
	}
	hint.Generation++
	hint.LastEvent = event
	hint.ObservedAt = now
	data, err := json.MarshalIndent(hint, "", "  ")
	if err != nil {
		return memoryLifecycleHint{}, err
	}
	return hint, writeBrainRelativeFileAtomic(brainDir, rel, append(data, '\n'), 0o600)
}

type memoryStateIssue struct {
	Kind       string `json:"kind"`
	File       string `json:"file"`
	Code       string `json:"code"`
	Version    int    `json:"version,omitempty"`
	jobID      string
	sessionRef string
}

func appendMemoryStateIssue(issues []memoryStateIssue, issue memoryStateIssue) []memoryStateIssue {
	if len(issues) < memoryStateIssueLimit {
		return append(issues, issue)
	}
	return issues
}

func memoryErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, code := range []string{
		memoryErrSessionInventory,
		memoryErrStateUnsafe,
		memoryErrUnsupportedVersion,
		memoryErrMigrationRequired,
		memoryErrSourceStale,
		memoryErrCancelTooLate,
		memoryErrCancelled,
		memoryErrLockBusy,
		memoryErrQueryTooBroad,
		memoryErrInputTooLarge,
		memoryErrPrivacyExcluded,
		memoryErrPrivacyDirty,
		memoryErrPrivacyBusy,
		memoryErrIdentityAmbiguous,
		memoryErrProviderUnavail,
		memoryErrStateCorrupt,
	} {
		if strings.Contains(message, code) {
			return code
		}
	}
	return memoryErrStateCorrupt
}

type memoryStateLoadError struct {
	Issues []memoryStateIssue
}

func (e *memoryStateLoadError) Error() string {
	if len(e.Issues) == 0 {
		return "memory_state_corrupt"
	}
	return fmt.Sprintf("%s: %s state file %s", e.Issues[0].Code, e.Issues[0].Kind, e.Issues[0].File)
}

// memoryStateSeverityRank orders issue codes so the reported one is the most
// urgent rather than whichever the filesystem happened to enumerate first.
func memoryStateSeverityRank(code string) int {
	switch code {
	case memoryErrStateUnsafe:
		return 0
	case memoryErrStateCorrupt:
		return 1
	case memoryErrMigrationRequired:
		return 2
	case memoryErrUnsupportedVersion:
		return 3
	default:
		return 4
	}
}

func memoryStateError(issues []memoryStateIssue) error {
	if len(issues) == 0 {
		return nil
	}
	// Error() reports Issues[0], and the issues arrive in directory-read order,
	// which is creation-ordered on APFS but hash-ordered on ext4. The same
	// damaged Brain therefore produced different text on different machines.
	// Sort a copy (the caller's slice keeps its discovery order) most severe
	// first, tie-broken by kind and file so the message is fully determined by
	// the state on disk.
	ordered := append([]memoryStateIssue(nil), issues...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := memoryStateSeverityRank(ordered[i].Code), memoryStateSeverityRank(ordered[j].Code)
		if ri != rj {
			return ri < rj
		}
		if ordered[i].Kind != ordered[j].Kind {
			return ordered[i].Kind < ordered[j].Kind
		}
		return ordered[i].File < ordered[j].File
	})
	return &memoryStateLoadError{Issues: ordered}
}

type memoryHintInventory struct {
	Hints           []memoryLifecycleHint
	Issues          []memoryStateIssue
	EntriesObserved int
	Truncated       bool
	Degraded        bool
}

func loadMemoryHintInventory(brainDir string) memoryHintInventory {
	directory, err := readMemoryStateDirectory(brainDir, memoryHintsDirRel, "hint directory", memoryStateInventoryMaxEntries)
	inventory := memoryHintInventory{EntriesObserved: directory.Total, Truncated: directory.Truncated, Degraded: directory.Degraded}
	if err != nil {
		inventory.Issues = []memoryStateIssue{{Kind: "hint_directory", File: filepath.Base(memoryHintsDirRel), Code: memoryErrorCode(err)}}
		return inventory
	}
	if directory.Truncated {
		inventory.Issues = appendMemoryStateIssue(inventory.Issues, memoryStateIssue{Kind: "hint_directory", File: filepath.Base(memoryHintsDirRel), Code: memoryErrStateUnsafe})
	}
	manifest, manifestErr := loadBrainManifest(brainDir)
	expectedRepoKey := ""
	if manifestErr == nil {
		expectedRepoKey = manifest.RepoKey
	}
	// A newly initialized/repair fixture may not have a manifest yet. In that
	// narrow case the self-authenticating canonical basename is still enforced;
	// once a manifest exists its RepoKey is authoritative.
	for _, entry := range directory.Entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			inventory.Degraded = true
			inventory.Issues = appendMemoryStateIssue(inventory.Issues, memoryStateIssue{Kind: "hint", File: entry.Name(), Code: memoryErrStateUnsafe})
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			inventory.Degraded = true
			inventory.Issues = appendMemoryStateIssue(inventory.Issues, memoryStateIssue{Kind: "hint", File: entry.Name(), Code: memoryErrStateUnsafe})
			continue
		}
		rel := filepath.ToSlash(filepath.Join(memoryHintsDirRel, entry.Name()))
		data, present, err := readMemoryStateFileRefreshed(brainDir, rel, "lifecycle hint", maxManifestBytes, info)
		if err != nil || !present {
			inventory.Degraded = true
			inventory.Issues = appendMemoryStateIssue(inventory.Issues, memoryStateIssue{Kind: "hint", File: entry.Name(), Code: memoryErrorCode(err)})
			continue
		}
		var hint memoryLifecycleHint
		if err := json.Unmarshal(data, &hint); err != nil {
			inventory.Degraded = true
			inventory.Issues = appendMemoryStateIssue(inventory.Issues, memoryStateIssue{Kind: "hint", File: entry.Name(), Code: memoryErrStateCorrupt})
			continue
		}
		if hint.SchemaVersion != memoryHintSchemaVersion {
			inventory.Degraded = true
			inventory.Issues = appendMemoryStateIssue(inventory.Issues, memoryStateIssue{Kind: "hint", File: entry.Name(), Code: "memory_unsupported_version"})
			continue
		}
		if (expectedRepoKey != "" && hint.RepoKey != expectedRepoKey) || hint.RepoKey == "" || hint.SessionID == "" || hint.Generation < 1 || !memoryHintEvents[hint.LastEvent] || entry.Name() != filepath.Base(memoryHintRel(hint.RepoKey, hint.SessionID, hint.Branch)) {
			inventory.Degraded = true
			inventory.Issues = appendMemoryStateIssue(inventory.Issues, memoryStateIssue{Kind: "hint", File: entry.Name(), Code: memoryErrStateUnsafe})
			continue
		}
		inventory.Hints = append(inventory.Hints, hint)
	}
	sort.Slice(inventory.Hints, func(i, j int) bool { return inventory.Hints[i].SessionID < inventory.Hints[j].SessionID })
	return inventory
}

func loadMemoryHintsChecked(brainDir string) ([]memoryLifecycleHint, []memoryStateIssue) {
	inventory := loadMemoryHintInventory(brainDir)
	return inventory.Hints, inventory.Issues
}

func loadMemoryHints(brainDir string) []memoryLifecycleHint {
	hints, _ := loadMemoryHintsChecked(brainDir)
	return hints
}

// removeMemoryHint deletes a hint only when its generation still matches, so
// an event racing cleanup is never lost. Caller holds the write lock.
func removeMemoryHint(brainDir string, hint memoryLifecycleHint) error {
	_, err := removeMemoryHintIfCurrent(brainDir, hint)
	return err
}

func removeMemoryHintIfCurrent(brainDir string, hint memoryLifecycleHint) (bool, error) {
	if manifest, err := loadBrainManifest(brainDir); err == nil && strings.TrimSpace(manifest.RepoKey) != "" && hint.RepoKey != manifest.RepoKey {
		return false, fmt.Errorf("%s: lifecycle hint repository identity does not match this Brain", memoryErrStateUnsafe)
	}
	rel := memoryHintRel(hint.RepoKey, hint.SessionID, hint.Branch)
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	data, present, err := readMemoryStateFile(brainDir, rel, "lifecycle hint", maxManifestBytes)
	if err != nil {
		return false, err
	}
	if !present {
		return false, nil
	}
	var current memoryLifecycleHint
	if err := json.Unmarshal(data, &current); err != nil {
		return false, fmt.Errorf("memory_state_corrupt: lifecycle hint cannot be parsed")
	}
	if current.SchemaVersion != memoryHintSchemaVersion {
		return false, fmt.Errorf("memory_unsupported_version: lifecycle hint schema %d", current.SchemaVersion)
	}
	if current.RepoKey != hint.RepoKey || current.SessionID != hint.SessionID || current.Branch != hint.Branch || filepath.Base(rel) != filepath.Base(memoryHintRel(current.RepoKey, current.SessionID, current.Branch)) {
		return false, fmt.Errorf("%s: lifecycle hint identity does not match its canonical path", memoryErrStateUnsafe)
	}
	if current.Generation > hint.Generation {
		return false, nil // a newer event arrived; keep the hint
	}
	if current.Generation != hint.Generation {
		return false, fmt.Errorf("memory_state_corrupt: lifecycle hint generation regressed")
	}
	if current.LastEvent != hint.LastEvent || !current.ObservedAt.Equal(hint.ObservedAt) {
		return false, fmt.Errorf("%s: lifecycle hint changed without advancing generation", memoryErrStateUnsafe)
	}
	err = os.Remove(full)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

// memoryJob is one durable, content-free unit of deterministic projection
// work, keyed by repository, session scope, input digest, and kind.
type memoryJob struct {
	SchemaVersion     int                        `json:"schema_version"`
	JobID             string                     `json:"job_id"`
	Kind              string                     `json:"kind"`
	RepoKey           string                     `json:"repo_key"`
	SessionID         string                     `json:"session_id"`
	SessionRef        string                     `json:"session_ref"`
	Branch            string                     `json:"branch,omitempty"`
	InputDigest       string                     `json:"input_digest"`
	Trigger           string                     `json:"trigger"`
	State             string                     `json:"state"`
	Attempt           int                        `json:"attempt"`
	CreatedAt         time.Time                  `json:"created_at"`
	AvailableAt       time.Time                  `json:"available_at"`
	StartedAt         *time.Time                 `json:"started_at,omitempty"`
	FinishedAt        *time.Time                 `json:"finished_at,omitempty"`
	HeartbeatAt       *time.Time                 `json:"heartbeat_at,omitempty"`
	OwnerToken        string                     `json:"owner_token,omitempty"`
	CancelRequestedAt *time.Time                 `json:"cancel_requested_at,omitempty"`
	Supersedes        string                     `json:"supersedes,omitempty"`
	AbstractOverride  *memoryAbstractJobOverride `json:"abstract_override,omitempty"`
	Error             *memoryJobError            `json:"error,omitempty"`
	HealthIssues      []string                   `json:"health_issues,omitempty"`
	sourcePaths       []string
}

type memoryJobError struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func (e *memoryJobError) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var legacy string
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		converted := newMemoryJobError(legacy)
		if converted == nil {
			*e = memoryJobError{}
		} else {
			*e = *converted
		}
		return nil
	}
	type plain memoryJobError
	return json.Unmarshal(data, (*plain)(e))
}

func memoryJobID(repoKey, sessionRef, inputDigest, kind string) string {
	h := sha256.New()
	for _, part := range []string{repoKey, sessionRef, inputDigest, kind} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "job:" + hex.EncodeToString(h.Sum(nil))[:32]
}

func memoryJobRel(jobID string) string {
	// Job IDs contain a colon, which is not a valid Windows filename. Hashing
	// the already-stable ID also prevents platform-specific escaping rules.
	sum := sha256.Sum256([]byte(jobID))
	return filepath.ToSlash(filepath.Join(memoryJobsDirRel, hex.EncodeToString(sum[:])[:40]+".json"))
}

func legacyMemoryJobRel(jobID string) string {
	return filepath.ToSlash(filepath.Join(memoryJobsDirRel, url.PathEscape(jobID)+".json"))
}

// memoryJobTransitions is the allowed state machine.
var memoryJobTransitions = map[string]map[string]bool{
	memoryJobStatePending:   {memoryJobStateRunning: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
	memoryJobStateRunning:   {memoryJobStateComplete: true, memoryJobStateRetryable: true, memoryJobStateInvalid: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
	memoryJobStateRetryable: {memoryJobStatePending: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
	memoryJobStateComplete:  {memoryJobStatePending: true, memoryJobStateSuperseded: true},
	memoryJobStateInvalid:   {memoryJobStatePending: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
	memoryJobStateCancelled: {memoryJobStatePending: true, memoryJobStateComplete: true},
}

func saveMemoryJob(brainDir string, job memoryJob) error {
	job.SchemaVersion = memoryJobSchemaVersion
	if job.Error != nil && len(job.Error.Detail) > memoryJobErrorMaxBytes {
		job.Error.Detail, _ = truncateUTF8Bytes(job.Error.Detail, memoryJobErrorMaxBytes)
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, memoryJobRel(job.JobID), append(data, '\n'), 0o600)
}

func validMemoryJobState(state string) bool {
	if _, ok := memoryJobTransitions[state]; ok {
		return true
	}
	switch state {
	case memoryJobStateExcluded, memoryJobStateSuperseded, memoryJobStateCancelled:
		return true
	}
	return false
}

func validMemoryJobKind(kind string) bool {
	return kind == memoryJobKindProjection || kind == memoryJobKindSessionAbstract
}

type memoryJobInventory struct {
	Jobs               []memoryJob
	Issues             []memoryStateIssue
	IssueCount         int
	Migrations         []memoryStateIssue
	EntriesObserved    int
	EntriesScanned     int
	Truncated          bool
	ScanComplete       bool
	Degraded           bool
	issuesByFile       map[string]memoryStateIssue
	issuesByJobID      map[string]memoryStateIssue
	issuesBySessionRef map[string]memoryStateIssue
	pathsByJobID       map[string][]string
}

func (inventory *memoryJobInventory) recordIssue(issue memoryStateIssue) {
	inventory.IssueCount++
	inventory.Degraded = true
	if inventory.issuesByFile == nil {
		inventory.issuesByFile = map[string]memoryStateIssue{}
		inventory.issuesByJobID = map[string]memoryStateIssue{}
		inventory.issuesBySessionRef = map[string]memoryStateIssue{}
		inventory.pathsByJobID = map[string][]string{}
	}
	inventory.issuesByFile[issue.File] = issue
	if issue.jobID != "" {
		inventory.issuesByJobID[issue.jobID] = issue
	}
	if issue.sessionRef != "" {
		inventory.issuesBySessionRef[issue.sessionRef] = issue
	}
	inventory.Issues = appendMemoryStateIssue(inventory.Issues, issue)
}

func (inventory memoryJobInventory) issueForJobID(jobID string) (memoryStateIssue, bool) {
	if issue, ok := inventory.issuesByJobID[jobID]; ok {
		return issue, true
	}
	issue, ok := inventory.issuesByFile[filepath.Base(memoryJobRel(jobID))]
	return issue, ok
}

func (inventory memoryJobInventory) issueForSessionRef(sessionRef string) (memoryStateIssue, bool) {
	issue, ok := inventory.issuesBySessionRef[sessionRef]
	return issue, ok
}

func memoryJobEnumerationError(issues []memoryStateIssue) error {
	for _, issue := range issues {
		if issue.Kind == "job_directory" {
			return &memoryStateLoadError{Issues: []memoryStateIssue{issue}}
		}
	}
	return nil
}

func loadMemoryJobInventory(brainDir string) memoryJobInventory {
	return loadMemoryJobInventoryPage(brainDir, 0, nil)
}

// loadMemoryJobInventoryPage enumerates a bounded name inventory but opens job
// leaves only until maxAccepted matching jobs have been decoded. This is for
// the read-only jobs command; completeness-dependent operations use the full
// loader above and fail closed at the shared ceiling.
func loadMemoryJobInventoryPage(brainDir string, maxAccepted int, accept func(memoryJob) bool) memoryJobInventory {
	inventory := memoryJobInventory{
		issuesByFile:       map[string]memoryStateIssue{},
		issuesByJobID:      map[string]memoryStateIssue{},
		issuesBySessionRef: map[string]memoryStateIssue{},
		pathsByJobID:       map[string][]string{},
		ScanComplete:       true,
	}
	dirRel := filepath.FromSlash(memoryJobsDirRel)
	directory, err := readMemoryStateDirectory(brainDir, filepath.ToSlash(dirRel), "job directory", memoryStateInventoryMaxEntries)
	if err != nil {
		// Nothing was enumerated at all, so this is the LEAST complete scan
		// there is. Returning with the initialised ScanComplete = true made
		// `memory jobs --json` report an empty queue as fully observed and
		// suppressed the "bounded partial inventory" warning.
		inventory.ScanComplete = false
		inventory.recordIssue(memoryStateIssue{Kind: "job_directory", File: filepath.Base(memoryJobsDirRel), Code: memoryErrorCode(err)})
		return inventory
	}
	inventory.EntriesObserved = directory.Total
	inventory.Truncated = directory.Truncated
	inventory.ScanComplete = !directory.Truncated
	inventory.Degraded = directory.Degraded
	if directory.Truncated {
		inventory.recordIssue(memoryStateIssue{Kind: "job_directory", File: filepath.Base(memoryJobsDirRel), Code: memoryErrStateUnsafe})
	}
	entries := directory.Entries
	manifest, manifestErr := loadBrainManifest(brainDir)
	expectedRepoKey := ""
	if manifestErr == nil {
		expectedRepoKey = strings.TrimSpace(manifest.RepoKey)
	}
	type loadedMemoryJob struct {
		job     memoryJob
		version int
	}
	byID := map[string]loadedMemoryJob{}
	for entryIndex, entry := range entries {
		inventory.EntriesScanned++
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: memoryErrStateUnsafe})
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: memoryErrStateUnsafe})
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(memoryJobsDirRel, entry.Name()))
		if err := rejectBrainRootPathSymlinks(brainDir, filepath.FromSlash(rel)); err != nil {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: memoryErrStateUnsafe})
			continue
		}
		data, present, err := readMemoryStateFileRefreshed(brainDir, rel, "memory job", maxManifestBytes, info)
		if err != nil || !present {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: memoryErrorCode(err)})
			continue
		}
		var header struct {
			SchemaVersion int    `json:"schema_version"`
			JobID         string `json:"job_id"`
			SessionRef    string `json:"session_ref"`
		}
		if err := json.Unmarshal(data, &header); err != nil {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_state_corrupt"})
			continue
		}
		if header.SchemaVersion > memoryJobSchemaVersion {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_unsupported_version", Version: header.SchemaVersion, jobID: header.JobID, sessionRef: header.SessionRef})
			continue
		}
		var job memoryJob
		if err := json.Unmarshal(data, &job); err != nil {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_state_corrupt", Version: header.SchemaVersion, jobID: header.JobID, sessionRef: header.SessionRef})
			continue
		}
		onDiskVersion := job.SchemaVersion
		if job.SchemaVersion != 1 && job.SchemaVersion != memoryJobSchemaVersion {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_state_corrupt", Version: job.SchemaVersion, jobID: job.JobID, sessionRef: job.SessionRef})
			continue
		}
		// v1 used a string-valued error. memoryJobError.UnmarshalJSON adapts
		// it to the v2 structured/redacted form; the next state write upgrades
		// the durable record without blocking queue progress.
		job.SchemaVersion = memoryJobSchemaVersion
		if job.JobID == "" || !validMemoryJobKind(job.Kind) || job.RepoKey == "" || (expectedRepoKey != "" && job.RepoKey != expectedRepoKey) || job.SessionRef == "" || job.InputDigest == "" ||
			job.JobID != memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind) || !validMemoryJobState(job.State) || job.Attempt < 0 || job.CreatedAt.IsZero() {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_state_corrupt", Version: onDiskVersion, jobID: job.JobID, sessionRef: job.SessionRef})
			continue
		}
		if job.Error != nil && (!validMemoryJobErrorCode(job.Error.Code) || len(job.Error.Detail) > memoryJobErrorMaxBytes) {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_state_corrupt", Version: onDiskVersion, jobID: job.JobID, sessionRef: job.SessionRef})
			continue
		}
		if (job.Kind != memoryJobKindSessionAbstract && job.AbstractOverride != nil) || !validMemoryAbstractJobOverride(job.AbstractOverride) {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: memoryErrStateCorrupt, Version: onDiskVersion, jobID: job.JobID, sessionRef: job.SessionRef})
			continue
		}
		healthValid := true
		for _, code := range job.HealthIssues {
			if code != "memory_worker_log_write_failed" && code != "memory_sidecar_write_failed" && code != "memory_heartbeat_write_failed" {
				healthValid = false
				break
			}
		}
		if !healthValid {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: memoryErrStateCorrupt, Version: onDiskVersion, jobID: job.JobID, sessionRef: job.SessionRef})
			continue
		}
		canonicalName := filepath.Base(memoryJobRel(job.JobID))
		legacyName := filepath.Base(legacyMemoryJobRel(job.JobID))
		if entry.Name() != canonicalName && !(onDiskVersion == 1 && entry.Name() == legacyName) {
			inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_state_corrupt", Version: onDiskVersion, jobID: job.JobID, sessionRef: job.SessionRef})
			continue
		}
		if accept != nil && !accept(job) {
			continue
		}
		inventory.pathsByJobID[job.JobID] = append(inventory.pathsByJobID[job.JobID], rel)
		if onDiskVersion == 1 {
			inventory.Migrations = appendMemoryStateIssue(inventory.Migrations, memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_migration_required", Version: 1, jobID: job.JobID, sessionRef: job.SessionRef})
		}
		if prior, exists := byID[job.JobID]; exists {
			if onDiskVersion > prior.version {
				byID[job.JobID] = loadedMemoryJob{job: job, version: onDiskVersion}
			} else if onDiskVersion == prior.version {
				inventory.recordIssue(memoryStateIssue{Kind: "job", File: entry.Name(), Code: "memory_state_corrupt", Version: onDiskVersion, jobID: job.JobID, sessionRef: job.SessionRef})
			}
			continue
		}
		byID[job.JobID] = loadedMemoryJob{job: job, version: onDiskVersion}
		if maxAccepted > 0 && len(byID) >= maxAccepted {
			if entryIndex+1 < len(entries) || directory.Truncated {
				inventory.ScanComplete = false
			}
			break
		}
	}
	for _, loaded := range byID {
		if _, blocked := inventory.issueForJobID(loaded.job.JobID); blocked {
			continue
		}
		loaded.job.sourcePaths = append([]string(nil), inventory.pathsByJobID[loaded.job.JobID]...)
		inventory.Jobs = append(inventory.Jobs, loaded.job)
	}
	sort.Slice(inventory.Jobs, func(i, j int) bool { return inventory.Jobs[i].JobID < inventory.Jobs[j].JobID })
	return inventory
}

func loadMemoryJobsChecked(brainDir string) ([]memoryJob, []memoryStateIssue) {
	inventory := loadMemoryJobInventory(brainDir)
	return inventory.Jobs, inventory.Issues
}

func loadMemoryJobs(brainDir string) []memoryJob {
	jobs, _ := loadMemoryJobsChecked(brainDir)
	return jobs
}

func memoryJobSourcePaths(job memoryJob) []string {
	if len(job.sourcePaths) == 0 {
		return []string{memoryJobRel(job.JobID)}
	}
	return append([]string(nil), job.sourcePaths...)
}

func scanMemoryJobSchemas(brainDir string) []memoryStateIssue {
	inventory := loadMemoryJobInventory(brainDir)
	var findings []memoryStateIssue
	for _, issue := range append(append([]memoryStateIssue{}, inventory.Issues...), inventory.Migrations...) {
		findings = appendMemoryStateIssue(findings, issue)
	}
	return findings
}

// transitionMemoryJob applies one validated state transition and persists it.
func transitionMemoryJob(brainDir string, job memoryJob, state string, now time.Time, errText string) (memoryJob, error) {
	allowed := memoryJobTransitions[job.State]
	if !allowed[state] {
		return job, fmt.Errorf("memory_state_corrupt: job %s cannot move %s -> %s", job.JobID, job.State, state)
	}
	ownerForLog := job.OwnerToken
	job.State = state
	job.Error = newMemoryJobError(errText)
	switch state {
	case memoryJobStateRunning:
		job.Attempt++
		started := now
		job.StartedAt = &started
		job.FinishedAt = nil
		job.CancelRequestedAt = nil
	case memoryJobStateComplete, memoryJobStateRetryable, memoryJobStateInvalid, memoryJobStateExcluded, memoryJobStateSuperseded, memoryJobStateCancelled:
		finished := now
		job.FinishedAt = &finished
		if state == memoryJobStateRetryable {
			delayIndex := job.Attempt - 1
			if delayIndex >= 0 && delayIndex < len(memoryJobRetryDelays) {
				job.AvailableAt = now.Add(memoryJobRetryDelays[delayIndex])
			} else {
				// Manual-only: park far in the future; retry resets it.
				job.AvailableAt = now.Add(100 * 365 * 24 * time.Hour)
			}
		}
	}
	if state != memoryJobStateRunning {
		job.OwnerToken = ""
		job.HeartbeatAt = nil
	}
	if state == memoryJobStatePending {
		job.StartedAt = nil
		job.FinishedAt = nil
		job.CancelRequestedAt = nil
	}
	if err := saveMemoryJob(brainDir, job); err != nil {
		return job, err
	}
	durationClass := "instant"
	if job.StartedAt != nil {
		duration := now.Sub(*job.StartedAt)
		switch {
		case duration >= time.Minute:
			durationClass = "long"
		case duration >= 10*time.Second:
			durationClass = "medium"
		case duration >= time.Second:
			durationClass = "short"
		}
	}
	jobPart := strings.TrimPrefix(job.JobID, "job:")
	if len(jobPart) > 8 {
		jobPart = jobPart[:8]
	}
	errorCode := "none"
	if job.Error != nil {
		errorCode = job.Error.Code
	}
	if logErr := appendMemoryWorkerLog(brainDir, now, "job_"+state+"_"+jobPart+"_"+durationClass+"_"+errorCode, ownerForLog); logErr != nil {
		seen := false
		for _, code := range job.HealthIssues {
			seen = seen || code == "memory_worker_log_write_failed"
		}
		if !seen {
			job.HealthIssues = append(job.HealthIssues, "memory_worker_log_write_failed")
			// The state transition is already durable. A second best-effort save
			// surfaces degraded observability without converting completed work
			// into a failed job when the sidecar path alone is unavailable.
			_ = saveMemoryJob(brainDir, job)
		}
	}
	return job, nil
}

// completeMemoryJobFromProof settles a job when a current durable receipt is
// already the proof of completion. It deliberately does not synthesize a
// running attempt: a shared projection can satisfy jobs that this pass never
// needed to claim.
func completeMemoryJobFromProof(brainDir string, job memoryJob, now time.Time) (memoryJob, error) {
	job.State = memoryJobStateComplete
	job.Error = nil
	job.OwnerToken = ""
	job.HeartbeatAt = nil
	job.CancelRequestedAt = nil
	finished := now
	job.FinishedAt = &finished
	if err := saveMemoryJob(brainDir, job); err != nil {
		return job, err
	}
	if err := removeMemoryCancellationRequest(brainDir, job.JobID); err != nil {
		return job, err
	}
	return job, nil
}

// restoreClaimedMemoryJob returns an unaffected co-claim to its exact runnable
// state after a different job cancels a shared projection generation. The
// abandoned claim is not an attempt and receives no retry delay or penalty.
func restoreClaimedMemoryJob(brainDir string, job memoryJob, now time.Time) (memoryJob, error) {
	if job.State != memoryJobStateRunning || job.Attempt < 1 {
		return job, fmt.Errorf("%s: cannot restore unclaimed job %s", memoryErrStateCorrupt, job.JobID)
	}
	owner := job.OwnerToken
	job.State = memoryJobStatePending
	job.Attempt--
	job.StartedAt = nil
	job.FinishedAt = nil
	job.HeartbeatAt = nil
	job.OwnerToken = ""
	job.CancelRequestedAt = nil
	job.Error = nil
	if err := saveMemoryJob(brainDir, job); err != nil {
		return job, err
	}
	if err := appendMemoryWorkerLog(brainDir, now, "job_claim_restored", owner); err != nil {
		job.HealthIssues = append(job.HealthIssues, "memory_worker_log_write_failed")
		_ = saveMemoryJob(brainDir, job)
	}
	return job, nil
}

// runnableMemoryJobs returns pending jobs whose available_at has passed, in
// fair deterministic order (available_at, created_at, job id).
func runnableMemoryJobs(jobs []memoryJob, now time.Time) []memoryJob {
	var runnable []memoryJob
	for _, job := range jobs {
		if (job.State == memoryJobStatePending || (job.State == memoryJobStateRetryable && job.Attempt < memoryJobMaxAttempts)) && !job.AvailableAt.After(now) {
			runnable = append(runnable, job)
		}
	}
	sort.Slice(runnable, func(i, j int) bool {
		if memoryJobKindPriority(runnable[i].Kind) != memoryJobKindPriority(runnable[j].Kind) {
			return memoryJobKindPriority(runnable[i].Kind) < memoryJobKindPriority(runnable[j].Kind)
		}
		// An explicit foreground request still uses the durable coordinator,
		// retry, and cancellation path, but it must not be stranded behind a
		// bounded automatic-drain share. At most one canonical job exists for
		// the session digest, so this priority cannot create a competing writer.
		if (runnable[i].Trigger == "manual") != (runnable[j].Trigger == "manual") {
			return runnable[i].Trigger == "manual"
		}
		if !runnable[i].AvailableAt.Equal(runnable[j].AvailableAt) {
			return runnable[i].AvailableAt.Before(runnable[j].AvailableAt)
		}
		if !runnable[i].CreatedAt.Equal(runnable[j].CreatedAt) {
			return runnable[i].CreatedAt.Before(runnable[j].CreatedAt)
		}
		return runnable[i].JobID < runnable[j].JobID
	})
	return runnable
}

func nextMemoryWorkerDelay(brainDir string, now time.Time) (time.Duration, bool, error) {
	inventory := loadMemoryJobInventory(brainDir)
	if err := memoryJobEnumerationError(inventory.Issues); err != nil {
		return 0, false, err
	}
	jobs := inventory.Jobs
	var earliest time.Time
	for _, job := range jobs {
		if job.State != memoryJobStatePending && !(job.State == memoryJobStateRetryable && job.Attempt < memoryJobMaxAttempts) {
			continue
		}
		if earliest.IsZero() || job.AvailableAt.Before(earliest) {
			earliest = job.AvailableAt
		}
	}
	if earliest.IsZero() {
		return 0, false, nil
	}
	delay := earliest.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true, nil
}

func memoryJobKindPriority(kind string) int {
	if kind == memoryJobKindProjection {
		return 0
	}
	if kind == memoryJobKindSessionAbstract {
		return 10
	}
	return 100
}

// recoverStaleMemoryJobsLocked may only be called by a worker while it owns
// the per-Brain coordinator OS lock. A different owner token is then proof
// that the prior process can no longer be active, even when its last
// heartbeat is recent (for example, a kill immediately after claim).
func recoverStaleMemoryJobsLocked(brainDir string, jobs []memoryJob, now time.Time, currentOwner string) (int, error) {
	if currentOwner == "" {
		return 0, errors.New("memory_state_corrupt: stale recovery requires coordinator ownership")
	}
	recovered := 0
	for _, job := range jobs {
		if job.State != memoryJobStateRunning {
			continue
		}
		if job.OwnerToken == currentOwner {
			continue
		}
		if _, err := transitionMemoryJob(brainDir, job, memoryJobStateRetryable, now, "worker_lost"); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func newMemoryJobError(text string) *memoryJobError {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if validMemoryJobErrorCode(text) && !strings.ContainsAny(text, " \t\r\n:/\\") {
		return &memoryJobError{Code: text}
	}
	// Callers must never persist raw provider/source errors. Retain a stable
	// redacted category and no potentially content-bearing detail.
	return &memoryJobError{Code: "memory_operation_failed", Detail: "redacted"}
}

func validMemoryJobErrorCode(code string) bool {
	return code == "worker_lost" || strings.HasPrefix(code, "memory_")
}

func requestMemoryJobCancellation(brainDir string, job memoryJob, now time.Time) (memoryJob, error) {
	if job.State != memoryJobStateRunning {
		return job, errors.New(memoryErrCancelTooLate + ": job is no longer running")
	}
	job.CancelRequestedAt = &now
	return job, writeMemoryCancellationRequest(brainDir, job, now)
}

// memoryOperationalError keeps durable work metadata content-free. Detailed
// provider/process errors stay in the foreground command response; the job
// ledger retains only stable operational categories.
func memoryOperationalError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, code := range []string{
		"memory_provider_unavailable",
		"memory_source_stale",
		"memory_privacy_blocked",
		"memory_privacy_excluded",
		memoryErrPrivacyBusy,
		"memory_cancelled",
		"memory_state_corrupt",
		"memory_state_unsafe",
		"memory_unsupported_version",
		"memory_query_too_broad",
		"memory_lock_busy",
	} {
		if strings.Contains(message, code) {
			return code
		}
	}
	return "memory_operation_failed"
}

// purgeMemoryWorkForSessionLocked removes all disposable operational metadata
// for a privacy-purged raw session. The caller holds the Brain write lock and
// invokes this in the same privacy transaction as source/derived deletion.
func purgeMemoryWorkForSessionLocked(brainDir, sessionID string) error {
	hints, hintIssues := loadMemoryHintsChecked(brainDir)
	inventory := loadMemoryJobInventory(brainDir)
	if err := memoryStateError(append(hintIssues, inventory.Issues...)); err != nil {
		return err
	}
	for _, hint := range hints {
		if hint.SessionID != sessionID {
			continue
		}
		if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(memoryHintRel(hint.RepoKey, hint.SessionID, hint.Branch)))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, job := range inventory.Jobs {
		if job.SessionID != sessionID {
			continue
		}
		if err := removeMemoryCancellationRequest(brainDir, job.JobID); err != nil {
			return err
		}
		for _, rel := range memoryJobSourcePaths(job) {
			if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
