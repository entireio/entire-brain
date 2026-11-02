package patterns

import (
	"database/sql"
	"errors"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	"github.com/entireio/entire-brain/internal/cli/workspace"
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

type WorkspaceCorpusCounts struct {
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

// RecoverWorkspaceCorpusPublication clears an interrupted publication on the
// WORKSPACE brain before the corpus-read-safety checks run.
//
// Workspace read surfaces fail closed while the publication marker survives
// ("run a locked pattern refresh to recover"). Recovery is what removes that
// marker, so if it only ran inside the gated build, refresh would be blocked
// by the very state it exists to repair and the workspace corpus would be
// permanently stuck. The repo path gets this ordering for free:
// buildPatternCorpusLocked recovers first, under the brain write lock, with
// no gate ahead of it. Read-only workspace commands deliberately keep failing
// closed; only the repair path is allowed through.
func RecoverWorkspaceCorpusPublication(env base.EntireEnv, manifest workspace.Manifest) error {
	wsBrainDir, err := workspace.DefaultStore(env).Dir(manifest.Name)
	if err != nil {
		return err
	}
	return brainstore.WithBrainWriteLock(wsBrainDir, func() error {
		return RecoverCorpusPublicationLocked(wsBrainDir, DecodeCorpusPublication)
	})
}

// BuildWorkspaceCorpus rebuilds the workspace corpus from member corpora.
// The workspace brain write lock serializes the build against workspace
// verify and skill synthesis; member corpora are read through the pinned
// snapshot opener and need no member locks.
func BuildWorkspaceCorpus(env base.EntireEnv, manifest workspace.Manifest, now time.Time) (WorkspaceCorpusCounts, error) {
	if err := RecoverWorkspaceCorpusPublication(env, manifest); err != nil {
		return WorkspaceCorpusCounts{}, err
	}
	wsBrainDir, err := workspace.DefaultStore(env).Dir(manifest.Name)
	if err != nil {
		return WorkspaceCorpusCounts{}, err
	}
	var counts WorkspaceCorpusCounts
	err = brainstore.WithBrainWriteLock(wsBrainDir, func() error {
		var buildErr error
		counts, buildErr = BuildWorkspaceCorpusLocked(env, manifest, now)
		return buildErr
	})
	return counts, err
}

func BuildWorkspaceCorpusLocked(env base.EntireEnv, manifest workspace.Manifest, now time.Time) (WorkspaceCorpusCounts, error) {
	var counts WorkspaceCorpusCounts
	wsBrainDir, err := workspace.DefaultStore(env).Dir(manifest.Name)
	if err != nil {
		return counts, err
	}
	if err := RecoverCorpusPublicationLocked(wsBrainDir, DecodeCorpusPublication); err != nil {
		return counts, err
	}
	path, cleanup, err := PrepareCorpusStaging(wsBrainDir)
	if err != nil {
		return counts, err
	}
	defer cleanup()
	db, err := sql.Open(sqlitedriver.Name, path)
	if err != nil {
		return counts, err
	}
	defer db.Close()
	for _, p := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(p); err != nil {
			return counts, err
		}
	}
	for _, stmt := range CorpusSchema {
		if _, err := db.Exec(stmt); err != nil {
			return counts, err
		}
	}

	repos := append([]workspace.Repo(nil), manifest.Repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].RepoKey < repos[j].RepoKey })
	counts.Members = len(repos)

	aggs := map[wsPatternKey]*wsAgg{}
	for _, repo := range repos {
		memberBrainDir, err := base.BrainDirForKey(env, repo.RepoKey)
		if err != nil {
			counts.Warnings = append(counts.Warnings, repo.RepoKey+": "+err.Error())
			continue
		}
		mdb, err := OpenCorpusReadDB(memberBrainDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// A genuinely absent member corpus degrades gracefully (refresh that repo).
				counts.Warnings = append(counts.Warnings, repo.RepoKey+": no corpus (run `"+base.BrainInvocation(os.LookupEnv, "refresh")+"` there)")
				continue
			}
			return counts, err
		}
		counts.WithCorpus++
		if err := collectMemberPatterns(mdb.DB, repo.RepoKey, aggs); err != nil {
			counts.Warnings = append(counts.Warnings, repo.RepoKey+": "+err.Error())
		}
		mdb.Close()
	}

	membershipFP := WorkspaceMembershipFingerprint(manifest)
	membershipChanged := CorpusMeta(db, "workspace_families_membership") != membershipFP
	tx, err := db.Begin()
	if err != nil {
		return counts, err
	}
	if membershipChanged {
		if _, err := tx.Exec(`UPDATE deep_dossiers SET status='stale' WHERE pattern_id LIKE 'family:%'`); err != nil {
			tx.Rollback()
			return counts, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('workspace_families_membership',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, membershipFP); err != nil {
		tx.Rollback()
		return counts, err
	}
	// Rebuildable: clear all prior workspace corpus content, including the
	// verifier-cache tables — they have no FK cascade, so stale verdicts must not
	// survive a rebuild and join onto a newly-rebuilt pattern row sharing the same
	// stable id (which could even suppress it as 'rejected').
	// themes has no FK to episodes (not cascaded) and is never populated at
	// workspace scope, so clear it too — a stray theme row must not persist
	// across rebuilds, be fetchable via `workspace get theme:…`, or skew run counts.
	// EXCEPTION: agent-verified workspace families (`family:%` deep_dossiers) are
	// NOT pattern rows (loadAcceptedWorkspaceFamilies reads them directly, never via
	// a pattern-id join), so the stale-join hazard above does not apply. They are an
	// expensive agent result keyed by their own member-evidence fingerprint, so a
	// deterministic rebuild preserves them while membership remains unchanged.
	// Membership changes mark them stale; explicit verification re-proposes them
	// when the member-evidence fingerprint changes.
	for _, stmt := range []string{`DELETE FROM patterns`, `DELETE FROM episodes`, `DELETE FROM workspace_pattern_repos`, `DELETE FROM dossiers`, `DELETE FROM deep_dossiers WHERE pattern_id NOT LIKE 'family:%'`, `DELETE FROM themes`} {
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return counts, err
		}
	}
	ts := now.UTC().Format(time.RFC3339)
	memberCount := counts.Members
	for _, a := range aggs {
		if len(a.repos) < WorkspaceMinRepos {
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
	if err := BuildSynapses(db, now); err != nil {
		return counts, err
	}
	run, err := RunFromStaging(db, now)
	if err != nil {
		return counts, err
	}
	if err := FinalizeCorpusStagingLocked(wsBrainDir, path, db, DecodeCorpusPublication); err != nil {
		return counts, err
	}
	_ = AppendRun(wsBrainDir, run)
	return counts, nil
}

// collectMemberPatterns reads one member corpus's patterns + top anchor and folds
// them into the cross-repo aggregates.
func collectMemberPatterns(mdb *sql.DB, repoKey string, aggs map[wsPatternKey]*wsAgg) error {
	anchors := memberTopAnchors(mdb)
	// Skip member patterns a verifier (shallow or deep) explicitly rejected — a
	// rejected pattern must not re-surface by contributing to a cross-repo
	// aggregate. (needs_split/low_confidence remain eligible; they are softer.)
	rows, err := mdb.Query(`
		SELECT p.id, p.type, COALESCE(p.intent_sig,''), COALESCE(p.gram,''), COALESCE(p.meta_id,''),
		       p.title, p.support, p.outcome_success, p.outcome_corrected, p.outcome_neutral
		FROM patterns p
		LEFT JOIN dossiers d ON d.pattern_id = p.id
		LEFT JOIN deep_dossiers dd ON dd.pattern_id = p.id
		WHERE p.scope='repo'
		  AND COALESCE(d.verdict,'') != 'rejected'
		  AND COALESCE(dd.verdict,'') != 'rejected'`)
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
	id := "pattern:" + base.HexSHA("workspace\x00"+workspace+"\x00"+cluster)
	strength := workspaceStrengthV2(a.key.typ, len(a.repos), memberCount, a.support, a.succ, a.corr, a.neut)
	strength = math.Round(strength*1000) / 1000

	if _, err := tx.Exec(`INSERT INTO patterns
		(id, type, scope, repo_key, workspace, cluster_key, title, intent_sig, gram, meta_id, strength, strength_label, support,
		 n_repos, n_authors, n_branches, outcome_success, outcome_corrected, outcome_neutral, outcome_failed, fingerprint, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, a.key.typ, "workspace", "", workspace, cluster, base.RedactText(a.title), a.key.intentSig, a.key.gram, a.key.metaID,
		strength, StrengthLabelV2(strength), a.support, len(a.repos), 0, 0, a.succ, a.corr, a.neut, 0,
		base.HexSHA(cluster+strconv.Itoa(a.support)), "active", ts, ts); err != nil {
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
		wsEpID := "wsep:" + base.HexSHA(rk+"\x00"+st.anchorEpisodeID)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO episodes
			(id, episode_key, repo_key, workspace, session_id, turn_ord, branch, source_path, start_line, end_line, outcome, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			wsEpID, wsEpID, rk, workspace, base.RedactText(st.anchorSession), 0, "", base.RedactText(st.anchorPath), st.anchorLine, st.anchorLine,
			base.NonEmptyOr(st.anchorOutcome, "neutral"), ts); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO pattern_evidence
			(pattern_id, episode_id, rank, outcome, source_path, start_line, end_line) VALUES (?,?,?,?,?,?,?)`,
			id, wsEpID, rank, base.NonEmptyOr(st.anchorOutcome, "neutral"), base.RedactText(st.anchorPath), st.anchorLine, st.anchorLine); err != nil {
			return err
		}
		rank++
	}
	return nil
}

// workspaceStrengthV2 rewards repo breadth on top of support and outcome — a
// pattern shared by more members of the workspace is stronger.
func workspaceStrengthV2(typ string, repos, memberCount, support, succ, corr, neut int) float64 {
	breadth := RepoBreadthScore(repos, memberCount)
	sup := SupportScoreV2(support)
	out := OutcomeScoreV2(support, succ, corr, 0)
	_ = neut
	score := 0.40*breadth + 0.35*sup + 0.25*out
	if typ == "risk" {
		score = math.Max(score, 0.5) // recorded cross-repo failure is inherently notable
	}
	return base.Clamp01(score)
}

// LoadWorkspaceRepoBreakdown returns the per-repo breakdown for workspace
// patterns, keyed by pattern id.
func LoadWorkspaceRepoBreakdown(db *sql.DB) map[string][]PatternRepoStat {
	breakdown, _ := LoadWorkspaceRepoBreakdownChecked(db)
	return breakdown
}

func LoadWorkspaceRepoBreakdownChecked(db *sql.DB) (map[string][]PatternRepoStat, error) {
	out := map[string][]PatternRepoStat{}
	rows, err := db.Query(`SELECT pattern_id, repo_key, support, outcome_success, outcome_corrected, outcome_neutral
		FROM workspace_pattern_repos ORDER BY repo_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid, rk string
		var support, succ, corr, neut int
		if err := rows.Scan(&pid, &rk, &support, &succ, &corr, &neut); err != nil {
			return nil, err
		}
		out[pid] = append(out[pid], PatternRepoStat{
			RepoKey: rk, Support: support,
			Reinforcement: ReinforcementCounts{Success: succ, Corrected: corr, Neutral: neut},
		})
	}
	return out, rows.Err()
}

// RepoBreadthScore is the fraction of OTHER members that also share the pattern:
// shared by all members -> 1.0, shared by 2 of many -> low.
func RepoBreadthScore(repos, memberCount int) float64 {
	denom := memberCount - 1
	if denom < 1 {
		denom = 1
	}
	return base.Clamp01(float64(repos-1) / float64(denom))
}
