package factstore

// Legacy cleanup removes only facts.ndjson files whose complete decoded content
// is already represented by a durably published flat store. Parked files are
// never loaded back into the store. A receipt precedes unlink so interruption,
// including later legitimate record deletion, does not strand verified copies.
import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

const cleanupReceiptName = ".cleanup-verified.json"
const cleanupReceiptTempPrefix = ".cleanup-receipt.tmp-"
const cleanupStatusPath = "facts/.cleanup-status.json"
const cleanupBatchFiles = 128
const cleanupBatchBytes = 64 << 20

// CleanupStatus is additive operational metadata, not another memory store.
// ReclaimedBytes counts removals recorded by completed maintenance passes.
// A process killed after unlink may undercount; the count never authorizes deletion.
type CleanupStatus struct {
	State          string   `json:"state"`
	ReclaimedBytes int64    `json:"reclaimed_bytes"`
	PendingDirs    int      `json:"pending_dirs"`
	Retained       []string `json:"retained,omitempty"`
	NextAfter      string   `json:"next_after,omitempty"`
}

type cleanupReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	SourceSHA256  string `json:"source_sha256"`
	StoreSHA256   string `json:"store_sha256"`
	Retained      string `json:"retained,omitempty"`
}

// cleanupFault is an internal crash/failure seam. Production leaves it nil.
var cleanupFault func(stage string) error

var errCleanupPending = errors.New("large legacy file awaits background verification")

func cleanupCheckpoint(stage string) error {
	if cleanupFault != nil {
		return cleanupFault(stage)
	}
	return nil
}

func contentDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// verifyFlatStore compares complete serialized records, not just their IDs.
func verifyFlatStore(brainDir string, expected []factmerge.Record) error {
	_, data, present, err := readStoreFile(brainDir)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("migration verification: published store is missing")
	}
	actual, err := decodeV3(data)
	if err != nil {
		return err
	}
	wanted := make(map[string]factmerge.Record, len(expected))
	for _, rec := range expected {
		wanted[rec.ID] = rec
	}
	if len(actual) != len(wanted) {
		return fmt.Errorf("migration verification: published record count differs")
	}
	for id, rec := range wanted {
		a, _ := json.Marshal(actual[id])
		b, _ := json.Marshal(rec)
		if !bytes.Equal(a, b) {
			return fmt.Errorf("migration verification: published record %s differs", id)
		}
	}
	return nil
}

// CleanupLegacy is maintenance for already-migrated stores. It never creates an
// unused store or waits behind an active writer. Migration itself remains in the
// normal write paths. Callers must not hold the brain write lock here.
func CleanupLegacy(brainDir string) (CleanupStatus, error) {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return CleanupStatus{}, err
	}
	dirs, err := legacyCleanupDirs(brainDir)
	if err != nil || len(dirs) == 0 {
		return CleanupStatus{State: "complete"}, err
	}
	probeUnlock, err := brainstore.AcquireBrainWriteLockTimeout(brainDir, 0)
	if errors.Is(err, brainstore.ErrFileLockTimeout) {
		return CleanupStatus{State: "pending", PendingDirs: len(dirs)}, nil
	}
	if err != nil {
		return CleanupStatus{}, err
	}
	probeUnlock()
	proofs, prepareErr := prepareLegacyCleanup(brainDir, dirs)
	if prepareErr != nil {
		return CleanupStatus{State: "retained", PendingDirs: len(dirs), Retained: []string{prepareErr.Error()}}, nil
	}
	if err := cleanupCheckpoint("prepared"); err != nil {
		return CleanupStatus{State: "pending", PendingDirs: len(dirs)}, err
	}
	unlock, err := brainstore.AcquireBrainWriteLockTimeout(brainDir, 0)
	if errors.Is(err, brainstore.ErrFileLockTimeout) {
		return CleanupStatus{State: "pending", PendingDirs: len(dirs)}, nil
	}
	if err != nil {
		return CleanupStatus{}, err
	}
	defer unlock()
	var result CleanupStatus
	err = withRepoFactStoreLock(brainDir, func() error { result = cleanupLegacyPreparedLocked(brainDir, proofs); return nil })
	return result, err
}

