package cli

import (
	"bytes"
	"context"
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

// facts_vitality.go is the Phase 1 slice of the memory-lifecycle plan's
// vitality ledger (docs/vitality_receipts.md): append-only serve receipts for
// facts emitted through the read surfaces, plus a deterministic compacted
// per-fact rollup. Receipts are evidence for the Phase 2 outcome join and
// Phase 3 decay — this phase only records and reports; it changes no ranking,
// gating, quarantine, or fact-schema behavior.
//
// Storage is branch-scoped next to the branch's facts.ndjson (vitality is
// branch-scoped like facts): facts/<branch-dir>/vitality.ndjson (the append
// log) and facts/<branch-dir>/vitality.json (the compacted rollup). Both are
// brain-local and never exported. Events carry only a hash of the task/query
// text — the raw text is never persisted.
//
// The whole layer is best-effort and NEVER-FAILS (mirroring the hook
// contract): a corrupt, missing, or unwritable sidecar must not change what a
// read surface returns; recording problems degrade to at most one bounded
// stderr diagnostic per command invocation.
const (
	factsVitalityLogFileName    = "vitality.ndjson"
	factsVitalityRollupFileName = "vitality.json"
	factsVitalitySchemaVersion  = 1

	// factsVitalityLockName serializes vitality appends and compaction under
	// the brain's locks/ directory. It is deliberately distinct from
	// write.lock: receipts are written by *read* surfaces, which must neither
	// contend with long fact-store writes nor ever nest inside them (no lock
	// ordering between the two exists, so no inversion is possible).
	factsVitalityLockName = "vitality.lock"

	// factsVitalityLockTimeout permits only a handful of standard lock retries.
	// Serve receipts are droppable telemetry, so they must not add a material
	// stall to a fast query or hook when another process owns the sidecar.
	factsVitalityLockTimeout = 50 * time.Millisecond

	// factsVitalityLogMaxBytes caps the append log; an append that grows the
	// log past this compacts it into the rollup inline, bounding disk growth.
	factsVitalityLogMaxBytes = 1 << 20 // 1 MiB

	// factsVitalityRollupMaxFacts hard-bounds the rollup: one entry per
	// distinct fact id ever served could otherwise grow without limit as
	// facts churn. Overflow evicts the least-recently-served entries
	// deterministically.
	factsVitalityRollupMaxFacts = 4096

	// maxVitalityFileBytes bounds reading either sidecar file into memory.
	maxVitalityFileBytes = 16 << 20 // 16 MiB

	vitalityEventServed = "served"
)

// vitalityEvent is one append-only receipt line in vitality.ndjson. Field
// names follow the memory-lifecycle plan's ledger schema.
type vitalityEvent struct {
	Type    string    `json:"t"`
	FactID  string    `json:"fact"`
	At      time.Time `json:"at"`
	Surface string    `json:"surface,omitempty"`
	// TaskSHA is "sha256:<hex>" of the task/query text that produced the
	// serve, or empty when the surface has none (e.g. get by id). The raw
	// text is never persisted.
	TaskSHA string `json:"task_sha256,omitempty"`
	Head    string `json:"head,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// vitalityRollup is the compacted per-fact index (vitality.json). It is a
// deterministic fold of the event log: same events in the same order always
// produce byte-identical JSON (map keys marshal sorted; timestamps come from
// events, never from the wall clock).
type vitalityRollup struct {
	SchemaVersion int                            `json:"schema_version"`
	Facts         map[string]*vitalityFactRollup `json:"facts"`
	// Log is the crash-safety marker for two-phase compaction: it records the
	// exact log prefix already folded into Facts, so a crash between "write
	// rollup" and "truncate log" never double-counts events on the next run.
	Log vitalityLogMarker `json:"log,omitzero"`
}

type vitalityFactRollup struct {
	Served      int            `json:"served"`
	FirstServed time.Time      `json:"first_served"`
	LastServed  time.Time      `json:"last_served"`
	LastSurface string         `json:"last_surface,omitempty"`
	LastHead    string         `json:"last_head,omitempty"`
	LastTaskSHA string         `json:"last_task_sha256,omitempty"`
	Surfaces    map[string]int `json:"surfaces,omitempty"`
}

type vitalityLogMarker struct {
	AbsorbedBytes  int64  `json:"absorbed_bytes,omitempty"`
	AbsorbedSHA256 string `json:"absorbed_sha256,omitempty"`
}

func newVitalityRollup() vitalityRollup {
	return vitalityRollup{SchemaVersion: factsVitalitySchemaVersion, Facts: map[string]*vitalityFactRollup{}}
}

func factsVitalityLogRelPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), factsVitalityLogFileName))
}

func factsVitalityRollupRelPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), factsVitalityRollupFileName))
}

// vitalityTaskHash hashes the task/query text that drove a serve. Only this
// hash is persisted, giving Phase 2 same-task correlation without ever
// storing what the user asked.
func vitalityTaskHash(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func vitalityContentSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// withVitalityLock serializes vitality writers (appends and compaction). The
// lock is never acquired while holding the brain write lock or vice versa.
func withVitalityLock(brainDir string, fn func() error) error {
	return withVitalityLockTimeout(brainDir, factsVitalityLockTimeout, fn)
}

func withVitalityLockTimeout(brainDir string, timeout time.Duration, fn func() error) error {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, brainLockDirName); err != nil {
		return err
	}
	lock, err := acquireFileLock(filepath.Join(brainDir, brainLockDirName, factsVitalityLockName), "vitality_locked", timeout)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	return fn()
}

// appendBrainRelativeFile appends data to a brain-relative file with the same
// symlink/hardlink discipline as writeBrainRelativeFileAtomic. Appends are not
// fsynced — a torn tail line on power loss is tolerated by the log parser.
func appendBrainRelativeFile(brainDir, rel string, data []byte) error {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	clean, err := cleanBrainRelativePath(rel)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(clean); dir != "." {
		if err := rejectExistingSymlinkPathComponents(brainDir, dir); err != nil {
			return err
		}
	}
	abs := filepath.Join(brainDir, clean)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return err
	}
	if err := rejectUnsafeExistingRegularFile(abs, "vitality log"); err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_APPEND|fileLockOpenFlags(), 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := rejectOpenFileAlias(abs, f, "vitality log"); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

// appendVitalityEvents appends serve receipts to the branch's vitality log
// and compacts inline when the log outgrows maxLogBytes. Callers treat any
// error as "receipts dropped" — never as a read failure.
func appendVitalityEvents(brainDir, branch string, events []vitalityEvent) error {
	return appendVitalityEventsBounded(brainDir, branch, events, factsVitalityLogMaxBytes)
}

func appendVitalityEventsBounded(brainDir, branch string, events []vitalityEvent, maxLogBytes int64) error {
	return appendVitalityEventsBoundedWithLockTimeout(
		brainDir, branch, events, maxLogBytes, factsVitalityLockTimeout,
	)
}

func appendVitalityEventsBoundedWithLockTimeout(brainDir, branch string, events []vitalityEvent, maxLogBytes int64, lockTimeout time.Duration) error {
	if len(events) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return withVitalityLockTimeout(brainDir, lockTimeout, func() error {
		rel := factsVitalityLogRelPath(branch)
		abs := filepath.Join(brainDir, filepath.FromSlash(rel))
		// Hard backstop: if the log has grown far past its cap, inline
		// compaction must have been failing (e.g. the rollup is unwritable
		// while the log is not). Refuse further appends rather than grow
		// without bound — receipts are droppable, disk is not.
		if info, err := os.Stat(abs); err == nil && info.Size() > 8*maxLogBytes {
			return fmt.Errorf("vitality log at %s exceeds %d bytes and cannot compact; dropping receipts", rel, 8*maxLogBytes)
		}
		if err := appendBrainRelativeFile(brainDir, rel, buf.Bytes()); err != nil {
			return err
		}
		info, err := os.Stat(abs)
		if err != nil || info.Size() <= maxLogBytes {
			return nil // sizing is advisory; the append itself succeeded
		}
		_, _, err = compactVitalityLocked(brainDir, branch)
		return err
	})
}

// recordServedFacts is the single entry point read surfaces call after they
// have decided what to emit. It never returns an error: any recording problem
// (unwritable sidecar, lock timeout, corrupt store) degrades to at most one
// diagnostic line on errW and leaves the read result untouched.
func recordServedFacts(errW io.Writer, at time.Time, brainDir, branch, surface, head, taskText string, factIDs []string) {
	recordServedFactsWithLockTimeout(
		errW, at, brainDir, branch, surface, head, taskText, factIDs, factsVitalityLockTimeout,
	)
}

func recordServedFactsWithLockTimeout(errW io.Writer, at time.Time, brainDir, branch, surface, head, taskText string, factIDs []string, lockTimeout time.Duration) {
	if brainDir == "" || len(factIDs) == 0 {
		return
	}
	taskSHA := vitalityTaskHash(taskText)
	events := make([]vitalityEvent, 0, len(factIDs))
	for _, id := range factIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		events = append(events, vitalityEvent{
			Type:    vitalityEventServed,
			FactID:  id,
			At:      at,
			Surface: surface,
			TaskSHA: taskSHA,
			Head:    head,
			Branch:  branch,
		})
	}
	if err := appendVitalityEventsBoundedWithLockTimeout(
		brainDir, branch, events, factsVitalityLogMaxBytes, lockTimeout,
	); err != nil && errW != nil {
		// The error may be a failed append (receipts dropped) or a failed
		// inline compaction after a successful append; either way it is one
		// bounded line per invocation and never a read failure.
		fmt.Fprintf(errW, "warning: facts vitality recording degraded: %v\n", err)
	}
}

// vitalityNow is the receipt clock: opts.Now when injected (tests, root
// command), the wall clock otherwise. Nil-safe because hook/MCP commands are
// sometimes constructed with a bare Options in tests.
func vitalityNow(opts Options) time.Time {
	if opts.Now != nil {
		return opts.Now().UTC()
	}
	return time.Now().UTC()
}

// vitalityHead resolves the worktree HEAD for a receipt, empty on any error
// (receipts degrade, reads never do).
func vitalityHead(ctx context.Context, runner CommandRunner, repoDir string) string {
	if runner == nil || repoDir == "" {
		return ""
	}
	head, err := gitScalar(ctx, runner, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return head
}

// factRecordIDs extracts ids from served fact records.
func factRecordIDs(facts []factRecord) []string {
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.ID)
	}
	return ids
}

// unifiedFactIDs extracts the fact-layer ids from unified retrieval results.
// Only source "fact" is a served fact; review groups, history, and docs carry
// no per-fact vitality.
func unifiedFactIDs(results []unifiedResult) []string {
	var ids []string
	for _, r := range results {
		if r.Source == "fact" {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

func loadVitalityRollup(brainDir, branch string) (vitalityRollup, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(factsVitalityRollupRelPath(branch)))
	data, err := safeReadFile(path, maxVitalityFileBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return newVitalityRollup(), nil
		}
		return newVitalityRollup(), err
	}
	var rollup vitalityRollup
	if err := json.Unmarshal(data, &rollup); err != nil {
		return newVitalityRollup(), fmt.Errorf("parse %s: %w", factsVitalityRollupFileName, err)
	}
	if rollup.Facts == nil {
		rollup.Facts = map[string]*vitalityFactRollup{}
	}
	rollup.SchemaVersion = factsVitalitySchemaVersion
	return rollup, nil
}

func writeVitalityRollup(brainDir, branch string, rollup vitalityRollup) error {
	data, err := json.MarshalIndent(rollup, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeBrainRelativeFileAtomic(brainDir, factsVitalityRollupRelPath(branch), data, 0o600)
}

func readVitalityLog(brainDir, branch string) ([]byte, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(factsVitalityLogRelPath(branch)))
	data, err := safeReadFile(path, maxVitalityFileBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// parseVitalityEvents decodes log lines best-effort: blank lines are skipped,
// malformed or incomplete lines (e.g. a torn tail after power loss) are
// counted and dropped rather than failing the layer, and unknown event types
// are ignored for forward compatibility.
func parseVitalityEvents(data []byte) (events []vitalityEvent, malformed int) {
	for line := range bytes.Lines(data) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event vitalityEvent
		if err := json.Unmarshal(line, &event); err != nil || event.FactID == "" || event.At.IsZero() {
			malformed++
			continue
		}
		if event.Type != vitalityEventServed {
			continue
		}
		events = append(events, event)
	}
	return events, malformed
}

// mergeVitalityEvents folds events (in log order) into the rollup and evicts
// overflow. The fold is deterministic given the log: "last" fields follow
// event time, with log order breaking exact ties.
func mergeVitalityEvents(rollup *vitalityRollup, events []vitalityEvent) {
	for _, event := range events {
		at := event.At.UTC()
		entry := rollup.Facts[event.FactID]
		if entry == nil {
			entry = &vitalityFactRollup{FirstServed: at}
			rollup.Facts[event.FactID] = entry
		}
		entry.Served++
		if entry.FirstServed.IsZero() || at.Before(entry.FirstServed) {
			entry.FirstServed = at
		}
		if !at.Before(entry.LastServed) {
			entry.LastServed = at
			entry.LastSurface = event.Surface
			entry.LastHead = event.Head
			entry.LastTaskSHA = event.TaskSHA
		}
		if event.Surface != "" {
			if entry.Surfaces == nil {
				entry.Surfaces = map[string]int{}
			}
			entry.Surfaces[event.Surface]++
		}
	}
	evictVitalityOverflow(rollup.Facts, factsVitalityRollupMaxFacts)
}

// evictVitalityOverflow drops the least-recently-served entries once the
// rollup exceeds maxFacts, keeping the index hard-bounded. Ordering is
// deterministic: oldest last_served first, fact id as the tiebreak.
func evictVitalityOverflow(facts map[string]*vitalityFactRollup, maxFacts int) {
	if maxFacts <= 0 || len(facts) <= maxFacts {
		return
	}
	ids := make([]string, 0, len(facts))
	for id := range facts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		fa, fb := facts[ids[a]], facts[ids[b]]
		if !fa.LastServed.Equal(fb.LastServed) {
			return fa.LastServed.Before(fb.LastServed)
		}
		return ids[a] < ids[b]
	})
	for _, id := range ids[:len(facts)-maxFacts] {
		delete(facts, id)
	}
}

type vitalityCompactStats struct {
	NewEvents   int
	Malformed   int
	RollupReset bool
}

// compactVitality folds the append log into the rollup under the vitality lock.
func compactVitality(brainDir, branch string) (rollup vitalityRollup, stats vitalityCompactStats, err error) {
	err = withVitalityLock(brainDir, func() error {
		var innerErr error
		rollup, stats, innerErr = compactVitalityLocked(brainDir, branch)
		return innerErr
	})
	return rollup, stats, err
}

// compactVitalityLocked is the two-phase, crash-safe compaction:
//
//  1. fold the log's unabsorbed suffix into the rollup and persist it with a
//     marker naming the exact log content now absorbed;
//  2. truncate the log, then clear the marker.
//
// A crash after (1) leaves the marker matching the log, so the next run skips
// the already-absorbed prefix instead of double-counting. A crash after the
// truncate leaves a stale marker over an empty log, which the next run simply
// clears. Rerunning with no new events writes nothing, so compaction is
// idempotent, and the rollup is a pure function of the event sequence, so it
// is deterministic.
//
// A corrupt rollup is self-healing: it is rebuilt from whatever the log still
// holds (the corrupt file's history is unrecoverable by definition), because
// receipts are droppable evidence and must never wedge the layer.
func compactVitalityLocked(brainDir, branch string) (vitalityRollup, vitalityCompactStats, error) {
	var stats vitalityCompactStats
	data, err := readVitalityLog(brainDir, branch)
	if err != nil {
		return newVitalityRollup(), stats, err
	}
	rollup, rollupErr := loadVitalityRollup(brainDir, branch)
	if rollupErr != nil {
		rollup = newVitalityRollup()
		stats.RollupReset = true
	}
	absorbed := int64(0)
	if m := rollup.Log; m.AbsorbedBytes > 0 && m.AbsorbedBytes <= int64(len(data)) &&
		vitalityContentSHA(data[:m.AbsorbedBytes]) == m.AbsorbedSHA256 {
		absorbed = m.AbsorbedBytes
	}
	events, malformed := parseVitalityEvents(data[absorbed:])
	stats.NewEvents = len(events)
	stats.Malformed = malformed
	dirty := stats.RollupReset
	if len(events) > 0 {
		mergeVitalityEvents(&rollup, events)
		dirty = true
	}
	if len(data) > 0 {
		rollup.Log = vitalityLogMarker{AbsorbedBytes: int64(len(data)), AbsorbedSHA256: vitalityContentSHA(data)}
		if err := writeVitalityRollup(brainDir, branch, rollup); err != nil {
			return rollup, stats, err
		}
		if err := writeBrainRelativeFileAtomic(brainDir, factsVitalityLogRelPath(branch), nil, 0o600); err != nil {
			return rollup, stats, err
		}
		rollup.Log = vitalityLogMarker{}
		dirty = true
	} else if rollup.Log != (vitalityLogMarker{}) {
		rollup.Log = vitalityLogMarker{}
		dirty = true
	}
	if dirty {
		if err := writeVitalityRollup(brainDir, branch, rollup); err != nil {
			return rollup, stats, err
		}
	}
	return rollup, stats, nil
}

// vitalityViewStats describes the freshness and health of a vitality view.
type vitalityViewStats struct {
	PendingEvents  int
	MalformedLines int
	LogBytes       int64
	RollupCorrupt  bool
}

// loadVitalityView returns the branch's rollup with the log's unabsorbed
// events folded in — the up-to-date picture without forcing a compaction
// write. It prefers to read under the vitality lock so it never interleaves
// with a concurrent compaction, but degrades to a lock-free read (the files
// are replaced atomically) rather than failing.
func loadVitalityView(brainDir, branch string) (vitalityRollup, vitalityViewStats, error) {
	var (
		rollup vitalityRollup
		stats  vitalityViewStats
		err    error
	)
	read := func() error {
		var rollupErr error
		rollup, rollupErr = loadVitalityRollup(brainDir, branch)
		if rollupErr != nil {
			rollup = newVitalityRollup()
			stats.RollupCorrupt = true
		}
		data, logErr := readVitalityLog(brainDir, branch)
		if logErr != nil {
			return logErr
		}
		stats.LogBytes = int64(len(data))
		absorbed := int64(0)
		if m := rollup.Log; m.AbsorbedBytes > 0 && m.AbsorbedBytes <= int64(len(data)) &&
			vitalityContentSHA(data[:m.AbsorbedBytes]) == m.AbsorbedSHA256 {
			absorbed = m.AbsorbedBytes
		}
		events, malformed := parseVitalityEvents(data[absorbed:])
		stats.PendingEvents = len(events)
		stats.MalformedLines = malformed
		mergeVitalityEvents(&rollup, events)
		rollup.Log = vitalityLogMarker{}
		return nil
	}
	ran := false
	err = withVitalityLock(brainDir, func() error {
		ran = true
		return read()
	})
	if !ran { // lock unavailable, not a read failure: degrade to lock-free
		stats = vitalityViewStats{}
		err = read()
	}
	return rollup, stats, err
}
