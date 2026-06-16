package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Pattern consolidation verifier (V2, Phase 4).
//
// An OPTIONAL agent pass that audits a deterministic dossier against its own
// cited evidence and returns a structured verdict. It is the adversarial check
// that rejects conflated or unsupported consolidations before they can become
// skills. It is reachable ONLY through explicit write/maintenance surfaces
// (`patterns verify`), never from refresh/watch/brief/query/MCP/workspace, is
// egress-gated, and is cached by evidence fingerprint so unchanged dossiers are
// not re-sent to an agent.

const dossierVerifyTimeout = 240 * time.Second

const dossierVerifySchemaVersion = 1

const dossierVerifySystemPrompt = `You audit a CONSOLIDATION RECORD (a "dossier") that was assembled deterministically from a code agent's session history for one recurring pattern in a repository.

You are given the dossier as JSON: its trigger, workflow (the actual commands run), variants, verification steps, failure modes, the durable facts that corroborate it, and exact source anchors.

Your job is to decide whether this dossier is a sound, single, non-conflated pattern worth consolidating — NOT to rewrite it. Judge ONLY against the evidence present. Do not invent claims.

Return EXACTLY one JSON object and nothing else (no prose, no code fences):
{
  "schema_version": 1,
  "verdict": "accepted" | "rejected" | "needs_split" | "low_confidence",
  "reason": "<one or two sentences>",
  "unsupported_claims": ["<claim in the dossier not backed by its evidence>"],
  "conflated_subpatterns": ["<distinct sub-pattern if the dossier conflates several>"],
  "required_edits": ["<concrete edit that would make it sound>"],
  "evidence_fingerprint": "<copy the dossier.fingerprint value verbatim>"
}

Verdicts:
- "accepted": one coherent pattern, claims supported, worth consolidating.
- "needs_split": the workflow/variants conflate two or more distinct patterns.
- "low_confidence": coherent but thin/weak evidence.
- "rejected": generic or unsupported — nothing repo-specific worth keeping.`

type dossierVerdict struct {
	SchemaVersion        int      `json:"schema_version"`
	Verdict              string   `json:"verdict"`
	Reason               string   `json:"reason"`
	UnsupportedClaims    []string `json:"unsupported_claims"`
	ConflatedSubpatterns []string `json:"conflated_subpatterns"`
	RequiredEdits        []string `json:"required_edits"`
	EvidenceFingerprint  string   `json:"evidence_fingerprint"`
}

var allowedVerdicts = map[string]bool{
	"accepted": true, "rejected": true, "needs_split": true, "low_confidence": true,
}

type dossierVerifyStats struct {
	Considered int
	Verified   int
	Cached     int
	Failed     int
}

// verifyDossiers runs the agent verifier over the promotable dossiers that need
// it (no cached verdict for the current evidence fingerprint). Egress-gated and
// cached; deterministic dossiers themselves are never mutated here.
func verifyDossiers(ctx context.Context, db *sql.DB, repoDir, agent, model, effort string, run distillAgentRunner, now time.Time) (dossierVerifyStats, error) {
	var stats dossierVerifyStats
	if err := rejectAgentForNoEgress(agent); err != nil {
		return stats, err
	}
	args, err := distillAgentCommandArgs(agent, nil, dossierVerifySystemPrompt)
	if err != nil {
		return stats, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)

	rows, err := db.Query(`SELECT pattern_id, fingerprint, json_redacted, COALESCE(verified_fingerprint,'') FROM dossiers ORDER BY pattern_id`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	type todo struct {
		patternID, fingerprint, jsonRedacted, verified string
	}
	var work []todo
	for rows.Next() {
		var t todo
		if err := rows.Scan(&t.patternID, &t.fingerprint, &t.jsonRedacted, &t.verified); err != nil {
			return stats, err
		}
		work = append(work, t)
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	rows.Close()

	ts := now.UTC().Format(time.RFC3339)
	for _, t := range work {
		stats.Considered++
		if t.verified == t.fingerprint && t.verified != "" {
			stats.Cached++
			continue
		}
		verdict, raw, err := runDossierVerifier(ctx, repoDir, args, t.jsonRedacted, t.fingerprint, run)
		if err != nil {
			stats.Failed++
			continue
		}
		if _, err := db.Exec(`UPDATE dossiers
			SET verifier_json_redacted=?, verdict=?, verified_fingerprint=?, status='current', updated_at=?
			WHERE pattern_id=?`, redactText(raw), verdict.Verdict, t.fingerprint, ts, t.patternID); err != nil {
			return stats, err
		}
		stats.Verified++
	}
	return stats, nil
}

func runDossierVerifier(ctx context.Context, repoDir string, args []string, dossierJSON, fingerprint string, run distillAgentRunner) (dossierVerdict, string, error) {
	// Redaction boundary: the dossier is already redacted at rest, but redact
	// again defensively before it leaves the brain as agent input.
	input := redactText(dossierJSON)
	out, err := run(ctx, repoDir, args, []byte(input), dossierVerifyTimeout)
	if err != nil {
		return dossierVerdict{}, "", fmt.Errorf("verifier agent: %w", err)
	}
	v, raw, err := parseDossierVerdict(out)
	if err != nil {
		return dossierVerdict{}, "", err
	}
	// Pin the fingerprint to the evidence actually verified, regardless of what
	// the agent echoed back.
	v.EvidenceFingerprint = fingerprint
	return v, raw, nil
}

func parseDossierVerdict(out string) (dossierVerdict, string, error) {
	raw := strings.TrimSpace(out)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.LastIndexByte(raw, '}'); j >= i {
			raw = raw[i : j+1]
		}
	}
	var v dossierVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return dossierVerdict{}, "", fmt.Errorf("parse verifier output: %w", err)
	}
	v.Verdict = strings.ToLower(strings.TrimSpace(v.Verdict))
	if !allowedVerdicts[v.Verdict] {
		return dossierVerdict{}, "", fmt.Errorf("invalid verdict %q", v.Verdict)
	}
	if v.SchemaVersion == 0 {
		v.SchemaVersion = dossierVerifySchemaVersion
	}
	return v, raw, nil
}
