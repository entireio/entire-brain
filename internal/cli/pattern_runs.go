package cli

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Pattern run history (Pattern Consolidation v2, Priority 3): an append-only,
// human-readable log of what each corpus build produced, so a user can answer
// "what did the brain update last time?". It lives beside the rebuildable corpus
// (patterns/runs.ndjson) — it is operational telemetry about the rebuildable
// layer, not user-state — and is surfaced through `patterns status`.

const patternRunsRelPath = "patterns/runs.ndjson"
const patternRunsKeep = 50
const patternRunsMaxBytes int64 = 4 << 20

type patternRun struct {
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

// patternRunFromStaging snapshots counts while the complete private staging
// database is still writable/open. The run is persisted only after successful
// corpus publication, so a failed commit cannot describe data that never became
// live and no post-publication SQLite connection is required.
func patternRunFromStaging(db *sql.DB, now time.Time) (patternRun, error) {
	run := patternRun{
		At:             now.UTC().Format(time.RFC3339),
		IndexerVersion: patternIndexerVersion,
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
			return patternRun{}, fmt.Errorf("snapshot staged pattern run count %s: %w", count.table, err)
		}
	}
	return run, nil
}

func appendPatternRun(brainDir string, run patternRun) error {
	runs, err := loadPatternRunsChecked(brainDir)
	if err != nil {
		return err
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
	return writeBrainRelativeFileAtomic(brainDir, patternRunsRelPath, []byte(b.String()), 0o600)
}

// loadPatternRunsChecked returns recorded runs oldest-first. A genuinely absent
// log is the one optional state; aliases, I/O failures, oversized data, invalid
// JSON, and scanner failures are typed state errors rather than an empty log.
func loadPatternRunsChecked(brainDir string) ([]patternRun, error) {
	data, present, err := readMemoryStateFile(brainDir, patternRunsRelPath, "pattern run history", patternRunsMaxBytes)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	var runs []patternRun
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNumber := 0
	for sc.Scan() {
		lineNumber++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r patternRun
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s: parse pattern run history line %d: %w", memoryErrStateCorrupt, lineNumber, err)
		}
		runs = append(runs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: scan pattern run history: %w", memoryErrStateCorrupt, err)
	}
	return runs, nil
}

// loadPatternRuns retains the historical best-effort helper for internal tests.
// Production response surfaces use loadPatternRunsChecked.
func loadPatternRuns(brainDir string) []patternRun {
	runs, _ := loadPatternRunsChecked(brainDir)
	return runs
}

// lastPatternRun returns the most recent run, if any.
func lastPatternRun(brainDir string) (patternRun, bool) {
	run, ok, _ := lastPatternRunChecked(brainDir)
	return run, ok
}

func lastPatternRunChecked(brainDir string) (patternRun, bool, error) {
	runs, err := loadPatternRunsChecked(brainDir)
	if err != nil {
		return patternRun{}, false, err
	}
	if len(runs) == 0 {
		return patternRun{}, false, nil
	}
	return runs[len(runs)-1], true, nil
}
