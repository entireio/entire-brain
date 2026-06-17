package cli

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Pattern corpus surface integration (V2, Phase 5).
//
// Read-only projections of the corpus dossiers into existing brain surfaces.
// Everything here is best-effort and graceful: a missing/!built corpus yields no
// rows (never an error that breaks brief/get), output is already redacted at
// rest, and a verifier `rejected` verdict suppresses a consolidation entirely —
// the agent verifier is the authority that can remove, the deterministic gate the
// authority that promotes.

// loadCorpusPatternViews lists the V2 corpus `patterns` rows as unified
// patternViews — the primary analytical source for the public `patterns`
// surface (replacing the legacy procedure/practice JSON views). Each view
// carries its dossier state (current/stale + verifier verdict) and top anchor.
// Rejected dossiers are suppressed. Graceful: nil when no corpus exists.
func loadCorpusPatternViews(brainDir string) ([]patternView, map[string]patternView, bool) {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return nil, nil, false
	}
	defer db.Close()

	anchors := topAnchorByPattern(db)
	rows, err := db.Query(`
		SELECT p.id, p.type, p.scope, COALESCE(p.repo_key,''), COALESCE(p.workspace,''),
		       COALESCE(p.intent_sig,''), COALESCE(p.gram,''), COALESCE(p.meta_id,''),
		       p.title, p.strength, p.strength_label, p.support, p.n_repos,
		       p.outcome_success, p.outcome_corrected, p.outcome_neutral,
		       COALESCE(d.status,''), COALESCE(d.verdict,'')
		FROM patterns p
		LEFT JOIN dossiers d ON d.pattern_id = p.id
		ORDER BY p.strength DESC`)
	if err != nil {
		return nil, nil, false
	}
	defer rows.Close()
	var views []patternView
	for rows.Next() {
		var (
			v                          patternView
			repoKey, workspace, metaID string
			succ, corr, neutral        int
			dossierStatus, verdict     string
		)
		if err := rows.Scan(&v.ID, &v.Type, &v.Scope, &repoKey, &workspace,
			&v.IntentSig, &v.Gram, &metaID, &v.Title, &v.Strength, &v.StrengthLabel, &v.Support, &v.Repos,
			&succ, &corr, &neutral, &dossierStatus, &verdict); err != nil {
			return nil, nil, false
		}
		if verdict == "rejected" { // the agent verifier is the authority that removes
			continue
		}
		v.Kind = metaID
		v.Workspace = workspace
		v.Title = redactText(v.Title)
		v.DossierStatus = dossierStatus
		v.Verdict = verdict
		rc := reinforcementCounts{Success: succ, Corrected: corr, Neutral: neutral}
		v.Reinforcement = &rc
		if a, ok := anchors[v.ID]; ok {
			ca := a
			v.Example = &ca
		}
		views = append(views, v)
	}
	if rows.Err() != nil {
		return nil, nil, false
	}
	idx := make(map[string]patternView, len(views))
	for _, v := range views {
		idx[v.ID] = v
	}
	return views, idx, true
}

// topAnchorByPattern returns each pattern's rank-0 evidence anchor (redacted).
func topAnchorByPattern(db *sql.DB) map[string]episodeAnchor {
	out := map[string]episodeAnchor{}
	rows, err := db.Query(`SELECT pattern_id, source_path, start_line FROM pattern_evidence WHERE rank=0`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, path string
		var line int
		if rows.Scan(&id, &path, &line) == nil {
			out[id] = episodeAnchor{Path: redactText(path), Line: line}
		}
	}
	return out
}

// briefConsolidation is the compact, task-relevant projection of a dossier the
// brief carries: enough to act (trigger, workflow, verification, failure modes)
// plus one anchor and the trust signals (confidence, verdict, staleness).
type briefConsolidation struct {
	PatternID    string         `json:"pattern_id"`
	Type         string         `json:"type"`
	Title        string         `json:"title"`
	Trigger      string         `json:"trigger"`
	Confidence   float64        `json:"confidence"`
	Status       string         `json:"status"`
	Verdict      string         `json:"verdict,omitempty"`
	Workflow     []string       `json:"workflow,omitempty"`
	Verification []string       `json:"verification,omitempty"`
	FailureModes []string       `json:"failure_modes,omitempty"`
	Anchor       *dossierAnchor `json:"anchor,omitempty"`
}

type loadedDossier struct {
	rec     dossierRecord
	typ     string
	status  string
	verdict string
}

// loadCorpusDossiers reads every dossier joined to its pattern type. Graceful:
// returns nil when the corpus is absent or unreadable.
func loadCorpusDossiers(brainDir string) []loadedDossier {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT d.json_redacted, p.type, d.status, COALESCE(d.verdict,'')
		FROM dossiers d JOIN patterns p ON p.id=d.pattern_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []loadedDossier
	for rows.Next() {
		var blob, typ, status, verdict string
		if err := rows.Scan(&blob, &typ, &status, &verdict); err != nil {
			return out
		}
		var rec dossierRecord
		if json.Unmarshal([]byte(blob), &rec) != nil {
			continue
		}
		out = append(out, loadedDossier{rec: rec, typ: typ, status: status, verdict: verdict})
	}
	return out
}

