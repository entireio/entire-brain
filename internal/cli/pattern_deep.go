package cli

import (
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// Deep dossiers (Pattern Consolidation v2, Priority 4).
//
// A deep dossier is a BOUNDED but evidence-deep export of a pattern's backing
// episodes — every backing episode resolved by stable episode_key (capped at
// deepMaxEpisodes anchors), the top deepExcerptEpisodes carrying capped, redacted
// transcript span excerpts, not the 3 sampled anchors a shallow dossier stores —
// stratified corrected/failed first so the verifier reasons over failures and
// recoveries. It is assembled deterministically and token-free; the optional deep
// verifier (pattern_verify.go) is the only agent step and runs solely through the
// explicit `patterns verify --deep`. The fingerprint covers the full backing-set
// identity (every episode_key + outcome), so it changes (and the cached verdict
// goes stale) whenever any supporting episode moves.

const deepSchemaVersion = 1
const deepMaxEpisodes = 40    // cap the full set handed onward
const deepMaxParameters = 8   // representative concrete invocations
const deepFailureModeCap = 12 // failure/recovery pairs
// Bounded evidence export: the first deepExcerptEpisodes anchors (corrected/
// failed first) carry a redacted transcript excerpt, each capped at
// deepExcerptLines / deepExcerptBytes, so the verifier sees real span text
// without an unbounded payload. This is a BOUNDED export, not the literal full text.
const deepExcerptEpisodes = 12
const deepExcerptLines = 60
const deepExcerptBytes = 1800

type deepFailureMode struct {
	Episode  string `json:"episode"`
	Failure  string `json:"failure"`
	Recovery string `json:"recovery,omitempty"`
}

type deepDossierRecord struct {
	SchemaVersion int    `json:"schema_version"`
	PatternID     string `json:"pattern_id"`
	ClusterKey    string `json:"cluster_key"`
	Fingerprint   string `json:"fingerprint"`
	// Archetype selects how the dossier renders into a SKILL.md: "procedure"
	// (a failure→recovery lesson from corrected episodes), "capability" (a
	// non-obvious convention/gotcha from durable facts), or "" for the legacy
	// command-sequence task dossier (now demoted to supporting evidence).
	Archetype        string            `json:"archetype,omitempty"`
	Title            string            `json:"title"`
	Trigger          string            `json:"trigger"`
	NotWhen          string            `json:"not_when,omitempty"`
	Knowledge        []string          `json:"knowledge,omitempty"`
	Preconditions    []string          `json:"preconditions"`
	Workflow         []string          `json:"workflow"`
	Variations       []string          `json:"variations"`
	Verification     []string          `json:"verification"`
	Parameters       []string          `json:"parameters"`
	FailureModes     []deepFailureMode `json:"failure_modes"`
	Facts            []string          `json:"facts"`
	SourceAnchors    []dossierAnchor   `json:"source_anchors"`
	Confidence       float64           `json:"confidence"`
	EvidenceEpisodes int               `json:"evidence_episodes"` // total in the full set
}

type deepEpisode struct {
	id, episodeKey, session, sourcePath, outcome string
	startLine, endLine                           int
}

// buildDeepDossier assembles the deep dossier for one pattern: the bounded but
// evidence-deep export over its complete backing-episode set, including redacted
// transcript excerpts for the top corrected/failed-first episodes.
func buildDeepDossier(db *sql.DB, brainDir, patternID string) (deepDossierRecord, error) {
	var (
		typ, cluster, title, intentSig, gram, metaID string
		strength                                     float64
	)
	err := db.QueryRow(`SELECT type, cluster_key, title, COALESCE(intent_sig,''), COALESCE(gram,''), COALESCE(meta_id,''), strength
		FROM patterns WHERE id=?`, patternID).Scan(&typ, &cluster, &title, &intentSig, &gram, &metaID, &strength)
	if err != nil {
		return deepDossierRecord{}, err
	}
	rec := deepDossierRecord{
		SchemaVersion: deepSchemaVersion,
		PatternID:     patternID,
		ClusterKey:    cluster,
		Title:         title,
		Trigger:       dossierTrigger(typ, intentSig, metaID),
		Confidence:    math.Round(strength*1000) / 1000,
		Preconditions: []string{},
		Workflow:      splitGramHeads(gram),
		Variations:    dossierVariants(db, typ, intentSig, gram),
		Verification:  []string{},
		Parameters:    []string{},
		FailureModes:  []deepFailureMode{},
		Facts:         []string{},
		SourceAnchors: []dossierAnchor{},
	}

	episodes, err := deepEvidenceEpisodes(db, typ, intentSig, gram, metaID)
	if err != nil {
		return rec, err
	}
	rec.EvidenceEpisodes = len(episodes)

	// Anchors: the full set, capped (already stratified corrected/failed first).
	// The first deepExcerptEpisodes carry a redacted transcript excerpt so the
	// verifier reasons over real span text, not just derived metadata.
	for i, e := range episodes {
		if i >= deepMaxEpisodes {
			break
		}
		a := dossierAnchor{
			SessionID: e.session, Transcript: redactText(e.sourcePath),
			StartLine: e.startLine, EndLine: e.endLine, Outcome: e.outcome,
		}
		if i < deepExcerptEpisodes {
			lines := e.endLine - e.startLine + 1
			if lines <= 0 || lines > deepExcerptLines {
				lines = deepExcerptLines
			}
			if ex := transcriptExcerpt(brainDir, episodeAnchor{Path: e.sourcePath, Line: e.startLine}, lines, deepExcerptBytes); ex != "" {
				a.Excerpt = redactText(ex)
			}
		}
		rec.SourceAnchors = append(rec.SourceAnchors, a)
	}

	epIDs := make([]string, 0, len(episodes))
	for _, e := range episodes {
		epIDs = append(epIDs, e.id)
	}
	rec.Verification = deepVerification(db, epIDs)
	rec.Parameters = deepParameters(db, epIDs, rec.Workflow)
	rec.FailureModes = deepFailureModes(db, episodes)
	rec.Facts = deepFacts(db, epIDs)
	rec.Fingerprint = deepFingerprint(rec, episodes, strength)
	return rec, nil
}

// deepEvidenceEpisodes resolves the FULL backing-episode set for a pattern by
// stable episode_key, corrected/failed first, then success, then recent.
func deepEvidenceEpisodes(db *sql.DB, typ, intentSig, gram, metaID string) ([]deepEpisode, error) {
	var q string
	var args []any
	switch typ {
	case "task", "risk":
		q = `SELECT DISTINCT e.id, e.episode_key, e.session_id, e.source_path, e.start_line, e.end_line, e.outcome
		     FROM episodes e JOIN grams g ON g.episode_id=e.id WHERE e.intent_sig=? AND g.gram=?`
		args = []any{intentSig, gram}
	case "procedure":
		q = `SELECT DISTINCT e.id, e.episode_key, e.session_id, e.source_path, e.start_line, e.end_line, e.outcome
		     FROM episodes e JOIN grams g ON g.episode_id=e.id WHERE g.gram=?`
		args = []any{gram}
	case "practice":
		q = `SELECT DISTINCT e.id, e.episode_key, e.session_id, e.source_path, e.start_line, e.end_line, e.outcome
		     FROM episodes e JOIN meta_hits mh ON mh.episode_id=e.id WHERE mh.meta_id=?`
		args = []any{metaID}
	default:
		return nil, nil
	}
	q += ` ORDER BY CASE e.outcome WHEN 'corrected' THEN 0 WHEN 'failed' THEN 1 WHEN 'success' THEN 2 ELSE 3 END, e.created_at DESC`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deepEpisode
	for rows.Next() {
		var e deepEpisode
		if err := rows.Scan(&e.id, &e.episodeKey, &e.session, &e.sourcePath, &e.startLine, &e.endLine, &e.outcome); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func deepVerification(db *sql.DB, epIDs []string) []string {
	seen := map[string]bool{}
	var out []string
	forEachCommand(db, epIDs, func(head, raw string, failed bool) {
		if validationCommandHeads[head] && !seen[head] {
			seen[head] = true
			out = append(out, head)
		}
	})
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}

// deepParameters returns representative concrete invocations of the workflow
// commands (distinct redacted command lines), so the verifier sees real flags.
func deepParameters(db *sql.DB, epIDs []string, workflow []string) []string {
	wf := map[string]bool{}
	for _, h := range workflow {
		wf[h] = true
	}
	seen := map[string]bool{}
	var out []string
	forEachCommand(db, epIDs, func(head, raw string, failed bool) {
		if len(out) >= deepMaxParameters || !wf[head] || raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, raw)
	})
	if out == nil {
		return []string{}
	}
	return out
}

// deepFailureModes pairs each corrected/failed episode's failing command (or
// corrected outcome) with the validation it used to recover, where present.
func deepFailureModes(db *sql.DB, episodes []deepEpisode) []deepFailureMode {
	var out []deepFailureMode
	for _, e := range episodes {
		if e.outcome != "corrected" && e.outcome != "failed" {
			continue
		}
		if len(out) >= deepFailureModeCap {
			break
		}
		var failure, recovery string
		rows, err := db.Query(`SELECT head, failed, exit_code FROM episode_commands WHERE episode_id=? ORDER BY ord`, e.id)
		if err == nil {
			for rows.Next() {
				var head string
				var failed bool
				var exit sql.NullInt64
				if rows.Scan(&head, &failed, &exit) != nil {
					continue
				}
				if failed {
					if failure == "" {
						failure = "failed: " + head
					}
					recovery = ""
					continue
				}
				if failure != "" && validationCommandHeads[head] && exit.Valid && exit.Int64 == 0 && recovery == "" {
					recovery = "validated with " + head + " after failure"
				}
			}
			rows.Close()
		}
		if failure == "" {
			failure = "outcome " + e.outcome
		}
		out = append(out, deepFailureMode{Episode: e.session, Failure: failure, Recovery: recovery})
	}
	return out
}

func deepFacts(db *sql.DB, epIDs []string) []string {
	if len(epIDs) == 0 {
		return []string{}
	}
	placeholders, args := inPlaceholders(epIDs)
	rows, err := db.Query(`SELECT DISTINCT ef.fact_id, COALESCE(ef.kind,''), COALESCE(ef.paths,'')
		FROM episode_facts ef WHERE ef.weight >= 2 AND ef.episode_id IN (`+placeholders+`)
		ORDER BY ef.fact_id LIMIT 16`, args...)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, kind, paths string
		if rows.Scan(&id, &kind, &paths) != nil {
			continue
		}
		entry := id
		if kind != "" {
			entry += " (" + kind + ")"
		}
		if paths != "" {
			entry += " @ " + redactText(paths)
		}
		out = append(out, entry)
	}
	if out == nil {
		return []string{}
	}
	return out
}

// forEachCommand runs fn over the (head, redacted-raw, failed) of every command
// in the given episodes, chunking the IN clause.
func forEachCommand(db *sql.DB, epIDs []string, fn func(head, raw string, failed bool)) {
	for start := 0; start < len(epIDs); start += 400 {
		end := start + 400
		if end > len(epIDs) {
			end = len(epIDs)
		}
		placeholders, args := inPlaceholders(epIDs[start:end])
		rows, err := db.Query(`SELECT head, COALESCE(raw_redacted,''), failed FROM episode_commands WHERE episode_id IN (`+placeholders+`) ORDER BY episode_id, ord`, args...)
		if err != nil {
			return
		}
		for rows.Next() {
			var head, raw string
			var failed int
			if rows.Scan(&head, &raw, &failed) == nil {
				fn(head, raw, failed != 0)
			}
		}
		rows.Close()
	}
}

func inPlaceholders(ids []string) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return strings.Join(ph, ","), args
}

// deepFingerprint binds the complete redacted verifier payload and backing set.
// The fingerprint field itself is excluded to avoid self-reference.
func deepFingerprint(rec deepDossierRecord, episodes []deepEpisode, strength float64) string {
	rec.Fingerprint = ""
	keys := make([]string, 0, len(episodes))
	for _, e := range episodes {
		keys = append(keys, e.episodeKey+":"+e.outcome)
	}
	sort.Strings(keys)
	payload, err := json.Marshal(struct {
		Dossier  deepDossierRecord
		Episodes []string
		Strength float64
	}{rec, keys, strength})
	if err != nil {
		return ""
	}
	return "sha256:" + hexSHA("deep/v2\x00"+redactText(string(payload)))
}
