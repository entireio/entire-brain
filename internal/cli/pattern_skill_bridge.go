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
// skill candidates (id = corpus pattern id), strongest first. Returns
// (nil, false) when no corpus exists so callers fall back to the legacy source.
//
// loadCorpusTaskCandidates is the FORM RESOLVER: it returns every promotable task
// (one that has a shallow dossier) regardless of verifier verdict, so
// `patterns skills form <id>` can resolve any promotable pattern and then apply
// the accepted-deep-dossier gate with a precise message. The `patterns skills`
// LISTING uses loadSkillProposals instead, which additionally requires an
// accepted deep dossier.
func loadCorpusTaskCandidates(brainDir string) ([]taskCandidate, bool) {
	return queryTaskCandidates(brainDir, false)
}

// loadSkillProposals returns the formable skill proposals: a promotable task
// backed by an ACCEPTED deep dossier (which by construction excludes rejected/
// needs_split/low_confidence), PLUS the knowledge-sourced proposals (accepted
// `lesson:`/`convention:` dossiers — non-obvious recoveries and conventions, the
// archetypes that actually earn being a skill). Skill-memory suppression is
// applied by the caller.
func loadSkillProposals(brainDir string) ([]taskCandidate, bool) {
	cands, ok := queryTaskCandidates(brainDir, true)
	if !ok {
		return nil, false
	}
	return append(cands, loadKnowledgeSkillProposals(brainDir)...), true
}

// loadKnowledgeSkillProposals returns proposals sourced from non-obvious
// knowledge rather than command sequences: accepted procedure (`lesson:`) and
// capability (`convention:`) deep dossiers. These carry their own archetype and
// are synthesized by converting the verified record, not by re-inferring from
// raw rows.
func loadKnowledgeSkillProposals(brainDir string) []taskCandidate {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT pattern_id, json_redacted FROM deep_dossiers
		WHERE verdict='accepted' AND (pattern_id LIKE 'lesson:%' OR pattern_id LIKE 'convention:%')
		ORDER BY pattern_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []taskCandidate
	for rows.Next() {
		var id, blob string
		if rows.Scan(&id, &blob) != nil {
			continue
		}
		var rec deepDossierRecord
		if json.Unmarshal([]byte(blob), &rec) != nil {
			continue
		}
		support := rec.EvidenceEpisodes
		strength := supportScoreV2(support)
		out = append(out, taskCandidate{
			ID: id, Label: rec.Title, Support: support, WithCommands: 0,
			Strength: strength, StrengthLabel: strengthLabelV2(strength),
			SampleIntents: []string{rec.Trigger},
		})
	}
	return out
}

func queryTaskCandidates(brainDir string, requireAcceptedDeep bool) ([]taskCandidate, bool) {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return nil, false
	}
	defer db.Close()

	q := `
		SELECT p.id, p.repo_key, COALESCE(p.intent_sig,''), p.title, p.gram, p.strength, p.strength_label,
		       p.support, p.outcome_success, p.outcome_corrected, p.outcome_neutral
		FROM patterns p JOIN dossiers d ON d.pattern_id = p.id
		LEFT JOIN deep_dossiers dd ON dd.pattern_id = p.id
		WHERE p.type='task' AND p.scope='repo'`
	if requireAcceptedDeep {
		// A proposal must be backed by an accepted deep dossier — the verified,
		// evidence-deep record skills are synthesized from.
		q += ` AND COALESCE(dd.verdict,'') = 'accepted'`
	}
	q += ` ORDER BY p.strength DESC`
	rows, err := db.Query(q)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var cands []taskCandidate
	for rows.Next() {
		var (
			id, repoKey, intentSig, title, gram, strengthLabel string
			strength                                           float64
			support, succ, corr, neut                          int
		)
		if err := rows.Scan(&id, &repoKey, &intentSig, &title, &gram, &strength, &strengthLabel,
			&support, &succ, &corr, &neut); err != nil {
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
