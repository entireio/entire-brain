package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Procedure detection (Pattern Consolidation, Phase 2).
//
// A procedure is a recurring operational shape: a contiguous command n-gram
// (length 2-3, consecutive duplicates collapsed) that recurs across multiple
// episodes. n-grams (not lone commands) are the unit on purpose — a single
// ubiquitous command ("sed", "cd") is noise, whereas a multi-step shape
// ("gofmt" -> "go test" -> "git commit") is a genuine workflow.
//
// SCORING is a weighted blend in [0,1], with specificity weighted highest
// because suppressing ubiquitous-command noise is the core problem (per the plan
// review): an idf term pushes shapes built from common commands down even when
// they recur often. The other terms are recurrence (log-saturated episode
// support), reinforcement quality (success lifts, corrected sinks), and author/
// branch diversity. Cutoffs map the blend to high/medium/low.

const patternsProceduresPath = "patterns/procedures.ndjson"

const (
	procedureMinSupport     = 3   // distinct episodes a shape needs to be a procedure
	procedureSupportSaturat = 10  // episode count at which the recurrence term saturates
	procedureSpecFloor      = 0.20 // drop shapes whose commands are too ubiquitous to matter
	procedureNGramMin       = 2
	procedureNGramMax       = 3
)

// strength term weights; sum to 1.
const (
	wSpecificity   = 0.40
	wRecurrence    = 0.30
	wReinforcement = 0.20
	wDiversity     = 0.10
)

type procedureRecord struct {
	ID            string              `json:"id"`
	Type          string              `json:"type"`  // always "procedure"
	Scope         string              `json:"scope"` // "repo" (workspace scope is Phase 6)
	RepoKey       string              `json:"repo_key,omitempty"`
	Commands      []string            `json:"commands"`
	Support       int                 `json:"support"` // distinct episodes
	Authors       int                 `json:"authors"`
	Branches      int                 `json:"branches"`
	Reinforcement reinforcementCounts `json:"reinforcement"`
	Strength      float64             `json:"strength"`
	StrengthLabel string              `json:"strength_label"` // high|medium|low
	Examples      []episodeAnchor     `json:"examples,omitempty"`
	LastSeen      *time.Time          `json:"last_seen,omitempty"`
}

func procedureID(scope, repoKey string, commands []string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + repoKey + "\x00" + strings.Join(commands, "\x00")))
	return "pattern:proc:" + hex.EncodeToString(sum[:])
}

type procedureAgg struct {
	commands []string
	episodes map[string]bool
	authors  map[string]bool
	branches map[string]bool
	counts   reinforcementCounts
	anchors  []episodeAnchor
	lastSeen time.Time
	hasSeen  bool
}

// buildBrainProcedures groups episodes into scored repo-local procedures. Pure
// over the episode set, deterministic, token-free.
func buildBrainProcedures(episodes []episodeRecord) []procedureRecord {
	idf, corpus := commandIDF(episodes)

	aggs := map[string]*procedureAgg{}
	for i := range episodes {
		ep := &episodes[i]
		cmds := collapseRuns(ep.CommandSequence)
		// Distinct n-gram keys this episode contains, so a shape repeated within
		// one episode counts that episode's support/reinforcement only once.
		keys := map[string][]string{}
		for n := procedureNGramMin; n <= procedureNGramMax; n++ {
			for _, w := range commandWindows(cmds, n) {
				keys[strings.Join(w, "\x00")] = w
			}
		}
		for key, window := range keys {
			a := aggs[key]
			if a == nil {
				a = &procedureAgg{
					commands: window,
					episodes: map[string]bool{},
					authors:  map[string]bool{},
					branches: map[string]bool{},
				}
				aggs[key] = a
			}
			a.episodes[ep.ID] = true
			if ep.Author != "" {
				a.authors[ep.Author] = true
			}
			if ep.Branch != "" {
				a.branches[ep.Branch] = true
			}
			switch ep.Reinforcement {
			case reinforcementSuccess:
				a.counts.Success++
			case reinforcementCorrected:
				a.counts.Corrected++
			default:
				a.counts.Neutral++
			}
			if len(a.anchors) < 3 {
				a.anchors = append(a.anchors, ep.Source)
			}
			if ep.CreatedAt != nil && (!a.hasSeen || ep.CreatedAt.After(a.lastSeen)) {
				a.lastSeen = *ep.CreatedAt
				a.hasSeen = true
			}
		}
	}

	repoKey := ""
	if len(episodes) > 0 {
		repoKey = episodes[0].RepoKey
	}

	var procedures []procedureRecord
	for _, a := range aggs {
		support := len(a.episodes)
		if support < procedureMinSupport {
			continue
		}
		spec := specificityScore(a.commands, idf, corpus)
		if spec < procedureSpecFloor {
			continue
		}
		strength := procedureStrength(spec, support, a.counts, len(a.authors), len(a.branches))
		rec := procedureRecord{
			ID:            procedureID("repo", repoKey, a.commands),
			Type:          "procedure",
			Scope:         "repo",
			RepoKey:       repoKey,
			Commands:      a.commands,
			Support:       support,
			Authors:       len(a.authors),
			Branches:      len(a.branches),
			Reinforcement: a.counts,
			Strength:      math.Round(strength*1000) / 1000,
			StrengthLabel: strengthLabel(strength),
			Examples:      a.anchors,
		}
		if a.hasSeen {
			seen := a.lastSeen
			rec.LastSeen = &seen
		}
		procedures = append(procedures, rec)
	}

	sort.Slice(procedures, func(i, j int) bool {
		if procedures[i].Strength != procedures[j].Strength {
			return procedures[i].Strength > procedures[j].Strength
		}
		if procedures[i].Support != procedures[j].Support {
			return procedures[i].Support > procedures[j].Support
		}
		return procedures[i].ID < procedures[j].ID
	})
	return procedures
}