func legacyCleanupDirs(brainDir string) ([]string, error) {
	if err := base.RejectExistingSymlinkPathComponents(brainDir, DirName); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(brainDir, DirName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), legacyParkPrefix) {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs, nil
}

func readCleanupStatus(brainDir string) CleanupStatus {
	data, present, err := brainstore.ReadMemoryStateFile(brainDir, cleanupStatusPath, "legacy cleanup status", base.DefaultMaxReadBytes)
	var report CleanupStatus
	if err == nil && present {
		_ = json.Unmarshal(data, &report)
	}
	return report
}

// Cleanup is best effort after publication: a cleanup failure must not turn a
// committed memory write into a failed write. Status keeps the retained reason.
func cleanupLegacyLocked(brainDir string) CleanupStatus {
	return cleanupLegacyPreparedLocked(brainDir, nil)
}

func cleanupLegacyPreparedLocked(brainDir string, proofs map[string]cleanupReceipt) CleanupStatus {
	result := readCleanupStatus(brainDir)
	dirs, err := legacyCleanupDirs(brainDir)
	if err == nil && len(dirs) == 0 {
		return result
	}
	result.State = "pending"
	after := result.NextAfter
	result.NextAfter = ""
	result.Retained = nil
	retain := func(path string, err error) {
		if len(result.Retained) < 8 {
			result.Retained = append(result.Retained, fmt.Sprintf("%s: %v", path, err))
		}
	}
	if err != nil {
		retain(DirName, err)
	} else if !cleanupInlineEligible(brainDir, dirs, proofs) {
		// Expensive verification belongs to unlocked maintenance.
	} else {
		_, data, present, readErr := readStoreFile(brainDir)
		if readErr != nil {
			retain(RepositoryPath, readErr)
		} else if !present {
			retain(RepositoryPath, fmt.Errorf("missing verified flat store"))
		} else if current, decodeErr := decodeV3(data); decodeErr != nil {
			retain(RepositoryPath, decodeErr)
		} else {
			facts, openErr := os.OpenRoot(filepath.Join(brainDir, DirName))
			if openErr != nil {
				retain(DirName, openErr)
			} else {
				defer facts.Close()
				started := time.Now()
				processed := 0
				var scanned int64
				for pass := 0; pass < 2; pass++ {
					for _, name := range dirs {
						if processed >= cleanupBatchFiles || scanned >= cleanupBatchBytes || (processed > 0 && time.Since(started) > 250*time.Millisecond) {
							break
						}
						n, size, removed, cleanErr := cleanupPark(brainDir, facts, name, current, contentDigest(data), cleanupBatchFiles-processed, cleanupBatchBytes-scanned, started, after, &result.NextAfter, proofs)
						processed += n
						scanned += size
						result.ReclaimedBytes += removed
						if cleanErr != nil {
							retain(name, cleanErr)
						}
					}
					if processed > 0 || after == "" {
						break
					}
					after = "" // Wrap once; a retained first batch cannot starve later files.
				}
			}
		}
	}
	remaining, listErr := legacyCleanupDirs(brainDir)
	result.PendingDirs = len(remaining)
	if listErr != nil {
		retain(DirName, listErr)
	}
	if len(result.Retained) > 0 {
		result.State = "retained"
	} else if result.PendingDirs == 0 {
		result.State = "complete"
		result.NextAfter = ""
	}
	data, _ := json.Marshal(result)
	if writeErr := brainstore.WriteBrainRelativeFileAtomic(brainDir, cleanupStatusPath, append(data, '\n'), 0600); writeErr != nil {
		retain(cleanupStatusPath, writeErr)
		result.State = "retained"
	}
	return result
}

