package cli

import (
	"database/sql"
	"encoding/json"
	"sort"
)

// Skill-formation bridge (Pattern Consolidation v2, Priority 7).
//
// Skill formation stays a downstream consumer, but its source is now the V2
// corpus: promotable, non-rejected task patterns adapted into taskCandidate so
// the existing synthesize/preview/record path works unchanged AND records
// skill-memory under the corpus pattern id. That id alignment is what makes the
// skill lifecycle (current/update/declined/reconsider) work against the V2
// `patterns` listing. The recorded fingerprint is patternEvidenceFingerprint of
// the same view the listing computes, so drift detection agrees across surfaces.

// loadCorpusTaskCandidates returns the corpus's promotable task patterns as
// skill candidates (id = corpus pattern id), strongest first. A dossier-backed,
// non-rejected task is "skill-worthy"; rejected dossiers are excluded. Returns
// (nil, false) when no corpus exists so callers fall back to the legacy source.
func loadCorpusTaskCandidates(brainDir string) ([]taskCandidate, bool) {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return nil, false
	}
	defer db.Close()

	// A task is skill-worthy when it is promotable (has a dossier) and no verifier
	// — shallow or deep — has returned an unresolved/negative verdict. The verifier
	// is the final arbiter: a rejected, needs_split, or low_confidence verdict
	// (from either the shallow or the deep pass) disqualifies the task. An
	// UNVERIFIED task (no verdict yet) remains formable — this is the explicit
	// pre-verification default; running `patterns verify [--deep]` is what can
	// then disqualify it.
	rows, err := db.Query(`
		SELECT p.id, p.repo_key, COALESCE(p.intent_sig,''), p.title, p.gram, p.strength, p.strength_label,
		       p.support, p.outcome_success, p.outcome_corrected, p.outcome_neutral,
		       COALESCE(d.verdict,'')
		FROM patterns p JOIN dossiers d ON d.pattern_id = p.id
		LEFT JOIN deep_dossiers dd ON dd.pattern_id = p.id
		WHERE p.type='task' AND p.scope='repo'
		  AND COALESCE(d.verdict,'') NOT IN ('rejected','needs_split','low_confidence')
		  AND COALESCE(dd.verdict,'') NOT IN ('rejected','needs_split','low_confidence')
		ORDER BY p.strength DESC`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var cands []taskCandidate
	for rows.Next() {
		var (
			id, repoKey, intentSig, title, gram, strengthLabel, verdict string
			strength                                                    float64
			support, succ, corr, neut                                   int
		)
		if err := rows.Scan(&id, &repoKey, &intentSig, &title, &gram, &strength, &strengthLabel,
			&support, &succ, &corr, &neut, &verdict); err != nil {
			return nil, false
		}
		c := taskCandidate{
			ID: id, RepoKey: repoKey, IntentSignature: intentSig, Label: title,
			Support: support, WithCommands: support,
			Reinforcement: reinforcementCounts{Success: succ, Corrected: corr, Neutral: neut},
			Commands:      splitGramHeads(gram),
			Strength:      strength, StrengthLabel: strengthLabel,
		}
		enrichCandidateFromCorpus(db, &c, id)
		cands = append(cands, c)
	}
	if rows.Err() != nil {
		return nil, false
	}
	return cands, true
}

// enrichCandidateFromCorpus fills the evidence the synthesis agent needs: sample
// intents and anchors from the pattern's evidence episodes, and facts from its
// dossier.
func enrichCandidateFromCorpus(db *sql.DB, c *taskCandidate, patternID string) {
	rows, err := db.Query(`
		SELECT COALESCE(e.intent_raw,''), pe.source_path, pe.start_line
		FROM pattern_evidence pe JOIN episodes e ON e.id = pe.episode_id
		WHERE pe.pattern_id=? ORDER BY pe.rank LIMIT 5`, patternID)
	if err == nil {
		seen := map[string]bool{}
		for rows.Next() {
			var intent, path string
			var line int
			if rows.Scan(&intent, &path, &line) != nil {
				continue
			}
			if intent != "" && !seen[intent] {
				seen[intent] = true
				c.SampleIntents = append(c.SampleIntents, redactText(intent))
			}
			c.Examples = append(c.Examples, episodeAnchor{Path: redactText(path), Line: line})
		}
		rows.Close()
	}
	// Facts from the dossier (already redacted at rest).
	var blob string
	if db.QueryRow(`SELECT json_redacted FROM dossiers WHERE pattern_id=?`, patternID).Scan(&blob) == nil {
		var rec dossierRecord
		if json.Unmarshal([]byte(blob), &rec) == nil {
			c.MatchingFacts = append(c.MatchingFacts, rec.Facts...)
		}
	}
	sort.Strings(c.MatchingFacts)
}

// filterSkillCandidatesByMemory drops candidates that skill-memory says should
// not be re-proposed: an already-formed-and-current skill, or a declined
// candidate whose evidence is unchanged. A materially-changed declined candidate
// (reconsider) or a drifted active skill (update) stays visible. This is the
// dedupe/drift gate for the skill listing.
func filterSkillCandidatesByMemory(brainDir string, cands []taskCandidate) []taskCandidate {
	mem := skillMemoryByPatternID(mustLoadSkillMemory(brainDir))
	if len(mem) == 0 {
		return cands
	}
	out := cands[:0:0]
	for _, c := range cands {
		if rec, ok := mem[c.ID]; ok {
			view := taskView(c)
			if evaluateSkillMemory(rec, &view).suppressInListing() {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// corpusTaskCandidateByID resolves one corpus task candidate by id for `form`.
func corpusTaskCandidateByID(brainDir, id string) (*taskCandidate, bool) {
	cands, ok := loadCorpusTaskCandidates(brainDir)
	if !ok {
		return nil, false
	}
	for i := range cands {
		if cands[i].ID == id {
			return &cands[i], true
		}
	}
	return nil, true // corpus exists but id not among task candidates
}
