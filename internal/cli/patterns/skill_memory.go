package patterns

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Skill memory (Pattern Consolidation, Phase 4).
//
// Skill memory is USER STATE — the record of which patterns the user has formed
// into skills or declined. Unlike episodes/procedures/practices it is NOT
// rebuilt by refresh (refresh never writes this file), so a decision survives
// every rebuild. The link is the pattern id, which is a stable content hash, so
// linkage holds even as the derived layer is regenerated.
//
// This phase establishes the record, its persistence, the state machine, and the
// recommendations surfaced in `patterns` / `patterns status`. Records are created
// by `patterns skills form`; here they are read and evaluated.

const SkillMemoryPath = "patterns/skill-memory.ndjson"

const (
	SkillStatusActive   = "active"
	SkillStatusDeclined = "declined"
)

// sub-states derived from comparing the recorded decision against the current
// evidence and the on-disk skill files.
const (
	SkillSubCurrent    = "current"    // accepted & unchanged, or declined & unchanged
	SkillSubUpdate     = "update"     // evidence changed materially since the decision
	SkillSubEdited     = "edited"     // a written skill file changed since formation
	SkillSubMissing    = "missing"    // a recorded skill file is gone
	SkillSubReconsider = "reconsider" // declined, but evidence changed materially
)

type SkillInstall struct {
	Agents     []string `json:"agents,omitempty"`
	Path       string   `json:"path"`
	ContentSHA string   `json:"content_sha,omitempty"`
}

type SkillMemoryRecord struct {
	PatternID     string              `json:"pattern_id"`
	Scope         string              `json:"scope,omitempty"`
	Status        string              `json:"status"` // active | declined
	SkillName     string              `json:"skill_name,omitempty"`
	Installs      []SkillInstall      `json:"installs,omitempty"`
	Fingerprint   string              `json:"fingerprint,omitempty"` // evidence fingerprint at decision time
	Support       int                 `json:"support,omitempty"`
	Reinforcement ReinforcementCounts `json:"reinforcement,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
	Note          string              `json:"note,omitempty"`
}

// SkillMemoryEval is the derived recommendation for one record against the
// current pattern set.
type SkillMemoryEval struct {
	Status string // active | declined
	Sub    string // current | update | edited | missing | reconsider
}

// SuppressInListing reports whether a pattern with this evaluation should be
// hidden from the default proposal listing — an already-formed-and-current skill
// or a declined-and-unchanged pattern is not re-proposed.
func (e SkillMemoryEval) SuppressInListing() bool {
	return e.Sub == SkillSubCurrent
}

// NeedsAttention reports whether the record should count toward "updates
// available": an accepted skill whose evidence/file drifted, or a declined
// pattern whose evidence changed enough to reconsider.
func (e SkillMemoryEval) NeedsAttention() bool {
	switch e.Sub {
	case SkillSubUpdate, SkillSubEdited, SkillSubMissing, SkillSubReconsider:
		return true
	}
	return false
}

func (e SkillMemoryEval) Annotation() string {
	switch e.Sub {
	case SkillSubUpdate:
		return "skill exists — evidence changed, update available"
	case SkillSubEdited:
		return "skill edited since formation — review before update"
	case SkillSubMissing:
		return "skill file missing — recreate or forget"
	case SkillSubReconsider:
		return "previously declined — evidence changed"
	}
	return ""
}

// EvaluateSkillMemory derives the recommendation for a record. current is the
// pattern's present view, or nil when the pattern no longer surfaces.
func EvaluateSkillMemory(rec SkillMemoryRecord, current *PatternView) SkillMemoryEval {
	changed := current != nil && rec.Fingerprint != "" && PatternEvidenceFingerprint(*current) != rec.Fingerprint

	if rec.Status == SkillStatusDeclined {
		if changed {
			return SkillMemoryEval{Status: SkillStatusDeclined, Sub: SkillSubReconsider}
		}
		return SkillMemoryEval{Status: SkillStatusDeclined, Sub: SkillSubCurrent}
	}

	// active: file integrity takes precedence over evidence drift — a gone or
	// hand-edited file is the more urgent signal.
	for _, in := range rec.Installs {
		sha, ok := fileContentSHA(in.Path)
		if !ok {
			return SkillMemoryEval{Status: SkillStatusActive, Sub: SkillSubMissing}
		}
		if in.ContentSHA != "" && sha != in.ContentSHA {
			return SkillMemoryEval{Status: SkillStatusActive, Sub: SkillSubEdited}
		}
	}
	if changed {
		return SkillMemoryEval{Status: SkillStatusActive, Sub: SkillSubUpdate}
	}
	return SkillMemoryEval{Status: SkillStatusActive, Sub: SkillSubCurrent}
}

// PatternEvidenceFingerprint captures a pattern's *material* evidence so a minor
// drift (one more supporting episode) does not read as a change while a strength
// re-tier or a support-magnitude jump does. Excludes identity (the id already
// pins that) and volatile exact counts.
func PatternEvidenceFingerprint(v PatternView) string {
	net := 0
	if v.Reinforcement != nil {
		net = sign(v.Reinforcement.Success - v.Reinforcement.Corrected)
	}
	bucket := int(math.Log2(float64(1 + v.Support)))
	payload := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", v.Type, v.Scope, v.StrengthLabel, bucket, net)
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}

// fileContentSHA returns the sha256 of a (possibly ~-prefixed) file's content
// and whether it could be read.
func fileContentSHA(path string) (string, bool) {
	data, err := os.ReadFile(ExpandHomePath(path))
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), true
}

func ExpandHomePath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}

// LoadBrainSkillMemory reads the user-state skill memory. Missing file -> empty.
func LoadBrainSkillMemory(brainDir string) ([]SkillMemoryRecord, error) {
	content, err := brainstore.ReadBrainRelativeStateFile(brainDir, SkillMemoryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var records []SkillMemoryRecord
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r SkillMemoryRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("parse skill-memory line: %w", err)
		}
		records = append(records, r)
	}
	return records, nil
}

// SkillMemoryByPatternID indexes records by pattern id (last write wins).
func SkillMemoryByPatternID(records []SkillMemoryRecord) map[string]SkillMemoryRecord {
	out := make(map[string]SkillMemoryRecord, len(records))
	for _, r := range records {
		out[r.PatternID] = r
	}
	return out
}

// WriteBrainSkillMemory persists the user-state skill memory atomically. Refresh
// never calls this; only the skill-creation surface (`patterns skills form`) does.
func WriteBrainSkillMemory(brainDir string, records []SkillMemoryRecord) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("encode skill-memory: %w", err)
		}
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, SkillMemoryPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write skill-memory: %w", err)
	}
	return nil
}
