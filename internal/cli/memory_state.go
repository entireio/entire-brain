package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// memory_state.go is the C3 durable work record: content-free lifecycle hint
// files and per-job state files under history/work/v1/. Jobs are operational
// history; the projection receipt (memory_receipts.go) is the durable proof
// of currentness, and reconciliation against the canonical manifest remains
// the correctness authority. Losing every file here loses no memory.

const (
	memoryWorkDirRel  = historyDirName + "/work/v1"
	memoryHintsDirRel = memoryWorkDirRel + "/hints"
	memoryJobsDirRel  = memoryWorkDirRel + "/jobs"

	memoryHintSchemaVersion = 1
	memoryJobSchemaVersion  = 1

	memoryJobKindProjection = "deterministic_projection"

	memoryJobStatePending    = "pending"
	memoryJobStateRunning    = "running"
	memoryJobStateComplete   = "complete"
	memoryJobStateRetryable  = "retryable_error"
	memoryJobStateInvalid    = "invalid"
	memoryJobStateExcluded   = "excluded"
	memoryJobStateSuperseded = "superseded"
	memoryJobStateCancelled  = "cancelled"

	memoryJobErrorMaxBytes = 512
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
	rel := memoryHintRel(repoKey, sessionID, branch)
	hint := memoryLifecycleHint{SchemaVersion: memoryHintSchemaVersion, RepoKey: repoKey, SessionID: sessionID, Branch: branch}
	if data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(rel)), maxManifestBytes); err == nil {
		var prev memoryLifecycleHint
		if json.Unmarshal(data, &prev) == nil && prev.SchemaVersion == memoryHintSchemaVersion {
			hint.Generation = prev.Generation
		}
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

func loadMemoryHints(brainDir string) []memoryLifecycleHint {
	entries, err := os.ReadDir(filepath.Join(brainDir, filepath.FromSlash(memoryHintsDirRel)))
	if err != nil {
		return nil
	}
	var hints []memoryLifecycleHint
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryHintsDirRel), entry.Name()), maxManifestBytes)
		if err != nil {
			continue
		}
		var hint memoryLifecycleHint
		if json.Unmarshal(data, &hint) == nil && hint.SchemaVersion == memoryHintSchemaVersion {
			hints = append(hints, hint)
		}
	}
	sort.Slice(hints, func(i, j int) bool { return hints[i].SessionID < hints[j].SessionID })
	return hints
}

// removeMemoryHint deletes a hint only when its generation still matches, so
// an event racing cleanup is never lost. Caller holds the write lock.
func removeMemoryHint(brainDir string, hint memoryLifecycleHint) error {
	rel := memoryHintRel(hint.RepoKey, hint.SessionID, hint.Branch)
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	data, err := safeReadFile(full, maxManifestBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var current memoryLifecycleHint
	if json.Unmarshal(data, &current) == nil && current.Generation > hint.Generation {
		return nil // a newer event arrived; keep the hint
	}
	err = os.Remove(full)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// memoryJob is one durable, content-free unit of deterministic projection
// work, keyed by repository, session scope, input digest, and kind.
type memoryJob struct {
	SchemaVersion int        `json:"schema_version"`
	JobID         string     `json:"job_id"`
	Kind          string     `json:"kind"`
	RepoKey       string     `json:"repo_key"`
	SessionID     string     `json:"session_id"`
	SessionRef    string     `json:"session_ref"`
	Branch        string     `json:"branch,omitempty"`
	InputDigest   string     `json:"input_digest"`
	Trigger       string     `json:"trigger"`
	State         string     `json:"state"`
	Attempt       int        `json:"attempt"`
	CreatedAt     time.Time  `json:"created_at"`
	AvailableAt   time.Time  `json:"available_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Supersedes    string     `json:"supersedes,omitempty"`
	Error         string     `json:"error,omitempty"`
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
	return filepath.ToSlash(filepath.Join(memoryJobsDirRel, url.PathEscape(jobID)+".json"))
}

// memoryJobTransitions is the allowed state machine (C3).
var memoryJobTransitions = map[string]map[string]bool{
	memoryJobStatePending:   {memoryJobStateRunning: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
	memoryJobStateRunning:   {memoryJobStateComplete: true, memoryJobStateRetryable: true, memoryJobStateInvalid: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
	memoryJobStateRetryable: {memoryJobStatePending: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
	memoryJobStateComplete:  {memoryJobStateSuperseded: true},
	memoryJobStateInvalid:   {memoryJobStatePending: true, memoryJobStateExcluded: true, memoryJobStateSuperseded: true, memoryJobStateCancelled: true},
}

func saveMemoryJob(brainDir string, job memoryJob) error {
	job.SchemaVersion = memoryJobSchemaVersion
	if len(job.Error) > memoryJobErrorMaxBytes {
		job.Error = job.Error[:memoryJobErrorMaxBytes]
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, memoryJobRel(job.JobID), append(data, '\n'), 0o600)
}

func loadMemoryJobs(brainDir string) []memoryJob {
	entries, err := os.ReadDir(filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel)))
	if err != nil {
		return nil
	}
	var jobs []memoryJob
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel), entry.Name()), maxManifestBytes)
		if err != nil {
			continue
		}
		var job memoryJob
		if json.Unmarshal(data, &job) == nil && job.SchemaVersion == memoryJobSchemaVersion && job.JobID != "" {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].JobID < jobs[j].JobID })
	return jobs
}

// transitionMemoryJob applies one validated state transition and persists it.
func transitionMemoryJob(brainDir string, job memoryJob, state string, now time.Time, errText string) (memoryJob, error) {
	allowed := memoryJobTransitions[job.State]
	if !allowed[state] {
		return job, fmt.Errorf("memory_state_corrupt: job %s cannot move %s -> %s", job.JobID, job.State, state)
	}
	job.State = state
	job.Error = errText
	switch state {
	case memoryJobStateRunning:
		job.Attempt++
		started := now
		job.StartedAt = &started
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
	return job, saveMemoryJob(brainDir, job)
}

// runnableMemoryJobs returns pending jobs whose available_at has passed, in
// fair deterministic order (available_at, created_at, job id).
func runnableMemoryJobs(jobs []memoryJob, now time.Time) []memoryJob {
	var runnable []memoryJob
	for _, job := range jobs {
		if job.State == memoryJobStatePending && !job.AvailableAt.After(now) {
			runnable = append(runnable, job)
		}
	}
	sort.Slice(runnable, func(i, j int) bool {
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
