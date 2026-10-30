package cli

import (
	"encoding/json"
	"fmt"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
)

// Skill-formation bridge (Pattern Consolidation v2, Priority 7).
//
// Skill formation stays a downstream consumer, but its source is now the V2
// corpus: promotable, non-rejected task patterns adapted into taskCandidate so
// the existing synthesize/preview/record path works unchanged AND records
// skill-memory under the corpus pattern id. That id alignment is what makes the
// skill lifecycle (current/update/declined/reconsider) work against the V2
// `patterns` listing. The recorded fingerprint is patterncore.PatternEvidenceFingerprint of
// the same view the listing computes, so drift detection agrees across surfaces.

// loadSkillProposals returns the formable skill proposals: a promotable task
// backed by an ACCEPTED deep dossier (which by construction excludes rejected/
// needs_split/low_confidence), PLUS the knowledge-sourced proposals (accepted
// `lesson:`/`convention:` dossiers — non-obvious recoveries and conventions, the
// archetypes that actually earn being a skill). Skill-memory suppression is
// applied by the caller.
func loadSkillProposals(brainDir string) ([]taskCandidate, bool) {
	candidates, present, _ := loadSkillProposalsChecked(brainDir)
	return candidates, present
}

func loadSkillProposalsChecked(brainDir string) ([]taskCandidate, bool, error) {
	cands, ok, err := patterncore.QueryTaskCandidatesChecked(brainDir, true)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	knowledge, err := loadKnowledgeSkillProposalsChecked(brainDir)
	if err != nil {
		return nil, false, err
	}
	return append(cands, knowledge...), true, nil
}

// loadKnowledgeSkillProposals returns proposals sourced from non-obvious
// knowledge rather than command sequences: accepted procedure (`lesson:`) and
// capability (`convention:`) deep dossiers. These carry their own archetype and
// are synthesized by converting the verified record, not by re-inferring from
// raw rows.
func loadKnowledgeSkillProposals(brainDir string) []taskCandidate {
	proposals, _ := loadKnowledgeSkillProposalsChecked(brainDir)
	return proposals
}

func loadKnowledgeSkillProposalsChecked(brainDir string) ([]taskCandidate, error) {
	db, present, err := patterncore.OpenCorpusReadDBIfPresent(brainDir)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	defer db.Close()
	// A corpus that predates the additive deep_dossiers table has no accepted
	// dossiers, not a broken read.
	if ok, err := patterncore.CorpusHasTable(db.DB, "deep_dossiers"); err != nil || !ok {
		return nil, err
	}
	rows, err := db.Query(`SELECT pattern_id, json_redacted FROM deep_dossiers
		WHERE verdict='accepted' AND (pattern_id LIKE 'lesson:%' OR pattern_id LIKE 'convention:%')
		ORDER BY pattern_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []taskCandidate
	for rows.Next() {
		var id, blob string
		if err := rows.Scan(&id, &blob); err != nil {
			return out, err
		}
		var rec deepDossierRecord
		if err := json.Unmarshal([]byte(blob), &rec); err != nil {
			return nil, fmt.Errorf("%s: parse accepted knowledge skill proposal: %w", brainstore.StateCorruptCode, err)
		}
		support := rec.EvidenceEpisodes
		strength := patterncore.SupportScoreV2(support)
		out = append(out, taskCandidate{
			ID: id, Label: rec.Title, Support: support, WithCommands: 0,
			Strength: strength, StrengthLabel: patterncore.StrengthLabelV2(strength),
			SampleIntents: []string{rec.Trigger},
		})
	}
	return out, rows.Err()
}

// filterSkillCandidatesByMemory drops candidates that skill-memory says should
// not be re-proposed: an already-formed-and-current skill, or a declined
// candidate whose evidence is unchanged. A materially-changed declined candidate
// (reconsider) or a drifted active skill (update) stays visible. This is the
// dedupe/drift gate for the skill listing.
func filterSkillCandidatesByMemory(brainDir string, cands []taskCandidate) []taskCandidate {
	filtered, _ := filterSkillCandidatesByMemoryChecked(brainDir, cands)
	return filtered
}

func filterSkillCandidatesByMemoryChecked(brainDir string, cands []taskCandidate) ([]taskCandidate, error) {
	records, err := patterncore.LoadBrainSkillMemory(brainDir)
	if err != nil {
		return nil, err
	}
	mem := patterncore.SkillMemoryByPatternID(records)
	if len(mem) == 0 {
		return cands, nil
	}
	out := cands[:0:0]
	for _, c := range cands {
		if rec, ok := mem[c.ID]; ok {
			view := taskView(c)
			if patterncore.EvaluateSkillMemory(rec, &view).SuppressInListing() {
				continue
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// corpusTaskCandidateByID resolves one corpus task candidate by id for `form`.
func corpusTaskCandidateByID(brainDir, id string) (*taskCandidate, bool) {
	candidate, present, _ := corpusTaskCandidateByIDChecked(brainDir, id)
	return candidate, present
}

func corpusTaskCandidateByIDChecked(brainDir, id string) (*taskCandidate, bool, error) {
	cands, ok, err := patterncore.LoadCorpusTaskCandidatesChecked(brainDir)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	for i := range cands {
		if cands[i].ID == id {
			return &cands[i], true, nil
		}
	}
	return nil, true, nil // corpus exists but id not among task candidates
}
