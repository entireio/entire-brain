package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
)

// Legacy artifact helpers are compiled only for migration and compatibility
// fixtures. Production Brain never builds or copies a code graph.

// buildSemanticGeneration ingests the filtered snapshot into a fresh SQLite
// generation. snapshot is scanned record-by-record (never fully buffered);
// contentSuffix is the hex digest fragment used to disambiguate the generation
// directory when a base ID already exists.
// buildSemanticGeneration takes a context because it is the LONGEST
// UNINTERRUPTIBLE STAGE of an index, and an MCP call is bounded at 60 seconds.
//
// It used to take none. brain_index_repository's deadline therefore decided only
// WHEN THE ERROR WAS CHOSEN, not when the call returned: the store build carried
// on to completion and the 60 second cap was reported after ~113 seconds of wall
// clock, on a call whose whole point was to protect a host configured to wait 60.
// The cap bought six seconds out of a hundred and nineteen. Threading the
// context here, and checking it inside the row loop, is what makes the advertised
// number true.
func buildSemanticGeneration(ctx context.Context, brainDir, repoDir, generationID string, snapshot io.Reader, contentSuffix string, header semanticHeader, counts semanticCounts, now time.Time) (string, semanticBuildMetrics, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	start := time.Now()
	generationsRoot := filepath.Join(brainDir, semstore.DirName, semanticGenerationsDir)
	if err := base.RejectExistingSymlinkPathComponents(brainDir, filepath.Join(semstore.DirName, semanticGenerationsDir)); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if err := os.MkdirAll(generationsRoot, 0o700); err != nil {
		return "", semanticBuildMetrics{}, fmt.Errorf("create semantic generations dir: %w", err)
	}
	finalID, err := semanticAvailableGenerationID(generationsRoot, generationID, contentSuffix)
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	tmpDir, err := os.MkdirTemp(generationsRoot, ".tmp-"+finalID+"-*")
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	dbPath := filepath.Join(tmpDir, semstore.SQLiteName)
	db, err := sql.Open(sqlitedriver.Name, dbPath)
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		return "", semanticBuildMetrics{}, err
	}
	metrics, err := populateSemanticSQLite(ctx, db, repoDir, finalID, snapshot, header, counts, now, tmpDir)
	if closeErr := db.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	metrics.StoreBytes = info.Size()
	metrics.BuildMillis = time.Since(start).Milliseconds()
	metricsData, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	metricsData = append(metricsData, '\n')
	if err := brainstore.WriteFileAtomic(filepath.Join(tmpDir, semanticMetricsName), metricsData, 0o600); err != nil {
		return "", semanticBuildMetrics{}, err
	}

	finalDir := filepath.Join(generationsRoot, finalID)
	if err := base.RejectExistingSymlinkPathComponents(brainDir, filepath.Join(semstore.DirName, semanticGenerationsDir, finalID)); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if _, err := os.Lstat(finalDir); err == nil {
		return "", semanticBuildMetrics{}, fmt.Errorf("semantic generation target already exists: %s", finalDir)
	} else if !os.IsNotExist(err) {
		return "", semanticBuildMetrics{}, err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	cleanup = false
	return filepath.ToSlash(filepath.Join(semstore.DirName, semanticGenerationsDir, finalID)), metrics, nil
}