func cleanupPark(brainDir string, facts *os.Root, name string, current map[string]factmerge.Record, storeHash string, limit int, byteLimit int64, started time.Time, after string, next *string, proofs map[string]cleanupReceipt) (processed int, scanned, removed int64, err error) {
	info, err := facts.Lstat(name)
	if err != nil {
		return
	}
	if !info.IsDir() {
		err = fmt.Errorf("not a regular legacy directory")
		return
	}
	park, err := facts.OpenRoot(name)
	if err != nil {
		return
	}
	defer park.Close()
	entries, err := rootEntries(park)
	if err != nil {
		return
	}
	var problems []error
	for _, entry := range entries {
		key := name + "/" + entry.Name()
		if key <= after {
			continue
		}
		if processed >= limit || scanned >= byteLimit || (processed > 0 && time.Since(started) > 250*time.Millisecond) {
			break
		}
		*next = key
		if !entry.IsDir() {
			processed++
			problems = append(problems, fmt.Errorf("retained unexpected entry %s", entry.Name()))
			continue
		}
		branchInfo, statErr := park.Lstat(entry.Name())
		if statErr != nil || !branchInfo.IsDir() {
			processed++
			problems = append(problems, fmt.Errorf("legacy branch changed before opening"))
			continue
		}
		branch, openErr := park.OpenRoot(entry.Name())
		if openErr != nil {
			processed++
			problems = append(problems, openErr)
			continue
		}
		size, freed, fileErr := cleanupBranch(brainDir, filepath.Join(DirName, name, entry.Name()), branch, current, storeHash, proofs)
		processed++
		scanned += size
		removed += freed
		branch.Close()
		if errors.Is(fileErr, errCleanupPending) {
			continue
		}
		if fileErr != nil {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), fileErr))
			continue
		}
		latestBranch, statErr := park.Lstat(entry.Name())
		if statErr != nil || !os.SameFile(branchInfo, latestBranch) {
			problems = append(problems, fmt.Errorf("legacy branch changed during cleanup"))
			continue
		}
		// The facts copy is verified gone; known orphans of removed
		// subsystems go with it, or the empty-dir sweep below logs
		// "retained directory" forever (COR-2317).
		if orphanErr := removeLegacyOrphans(park, entry.Name()); orphanErr != nil {
			problems = append(problems, fmt.Errorf("retained directory %s: %w", entry.Name(), orphanErr))
			continue
		}
		// Remove only an empty directory; unknown entries can never be swept.
		if rmErr := park.Remove(entry.Name()); rmErr != nil {
			problems = append(problems, fmt.Errorf("retained directory %s: %w", entry.Name(), rmErr))
		} else if syncErr := brainstore.SyncPinnedDirectory(park); syncErr != nil {
			problems = append(problems, syncErr)
		}
	}
	entries, readErr := rootEntries(park)
	if readErr != nil {
		problems = append(problems, readErr)
	} else if len(entries) == 0 {
		latest, statErr := facts.Lstat(name)
		if statErr != nil {
			problems = append(problems, statErr)
		} else if !os.SameFile(info, latest) {
			problems = append(problems, fmt.Errorf("legacy directory changed during cleanup"))
		} else {
			// Close before removing on Windows.
			park.Close()
			if rmErr := facts.Remove(name); rmErr != nil {
				problems = append(problems, rmErr)
			} else if syncErr := brainstore.SyncPinnedDirectory(facts); syncErr != nil {
				problems = append(problems, syncErr)
			}
		}
	}
	err = errors.Join(problems...)
	return
}

func rootEntries(root *os.Root) ([]os.DirEntry, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}

func regularRootOpen(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, fmt.Errorf("%s changed while opening", name)
	}
	return f, nil
}