// rankTaskRelevantConsolidations returns the dossiers whose title/trigger/
// workflow share a term with the task, ranked by overlap then confidence, capped.
// Rejected consolidations are suppressed; unrelated tasks yield none (no ambient
// noise — the same discipline as rankTaskRelevantPatterns).
func rankTaskRelevantConsolidations(dossiers []loadedDossier, terms []string, limit int) []briefConsolidation {
	if limit <= 0 || len(terms) == 0 {
		return nil
	}
	termSet := make(map[string]bool, len(terms))
	for _, t := range terms {
		termSet[t] = true
	}
	type scored struct {
		c       briefConsolidation
		overlap int
	}
	var matched []scored
	for _, d := range dossiers {
		if d.verdict == "rejected" {
			continue
		}
		hay := strings.ToLower(d.rec.Title + " " + d.rec.Trigger + " " + strings.Join(d.rec.Workflow, " "))
		seen := map[string]bool{}
		overlap := 0
		for _, w := range brainBriefTaskWordPattern.FindAllString(hay, -1) {
			if termSet[w] && !seen[w] {
				seen[w] = true
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}
		matched = append(matched, scored{consolidationView(d), overlap})
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].overlap != matched[j].overlap {
			return matched[i].overlap > matched[j].overlap
		}
		return matched[i].c.Confidence > matched[j].c.Confidence
	})
	out := make([]briefConsolidation, 0, limit)
	for _, m := range matched {
		if len(out) >= limit {
			break
		}
		out = append(out, m.c)
	}
	return out
}

func consolidationView(d loadedDossier) briefConsolidation {
	c := briefConsolidation{
		PatternID:    d.rec.PatternID,
		Type:         d.typ,
		Title:        d.rec.Title,
		Trigger:      d.rec.Trigger,
		Confidence:   d.rec.Confidence,
		Status:       d.status,
		Verdict:      d.verdict,
		Workflow:     d.rec.Workflow,
		Verification: d.rec.Verification,
		FailureModes: d.rec.FailureModes,
	}
	if len(d.rec.SourceAnchors) > 0 {
		a := d.rec.SourceAnchors[0]
		c.Anchor = &a
	}
	return c
}

// loadBriefConsolidations is the brief's entry point: task-relevant, capped,
// corpus-backed consolidations. Empty (not error) when no corpus exists.
func loadBriefConsolidations(brainDir string, terms []string, limit int) []briefConsolidation {
	return rankTaskRelevantConsolidations(loadCorpusDossiers(brainDir), terms, limit)
}

// strongestConsolidations is the overview's entry point: the repo's top current
// dossiers by confidence, capped so the overview shows strength without flooding.
// Rejected and stale dossiers are excluded — the overview shows what currently
// holds. Empty (not error) when no corpus exists.
func strongestConsolidations(brainDir string, limit int) []briefConsolidation {
	if limit <= 0 {
		return nil
	}
	dossiers := loadCorpusDossiers(brainDir)
	views := make([]briefConsolidation, 0, len(dossiers))
	for _, d := range dossiers {
		if d.verdict == "rejected" || d.status != "current" {
			continue
		}
		views = append(views, consolidationView(d))
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Confidence > views[j].Confidence })
	if len(views) > limit {
		views = views[:limit]
	}
	return views
}

// getCorpusConsolidation addresses one dossier by its pattern id for `get`.
// Returns the consolidation as a unifiedResult, or false if absent.
func getCorpusConsolidation(brainDir, patternID string) (unifiedResult, bool) {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return unifiedResult{}, false
	}
	defer db.Close()
	var blob, status, verdict string
	err = db.QueryRow(`SELECT json_redacted, status, COALESCE(verdict,'') FROM dossiers WHERE pattern_id=?`, patternID).
		Scan(&blob, &status, &verdict)
	if err != nil {
		return unifiedResult{}, false
	}
	var rec dossierRecord
	if json.Unmarshal([]byte(blob), &rec) != nil {
		return unifiedResult{}, false
	}
	return unifiedResult{
		Source: "consolidation",
		ID:     patternID,
		Text:   renderConsolidationText(rec, status, verdict),
	}, true
}

func renderConsolidationText(rec dossierRecord, status, verdict string) string {
	var b strings.Builder
	b.WriteString(rec.Title)
	b.WriteString("\n")
	if rec.Trigger != "" {
		b.WriteString("trigger: " + rec.Trigger + "\n")
	}
	if len(rec.Workflow) > 0 {
		b.WriteString("workflow: " + strings.Join(rec.Workflow, " → ") + "\n")
	}
	if len(rec.Verification) > 0 {
		b.WriteString("verify: " + strings.Join(rec.Verification, ", ") + "\n")
	}
	if len(rec.FailureModes) > 0 {
		b.WriteString("failure modes: " + strings.Join(rec.FailureModes, "; ") + "\n")
	}
	if len(rec.Facts) > 0 {
		b.WriteString("facts: " + strings.Join(rec.Facts, "; ") + "\n")
	}
	state := status
	if verdict != "" {
		state += ", verdict=" + verdict
	}
	b.WriteString("confidence ")
	b.WriteString(strconv.FormatFloat(rec.Confidence, 'g', -1, 64))
	b.WriteString(" (" + state + ")")
	// Already redacted at rest; defensive redaction on egress.
	return redactText(b.String())
}