func initializeSemanticSQLite(db *sql.DB) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE files (path TEXT PRIMARY KEY, blob TEXT, content_hash TEXT, language TEXT)`,
		`CREATE TABLE symbols (id TEXT PRIMARY KEY, kind TEXT, name TEXT, qualified_name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, signature TEXT, language TEXT, stable_id_version TEXT)`,
		`CREATE TABLE relations (id INTEGER PRIMARY KEY AUTOINCREMENT, from_id TEXT NOT NULL, to_id TEXT NOT NULL, type TEXT NOT NULL, confidence REAL, reason TEXT, warning_codes TEXT)`,
		`CREATE TABLE reverse_relations (to_id TEXT NOT NULL, from_id TEXT NOT NULL, type TEXT NOT NULL)`,
		`CREATE TABLE runtime_traces (id INTEGER PRIMARY KEY AUTOINCREMENT, imported_at TEXT NOT NULL, source_path TEXT NOT NULL, from_id TEXT NOT NULL, to_id TEXT NOT NULL, observed_type TEXT, matched_static_edge INTEGER NOT NULL)`,
		`CREATE INDEX idx_symbols_name ON symbols(name)`,
		`CREATE INDEX idx_symbols_qualified_name ON symbols(qualified_name)`,
		`CREATE INDEX idx_symbols_file_path ON symbols(file_path)`,
		`CREATE INDEX idx_relations_from ON relations(from_id)`,
		`CREATE INDEX idx_relations_to ON relations(to_id)`,
		`CREATE INDEX idx_reverse_to ON reverse_relations(to_id)`,
		`CREATE INDEX idx_runtime_traces_from ON runtime_traces(from_id)`,
		`CREATE INDEX idx_runtime_traces_to ON runtime_traces(to_id)`,
		`CREATE TABLE symbol_fts (id TEXT PRIMARY KEY, text TEXT NOT NULL)`,
		`CREATE TABLE parse_cache (path TEXT PRIMARY KEY, blob TEXT, content_hash TEXT, artifact_path TEXT NOT NULL)`,
		`CREATE TABLE warnings (id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT, severity TEXT, path TEXT, effect TEXT, detail TEXT, source TEXT)`,
		`CREATE TABLE metrics (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("initialize semantic sqlite: %w", err)
		}
	}
	return nil
}

