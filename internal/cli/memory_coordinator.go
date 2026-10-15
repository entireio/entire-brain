package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	memoryCoordinatorSchema    = 1
	memoryCancellationSchema   = 1
	memoryWorkerOriginEnv      = "ENTIRE_BRAIN_MEMORY_WORKER"
	memoryCoordinatorLockName  = "memory-worker.lock"
	memoryCoordinatorStateRel  = memoryWorkDirRel + "/coordinator.json"
	memoryWorkerLogRel         = memoryWorkDirRel + "/worker.log"
	memoryWorkerLogArchiveRel  = memoryWorkDirRel + "/worker.log.1"
	memoryWorkerLogArchive2Rel = memoryWorkDirRel + "/worker.log.2"
	memoryCancelDirRel         = memoryWorkDirRel + "/cancellations"
	memoryWorkerLogMaxBytes    = 1024 * 1024
	memoryTerminalRetention    = 30 * 24 * time.Hour
	memoryPruneLimit           = 100
)

func memoryWorkerOrigin() bool {
	return os.Getenv(memoryWorkerOriginEnv) == "1"
}

var errMemoryCancellationRequested = errors.New("memory_cancelled")

type memoryCancellationRequest struct {
	SchemaVersion int       `json:"schema_version"`
	JobID         string    `json:"job_id"`
	RequestedAt   time.Time `json:"requested_at"`
}

