package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Practice detection (Pattern Consolidation, Phase 3).
//
// A practice is a recurring judgment/convention/gotcha — the "how the user wants
// work done" axis, distinct from procedures (operational command shapes). The
// richest source already exists: durable facts. Each active fact of a
// practice-bearing kind becomes a practice, scored separately from procedures.
//
// Two data realities this code handles (verified against the live brain):
//   - facts distilled before kind-storage have an empty Kind, so the kind is
//     taken via factKindOrInferred (deterministic paths+text inference), never
//     raw f.Kind.
//   - a fact's UpdatedAt is its distill time (uniform across a run), so recency
//     is derived from the newest session timestamp among the fact's provenance,
//     looked up in the manifest — real content recency, not distill recency.
//
// SCOPE: facts are the Phase 3 source. History-record/validation and
// read-only-episode augmentation (the plan's secondary practice sources) are
// deferred; so is cross-linking episode reinforcement onto practices.

const patternsPracticesPath = "patterns/practices.ndjson"

const practiceSupportSaturat = 3 // provenance-session count at which recurrence saturates

// practice strength term weights; sum to 1. Kind dominates because, in the
// current corpus, support/confidence/recency vary little (most facts have one
// provenance anchor, confidence 1.0, and a single distill time); the blend still
// rewards the other axes when richer data arrives.
const (
	wPracticeKind    = 0.45
	wPracticeRecency = 0.25
	wPracticeSupport = 0.20
	wPracticeConf    = 0.10
)

