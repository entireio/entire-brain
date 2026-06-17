package cli

import (
	"database/sql"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Workspace V2 corpus (Pattern Consolidation v2, Priority 2).
//
// Cross-repo patterns are aggregated from each member repo's corpus.sqlite (the
// already-built V2 analytical index), NOT from the legacy merged JSON files and
// NOT by copying raw member episodes between repo stores. A pattern surfaces at
// the workspace level only when ≥2 member repos share the same
// (type, intent_sig, gram, meta_id) key. The aggregate is written to a workspace
// corpus at <workspaceDir>/patterns/corpus.sqlite using the same schema as a repo
// corpus (scope='workspace'), so every existing surface — patterns list, get,
// brief — works uniformly over it. Only anchor pointers (repo_key + session +
// path + line, redacted) are copied into the workspace store; transcripts and
// raw evidence stay in the member repos.

type workspaceCorpusCounts struct {
	Members    int      `json:"members"`
	WithCorpus int      `json:"members_with_corpus"`
	Patterns   int      `json:"patterns"`
	Warnings   []string `json:"warnings,omitempty"`
}

// wsPatternKey identifies a candidate shared across member repos.
type wsPatternKey struct {
	typ, intentSig, gram, metaID string
}

type wsRepoStat struct {
	repoKey                      string
	support, succ, corr, neut    int
	anchorPath                   string
	anchorLine                   int
	anchorSession, anchorOutcome string
	anchorEpisodeID              string
}

type wsAgg struct {
	key                       wsPatternKey
	title, scope              string
	support, succ, corr, neut int
	repos                     map[string]*wsRepoStat
}

// buildWorkspacePatternCorpus rebuilds the workspace corpus from member corpora.
func buildWorkspacePatternCorpus(env EntireEnv, manifest workspaceManifest, now time.Time) (workspaceCorpusCounts, error) {
	var counts workspaceCorpusCounts
	wsBrainDir, err := workspaceDir(env, manifest.Name)
	if err != nil {
		return counts, err
	}
	path, err := prepareBrainRelativeSQLiteFile(wsBrainDir, patternCorpusPath)
	if err != nil {
		return counts, err
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return counts, err
	}
	defer db.Close()
	for _, p := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(p); err != nil {
			return counts, err
		}
	}
	for _, stmt := range patternCorpusSchema {
		if _, err := db.Exec(stmt); err != nil {
			return counts, err
		}
	}

	repos := append([]workspaceRepo(nil), manifest.Repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].RepoKey < repos[j].RepoKey })
	counts.Members = len(repos)

	aggs := map[wsPatternKey]*wsAgg{}
	for _, repo := range repos {
		memberBrainDir, err := brainDirForKey(env, repo.RepoKey)
		if err != nil {
			counts.Warnings = append(counts.Warnings, repo.RepoKey+": "+err.Error())
			continue
		}
		mdb, err := openPatternCorpusDB(memberBrainDir)
		if err != nil {
			// Missing/stale member corpus degrades gracefully (refresh that repo).
			counts.Warnings = append(counts.Warnings, repo.RepoKey+": no corpus (run `entire brain refresh` there)")
			continue
		}
		counts.WithCorpus++
		if err := collectMemberPatterns(mdb, repo.RepoKey, aggs); err != nil {
			counts.Warnings = append(counts.Warnings, repo.RepoKey+": "+err.Error())
		}
		mdb.Close()
	}

	tx, err := db.Begin()
	if err != nil {
		return counts, err
	}
	// Rebuildable: clear all prior workspace corpus content, including the
	// verifier-cache tables — they have no FK cascade, so stale verdicts must not
	// survive a rebuild and join onto a newly-rebuilt pattern row sharing the same
	// stable id (which could even suppress it as 'rejected').
	for _, stmt := range []string{`DELETE FROM patterns`, `DELETE FROM episodes`, `DELETE FROM workspace_pattern_repos`, `DELETE FROM dossiers`, `DELETE FROM deep_dossiers`} {
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return counts, err
		}
	}
	ts := now.UTC().Format(time.RFC3339)
	memberCount := counts.Members
	for _, a := range aggs {
		if len(a.repos) < workspaceMinRepos {
			continue // a workspace pattern must be shared across ≥2 repos
		}
		if err := writeWorkspacePattern(tx, manifest.Name, a, memberCount, ts); err != nil {
			tx.Rollback()
			return counts, err
		}
		counts.Patterns++
	}
	if err := tx.Commit(); err != nil {
		return counts, err
	}
	// Materialize the explanation graph (workspace_repo edges from the per-repo
	// breakdown) and record the run, mirroring the repo corpus build.
	if err := buildSynapses(db, now); err != nil {
		return counts, err
	}
	recordPatternRun(db, wsBrainDir, now)
	return counts, nil
}