func regularRootRead(root *os.Root, name string) ([]byte, error) {
	f, err := regularRootOpen(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if len(data) > 4096 {
		return nil, fmt.Errorf("cleanup receipt exceeds size limit")
	}
	return data, err
}

func hashLegacyFile(root *os.Root) (string, int64, error) { return hashRootFile(root, FileName) }

func hashRootFile(root *os.Root, name string) (string, int64, error) {
	f, err := regularRootOpen(root, name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	return hex.EncodeToString(hash.Sum(nil)), size, err
}

func cleanupBranch(brainDir, rel string, branch *os.Root, current map[string]factmerge.Record, storeHash string, proofs map[string]cleanupReceipt) (scanned, removed int64, err error) {
	info, statErr := branch.Lstat(FileName)
	prepared, hasProof := proofs[filepath.ToSlash(rel)]
	if hasProof && prepared.Retained != "" {
		return 0, 0, errors.New(prepared.Retained)
	}
	if statErr == nil && info.Size() > 1<<20 && !hasProof {
		if _, receiptErr := branch.Lstat(cleanupReceiptName); os.IsNotExist(receiptErr) {
			return 0, 0, errCleanupPending
		}
	}
	sourceHash, size, readErr := hashLegacyFile(branch)
	receiptBytes, receiptErr := regularRootRead(branch, cleanupReceiptName)
	var receipt cleanupReceipt
	if receiptErr == nil {
		if err := strictCleanupJSON(receiptBytes, &receipt); err != nil {
			return 0, 0, fmt.Errorf("invalid cleanup receipt: %w", err)
		}
		if receipt.Retained != "" || receipt.SchemaVersion != 1 || !validDigest(receipt.SourceSHA256) || !validDigest(receipt.StoreSHA256) {
			return 0, 0, fmt.Errorf("invalid cleanup receipt")
		}
	} else if !os.IsNotExist(receiptErr) {
		return 0, 0, receiptErr
	}
	if os.IsNotExist(readErr) {
		if receiptErr == nil {
			if err := removeCleanupReceiptTemps(branch); err != nil {
				return 0, 0, err
			}
			if err := branch.Remove(cleanupReceiptName); err != nil {
				return 0, 0, err
			}
			return 0, 0, brainstore.SyncPinnedDirectory(branch)
		}
		return 0, 0, nil
	}
	if readErr != nil {
		return 0, 0, readErr
	}
	scanned = size
	if receiptErr == nil {
		if receipt.SourceSHA256 != sourceHash {
			return scanned, 0, fmt.Errorf("legacy bytes changed after verification")
		}
	} else {
		if hasProof {
			if prepared.SourceSHA256 != sourceHash || prepared.StoreSHA256 != storeHash {
				return scanned, 0, errCleanupPending
			}
		} else {
			file, err := regularRootOpen(branch, FileName)
			if err != nil {
				return scanned, 0, err
			}
			hash := sha256.New()
			verifyErr := verifyLegacyContained(io.TeeReader(file, hash), current)
			file.Close()
			if verifyErr != nil {
				return scanned, 0, verifyErr
			}
			if hex.EncodeToString(hash.Sum(nil)) != sourceHash {
				return scanned, 0, fmt.Errorf("legacy bytes changed during verification")
			}
		}
		receipt = cleanupReceipt{SchemaVersion: 1, SourceSHA256: sourceHash, StoreSHA256: storeHash}
	}
	{
		encoded, _ := json.Marshal(receipt)
		if err := writeCleanupReceipt(branch, append(encoded, '\n')); err != nil {
			return scanned, 0, err
		}
		// Verify the receipt through the same pinned directory used for unlink.
		saved, err := regularRootRead(branch, cleanupReceiptName)
		if err != nil {
			return scanned, 0, err
		}
		if !bytes.Equal(bytes.TrimSpace(saved), encoded) {
			return scanned, 0, fmt.Errorf("cleanup receipt changed during publication")
		}
	}
	if err := removeCleanupReceiptTemps(branch); err != nil {
		return scanned, 0, err
	}
	if err := cleanupCheckpoint("verified"); err != nil {
		return scanned, 0, err
	}
	// Recheck the exact bytes immediately before unlink. All cooperative writes
	// are excluded by the brain/store locks; this also rejects external edits.
	latest, _, err := hashLegacyFile(branch)
	if err != nil {
		return scanned, 0, err
	}
	if latest != sourceHash {
		return scanned, 0, fmt.Errorf("legacy bytes changed before removal")
	}
	if err := branch.Remove(FileName); err != nil {
		return scanned, 0, err
	}
	removed = scanned
	if err := brainstore.SyncPinnedDirectory(branch); err != nil {
		return scanned, removed, err
	}
	if err := cleanupCheckpoint("removed"); err != nil {
		return scanned, removed, err
	}
	if err := branch.Remove(cleanupReceiptName); err != nil {
		return scanned, removed, err
	}
	return scanned, removed, brainstore.SyncPinnedDirectory(branch)
}

func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

func verifyLegacyContained(raw io.Reader, current map[string]factmerge.Record) error {
	scanner := bufio.NewScanner(raw)
	scanner.Buffer(make([]byte, 4096), 64<<20)
	seen := map[string]bool{}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec factmerge.Record
		if err := strictCleanupJSON(line, &rec); err != nil {
			return fmt.Errorf("legacy record is not safely readable: %w", err)
		}
		if strings.TrimSpace(rec.ID) == "" || seen[rec.ID] || rec.ID != factmerge.RecordID(rec.Text, rec.Paths) {
			return fmt.Errorf("legacy record has a missing, invalid, or repeated ID")
		}
		seen[rec.ID] = true
		stored, ok := current[rec.ID]
		if !ok {
			return fmt.Errorf("legacy record %s is absent from the current store", rec.ID)
		}
		if err := verifyLegacyPayload(rec, stored); err != nil {
			return err
		}
		// Anchor identity alone omits Verified. Preserve every anchor field.
		anchors := make(map[factmerge.Anchor]bool, len(stored.Provenance))
		for _, have := range stored.Provenance {
			anchors[have] = true
		}
		for _, anchor := range rec.Provenance {
			if !anchors[anchor] {
				return fmt.Errorf("legacy record %s has unpreserved provenance", rec.ID)
			}
		}
	}
	return scanner.Err()
}

// verifyLegacyPayload permits only established branch and lifecycle changes.
// Conflicting metadata remains recoverable even when the canonical fold chose
// one copy. Provenance receives a separate exact containment check.
func verifyLegacyPayload(rec, stored factmerge.Record) error {
	// An ID match does not explain changed payload fields. Only branch
	// placement and the established lifecycle/provenance merge can differ.
	sourceBody, storedBody := rec, stored
	sourceBody.Branch = storedBody.Branch
	sourceBody.Provenance = nil
	storedBody.Provenance = nil
	sourceBody.Status = storedBody.Status
	sourceBody.SupersededBy = storedBody.SupersededBy
	sourceBody.UpdatedAt = storedBody.UpdatedAt
	if !rec.CreatedAt.Equal(stored.CreatedAt) {
		return fmt.Errorf("legacy record %s has a different creation time", rec.ID)
	}
	sourceBody.CreatedAt = storedBody.CreatedAt
	sourceJSON, _ := json.Marshal(sourceBody)
	storedJSON, _ := json.Marshal(storedBody)
	if !bytes.Equal(sourceJSON, storedJSON) {
		return fmt.Errorf("legacy record %s has unpreserved fields", rec.ID)
	}
	if rec.UpdatedAt.After(stored.UpdatedAt) || (incomingLifecycleWins(stored, rec) && rec.Status != stored.Status) || (rec.Status == stored.Status && rec.SupersededBy != stored.SupersededBy) {
		return fmt.Errorf("legacy record %s has an unpreserved lifecycle", rec.ID)
	}
	return nil
}

type migrationCleanupEvidence struct {
	digest  string
	records []factmerge.Record // Provenance omitted to avoid retaining all branch copies.
}

func (e migrationCleanupEvidence) containedBy(current map[string]factmerge.Record) bool {
	for _, rec := range e.records {
		stored, ok := current[rec.ID]
		if !ok || verifyLegacyPayload(rec, stored) != nil {
			return false
		}
		// readMigrationRecords accepts only unsigned anchors. If the canonical
		// merge includes signed anchors, use exact containment after migration.
		for _, anchor := range stored.Provenance {
			if anchor.Verified {
				return false
			}
		}
	}
	return true
}

// Reject unknown fields, duplicate object keys, and trailing JSON. Typed decode
// alone would silently discard data that cannot then be recovered after unlink.
func strictCleanupJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := key.(string)
				if !ok || keys[s] {
					return fmt.Errorf("duplicate or invalid JSON key")
				}
				keys[s] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	// This decode authorizes removal of persisted source bytes. Unknown fields
	// must therefore fail closed, rather than be discarded by a tolerant read.
	_, err := brainstore.DecodeVersionedJSONBody(data, target, false)
	return err
}

