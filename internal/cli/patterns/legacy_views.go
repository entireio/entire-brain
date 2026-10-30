package patterns

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// Legacy (pre-corpus) pattern records: the procedure/practice NDJSON views the
// derived layer writes under the brain dir. The V2 corpus is the primary
// analytical source; these remain the fallback when no corpus has been built.

const ProceduresPath = "patterns/procedures.ndjson"

const PracticesPath = "patterns/practices.ndjson"

type ProcedureRecord struct {
	ID            string              `json:"id"`
	Type          string              `json:"type"`  // always "procedure"
	Scope         string              `json:"scope"` // "repo" | "workspace"
	RepoKey       string              `json:"repo_key,omitempty"`
	Workspace     string              `json:"workspace,omitempty"`
	Commands      []string            `json:"commands"`
	Support       int                 `json:"support"` // distinct episodes
	Authors       int                 `json:"authors"`
	Branches      int                 `json:"branches"`
	Repos         int                 `json:"repos,omitempty"`          // workspace scope: repos sharing this shape
	RepoBreakdown []PatternRepoStat   `json:"repo_breakdown,omitempty"` // workspace scope
	Reinforcement ReinforcementCounts `json:"reinforcement"`
	Strength      float64             `json:"strength"`
	StrengthLabel string              `json:"strength_label"` // high|medium|low
	Examples      []EpisodeAnchor     `json:"examples,omitempty"`
	LastSeen      *time.Time          `json:"last_seen,omitempty"`
}

type PracticeRecord struct {
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
	RepoBreakdown []PatternRepoStat `json:"repo_breakdown,omitempty"` // workspace scope
	Workspace     string            `json:"workspace,omitempty"`
	Confidence    float64           `json:"confidence,omitempty"`
	Strength      float64           `json:"strength"`
	StrengthLabel string            `json:"strength_label"`
	Examples      []EpisodeAnchor   `json:"examples,omitempty"`
	LastSeen      *time.Time        `json:"last_seen,omitempty"`
}

func LoadBrainProcedures(brainDir string) ([]ProcedureRecord, error) {
	content, err := brainstore.ReadBrainRelativeStateFile(brainDir, ProceduresPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var procedures []ProcedureRecord
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p ProcedureRecord
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse procedure line: %w", err)
		}
		procedures = append(procedures, p)
	}
	return procedures, nil
}

func LoadBrainPractices(brainDir string) ([]PracticeRecord, error) {
	content, err := brainstore.ReadBrainRelativeStateFile(brainDir, PracticesPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var practices []PracticeRecord
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p PracticeRecord
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse practice line: %w", err)
		}
		practices = append(practices, p)
	}
	return practices, nil
}
