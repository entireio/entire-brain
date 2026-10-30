package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
)

// Pattern corpus surface integration (V2, Phase 5).
//
// Read-only projections of the corpus dossiers into existing brain surfaces.
// Everything here is best-effort and graceful: a missing/!built corpus yields no
// rows (never an error that breaks brief/get), output is already redacted at
// rest, and a verifier `rejected` verdict suppresses a consolidation entirely —
// the agent verifier is the authority that can remove, the deterministic gate the
// authority that promotes.

// briefConsolidation is the compact, task-relevant projection of a dossier the
// brief carries: enough to act (trigger, workflow, verification, failure modes)
// plus one anchor and the trust signals (confidence, verdict, staleness).
type briefConsolidation struct {
	PatternID    string                     `json:"pattern_id"`
	Type         string                     `json:"type"`
	Title        string                     `json:"title"`
	Trigger      string                     `json:"trigger"`
	Confidence   float64                    `json:"confidence"`
	Status       string                     `json:"status"`
	Verdict      string                     `json:"verdict,omitempty"`
	Workflow     []string                   `json:"workflow,omitempty"`
	Verification []string                   `json:"verification,omitempty"`
	FailureModes []string                   `json:"failure_modes,omitempty"`
	Anchor       *patterncore.DossierAnchor `json:"anchor,omitempty"`
}

type loadedDossier struct {
	rec     patterncore.DossierRecord
	typ     string
	status  string
	verdict string
}

// loadCorpusDossiers reads every dossier joined to its pattern type. Graceful:
// returns nil when the corpus is absent or unreadable.
func loadCorpusDossiersChecked(brainDir string) ([]loadedDossier, error) {
	db, present, err := patterncore.OpenCorpusReadDBIfPresent(brainDir)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT d.json_redacted, p.type, d.status, COALESCE(d.verdict,'')
		FROM dossiers d JOIN patterns p ON p.id=d.pattern_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []loadedDossier
	for rows.Next() {
		var blob, typ, status, verdict string
		if err := rows.Scan(&blob, &typ, &status, &verdict); err != nil {
			return out, err
		}
		var rec patterncore.DossierRecord
		if err := json.Unmarshal([]byte(blob), &rec); err != nil {
			return nil, fmt.Errorf("%s: parse corpus dossier for pattern surface: %w", brainstore.StateCorruptCode, err)
		}
		out = append(out, loadedDossier{rec: rec, typ: typ, status: status, verdict: verdict})
	}
	return out, rows.Err()
}

func loadCorpusDossiers(brainDir string) []loadedDossier {
	dossiers, _ := loadCorpusDossiersChecked(brainDir)
	return dossiers
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
	consolidations, _ := loadBriefConsolidationsChecked(brainDir, terms, limit)
	return consolidations
}

func loadBriefConsolidationsChecked(brainDir string, terms []string, limit int) ([]briefConsolidation, error) {
	dossiers, err := loadCorpusDossiersChecked(brainDir)
	if err != nil {
		return nil, err
	}
	return rankTaskRelevantConsolidations(dossiers, terms, limit), nil
}

// strongestConsolidations is the overview's entry point: the repo's top current
// dossiers by confidence, capped so the overview shows strength without flooding.
// Rejected and stale dossiers are excluded — the overview shows what currently
// holds. Empty (not error) when no corpus exists.
func strongestConsolidations(brainDir string, limit int) []briefConsolidation {
	consolidations, _ := strongestConsolidationsChecked(brainDir, limit)
	return consolidations
}

func strongestConsolidationsChecked(brainDir string, limit int) ([]briefConsolidation, error) {
	if limit <= 0 {
		return nil, nil
	}
	dossiers, err := loadCorpusDossiersChecked(brainDir)
	if err != nil {
		return nil, err
	}
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
	return views, nil
}

const patternPointerCap = 5

// relatedPatternPointers returns capped pattern:/theme: pointers relevant to a
// query by term overlap — discoverability only, never part of facts/history/docs
// ranking. Rejected dossiers/themes are excluded; graceful (nil) when no corpus.
func relatedPatternPointers(brainDir, query string, limit int) []relatedPatternRef {
	pointers, _ := relatedPatternPointersChecked(brainDir, query, limit)
	return pointers
}

func relatedPatternPointersChecked(brainDir, query string, limit int) ([]relatedPatternRef, error) {
	terms := brainBriefFileMatchTerms(query)
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	views, _, ok, err := patterncore.LoadCorpusPatternViewsChecked(brainDir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	themes, err := patterncore.LoadThemeViewsChecked(brainDir, true)
	if err != nil {
		return nil, err
	}
	for _, th := range themes { // verified themes only
		views = append(views, patterncore.ThemePatternView(th))
	}
	termSet := map[string]bool{}
	for _, t := range terms {
		termSet[t] = true
	}
	type scored struct {
		ref     relatedPatternRef
		overlap int
		str     float64
	}
	var matched []scored
	for _, v := range views {
		if v.Verdict == "rejected" {
			continue
		}
		hay := strings.ToLower(v.Title + " " + v.IntentSig + " " + v.Gram + " " + v.Kind)
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
		matched = append(matched, scored{relatedPatternRef{Type: v.Type, ID: v.ID, Title: v.Title}, overlap, v.Strength})
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].overlap != matched[j].overlap {
			return matched[i].overlap > matched[j].overlap
		}
		return matched[i].str > matched[j].str
	})
	out := make([]relatedPatternRef, 0, limit)
	for _, m := range matched {
		if len(out) >= limit {
			break
		}
		out = append(out, m.ref)
	}
	return out, nil
}