// prepareLegacyCleanup performs expensive parsing without the write lock. The
// final pass rechecks both source bytes and the exact published store digest.
// A concurrent mutation invalidates the proof and simply defers that removal.
func prepareLegacyCleanup(brainDir string, dirs []string) (map[string]cleanupReceipt, error) {
	proofs := map[string]cleanupReceipt{}
	_, data, present, err := readStoreFile(brainDir)
	if err != nil {
		return nil, err
	}
	if !present {
		return proofs, nil
	}
	current, err := decodeV3(data)
	if err != nil {
		return nil, err
	}
	storeHash := contentDigest(data)
	facts, err := os.OpenRoot(filepath.Join(brainDir, DirName))
	if err != nil {
		return nil, err
	}
	defer facts.Close()
	after := readCleanupStatus(brainDir).NextAfter
	// Rotate groups as well as branches, so a retained early file cannot starve
	// later groups. The locked pass owns the persisted traversal cursor.
	type source struct {
		rel string
		key string
	}
	var sources []source
	for _, dir := range dirs {
		info, err := facts.Lstat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		park, err := facts.OpenRoot(dir)
		if err != nil {
			continue
		}
		entries, err := rootEntries(park)
		park.Close()
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				sources = append(sources, source{filepath.Join(dir, entry.Name()), dir + "/" + entry.Name()})
			}
		}
	}
	sort.SliceStable(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		if (a.key > after) != (b.key > after) {
			return a.key > after
		}
		return a.key < b.key
	})
	for _, source := range sources {
		branch, err := facts.OpenRoot(source.rel)
		if err != nil {
			continue
		}
		info, statErr := branch.Lstat(FileName)
		_, receiptErr := branch.Lstat(cleanupReceiptName)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 1<<20 || receiptErr == nil {
			branch.Close()
			continue
		}
		file, err := regularRootOpen(branch, FileName)
		if err != nil {
			branch.Close()
			continue
		}
		hash := sha256.New()
		err = verifyLegacyContained(io.TeeReader(file, hash), current)
		file.Close()
		branch.Close()
		if err != nil {
			proofs[filepath.ToSlash(filepath.Join(DirName, source.rel))] = cleanupReceipt{Retained: err.Error()}
			break
		}
		proofs[filepath.ToSlash(filepath.Join(DirName, source.rel))] = cleanupReceipt{SchemaVersion: 1, SourceSHA256: hex.EncodeToString(hash.Sum(nil)), StoreSHA256: storeHash}
		break // At most one large file per maintenance pass.
	}
	return proofs, nil
}

