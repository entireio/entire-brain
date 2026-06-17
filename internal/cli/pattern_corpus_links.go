package cli

import (
	"database/sql"
	"time"
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
		syms, err := semanticSymbolsForFiles(brainDir, sem, batch, len(batch)*200+200)
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
				e.ep, s.ID, s.Name, redactText(s.FilePath)); err != nil {
				return err
			}
			perEpisode[e.ep]++
		}
	}
	return tx.Commit()
}

// buildSynapses materializes the explanation graph for promotable evidence:
// pattern→evidence-episode, each evidence episode→fact/file/symbol/commit, and
// workspace-pattern→member-repo. Scoped to episodes that back a pattern so the
// graph explains patterns rather than mirroring the whole corpus.
func buildSynapses(db *sql.DB, now time.Time) error {
	if _, err := db.Exec(`DELETE FROM synapses`); err != nil {
		return err
	}
	ts := now.UTC().Format(time.RFC3339)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	add := func(from, to, kind string) error {
		if from == "" || to == "" {
			return nil
		}
		id := "syn:" + hexSHA(from+"\x00"+to+"\x00"+kind)
		_, err := tx.Exec(`INSERT OR IGNORE INTO synapses (id, from_id, to_id, kind, created_at) VALUES (?,?,?,?,?)`,
			id, from, to, kind, ts)
		return err
	}

	// pattern -> evidence episode
	evRows, err := tx.Query(`SELECT pattern_id, episode_id FROM pattern_evidence`)
	if err != nil {
		return err
	}
	type pe struct{ pid, ep string }
	var pes []pe
	for evRows.Next() {
		var p pe
		if evRows.Scan(&p.pid, &p.ep) == nil {
			pes = append(pes, p)
		}
	}
	evRows.Close()
	evidenceEpisodes := map[string]bool{}
	for _, p := range pes {
		if err := add(p.pid, p.ep, "pattern_evidence"); err != nil {
			return err
		}
		evidenceEpisodes[p.ep] = true
	}

	// each evidence episode -> fact / file / symbol / commit
	edge := func(query, kind string, toPrefix string) error {
		rows, err := tx.Query(query)
		if err != nil {
			return err
		}
		type link struct{ ep, to string }
		var links []link
		for rows.Next() {
			var l link
			if rows.Scan(&l.ep, &l.to) == nil {
				links = append(links, l)
			}
		}
		rows.Close()
		for _, l := range links {
			if !evidenceEpisodes[l.ep] {
				continue
			}
			if err := add(l.ep, toPrefix+l.to, kind); err != nil {
				return err
			}
		}
		return nil
	}
	if err := edge(`SELECT episode_id, fact_id FROM episode_facts`, "episode_fact", ""); err != nil {
		return err
	}
	if err := edge(`SELECT episode_id, path FROM episode_files WHERE path != ''`, "episode_file", "file:"); err != nil {
		return err
	}
	if err := edge(`SELECT episode_id, symbol_id FROM episode_symbols`, "episode_symbol", ""); err != nil {
		return err
	}
	if err := edge(`SELECT episode_id, commit_hash FROM episode_commits`, "episode_commit", "commit:"); err != nil {
		return err
	}

	// workspace pattern -> member repo variant
	wsRows, err := tx.Query(`SELECT pattern_id, repo_key FROM workspace_pattern_repos`)
	if err != nil {
		return err
	}
	type wr struct{ pid, repo string }
	var wrs []wr
	for wsRows.Next() {
		var w wr
		if wsRows.Scan(&w.pid, &w.repo) == nil {
			wrs = append(wrs, w)
		}
	}
	wsRows.Close()
	for _, w := range wrs {
		if err := add(w.pid, "repo:"+w.repo, "workspace_repo"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