// loadReviewPatternContext returns V2 risk/practice/task patterns whose recorded
// evidence touched any of the given files — diff-less regression context for
// `review --patterns`. Capped; graceful (nil) when no corpus. Rejected dossiers
// are excluded.
func loadReviewPatternContext(brainDir string, files []string, limit int) []reviewPatternRef {
	context, _ := loadReviewPatternContextChecked(brainDir, files, limit)
	return context
}

func loadReviewPatternContextChecked(brainDir string, files []string, limit int) ([]reviewPatternRef, error) {
	if len(files) == 0 || limit <= 0 {
		return nil, nil
	}
	db, present, err := patterncore.OpenCorpusReadDBIfPresent(brainDir)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	defer db.Close()
	ph := make([]string, len(files))
	args := make([]any, 0, len(files)+1)
	for i, f := range files {
		ph[i] = "?"
		args = append(args, f)
	}
	args = append(args, limit*4)
	rows, err := db.Query(`
		SELECT DISTINCT p.id, p.type, p.title, ef.path
		FROM patterns p
		JOIN pattern_evidence pe ON pe.pattern_id = p.id
		JOIN episode_files ef ON ef.episode_id = pe.episode_id
		LEFT JOIN dossiers d ON d.pattern_id = p.id
		WHERE p.type IN ('risk','practice','task')
		  AND COALESCE(d.verdict,'') != 'rejected'
		  AND ef.path IN (`+strings.Join(ph, ",")+`)
		ORDER BY p.strength DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reviewPatternRef
	seen := map[string]bool{}
	for rows.Next() {
		var id, typ, title, path string
		if err := rows.Scan(&id, &typ, &title, &path); err != nil {
			return out, err
		}
		key := id + "\x00" + path
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(out) >= limit {
			break
		}
		out = append(out, reviewPatternRef{File: base.RedactText(path), Type: typ, Title: base.RedactText(title), ID: id})
	}
	return out, rows.Err()
}

// handoffConsolidations returns the strongest dossiers for a handoff: current AND
// stale (so staleness is visible on resume), excluding rejected, capped. Empty
// when no corpus.
func handoffConsolidations(brainDir string, limit int) []briefConsolidation {
	consolidations, _ := handoffConsolidationsChecked(brainDir, limit)
	return consolidations
}

func handoffConsolidationsChecked(brainDir string, limit int) ([]briefConsolidation, error) {
	if limit <= 0 {
		return nil, nil
	}
	dossiers, err := loadCorpusDossiersChecked(brainDir)
	if err != nil {
		return nil, err
	}
	views := make([]briefConsolidation, 0, len(dossiers))
	for _, d := range dossiers {
		if d.verdict == "rejected" {
			continue
		}
		views = append(views, consolidationView(d))
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Confidence > views[j].Confidence })
	if len(views) > limit {
		views = views[:limit]
	}
	return views, nil
}

// getCorpusConsolidation addresses one dossier by its pattern id for `get`.
// Returns the consolidation as a unifiedResult, or false if absent.
func getCorpusConsolidation(brainDir, patternID string) (unifiedResult, bool) {
	result, ok, _ := getCorpusConsolidationChecked(brainDir, patternID)
	return result, ok
}

func getCorpusConsolidationChecked(brainDir, patternID string) (unifiedResult, bool, error) {
	db, present, err := patterncore.OpenCorpusReadDBIfPresent(brainDir)
	if err != nil {
		return unifiedResult{}, false, err
	}
	if !present {
		return unifiedResult{}, false, nil
	}
	defer db.Close()
	var blob, status, verdict string
	err = db.QueryRow(`SELECT json_redacted, status, COALESCE(verdict,'') FROM dossiers WHERE pattern_id=?`, patternID).
		Scan(&blob, &status, &verdict)
	if err != nil {
		if err == sql.ErrNoRows {
			return unifiedResult{}, false, nil
		}
		return unifiedResult{}, false, err
	}
	var rec patterncore.DossierRecord
	if json.Unmarshal([]byte(blob), &rec) != nil {
		return unifiedResult{}, false, nil
	}
	return unifiedResult{
		Source: "consolidation",
		ID:     patternID,
		Text:   renderConsolidationText(rec, status, verdict),
	}, true, nil
}

func renderConsolidationText(rec patterncore.DossierRecord, status, verdict string) string {
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
	return base.RedactText(b.String())
}