func writeCleanupReceipt(root *os.Root, data []byte) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	name := cleanupReceiptTempPrefix + hex.EncodeToString(random[:])
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|base.FileLockOpenFlags(), 0600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = root.Remove(name) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := cleanupCheckpoint("receipt-written"); err != nil {
		return err
	}
	if err := root.Rename(name, cleanupReceiptName); err != nil {
		return err
	}
	return brainstore.SyncPinnedDirectory(root)
}

func removeCleanupReceiptTemps(root *os.Root) error {
	entries, err := rootEntries(root)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), cleanupReceiptTempPrefix)
		if !ok {
			continue
		}
		decoded, err := hex.DecodeString(suffix)
		if err != nil || len(decoded) != 16 {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe cleanup temporary file")
		}
		if err := root.Remove(entry.Name()); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return brainstore.SyncPinnedDirectory(root)
	}
	return nil
}

func cleanupInlineEligible(brainDir string, dirs []string, proofs map[string]cleanupReceipt) bool {
	if len(proofs) > 0 {
		return true
	}
	facts, err := os.OpenRoot(filepath.Join(brainDir, DirName))
	if err != nil {
		return true
	}
	defer facts.Close()
	for _, dir := range dirs {
		info, err := facts.Lstat(dir)
		if err != nil || !info.IsDir() {
			return true
		}
		park, err := facts.OpenRoot(dir)
		if err != nil {
			return true
		}
		entries, err := rootEntries(park)
		if err != nil || len(entries) == 0 {
			park.Close()
			return true
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				park.Close()
				return true
			}
			rel := filepath.Join(entry.Name(), FileName)
			info, err := park.Lstat(rel)
			if err != nil || !info.Mode().IsRegular() || info.Size() <= 1<<20 {
				park.Close()
				return true
			}
			if _, err := park.Lstat(filepath.Join(entry.Name(), cleanupReceiptName)); !os.IsNotExist(err) {
				park.Close()
				return true
			}
		}
		park.Close()
	}
	return false
}

