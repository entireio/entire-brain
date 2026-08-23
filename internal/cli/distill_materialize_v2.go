package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const distillCandidateMaterializationMaxLineV2 = 1 << 20

// distillCandidateMaterializationTargetV2 is the branch/provenance view for
// one candidate result. Extraction is branch-independent; these fields are
// deliberately supplied only at materialization time.
//
// TriggerTurnID is stored in factAnchor.DistillTurnID, which is the existing
// candidate-pipeline anchor field used to remove and replace relocated turns.
// CandidateID is materialization identity, not fact content identity: it is
// validated and returned with the materialization, but never put in a fact ID.
type distillCandidateMaterializationTargetV2 struct {
	CandidateID      string
	Branch           string
	SourceSessionID  string
	SourceCheckpoint string
	SourceTranscript string
	TriggerTurnID    string
	StartLine        int
	EndLine          int
}

// distillCandidateMaterializationV2 is the pure output of materialization.
// FactIDs is a deterministic, duplicate-free projection suitable for an
// application receipt; it contains IDs even when an ID already existed in the
// active store and was only given another provenance anchor.
type distillCandidateMaterializationV2 struct {
	CandidateID string
	Branch      string
	Empty       bool
	Facts       []factRecord
	FactIDs     []string
}

// materializeDistillCandidateResultV2 validates a cached extraction result and
// attaches only the explicitly supplied branch/provenance target. It has no
// filesystem, cache, receipt, privacy, or provider behavior.
func materializeDistillCandidateResultV2(result distillCandidateCacheResultV2, target distillCandidateMaterializationTargetV2, now time.Time) (distillCandidateMaterializationV2, error) {
	if err := validateDistillCandidateCacheResultV2(result); err != nil {
		return distillCandidateMaterializationV2{}, fmt.Errorf("invalid candidate result: %w", err)
	}
	if err := validateDistillCandidateMaterializationTargetV2(target); err != nil {
		return distillCandidateMaterializationV2{}, err
	}
	if now.IsZero() {
		return distillCandidateMaterializationV2{}, fmt.Errorf("materialization timestamp is zero")
	}

	out := distillCandidateMaterializationV2{
		CandidateID: target.CandidateID,
		Branch:      target.Branch,
		Empty:       result.Empty,
	}
	if result.Empty {
		return out, nil
	}

	anchor := factAnchor{
		SessionID:     target.SourceSessionID,
		CheckpointID:  target.SourceCheckpoint,
		DistillTurnID: target.TriggerTurnID,
		Transcript:    target.SourceTranscript,
		Line:          target.StartLine,
		EndLine:       target.EndLine,
	}
	out.Facts = make([]factRecord, 0, len(result.Facts))
	ids := make(map[string]struct{}, len(result.Facts))
	for _, cached := range result.Facts {
		// The cache validator guarantees these are canonical. Rechecking the
		// normalized paths here makes the record invariant explicit at this
		// boundary and prevents an accidental future cache relaxation from
		// changing content identity.
		paths := append([]string(nil), cached.Paths...)
		if normalized := normalizeFactPaths(paths); len(normalized) == 0 || !sameStrings(normalized, paths) {
			return distillCandidateMaterializationV2{}, fmt.Errorf("cached fact paths are not canonical")
		}
		if !validFactKind(cached.Kind) || cached.Text == "" || cached.Text != redactText(cached.Text) || len(cached.Text) > distillFactMaxTextSize {
			return distillCandidateMaterializationV2{}, fmt.Errorf("cached fact is invalid")
		}
		id := factRecordID(cached.Text, paths)
		record := factRecord{
			ID:         id,
			Paths:      paths,
			Kind:       cached.Kind,
			Locus:      factLocus(cached.Text),
			Text:       cached.Text,
			Branch:     target.Branch,
			Origin:     factOriginDistilled,
			Status:     factStatusActive,
			Provenance: []factAnchor{anchor},
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if record.ID != factRecordID(record.Text, record.Paths) || record.Status != factStatusActive || record.Origin != factOriginDistilled {
			return distillCandidateMaterializationV2{}, fmt.Errorf("materialized fact identity or state is invalid")
		}
		out.Facts = append(out.Facts, record)
		if _, seen := ids[id]; !seen {
			ids[id] = struct{}{}
			out.FactIDs = append(out.FactIDs, id)
		}
	}
	// Keep receipt-facing IDs deterministic independent of model result order.
	sort.Strings(out.FactIDs)
	return out, nil
}

// applyDistillCandidateResultV2 materializes a result and applies it using
// exact-only actions. factmerge.Upsert unions provenance for equal IDs,
// preserving existing authored/legacy anchors and record state. Different IDs
// are always inserted independently; this function never returns proposals.
func applyDistillCandidateResultV2(active []factRecord, result distillCandidateCacheResultV2, target distillCandidateMaterializationTargetV2, now time.Time) ([]factRecord, distillCandidateMaterializationV2, error) {
	materialized, err := materializeDistillCandidateResultV2(result, target, now)
	if err != nil {
		return nil, distillCandidateMaterializationV2{}, err
	}
	if materialized.Empty {
		return append([]factRecord(nil), active...), materialized, nil
	}
	actions := newDistilledFactActions(materialized.Facts)
	updated, proposals := applyFactActions(active, actions, defaultFactConfidenceThreshold, now)
	if len(proposals) != 0 {
		return nil, distillCandidateMaterializationV2{}, fmt.Errorf("exact-only materialization produced %d proposals", len(proposals))
	}
	return updated, materialized, nil
}

func validateDistillCandidateMaterializationTargetV2(target distillCandidateMaterializationTargetV2) error {
	for name, value := range map[string]string{
		"candidate ID":      target.CandidateID,
		"branch":            target.Branch,
		"source session ID": target.SourceSessionID,
		"source transcript": target.SourceTranscript,
		"trigger turn ID":   target.TriggerTurnID,
	} {
		if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n\t") || len(value) > distillCandidateMaxSourceFieldBytes {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	if target.CandidateID == "NO_FACTS" {
		return fmt.Errorf("candidate ID is invalid")
	}
	transcript := filepath.ToSlash(filepath.Clean(filepath.FromSlash(target.SourceTranscript)))
	if transcript != target.SourceTranscript || transcript == "." || strings.HasPrefix(transcript, "../") || transcript == ".." || filepath.IsAbs(filepath.FromSlash(transcript)) {
		return fmt.Errorf("source transcript is not canonical")
	}
	if target.SourceCheckpoint != "" && (target.SourceCheckpoint != strings.TrimSpace(target.SourceCheckpoint) || strings.ContainsAny(target.SourceCheckpoint, "\x00\r\n\t") || len(target.SourceCheckpoint) > distillCandidateMaxSourceFieldBytes) {
		return fmt.Errorf("source checkpoint is invalid")
	}
	if target.StartLine < 1 || target.EndLine < target.StartLine || target.EndLine > distillCandidateMaterializationMaxLineV2 {
		return fmt.Errorf("source line range is invalid")
	}
	return nil
}
