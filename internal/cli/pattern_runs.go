package cli

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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

// recordPatternRun snapshots the corpus counts and appends a run record (keeping
// the most recent patternRunsKeep). Best-effort: a logging failure never fails
// the build.
func recordPatternRun(db *sql.DB, brainDir string, now time.Time) {
	run := patternRun{
		At:             now.UTC().Format(time.RFC3339),
		IndexerVersion: patternIndexerVersion,
		Episodes:       corpusScalar(db, `SELECT COUNT(*) FROM episodes`),
		Commands:       corpusScalar(db, `SELECT COUNT(*) FROM episode_commands`),
		Grams:          corpusScalar(db, `SELECT COUNT(*) FROM grams`),
		FactLinks:      corpusScalar(db, `SELECT COUNT(*) FROM episode_facts`),
		SymbolLinks:    corpusScalar(db, `SELECT COUNT(*) FROM episode_symbols`),
		Commits:        corpusScalar(db, `SELECT COUNT(*) FROM episode_commits`),
		Patterns:       corpusScalar(db, `SELECT COUNT(*) FROM patterns`),
		Dossiers:       corpusScalar(db, `SELECT COUNT(*) FROM dossiers`),
		Themes:         corpusScalar(db, `SELECT COUNT(*) FROM themes`),
		Synapses:       corpusScalar(db, `SELECT COUNT(*) FROM synapses`),
	}
	_ = appendPatternRun(brainDir, run)
}

func appendPatternRun(brainDir string, run patternRun) error {
	path := filepath.Join(brainDir, filepath.FromSlash(patternRunsRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	runs := append(loadPatternRuns(brainDir), run)
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
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// loadPatternRuns returns the recorded runs oldest-first; empty when none.
func loadPatternRuns(brainDir string) []patternRun {
	path := filepath.Join(brainDir, filepath.FromSlash(patternRunsRelPath))
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var runs []patternRun
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r patternRun
		if json.Unmarshal([]byte(line), &r) == nil {
			runs = append(runs, r)
		}
	}
	return runs
}

// lastPatternRun returns the most recent run, if any.
func lastPatternRun(brainDir string) (patternRun, bool) {
	runs := loadPatternRuns(brainDir)
	if len(runs) == 0 {
		return patternRun{}, false
	}
	return runs[len(runs)-1], true
}