type practiceRecord struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`  // always "practice"
	Scope         string            `json:"scope"` // "repo" (workspace is Phase 6)
	RepoKey       string            `json:"repo_key,omitempty"`
	Kind          string            `json:"kind"`
	Statement     string            `json:"statement"`
	Paths         []string          `json:"paths,omitempty"`
	Locus         []string          `json:"locus,omitempty"`
	Support       int               `json:"support"` // distinct provenance sessions
	Branches      int               `json:"branches"`
	Repos         int               `json:"repos,omitempty"`          // workspace scope
	RepoBreakdown []patternRepoStat `json:"repo_breakdown,omitempty"` // workspace scope
	Workspace     string            `json:"workspace,omitempty"`
	Confidence    float64           `json:"confidence,omitempty"`
	Strength      float64           `json:"strength"`
	StrengthLabel string            `json:"strength_label"`
	Examples      []episodeAnchor   `json:"examples,omitempty"`
	LastSeen      *time.Time        `json:"last_seen,omitempty"`
}

type practiceAgg struct {
	fact     factRecord
	branches map[string]bool
}

// buildBrainPractices derives scored repo-local practices from the durable facts.
// Deterministic and token-free; reads the fact store, not a model.
func buildBrainPractices(brainDir string, manifest *exportManifest, now time.Time) ([]practiceRecord, error) {
	branches := factBranchList(manifest)
	sessionTime := sessionCreatedAtByID(manifest)
	repoKey := ""
	if manifest != nil {
		repoKey = manifest.RepoKey
	}

	// Dedupe facts by content id across branches: a fact present on several
	// branches is one practice; union its provenance and branch set.
	aggs := map[string]*practiceAgg{}
	order := []string{}
	for _, branch := range branches {
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			return nil, err
		}
		for _, f := range facts {
			if f.Status != "active" || strings.TrimSpace(f.Text) == "" {
				continue
			}
			a := aggs[f.ID]
			if a == nil {
				a = &practiceAgg{fact: f, branches: map[string]bool{}}
				aggs[f.ID] = a
				order = append(order, f.ID)
			} else {
				a.fact.Provenance = append(a.fact.Provenance, f.Provenance...)
			}
			if f.Branch != "" {
				a.branches[f.Branch] = true
			}
		}
	}

	practices := make([]practiceRecord, 0, len(order))
	for _, id := range order {
		a := aggs[id]
		f := a.fact
		kind := factKindOrInferred(f)

		sessions := map[string]bool{}
		var anchors []episodeAnchor
		var lastSeen time.Time
		for _, p := range f.Provenance {
			if p.SessionID != "" {
				sessions[p.SessionID] = true
				if t, ok := sessionTime[p.SessionID]; ok && t.After(lastSeen) {
					lastSeen = t
				}
			}
			if p.Transcript != "" && len(anchors) < 3 {
				anchors = append(anchors, episodeAnchor{Path: p.Transcript, Line: p.Line})
			}
		}
		if lastSeen.IsZero() {
			lastSeen = f.UpdatedAt
		}
		support := len(sessions)
		if support == 0 {
			support = len(f.Provenance)
		}
		conf := parsePracticeConfidence(f.Confidence)

		strength := practiceStrength(kind, support, lastSeen, now, conf)
		rec := practiceRecord{
			ID:            "pattern:practice:" + f.ID,
			Type:          "practice",
			Scope:         "repo",
			RepoKey:       repoKey,
			Kind:          kind,
			Statement:     truncateString(f.Text, 280),
			Paths:         f.Paths,
			Locus:         f.Locus,
			Support:       support,
			Branches:      len(a.branches),
			Confidence:    conf,
			Strength:      math.Round(strength*1000) / 1000,
			StrengthLabel: strengthLabel(strength),
			Examples:      anchors,
		}
		if !lastSeen.IsZero() {
			seen := lastSeen
			rec.LastSeen = &seen
		}
		practices = append(practices, rec)
	}

	sort.Slice(practices, func(i, j int) bool {
		if practices[i].Strength != practices[j].Strength {
			return practices[i].Strength > practices[j].Strength
		}
		return practices[i].ID < practices[j].ID
	})
	return practices, nil
}

func practiceStrength(kind string, support int, lastSeen, now time.Time, confidence float64) float64 {
	kindScore := clamp01(float64(factKindPriority[kind]) / 6.0)
	supportScore := clamp01(math.Log2(1+float64(support)) / math.Log2(1+float64(practiceSupportSaturat)))
	recencyScore := recencyScore(lastSeen, now)
	confScore := clamp01(confidence)
	return wPracticeKind*kindScore + wPracticeRecency*recencyScore + wPracticeSupport*supportScore + wPracticeConf*confScore
}

// recencyScore decays linearly from 1 (now) to 0 over a 180-day window.
func recencyScore(lastSeen, now time.Time) float64 {
	if lastSeen.IsZero() {
		return 0
	}
	const window = 180 * 24 * time.Hour
	age := now.Sub(lastSeen)
	if age < 0 {
		age = 0
	}
	return clamp01(1 - float64(age)/float64(window))
}

// parsePracticeConfidence reads a fact's confidence — a numeric string in the
// current store, with a legacy high/medium/low label fallback. Unlike the
// reconcile parseConfidence (which defaults unreadable to 0 to gate auto-apply),
// an absent confidence here defaults to 0.5: a practice with no recorded
// confidence is neutral, not low.
func parsePracticeConfidence(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0.5
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return clamp01(v)
	}
	// Legacy label fallback.
	switch strings.ToLower(s) {
	case "high":
		return 1.0
	case "medium":
		return 0.6
	case "low":
		return 0.3
	}
	return 0.5
}

// factBranchList returns the fact branches to read: the manifest's recorded fact
// branches, falling back to the default branch.
func factBranchList(manifest *exportManifest) []string {
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Facts != nil && len(manifest.Sources.Facts.Branches) > 0 {
		return manifest.Sources.Facts.Branches
	}
	return []string{distillDefaultBranch}
}

func sessionCreatedAtByID(manifest *exportManifest) map[string]time.Time {
	out := map[string]time.Time{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return out
	}
	for _, s := range manifest.Sources.Sessions.Sessions {
		if s.SessionID != "" && !s.CreatedAt.IsZero() {
			out[s.SessionID] = s.CreatedAt
		}
	}
	return out
}

func writeBrainPracticesFile(outputDir string, practices []practiceRecord) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, p := range practices {
		if err := enc.Encode(p); err != nil {
			return fmt.Errorf("encode practice: %w", err)
		}
	}
	if err := writeBrainRelativeFileAtomic(outputDir, patternsPracticesPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write practices: %w", err)
	}
	return nil
}

func loadBrainPractices(brainDir string) ([]practiceRecord, error) {
	content, err := readBrainRelativeFile(brainDir, patternsPracticesPath)
	if err != nil {
		return nil, nil
	}
	var practices []practiceRecord
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p practiceRecord
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse practice line: %w", err)
		}
		practices = append(practices, p)
	}
	return practices, nil
}