// collectMemberPatterns reads one member corpus's patterns + top anchor and folds
// them into the cross-repo aggregates.
func collectMemberPatterns(mdb *sql.DB, repoKey string, aggs map[wsPatternKey]*wsAgg) error {
	anchors := memberTopAnchors(mdb)
	rows, err := mdb.Query(`
		SELECT id, type, COALESCE(intent_sig,''), COALESCE(gram,''), COALESCE(meta_id,''),
		       title, support, outcome_success, outcome_corrected, outcome_neutral
		FROM patterns WHERE scope='repo'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, typ, intentSig, gram, metaID, title string
			support, succ, corr, neut               int
		)
		if err := rows.Scan(&id, &typ, &intentSig, &gram, &metaID, &title, &support, &succ, &corr, &neut); err != nil {
			return err
		}
		key := wsPatternKey{typ, intentSig, gram, metaID}
		a := aggs[key]
		if a == nil {
			a = &wsAgg{key: key, title: title, scope: "workspace", repos: map[string]*wsRepoStat{}}
			aggs[key] = a
		}
		a.support += support
		a.succ += succ
		a.corr += corr
		a.neut += neut
		st := &wsRepoStat{repoKey: repoKey, support: support, succ: succ, corr: corr, neut: neut}
		if an, ok := anchors[id]; ok {
			st.anchorPath, st.anchorLine = an.path, an.line
			st.anchorSession, st.anchorOutcome, st.anchorEpisodeID = an.session, an.outcome, an.episodeID
		}
		a.repos[repoKey] = st
	}
	return rows.Err()
}

type memberAnchor struct {
	path, session, outcome, episodeID string
	line                              int
}

func memberTopAnchors(mdb *sql.DB) map[string]memberAnchor {
	out := map[string]memberAnchor{}
	rows, err := mdb.Query(`
		SELECT pe.pattern_id, pe.source_path, pe.start_line, COALESCE(pe.outcome,''), e.id, e.session_id
		FROM pattern_evidence pe JOIN episodes e ON e.id=pe.episode_id
		WHERE pe.rank=0`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var pid, path, outcome, epID, session string
		var line int
		if rows.Scan(&pid, &path, &line, &outcome, &epID, &session) == nil {
			out[pid] = memberAnchor{path: path, line: line, outcome: outcome, episodeID: epID, session: session}
		}
	}
	return out
}

// writeWorkspacePattern persists one cross-repo pattern: minimal anchor episodes
// (pointers only, redacted), the workspace pattern row, its evidence, and the
// per-repo breakdown.
func writeWorkspacePattern(tx *sql.Tx, workspace string, a *wsAgg, memberCount int, ts string) error {
	repoKeys := make([]string, 0, len(a.repos))
	for k := range a.repos {
		repoKeys = append(repoKeys, k)
	}
	sort.Strings(repoKeys)

	cluster := "workspace:" + workspace + "\x00" + a.key.typ + "\x00" + a.key.intentSig + "\x00gram:" + a.key.gram + "\x00meta:" + a.key.metaID
	id := "pattern:" + hexSHA("workspace\x00"+workspace+"\x00"+cluster)
	strength := workspaceStrengthV2(a.key.typ, len(a.repos), memberCount, a.support, a.succ, a.corr, a.neut)
	strength = math.Round(strength*1000) / 1000

	if _, err := tx.Exec(`INSERT INTO patterns
		(id, type, scope, repo_key, workspace, cluster_key, title, intent_sig, gram, meta_id, strength, strength_label, support,
		 n_repos, n_authors, n_branches, outcome_success, outcome_corrected, outcome_neutral, outcome_failed, fingerprint, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, a.key.typ, "workspace", "", workspace, cluster, redactText(a.title), a.key.intentSig, a.key.gram, a.key.metaID,
		strength, strengthLabelV2(strength), a.support, len(a.repos), 0, 0, a.succ, a.corr, a.neut, 0,
		hexSHA(cluster+strconv.Itoa(a.support)), "active", ts, ts); err != nil {
		return err
	}

	rank := 0
	for _, rk := range repoKeys {
		st := a.repos[rk]
		if _, err := tx.Exec(`INSERT OR IGNORE INTO workspace_pattern_repos
			(pattern_id, repo_key, support, outcome_success, outcome_corrected, outcome_neutral) VALUES (?,?,?,?,?,?)`,
			id, rk, st.support, st.succ, st.corr, st.neut); err != nil {
			return err
		}
		if st.anchorEpisodeID == "" || st.anchorPath == "" {
			continue
		}
		// Minimal anchor episode (pointer only; redacted). Composite-safe id keeps
		// member episodes from colliding across repos within the workspace corpus.
		wsEpID := "wsep:" + hexSHA(rk+"\x00"+st.anchorEpisodeID)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO episodes
			(id, episode_key, repo_key, workspace, session_id, turn_ord, branch, source_path, start_line, end_line, outcome, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			wsEpID, wsEpID, rk, workspace, redactText(st.anchorSession), 0, "", redactText(st.anchorPath), st.anchorLine, st.anchorLine,
			nonEmptyOr(st.anchorOutcome, "neutral"), ts); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO pattern_evidence
			(pattern_id, episode_id, rank, outcome, source_path, start_line, end_line) VALUES (?,?,?,?,?,?,?)`,
			id, wsEpID, rank, nonEmptyOr(st.anchorOutcome, "neutral"), redactText(st.anchorPath), st.anchorLine, st.anchorLine); err != nil {
			return err
		}
		rank++
	}
	return nil
}

func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// workspaceStrengthV2 rewards repo breadth on top of support and outcome — a
// pattern shared by more members of the workspace is stronger.
func workspaceStrengthV2(typ string, repos, memberCount, support, succ, corr, neut int) float64 {
	breadth := repoBreadthScore(repos, memberCount)
	sup := supportScoreV2(support)
	out := outcomeScoreV2(support, succ, corr, 0)
	_ = neut
	base := 0.40*breadth + 0.35*sup + 0.25*out
	if typ == "risk" {
		base = math.Max(base, 0.5) // recorded cross-repo failure is inherently notable
	}
	return clamp01(base)
}

// getWorkspaceCorpusRecord resolves a bare workspace-aggregate id (pattern: or
// theme:) from the workspace corpus, so the ids `workspace patterns` prints are
// directly fetchable via `workspace get <ws> <id>`. Graceful: false when absent.
func getWorkspaceCorpusRecord(env EntireEnv, workspaceName, id string) (unifiedResult, bool) {
	wsBrainDir, err := workspaceDir(env, workspaceName)
	if err != nil {
		return unifiedResult{}, false
	}
	switch {
	case strings.HasPrefix(id, "pattern:"):
		return getWorkspacePattern(wsBrainDir, id)
	case strings.HasPrefix(id, "theme:"):
		return getCorpusTheme(wsBrainDir, id)
	default:
		return unifiedResult{}, false
	}
}

// getWorkspacePattern renders a workspace-scope aggregate pattern (with its
// per-repo breakdown) as a unifiedResult.
func getWorkspacePattern(wsBrainDir, patternID string) (unifiedResult, bool) {
	db, err := openPatternCorpusDB(wsBrainDir)
	if err != nil {
		return unifiedResult{}, false
	}
	defer db.Close()
	var typ, title, intentSig, gram, metaID string
	var strength float64
	var nRepos int
	err = db.QueryRow(`SELECT type, title, COALESCE(intent_sig,''), COALESCE(gram,''), COALESCE(meta_id,''), strength, n_repos
		FROM patterns WHERE id=? AND scope='workspace'`, patternID).Scan(&typ, &title, &intentSig, &gram, &metaID, &strength, &nRepos)
	if err != nil {
		return unifiedResult{}, false
	}
	var b strings.Builder
	b.WriteString(redactText(title) + "\n")
	b.WriteString("workspace " + typ + " across " + strconv.Itoa(nRepos) + " repo(s); strength " +
		strconv.FormatFloat(strength, 'g', -1, 64) + "\n")
	rows, err := db.Query(`SELECT repo_key, support FROM workspace_pattern_repos WHERE pattern_id=? ORDER BY repo_key`, patternID)
	if err == nil {
		for rows.Next() {
			var rk string
			var support int
			if rows.Scan(&rk, &support) == nil {
				b.WriteString("  " + redactText(rk) + " (" + strconv.Itoa(support) + ")\n")
			}
		}
		rows.Close()
	}
	return unifiedResult{Source: "workspace_pattern", ID: patternID, Text: redactText(strings.TrimRight(b.String(), "\n"))}, true
}

// loadWorkspaceRepoBreakdown returns the per-repo breakdown for workspace
// patterns, keyed by pattern id.
func loadWorkspaceRepoBreakdown(db *sql.DB) map[string][]patternRepoStat {
	out := map[string][]patternRepoStat{}
	rows, err := db.Query(`SELECT pattern_id, repo_key, support, outcome_success, outcome_corrected, outcome_neutral
		FROM workspace_pattern_repos ORDER BY repo_key`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var pid, rk string
		var support, succ, corr, neut int
		if rows.Scan(&pid, &rk, &support, &succ, &corr, &neut) == nil {
			out[pid] = append(out[pid], patternRepoStat{
				RepoKey: rk, Support: support,
				Reinforcement: reinforcementCounts{Success: succ, Corrected: corr, Neutral: neut},
			})
		}
	}
	return out
}