// commandIDF returns the inverse-document-frequency weight per command (episode
// is the document) and the corpus size (episodes carrying >=1 command).
func commandIDF(episodes []episodeRecord) (map[string]float64, int) {
	df := map[string]int{}
	corpus := 0
	for i := range episodes {
		seen := map[string]bool{}
		for _, c := range episodes[i].CommandSequence {
			seen[c] = true
		}
		if len(seen) == 0 {
			continue
		}
		corpus++
		for c := range seen {
			df[c]++
		}
	}
	idf := make(map[string]float64, len(df))
	n := float64(corpus)
	for c, d := range df {
		idf[c] = math.Log((1 + n) / (1 + float64(d)))
	}
	return idf, corpus
}

// specificityScore is the average idf of the n-gram's commands normalized by the
// maximum possible idf, in [0,1]. Ubiquitous commands -> near 0; rare ones -> 1.
func specificityScore(commands []string, idf map[string]float64, corpus int) float64 {
	if corpus == 0 || len(commands) == 0 {
		return 0
	}
	maxIDF := math.Log(1 + float64(corpus))
	if maxIDF == 0 {
		return 0
	}
	sum := 0.0
	for _, c := range commands {
		sum += idf[c]
	}
	return clamp01((sum / float64(len(commands))) / maxIDF)
}

func procedureStrength(spec float64, support int, counts reinforcementCounts, authors, branches int) float64 {
	recurrence := clamp01(math.Log2(1+float64(support)) / math.Log2(1+float64(procedureSupportSaturat)))
	// reinforcement: net (success - corrected) over support, mapped to [0,1] with
	// neutral at 0.5; sparse positives mean this rarely exceeds ~0.6 in practice.
	reinf := 0.5
	if support > 0 {
		reinf = clamp01(0.5 + 0.5*float64(counts.Success-counts.Corrected)/float64(support))
	}
	diversity := clamp01(float64((authors-1)+(branches-1)) / 3.0)
	return wSpecificity*spec + wRecurrence*recurrence + wReinforcement*reinf + wDiversity*diversity
}

func strengthLabel(s float64) string {
	switch {
	case s >= 0.6:
		return "high"
	case s >= 0.4:
		return "medium"
	default:
		return "low"
	}
}

func collapseRuns(cmds []string) []string {
	if len(cmds) == 0 {
		return nil
	}
	out := make([]string, 0, len(cmds))
	for i, c := range cmds {
		if i == 0 || cmds[i-1] != c {
			out = append(out, c)
		}
	}
	return out
}

func commandWindows(cmds []string, n int) [][]string {
	if len(cmds) < n {
		return nil
	}
	out := make([][]string, 0, len(cmds)-n+1)
	for i := 0; i+n <= len(cmds); i++ {
		out = append(out, cmds[i:i+n])
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func writeBrainProceduresFile(outputDir string, procedures []procedureRecord) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, p := range procedures {
		if err := enc.Encode(p); err != nil {
			return fmt.Errorf("encode procedure: %w", err)
		}
	}
	if err := writeBrainRelativeFileAtomic(outputDir, patternsProceduresPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write procedures: %w", err)
	}
	return nil
}

func loadBrainProcedures(brainDir string) ([]procedureRecord, error) {
	content, err := readBrainRelativeFile(brainDir, patternsProceduresPath)
	if err != nil {
		return nil, nil
	}
	var procedures []procedureRecord
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p procedureRecord
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse procedure line: %w", err)
		}
		procedures = append(procedures, p)
	}
	return procedures, nil
}
