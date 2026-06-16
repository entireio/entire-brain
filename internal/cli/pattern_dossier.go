package cli

import (
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pattern consolidation records (V2, Phase 4).
//
// A dossier is the reusable internal consolidation of one promotable pattern:
// trigger, workflow, variants, verification, failure modes, relevant facts, and
// exact source anchors. It is assembled DETERMINISTICALLY and token-free from the
// corpus on every refresh (so `refresh`/`watch`/`brief`/`query`/MCP never call an
// agent), then optionally verified by an explicit, egress-gated, cached agent
// pass (pattern_verify.go). The deterministic draft is the cheap part and is
// rebuilt each time; the agent verdict is the expensive part and is cached by
// evidence fingerprint, surviving pattern rebuilds.

const dossierSchemaVersion = 1

// maxDossierAnchors caps how many source episodes a dossier cites.
const maxDossierAnchors = 6

type dossierAnchor struct {
	SessionID    string `json:"session_id"`
	CheckpointID string `json:"checkpoint_id"`
	Transcript   string `json:"transcript"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	Outcome      string `json:"outcome"`
}

type dossierRecord struct {
	SchemaVersion int             `json:"schema_version"`
	PatternID     string          `json:"pattern_id"`
	ClusterKey    string          `json:"cluster_key"`
	Fingerprint   string          `json:"fingerprint"`
	Title         string          `json:"title"`
	Trigger       string          `json:"trigger"`
	Preconditions []string        `json:"preconditions"`
	Workflow      []string        `json:"workflow"`
	Variants      []string        `json:"variants"`
	Verification  []string        `json:"verification"`
	FailureModes  []string        `json:"failure_modes"`
	Facts         []string        `json:"facts"`
	SourceAnchors []dossierAnchor `json:"source_anchors"`
	Confidence    float64         `json:"confidence"`
}

// buildDossiers rebuilds the deterministic consolidation for every promotable
// pattern and reconciles the dossiers cache: fresh consolidation each time;
// cached agent verdicts survive but are marked stale when the evidence
// fingerprint changes; dossiers for vanished patterns are pruned.
func buildDossiers(db *sql.DB, repoKey string, now time.Time) error {
	rows, err := db.Query(`
		SELECT id, type, cluster_key, title, intent_sig, gram, meta_id, strength,
		       outcome_corrected, outcome_failed
		FROM patterns
		WHERE repo_key=? AND strength >= ?
		ORDER BY strength DESC`, repoKey, promotionThreshold)
	if err != nil {
		return err
	}
	defer rows.Close()
	type promotable struct {
		id, typ, cluster, title, intentSig, gram, metaID string
		strength                                         float64
		corrected, failed                                int
	}
	var ps []promotable
	for rows.Next() {
		var p promotable
		if err := rows.Scan(&p.id, &p.typ, &p.cluster, &p.title, &p.intentSig, &p.gram, &p.metaID, &p.strength, &p.corrected, &p.failed); err != nil {
			return err
		}
		ps = append(ps, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	keep := map[string]bool{}
	ts := now.UTC().Format(time.RFC3339)
	for _, p := range ps {
		keep[p.id] = true
		rec, err := assembleDossier(db, p.id, p.typ, p.cluster, p.title, p.intentSig, p.gram, p.metaID, p.strength)
		if err != nil {
			return err
		}
		blob, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if err := upsertDossier(db, rec, redactText(string(blob)), ts); err != nil {
			return err
		}
	}
	return pruneDossiers(db, keep)
}

// assembleDossier builds the consolidation record for one pattern from corpus
// evidence. Deterministic and token-free.
func assembleDossier(db *sql.DB, patternID, typ, cluster, title, intentSig, gram, metaID string, strength float64) (dossierRecord, error) {
	rec := dossierRecord{
		SchemaVersion: dossierSchemaVersion,
		PatternID:     patternID,
		ClusterKey:    cluster,
		Title:         title,
		Confidence:    math.Round(strength*1000) / 1000,
		Preconditions: []string{},
		Workflow:      []string{},
		Variants:      []string{},
		Verification:  []string{},
		FailureModes:  []string{},
		Facts:         []string{},
		SourceAnchors: []dossierAnchor{},
	}
	rec.Trigger = dossierTrigger(typ, intentSig, metaID)
	rec.Workflow = splitGramHeads(gram)
	rec.Variants = dossierVariants(db, typ, intentSig, gram)

	anchors, err := dossierAnchors(db, patternID)
	if err != nil {
		return rec, err
	}
	rec.SourceAnchors = anchors

	rec.Verification = dossierVerification(db, patternID)
	rec.FailureModes = dossierFailureModes(typ, anchors)
	rec.Facts, err = dossierFacts(db, patternID)
	if err != nil {
		return rec, err
	}
	rec.Fingerprint = dossierFingerprint(rec, strength)
	return rec, nil
}

func dossierTrigger(typ, intentSig, metaID string) string {
	switch typ {
	case "practice":
		return "way-of-working: " + metaID
	case "risk":
		return "recurring intent that has gone wrong: " + intentSig
	default:
		return "recurring intent: " + intentSig
	}
}

func splitGramHeads(gram string) []string {
	var out []string
	for _, h := range strings.Split(gram, gramSep) {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// dossierVariants lists other command shapes recorded for the same intent — the
// alternative methods seen for one task/risk.
func dossierVariants(db *sql.DB, typ, intentSig, gram string) []string {
	if typ == "practice" || intentSig == "" {
		return []string{}
	}
	rows, err := db.Query(`SELECT DISTINCT gram FROM patterns WHERE intent_sig=? AND gram!='' AND gram!=? ORDER BY gram LIMIT 5`, intentSig, gram)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if rows.Scan(&g) == nil {
			out = append(out, strings.Join(splitGramHeads(g), " → "))
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func dossierAnchors(db *sql.DB, patternID string) ([]dossierAnchor, error) {
	rows, err := db.Query(`
		SELECT e.session_id, COALESCE(e.checkpoint_id,''), pe.source_path, pe.start_line, pe.end_line, COALESCE(pe.outcome,e.outcome)
		FROM pattern_evidence pe JOIN episodes e ON e.id=pe.episode_id
		WHERE pe.pattern_id=?
		ORDER BY pe.rank LIMIT ?`, patternID, maxDossierAnchors)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dossierAnchor
	for rows.Next() {
		var a dossierAnchor
		if err := rows.Scan(&a.SessionID, &a.CheckpointID, &a.Transcript, &a.StartLine, &a.EndLine, &a.Outcome); err != nil {
			return nil, err
		}
		a.Transcript = redactText(a.Transcript)
		out = append(out, a)
	}
	if out == nil {
		return []dossierAnchor{}, rows.Err()
	}
	return out, rows.Err()
}

// dossierVerification reports the validation commands actually run in the
// pattern's evidence episodes (go test, gofmt, ...). Concrete, repo-real.
func dossierVerification(db *sql.DB, patternID string) []string {
	rows, err := db.Query(`
		SELECT DISTINCT ec.head FROM episode_commands ec
		JOIN pattern_evidence pe ON pe.episode_id=ec.episode_id
		WHERE pe.pattern_id=? ORDER BY ec.head`, patternID)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if rows.Scan(&h) == nil && validationCommandHeads[h] {
			out = append(out, h)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func dossierFailureModes(typ string, anchors []dossierAnchor) []string {
	var out []string
	for _, a := range anchors {
		if a.Outcome == "corrected" || a.Outcome == "failed" {
			out = append(out, "observed "+a.Outcome+" in "+a.SessionID)
		}
	}
	if typ == "risk" && len(out) == 0 {
		out = append(out, "this pattern is derived from corrected/failed work")
	}
	if out == nil {
		return []string{}
	}
	return out
}

// dossierFacts lists the durable facts corroborating the pattern's evidence
// (fact id + kind + paths from episode_facts; text resolution is a surface-layer
// concern). Capped and deduped.
func dossierFacts(db *sql.DB, patternID string) ([]string, error) {
	rows, err := db.Query(`
		SELECT DISTINCT ef.fact_id, COALESCE(ef.kind,''), COALESCE(ef.paths,'')
		FROM episode_facts ef JOIN pattern_evidence pe ON pe.episode_id=ef.episode_id
		WHERE pe.pattern_id=? AND ef.weight >= 2
		ORDER BY ef.fact_id LIMIT 8`, patternID)
	if err != nil {
		return []string{}, nil // facts optional; degrade gracefully
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, kind, paths string
		if err := rows.Scan(&id, &kind, &paths); err != nil {
			return nil, err
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
		return []string{}, rows.Err()
	}
	return out, rows.Err()
}

// dossierFingerprint is the evidence fingerprint: stable while the consolidated
// evidence is unchanged, different the moment anchors, facts, verification, or
// confidence-bucket move. Drives verifier cache invalidation.
func dossierFingerprint(rec dossierRecord, strength float64) string {
	var b strings.Builder
	b.WriteString(rec.PatternID)
	b.WriteByte('\x00')
	b.WriteString(strconv.Itoa(int(math.Round(strength * 100))))
	b.WriteByte('\x00')
	anchors := make([]string, 0, len(rec.SourceAnchors))
	for _, a := range rec.SourceAnchors {
		anchors = append(anchors, a.SessionID+":"+strconv.Itoa(a.StartLine)+":"+a.Outcome)
	}
	sort.Strings(anchors)
	b.WriteString(strings.Join(anchors, "|"))
	b.WriteByte('\x00')
	facts := append([]string(nil), rec.Facts...)
	sort.Strings(facts)
	b.WriteString(strings.Join(facts, "|"))
	b.WriteByte('\x00')
	verif := append([]string(nil), rec.Verification...)
	sort.Strings(verif)
	b.WriteString(strings.Join(verif, "|"))
	return "sha256:" + hexSHA(b.String())
}

func upsertDossier(db *sql.DB, rec dossierRecord, jsonRedacted, ts string) error {
	var (
		existVerdict, existVerified string
		exists                      bool
	)
	err := db.QueryRow(`SELECT COALESCE(verdict,''), COALESCE(verified_fingerprint,'') FROM dossiers WHERE pattern_id=?`, rec.PatternID).
		Scan(&existVerdict, &existVerified)
	switch err {
	case nil:
		exists = true
	case sql.ErrNoRows:
		exists = false
	default:
		return err
	}
	if !exists {
		_, err := db.Exec(`INSERT INTO dossiers
			(pattern_id, cluster_key, fingerprint, json_redacted, verifier_json_redacted, verdict, verified_fingerprint, status, created_at, updated_at)
			VALUES (?,?,?,?,NULL,NULL,NULL,'current',?,?)`,
			rec.PatternID, rec.ClusterKey, rec.Fingerprint, jsonRedacted, ts, ts)
		return err
	}
	// Keep the cached verdict; mark stale if the evidence moved under it.
	status := "current"
	if existVerdict != "" && existVerified != rec.Fingerprint {
		status = "stale"
	}
	_, err = db.Exec(`UPDATE dossiers
		SET cluster_key=?, fingerprint=?, json_redacted=?, status=?, updated_at=?
		WHERE pattern_id=?`,
		rec.ClusterKey, rec.Fingerprint, jsonRedacted, status, ts, rec.PatternID)
	return err
}

func pruneDossiers(db *sql.DB, keep map[string]bool) error {
	rows, err := db.Query(`SELECT pattern_id FROM dossiers`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var drop []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !keep[id] {
			drop = append(drop, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, id := range drop {
		if _, err := db.Exec(`DELETE FROM dossiers WHERE pattern_id=?`, id); err != nil {
			return err
		}
	}
	return nil
}