type memoryCoordinatorState struct {
	SchemaVersion int        `json:"schema_version"`
	OwnerToken    string     `json:"owner_token,omitempty"`
	PID           int        `json:"pid,omitempty"`
	State         string     `json:"state"`
	StartedAt     time.Time  `json:"started_at"`
	HeartbeatAt   time.Time  `json:"heartbeat_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	LastOutcome   string     `json:"last_outcome,omitempty"`
	BinaryVersion string     `json:"binary_version,omitempty"`
}

type memoryCoordinator struct {
	brainDir     string
	token        string
	started      time.Time
	version      string
	lock         *fileLock
	mu           sync.Mutex
	healthIssues []string
}

func newMemoryOwnerToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func acquireMemoryCoordinator(brainDir string, now time.Time, version string) (*memoryCoordinator, error) {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, brainLockDirName); err != nil {
		return nil, err
	}
	lock, err := acquireFileLock(filepath.Join(brainDir, brainLockDirName, memoryCoordinatorLockName), "memory_worker_active", 0)
	if err != nil {
		return nil, err
	}
	token, err := newMemoryOwnerToken()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	c := &memoryCoordinator{brainDir: brainDir, token: token, started: now, version: version, lock: lock}
	if err := c.writeState("running", now, nil, ""); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := appendMemoryWorkerLog(brainDir, now, "worker_started", token); err != nil {
		c.recordHealthIssue("memory_worker_log_write_failed")
	}
	return c, nil
}

func (c *memoryCoordinator) heartbeat(now time.Time) error {
	err := c.writeState("running", now, nil, "")
	if err != nil {
		c.recordHealthIssue("memory_heartbeat_write_failed")
	}
	return err
}

func (c *memoryCoordinator) close(now time.Time, outcome string) {
	if c == nil {
		return
	}
	if err := c.writeState("idle", now, &now, outcome); err != nil {
		c.recordHealthIssue("memory_coordinator_state_write_failed")
	}
	if err := appendMemoryWorkerLog(c.brainDir, now, "worker_"+outcome, c.token); err != nil {
		c.recordHealthIssue("memory_worker_log_write_failed")
	}
	if c.lock != nil {
		_ = c.lock.Close()
		c.lock = nil
	}
}

func (c *memoryCoordinator) recordHealthIssue(code string) {
	if c == nil || code == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.healthIssues {
		if existing == code {
			return
		}
	}
	c.healthIssues = append(c.healthIssues, code)
}

func (c *memoryCoordinator) healthIssuesSnapshot() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.healthIssues...)
}

func (c *memoryCoordinator) writeState(state string, heartbeat time.Time, finished *time.Time, outcome string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Holding the coordinator lock authorizes stale current-version recovery,
	// but never authorizes down-converting an exact leaf written by a newer
	// binary or replacing state whose bytes/identity cannot be trusted.
	if _, err := loadMemoryCoordinatorState(c.brainDir); err != nil {
		return err
	}
	payload := memoryCoordinatorState{
		SchemaVersion: memoryCoordinatorSchema,
		OwnerToken:    c.token,
		PID:           os.Getpid(),
		State:         state,
		StartedAt:     c.started,
		HeartbeatAt:   heartbeat,
		FinishedAt:    finished,
		LastOutcome:   outcome,
		BinaryVersion: c.version,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(c.brainDir, memoryCoordinatorStateRel, append(data, '\n'), 0o600)
}

func memoryCoordinatorHealth(state memoryCoordinatorState, now time.Time) map[string]any {
	health := map[string]any{"state": state}
	if state.HeartbeatAt.IsZero() {
		health["heartbeat_present"] = false
		return health
	}
	age := now.Sub(state.HeartbeatAt)
	if age < 0 {
		age = 0
	}
	health["heartbeat_present"] = true
	health["heartbeat_age_seconds"] = int64(age / time.Second)
	health["stale_after_seconds"] = int64(30)
	health["stale"] = state.State == "running" && age > 30*time.Second
	return health
}

func (c *memoryCoordinator) startHeartbeat(interval time.Duration, clock memoryClock) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = c.heartbeat(memoryClockNow(clock))
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func loadMemoryCoordinatorState(brainDir string) (memoryCoordinatorState, error) {
	data, present, err := readMemoryStateFile(brainDir, memoryCoordinatorStateRel, "coordinator state", maxManifestBytes)
	if err != nil {
		return memoryCoordinatorState{}, err
	}
	if !present {
		return memoryCoordinatorState{}, nil
	}
	var state memoryCoordinatorState
	version, err := decodeStrictVersionedJSON(data, &state, memoryCoordinatorSchema, "coordinator state")
	if err != nil {
		return memoryCoordinatorState{}, err
	}
	if version != memoryCoordinatorSchema || state.SchemaVersion != memoryCoordinatorSchema ||
		(state.State != "running" && state.State != "idle") || state.StartedAt.IsZero() || state.HeartbeatAt.IsZero() {
		return memoryCoordinatorState{}, errors.New("memory_state_corrupt: coordinator state is invalid")
	}
	return state, nil
}

// appendMemoryWorkerLog stores an operational event name and an opaque owner
// prefix, and nothing else.
//
// Its sibling appendMemoryWorkerLogReason adds one bounded `reason` field, so
// the log as a whole is no longer content-free: a reason is error text, and
// error text carries paths. It is redacted (memoryWorkerLogReason) and bounded,
// never prompts, transcripts or session IDs. Saying "no error text" here while
// the file next to it wrote exactly that is how a reader comes to trust a
// claim the bytes do not support.
// memoryWorkerLogReasonMax bounds the reason recorded beside an event. The log
// is a bounded ring (memoryWorkerLogMaxBytes), so an unbounded error -- a Go
// error can carry a whole command's stderr -- would evict the history it is
// meant to explain.
const memoryWorkerLogReasonMax = 240

// memoryWorkerLogReason renders a reason as one trailing key=value field.
//
// The reader splits a line with strings.Fields and takes the field prefixed
// "event=", so extra fields are ignored -- but a reason containing "event="
// would be read as the event itself, and a newline would forge a whole log
// entry. Both are neutralised here rather than trusted.
func memoryWorkerLogReason(reason string) string {
	// Redact BEFORE truncating. Truncation can cut a credential in half, and
	// half a token no longer matches the patterns that recognise it, so a
	// reason trimmed first would persist a fragment nothing redacts. Every
	// other durable free-form path here (pattern corpus, dossiers, skill
	// bridges) runs through redactText for the same reason; this one is a
	// worker log a reader is asked to paste into an issue.
	reason = redactText(reason)
	reason = strings.Join(strings.Fields(reason), " ")
	reason = strings.ReplaceAll(reason, "event=", "event:")
	if reason == "" {
		return ""
	}
	if len(reason) > memoryWorkerLogReasonMax {
		// Rune-safe: a byte slice can cut a multi-byte character in half, and
		// strconv.Quote then renders the trailing fragment as a replacement
		// character instead of text. Errors carry paths and messages that are
		// routinely not ASCII.
		cut := memoryWorkerLogReasonMax
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut] + "..."
	}
	return " reason=" + strconv.Quote(reason)
}

// appendMemoryWorkerLogReason records an event together with WHY it happened.
//
// `prepass_failed` used to say only THAT a pass failed. A brain whose
// background projection fails keeps relaunching on a 1m..30m backoff, so the
// silence is not one missing line but a retry storm no reader can diagnose --
// the comment at the call site records 1285 consecutive failures accumulating
// behind exactly this kind of quiet.
func appendMemoryWorkerLogReason(brainDir string, now time.Time, event, owner, reason string) error {
	return appendMemoryWorkerLogLine(brainDir, now, event, owner, memoryWorkerLogReason(reason))
}

func appendMemoryWorkerLog(brainDir string, now time.Time, event, owner string) error {
	return appendMemoryWorkerLogLine(brainDir, now, event, owner, "")
}

func appendMemoryWorkerLogLine(brainDir string, now time.Time, event, owner, suffix string) error {
	var previous []byte
	if data, present, err := readMemoryStateFile(brainDir, memoryWorkerLogRel, "worker log", memoryWorkerLogMaxBytes+1); err == nil && present {
		previous = data
		if len(previous) > memoryWorkerLogMaxBytes {
			previous = nil
		}
		if len(previous) > memoryWorkerLogMaxBytes/2 {
			if archived, archivedPresent, err := readMemoryStateFile(brainDir, memoryWorkerLogArchiveRel, "worker log archive", memoryWorkerLogMaxBytes); err == nil && archivedPresent {
				if err := writeBrainRelativeFileAtomic(brainDir, memoryWorkerLogArchive2Rel, archived, 0o600); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if err := writeBrainRelativeFileAtomic(brainDir, memoryWorkerLogArchiveRel, previous, 0o600); err != nil {
				return err
			}
			previous = nil
		}
	} else if err != nil {
		return err
	}
	if len(owner) > 8 {
		owner = owner[:8]
	}
	line := []byte(fmt.Sprintf("%s event=%s owner=%s%s\n", now.UTC().Format(time.RFC3339), event, owner, suffix))
	return writeBrainRelativeFileAtomic(brainDir, memoryWorkerLogRel, append(previous, line...), 0o600)
}

func memoryWorkerLogHealth(brainDir string) (map[string]any, error) {
	data, present, err := readMemoryStateFile(brainDir, memoryWorkerLogRel, "worker log", memoryWorkerLogMaxBytes)
	if err != nil {
		return nil, err
	}
	if !present {
		return map[string]any{"present": false, "max_bytes": memoryWorkerLogMaxBytes}, nil
	}
	health := map[string]any{
		"present":   true,
		"bytes":     len(data),
		"max_bytes": memoryWorkerLogMaxBytes,
		"bounded":   true,
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 0 {
		for _, field := range strings.Fields(lines[len(lines)-1]) {
			if strings.HasPrefix(field, "event=") {
				health["last_event"] = strings.TrimPrefix(field, "event=")
			}
		}
	}
	if archived, present, err := readMemoryStateFile(brainDir, memoryWorkerLogArchiveRel, "worker log archive", memoryWorkerLogMaxBytes); err != nil {
		return nil, err
	} else if present {
		health["rotated"] = true
		health["rotated_bytes"] = len(archived)
	} else {
		health["rotated"] = false
	}
	if archived, present, err := readMemoryStateFile(brainDir, memoryWorkerLogArchive2Rel, "oldest worker log archive", memoryWorkerLogMaxBytes); err != nil {
		return nil, err
	} else if present {
		health["rotated_files"] = 2
		health["oldest_rotated_bytes"] = len(archived)
	} else if rotated, _ := health["rotated"].(bool); rotated {
		health["rotated_files"] = 1
	} else {
		health["rotated_files"] = 0
	}
	return health, nil
}

func memoryCancellationRel(jobID string) string {
	sum := sha256.Sum256([]byte(jobID))
	return filepath.ToSlash(filepath.Join(memoryCancelDirRel, hex.EncodeToString(sum[:])[:40]+".json"))
}

func writeMemoryCancellationRequest(brainDir string, job memoryJob, now time.Time) error {
	existing, err := loadMemoryCancellationRequest(brainDir, job.JobID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil // preserve the first durable cancellation intent byte-for-byte
	}
	request := memoryCancellationRequest{SchemaVersion: memoryCancellationSchema, JobID: job.JobID, RequestedAt: now}
	data, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, memoryCancellationRel(job.JobID), append(data, '\n'), 0o600)
}

func loadMemoryCancellationRequest(brainDir, jobID string) (*memoryCancellationRequest, error) {
	data, present, err := readMemoryStateFile(brainDir, memoryCancellationRel(jobID), "cancellation request", maxManifestBytes)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	var request memoryCancellationRequest
	version, err := decodeStrictVersionedJSON(data, &request, memoryCancellationSchema, "cancellation request")
	if err != nil {
		return nil, err
	}
	if version != memoryCancellationSchema || request.SchemaVersion != memoryCancellationSchema || request.JobID != jobID || request.RequestedAt.IsZero() {
		return nil, errors.New("memory_state_corrupt: cancellation request is invalid")
	}
	return &request, nil
}

func removeMemoryCancellationRequest(brainDir, jobID string) error {
	return removeCheckedMemoryStateFile(brainDir, memoryCancellationRel(jobID), "cancellation request", maxManifestBytes, func(data []byte) error {
		var request memoryCancellationRequest
		version, err := decodeStrictVersionedJSON(data, &request, memoryCancellationSchema, "cancellation request")
		if err != nil {
			return err
		}
		if version != memoryCancellationSchema || request.SchemaVersion != memoryCancellationSchema || request.JobID != jobID || request.RequestedAt.IsZero() {
			return errors.New("memory_state_corrupt: cancellation request is invalid")
		}
		return nil
	})
}

func pruneMemoryJobsLocked(brainDir string, jobs []memoryJob, now time.Time) (int, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return 0, err
	}
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	receipts, receiptsCurrent := loadProjectionState(brainDir, source)
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return 0, err
	}
	abstracts := newSessionAbstractResolver(brainDir)
	hasTruthProof := func(job memoryJob) bool {
		switch {
		case job.State == memoryJobStateComplete && job.Kind == memoryJobKindProjection && receiptsCurrent:
			receipt, ok := receipts.receiptFor(job.SessionRef)
			return ok && receipt.InputDigest == job.InputDigest
		case job.State == memoryJobStateComplete && job.Kind == memoryJobKindSessionAbstract:
			entry, ok := abstracts.inventory.ByDigest[job.InputDigest]
			if !ok || entry.State != sessionAbstractCurrent || entry.Artifact.SessionRef != job.SessionRef {
				return false
			}
			view, err := loadConversationSessionViewByRef(brainDir, job.SessionRef)
			return err == nil && validateSessionAbstract(entry.Artifact, view) == nil
		case job.State == memoryJobStateExcluded:
			_, ok := stones.Excluded[job.SessionID]
			return ok
		}
		return false
	}
	type proofScope struct{ kind, sessionRef string }
	type proofDigests struct{ first, second string }
	proofByJob := make(map[string]bool, len(jobs))
	proofsByScope := make(map[proofScope]proofDigests)
	for _, job := range jobs {
		proven := hasTruthProof(job)
		proofByJob[job.JobID] = proven
		if !proven {
			continue
		}
		scope := proofScope{kind: job.Kind, sessionRef: job.SessionRef}
		digests := proofsByScope[scope]
		switch {
		case digests.first == "":
			digests.first = job.InputDigest
		case digests.first != job.InputDigest && digests.second == "":
			digests.second = job.InputDigest
		}
		proofsByScope[scope] = digests
	}
	replacementIsProven := func(job memoryJob) bool {
		digests := proofsByScope[proofScope{kind: job.Kind, sessionRef: job.SessionRef}]
		return (digests.first != "" && digests.first != job.InputDigest) || (digests.second != "" && digests.second != job.InputDigest)
	}
	pruned := 0
	for _, job := range jobs {
		if pruned >= memoryPruneLimit || job.FinishedAt == nil || now.Sub(*job.FinishedAt) < memoryTerminalRetention {
			continue
		}
		proven := false
		switch {
		case job.State == memoryJobStateComplete || job.State == memoryJobStateExcluded:
			proven = proofByJob[job.JobID]
		case job.State == memoryJobStateSuperseded || job.State == memoryJobStateCancelled:
			_, tombstoned := stones.Excluded[job.SessionID]
			proven = tombstoned || replacementIsProven(job)
		}
		if !proven {
			continue
		}
		for _, rel := range memoryJobSourcePaths(job) {
			path := filepath.Join(brainDir, filepath.FromSlash(rel))
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return pruned, err
			}
		}
		pruned++
	}
	return pruned, nil
}