func populateSemanticSQLite(ctx context.Context, db *sql.DB, repoDir, generationID string, snapshot io.Reader, header semanticHeader, counts semanticCounts, now time.Time, generationDir string) (semanticBuildMetrics, error) {
	tx, err := db.Begin()
	if err != nil {
		return semanticBuildMetrics{}, err
	}
	commit := func(err error) (semanticBuildMetrics, error) {
		if err != nil {
			_ = tx.Rollback()
			return semanticBuildMetrics{}, err
		}
		if err := tx.Commit(); err != nil {
			return semanticBuildMetrics{}, err
		}
		return semanticBuildMetrics{}, nil
	}

	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?), (?, ?), (?, ?), (?, ?)`,
		"schema_version", header.SchemaVersion,
		"provider", header.Provider,
		"provider_version", header.ProviderVersion,
		"generation_id", generationID,
	); err != nil {
		return commit(err)
	}
	for _, warning := range header.Warnings {
		if err := insertSemanticWarning(tx, warning, "warning"); err != nil {
			return commit(err)
		}
	}
	for _, failure := range header.PartialFailures {
		if err := insertSemanticWarning(tx, failure, "partial_failure"); err != nil {
			return commit(err)
		}
	}

	files := make(map[string]semanticRecord)
	scanner := newSemanticScanner(snapshot)
	if !scanner.Scan() {
		return commit(errors.New("semantic snapshot missing header"))
	}
	// One check per batch of records, not per record: ctx.Err() is cheap but
	// 200k of them is still work, and 1024 records is a small fraction of a
	// second on any snapshot this code has seen.
	scanned := 0
	for scanner.Scan() {
		if scanned++; scanned%semanticBuildCancelCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return commit(err)
			}
		}
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			return commit(err)
		}
		switch record.RecordType {
		case "file":
			path := record.SemanticPath()
			if path != "" {
				path, err = validateSemanticProviderPath(path)
				if err != nil {
					return commit(err)
				}
				record.SetSemanticPath(path)
				files[path] = record
			}
		case "symbol":
			path := record.SemanticPath()
			if path != "" {
				path, err = validateSemanticProviderPath(path)
				if err != nil {
					return commit(err)
				}
				record.SetSemanticPath(path)
				if existing, ok := files[path]; ok {
					if strings.TrimSpace(existing.Language) == "" && strings.TrimSpace(record.Language) != "" {
						existing.Language = record.Language
						files[path] = existing
					}
				} else {
					files[path] = semanticRecord{RecordType: "file", FilePath: path}
				}
			}
			if err := insertSemanticSymbol(tx, record); err != nil {
				return commit(err)
			}
		case "relation":
			if err := insertSemanticRelation(tx, record); err != nil {
				return commit(err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return commit(err)
	}

	parseCacheFiles := 0
	for path, record := range files {
		if parseCacheFiles%semanticBuildCancelCheckEveryFile == 0 {
			if err := ctx.Err(); err != nil {
				return commit(err)
			}
		}
		contentHash := semanticFileContentHash(repoDir, path)
		if contentHash == "" && record.Blob != "" {
			contentHash = "git-blob:" + record.Blob
		}
		artifactRel, err := writeSemanticParseCacheArtifact(generationDir, path, record.Blob, contentHash)
		if err != nil {
			return commit(err)
		}
		parseCacheFiles++
		if _, err := tx.Exec(`INSERT OR REPLACE INTO files(path, blob, content_hash, language) VALUES (?, ?, ?, ?)`, path, record.Blob, contentHash, strings.TrimSpace(record.Language)); err != nil {
			return commit(err)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO parse_cache(path, blob, content_hash, artifact_path) VALUES (?, ?, ?, ?)`, path, record.Blob, contentHash, artifactRel); err != nil {
			return commit(err)
		}
	}

	metrics := semanticBuildMetrics{
		GeneratedAt:     now,
		GenerationID:    generationID,
		Commit:          header.Commit,
		Tree:            header.Tree,
		Files:           len(files),
		Symbols:         counts.Symbols,
		Relations:       counts.Relations,
		ReverseEdges:    counts.Relations,
		Warnings:        len(header.Warnings),
		PartialFailures: len(header.PartialFailures),
		ParseCacheFiles: parseCacheFiles,
	}
	metricsJSON, _ := json.Marshal(metrics)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO metrics(key, value) VALUES (?, ?)`, "build", string(metricsJSON)); err != nil {
		return commit(err)
	}
	if err := tx.Commit(); err != nil {
		return semanticBuildMetrics{}, err
	}
	return metrics, nil
}

func insertSemanticWarning(tx *sql.Tx, warning semanticWarning, source string) error {
	_, err := tx.Exec(`INSERT INTO warnings(code, severity, path, effect, detail, source) VALUES (?, ?, ?, ?, ?, ?)`,
		warning.Code, warning.Severity, warning.Path, warning.Effect, warning.Detail, source)
	return err
}

func insertSemanticSymbol(tx *sql.Tx, record semanticRecord) error {
	if record.ID == "" {
		return nil
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO symbols(id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Kind, record.Name, record.QualifiedName, record.FilePath, record.StartLine, record.EndLine, record.Signature, record.Language, record.StableIDVersion)
	if err != nil {
		return err
	}
	searchText := strings.Join([]string{record.Name, record.QualifiedName, record.Signature, record.FilePath, record.Kind, record.Language}, " ")
	_, err = tx.Exec(`INSERT OR REPLACE INTO symbol_fts(id, text) VALUES (?, ?)`, record.ID, searchText)
	return err
}

func insertSemanticRelation(tx *sql.Tx, record semanticRecord) error {
	if record.FromID == "" || record.ToID == "" || record.Type == "" {
		return nil
	}
	warningCodes, err := json.Marshal(record.WarningCodes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO relations(from_id, to_id, type, confidence, reason, warning_codes) VALUES (?, ?, ?, ?, ?, ?)`,
		record.FromID, record.ToID, record.Type, record.Confidence, record.Reason, string(warningCodes)); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO reverse_relations(to_id, from_id, type) VALUES (?, ?, ?)`, record.ToID, record.FromID, record.Type)
	return err
}

func writeSemanticParseCacheArtifact(generationDir, path, blob, contentHash string) (string, error) {
	key := contentHash
	if key == "" {
		sum := sha256.Sum256([]byte(path + "\x00" + blob))
		key = "sha256:" + hex.EncodeToString(sum[:])
	}
	if !strings.HasPrefix(key, "sha256:") {
		sum := sha256.Sum256([]byte(path + "\x00" + blob + "\x00" + key))
		key = "sha256:" + hex.EncodeToString(sum[:])
	}
	cleanKey := strings.TrimPrefix(key, "sha256:")
	if len(cleanKey) < 4 {
		cleanKey = hex.EncodeToString([]byte(cleanKey))
	}
	rel := filepath.Join("parse-cache", cleanKey[:2], cleanKey[2:4], cleanKey+".json")
	payload := map[string]string{
		"path":         path,
		"blob":         blob,
		"content_hash": key,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := brainstore.WriteFileAtomic(filepath.Join(generationDir, rel), data, 0o600); err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}
