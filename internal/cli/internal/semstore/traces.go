package semstore

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

const TraceFile = "semantic/traces/observations.json"

const TraceMaxBytes = 64 << 20

type TraceObservation struct {
	ImportedAt   string `json:"imported_at"`
	SourcePath   string `json:"source_path"`
	FromID       string `json:"from_id"`
	ToID         string `json:"to_id"`
	ObservedType string `json:"observed_type"`
	Matched      int    `json:"matched_static_edge"`
}

func ReadTraces(brainDir string) ([]TraceObservation, error) {
	if brainDir == "" {
		return nil, nil
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, TraceFile); err != nil {
		return nil, err
	}
	data, err := base.SafeReadFile(filepath.Join(brainDir, TraceFile), TraceMaxBytes)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []TraceObservation
	if err = json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func loadGraphTraceTable(db *sql.DB, brainDir string) error {
	_, err := db.Exec("CREATE TEMP TABLE runtime_traces (id INTEGER PRIMARY KEY, imported_at TEXT NOT NULL, source_path TEXT NOT NULL, from_id TEXT NOT NULL, to_id TEXT NOT NULL, observed_type TEXT, matched_static_edge INTEGER NOT NULL)")
	if err != nil {
		return err
	}
	rows, err := ReadTraces(brainDir)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO runtime_traces(imported_at,source_path,from_id,to_id,observed_type,matched_static_edge) VALUES(?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, o := range rows {
		if _, err = stmt.Exec(o.ImportedAt, o.SourcePath, o.FromID, o.ToID, o.ObservedType, o.Matched); err != nil {
			return err
		}
	}
	return tx.Commit()
}
