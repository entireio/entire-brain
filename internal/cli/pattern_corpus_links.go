package cli

import (
	"database/sql"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// Corpus link layers (Pattern Consolidation v2, Priority 3): episode→symbol
// links from the semantic graph, and synapses — internal explanation edges that
// connect a pattern to its evidence and each evidence episode to the
// facts/files/symbols/commits it touched. Both are deterministic, token-free,
// and rebuilt on every corpus build; both degrade to empty when their source
// (semantic index / no patterns) is absent.

const episodeSymbolCapPerEpisode = 25

// linkEpisodeSymbols populates episode_symbols from the semantic graph: each
// episode is linked to the symbols defined in the files it touched. No-op when no
// semantic index exists.
func linkEpisodeSymbols(db *sql.DB, brainDir string, sem *semanticSourceManifest) error {
	// Always clear first: if a semantic index was present before and has since
	// disappeared (or become unreadable), the table must degrade to empty rather
	// than keep stale links.
	if _, err := db.Exec(`DELETE FROM episode_symbols`); err != nil {
		return err
	}
	if sem == nil {
		return nil
	}
	// Distinct touched files across all episodes.
	fileRows, err := db.Query(`SELECT DISTINCT path FROM episode_files WHERE path != ''`)
	if err != nil {
		return err
	}
	var files []string
	for fileRows.Next() {
		var p string
		if fileRows.Scan(&p) == nil {
			files = append(files, p)
		}
	}
	fileRows.Close()
	if len(files) == 0 {
		return nil
	}

	// file_path -> symbols, fetched in chunks.
	symbolsByFile := map[string][]semanticRecord{}
	for start := 0; start < len(files); start += 400 {
		end := start + 400
		if end > len(files) {
			end = len(files)
		}
		batch := files[start:end]
		syms, err := semstore.SymbolsForFiles(brainDir, sem, batch, len(batch)*200+200)
		if err != nil {
			return nil // semantic unreadable: degrade to no symbol links
		}
		for _, s := range syms {
			symbolsByFile[s.FilePath] = append(symbolsByFile[s.FilePath], s)
		}
	}
	if len(symbolsByFile) == 0 {
		return nil
	}

	// Link each episode to the symbols in its touched files (capped).
	epRows, err := db.Query(`SELECT episode_id, path FROM episode_files WHERE path != ''`)
	if err != nil {
		return err
	}
	type ef struct{ ep, path string }
	var efs []ef
	for epRows.Next() {
		var e ef
		if epRows.Scan(&e.ep, &e.path) == nil {
			efs = append(efs, e)
		}
	}
	epRows.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	perEpisode := map[string]int{}
	for _, e := range efs {
		for _, s := range symbolsByFile[e.path] {
			if perEpisode[e.ep] >= episodeSymbolCapPerEpisode {
				break
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO episode_symbols (episode_id, symbol_id, name, file_path) VALUES (?,?,?,?)`,
				e.ep, s.ID, s.Name, base.RedactText(s.FilePath)); err != nil {
				return err
			}
			perEpisode[e.ep]++
		}
	}
	return tx.Commit()
}
