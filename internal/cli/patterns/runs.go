package patterns

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// Pattern run history (Pattern Consolidation v2, Priority 3): an append-only,
// human-readable log of what each corpus build produced, so a user can answer
// "what did the brain update last time?". It lives beside the rebuildable corpus
// (patterns/runs.ndjson) — it is operational telemetry about the rebuildable
// layer, not user-state — and is surfaced through `patterns status`.

// IndexerVersion identifies the corpus indexer that produced a build; bump it
// when extraction/classification output changes so consumers can detect stale
// corpora.
const IndexerVersion = 11

const RunsRelPath = "patterns/runs.ndjson"
const patternRunsKeep = 50
const patternRunsMaxBytes int64 = 4 << 20

type Run struct {
	At             string `json:"at"`
	IndexerVersion int    `json:"indexer_version"`
	Episodes       int    `json:"episodes"`
	Commands       int    `json:"commands"`
	Grams          int    `json:"grams"`
	FactLinks      int    `json:"fact_links"`
	SymbolLinks    int    `json:"symbol_links"`
	Commits        int    `json:"commits"`
	Patterns       int    `json:"patterns"`
	Dossiers       int    `json:"dossiers"`
	Themes         int    `json:"themes"`
	Synapses       int    `json:"synapses"`
}

// RunFromStaging snapshots counts while the complete private staging
// database is still writable/open. The run is persisted only after successful
// corpus publication, so a failed commit cannot describe data that never became
// live and no post-publication SQLite connection is required.
func RunFromStaging(db *sql.DB, now time.Time) (Run, error) {
	run := Run{
		At:             now.UTC().Format(time.RFC3339),
		IndexerVersion: IndexerVersion,
	}
	counts := []struct {
		target *int
		table  string
	}{
		{&run.Episodes, "episodes"},
		{&run.Commands, "episode_commands"},
		{&run.Grams, "grams"},
		{&run.FactLinks, "episode_facts"},
		{&run.SymbolLinks, "episode_symbols"},
		{&run.Commits, "episode_commits"},
		{&run.Patterns, "patterns"},
		{&run.Dossiers, "dossiers"},
		{&run.Themes, "themes"},
		{&run.Synapses, "synapses"},
	}
	for _, count := range counts {
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + count.table).Scan(count.target); err != nil {
			return Run{}, fmt.Errorf("snapshot staged pattern run count %s: %w", count.table, err)
		}
	}
	return run, nil
}

func AppendRun(brainDir string, run Run) error {
	runs, err := LoadRunsChecked(brainDir)
	if err != nil {
		// A corrupt run log must not be permanent. AppendRun is the ONLY
		// writer and it rewrites the file wholesale, so returning here left the
		// file unrepairable: `patterns status` and `patterns list` failed
		// forever, and the `patterns refresh` that is supposed to repair them
		// bailed out before writing. The log was also written non-atomically by
		// older builds, so a truncated final line is reachable on any existing
		// brain. Run history is bounded, disposable telemetry and never a
		// source of truth, so recover by starting a fresh log. An UNSAFE path
		// (alias, symlink, irregular file) is not content corruption and still
		// fails closed.
		if brainstore.MemoryErrorCode(err) != brainstore.StateCorruptCode {
			return err
		}
		runs = nil
	}
	runs = append(runs, run)
	if len(runs) > patternRunsKeep {
		runs = runs[len(runs)-patternRunsKeep:]
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, r := range runs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return brainstore.WriteBrainRelativeFileAtomic(brainDir, RunsRelPath, []byte(b.String()), 0o600)
}

// LoadRunsChecked returns recorded runs oldest-first. A genuinely absent
// log is the one optional state; aliases, I/O failures, oversized data, invalid
// JSON, and scanner failures are typed state errors rather than an empty log.
func LoadRunsChecked(brainDir string) ([]Run, error) {
	data, present, err := brainstore.ReadMemoryStateFile(brainDir, RunsRelPath, "pattern run history", patternRunsMaxBytes)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	var runs []Run
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNumber := 0
	for sc.Scan() {
		lineNumber++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Run
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s: parse pattern run history line %d: %w", brainstore.StateCorruptCode, lineNumber, err)
		}
		runs = append(runs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: scan pattern run history: %w", brainstore.StateCorruptCode, err)
	}
	return runs, nil
}

// LoadRuns retains the historical best-effort helper for internal tests.
// Production response surfaces use LoadRunsChecked.
func LoadRuns(brainDir string) []Run {
	runs, _ := LoadRunsChecked(brainDir)
	return runs
}

// LastRun returns the most recent run, if any.
func LastRun(brainDir string) (Run, bool) {
	run, ok, _ := LastRunChecked(brainDir)
	return run, ok
}

func LastRunChecked(brainDir string) (Run, bool, error) {
	runs, err := LoadRunsChecked(brainDir)
	if err != nil {
		return Run{}, false, err
	}
	if len(runs) == 0 {
		return Run{}, false, nil
	}
	return runs[len(runs)-1], true, nil
}