// readMigrationRecords retains the existing permissive read contract, but only
// authorizes automatic removal for known, unambiguous records with valid
// content identities. Unknown fields and signed-anchor ambiguity keep recovery
// bytes. The migration's full read-back check verifies the canonical merge of
// these inputs before any receipt is published.
func readMigrationRecords(path string) ([]factmerge.Record, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, hash))
	scanner.Buffer(make([]byte, 8192), factmerge.MaxLineBytes+2)
	var records []factmerge.Record
	eligible := true
	seen := map[string]bool{}
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		raw := scanner.Bytes()
		if len(raw) > factmerge.MaxLineBytes {
			return nil, "", fmt.Errorf("parse %s line %d: record exceeds line limit", FileName, lineNumber)
		}
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var rec factmerge.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, "", fmt.Errorf("parse %s line %d: %w", FileName, lineNumber, err)
		}
		if eligible {
			var checked factmerge.Record
			if !utf8.Valid(line) || strictCleanupJSON(line, &checked) != nil || rec.ID != factmerge.RecordID(rec.Text, rec.Paths) || seen[rec.ID] {
				eligible = false
			}
			// Ordinary merge deduplicates anchor identity without comparing Verified.
			// Leave these inputs for the exact containment check after publication.
			for _, anchor := range rec.Provenance {
				if anchor.Verified || strings.ContainsRune(anchor.SessionID+anchor.Commit+anchor.CheckpointID+anchor.TurnID+anchor.Transcript, '\x00') {
					eligible = false
				}
			}
		}
		seen[rec.ID] = true
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("parse %s line %d: %w", FileName, lineNumber+1, err)
	}
	if eligible {
		return records, hex.EncodeToString(hash.Sum(nil)), nil
	}
	return records, "", nil
}


// legacyOrphanNames are files abandoned subsystems left behind in legacy
// branch dirs: the proposal framework (removed with the per-branch stores),
// per-branch vitality copies (vitality lives at the flat facts/ root now),
// and vector embeddings (semantic retrieval removed, trail #278/#416).
// Nothing reads them, but while one sits in a branch dir the empty-dir sweep
// retains the directory forever — 5.8 MB of noise on one dev machine
// (COR-2317). Unknown names still retain the directory: this list is the
// whole license to delete.
var legacyOrphanNames = []string{"proposals.ndjson", "vitality.ndjson", "vitality.json", "embeddings"}

// removeLegacyOrphans deletes known orphan files inside one legacy branch
// dir after its facts copy is verified gone. Only names on
// legacyOrphanNames are touched.
func removeLegacyOrphans(park *os.Root, branchDir string) error {
	branch, err := park.OpenRoot(branchDir)
	if err != nil {
		return err
	}
	defer branch.Close()
	removedAny := false
	for _, name := range legacyOrphanNames {
		if _, statErr := branch.Lstat(name); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return statErr
		}
		if rmErr := branch.RemoveAll(name); rmErr != nil {
			return rmErr
		}
		removedAny = true
	}
	if !removedAny {
		return nil
	}
	return brainstore.SyncPinnedDirectory(branch)
}
