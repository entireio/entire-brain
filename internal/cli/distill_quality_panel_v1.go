package cli

// This file intentionally contains only the offline contract for the Phase 2
// advisory panel.  It has no provider client, filesystem policy, or CLI
// registration.  In particular, an advisory verdict is not a proof label:
// only distillQualityHumanAdjudicationV1 can carry proof labels.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	distillQualityPanelSchemaVersion = 1

	distillQualityPanelMaxPacketIDBytes       = 128
	distillQualityPanelMaxOpaqueIDBytes       = 128
	distillQualityPanelMaxItemTextBytes       = 16 << 10
	distillQualityPanelMaxPromptBytes         = 32 << 10
	distillQualityPanelMaxFragmentTextBytes   = 16 << 10
	distillQualityPanelMaxFragments           = 32
	distillQualityPanelMaxPayloadBytes        = 128 << 10
	distillQualityPanelMaxScores              = 12
	distillQualityPanelMaxRawScoreBytes       = 1024
	distillQualityPanelMaxRationaleBytes      = 4096
	distillQualityPanelMaxHumanDimensions     = 12
	distillQualityPanelMaxHumanRationaleBytes = 8192
	distillQualityPanelMaxJSONBytes           = 256 << 10
	distillQualityPanelMaxJSONLLineBytes      = 64 << 10
)

type distillQualityJudgeV1 string

const (
	distillQualityJudgeCopilotV1 distillQualityJudgeV1 = "copilot"
	distillQualityJudgeCursorV1  distillQualityJudgeV1 = "cursor"
	distillQualityJudgeClaudeV1  distillQualityJudgeV1 = "claude"
)

var distillQualityPanelJudgesV1 = []distillQualityJudgeV1{
	distillQualityJudgeClaudeV1,
	distillQualityJudgeCopilotV1,
	distillQualityJudgeCursorV1,
}

type distillQualityVerdictStateV1 string

const (
	distillQualityVerdictCompletedV1 distillQualityVerdictStateV1 = "completed"
	distillQualityVerdictAbstainV1   distillQualityVerdictStateV1 = "abstain"
	distillQualityVerdictInvalidV1   distillQualityVerdictStateV1 = "invalid"
)

// These are advisory labels, deliberately not a numeric or proof-label scale.
// Aggregation preserves them as independent signals and never averages them.
type distillQualityAdvisoryLabelV1 string

const (
	distillQualityAdvisoryPassV1     distillQualityAdvisoryLabelV1 = "pass"
	distillQualityAdvisoryConcernV1  distillQualityAdvisoryLabelV1 = "concern"
	distillQualityAdvisoryCriticalV1 distillQualityAdvisoryLabelV1 = "critical"
	distillQualityAdvisoryAbstainV1  distillQualityAdvisoryLabelV1 = "abstain"
	distillQualityAdvisoryInvalidV1  distillQualityAdvisoryLabelV1 = "invalid"
)

type distillQualityDimensionV1 string

const (
	distillQualityDimensionAdmissionV1    distillQualityDimensionV1 = "admission"
	distillQualityDimensionFaithfulnessV1 distillQualityDimensionV1 = "faithfulness"
	distillQualityDimensionAuthorityV1    distillQualityDimensionV1 = "authority"
	distillQualityDimensionTaxonomyV1     distillQualityDimensionV1 = "taxonomy"
	distillQualityDimensionLocusV1        distillQualityDimensionV1 = "locus"
	distillQualityDimensionSafetyV1       distillQualityDimensionV1 = "safety"
)

// distillQualityPanelPacketV1 contains redacted evaluation material only.  The
// schema intentionally has no transcript path, session ID, source line, or
// arbitrary metadata field.  Opaque IDs are rejected when they look like paths
// or source/session identifiers; callers must map them outside this contract.
type distillQualityPanelPacketV1 struct {
	SchemaVersion int                          `json:"schema_version"`
	PacketID      string                       `json:"packet_id"`
	PacketDigest  string                       `json:"packet_digest"`
	Item          distillQualityPanelItemV1    `json:"item"`
	Prompt        distillQualityPanelPromptV1  `json:"prompt"`
	Payload       distillQualityPanelPayloadV1 `json:"payload"`
}

type distillQualityPanelItemV1 struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Digest string `json:"digest"`
}

type distillQualityPanelPromptV1 struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	Instructions string `json:"instructions"`
	Digest       string `json:"digest"`
}

type distillQualityPanelPayloadV1 struct {
	Fragments []distillQualityPanelFragmentV1 `json:"fragments"`
	Digest    string                          `json:"digest"`
}

type distillQualityPanelFragmentV1 struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Digest string `json:"digest"`
}

// distillQualityPanelVerdictV1 retains every bounded structured model score
// and rationale.  Its state and labels can guide a human reviewer, but there
// is purposefully no field named proof_label here.
type distillQualityPanelVerdictV1 struct {
	SchemaVersion int                          `json:"schema_version"`
	PacketID      string                       `json:"packet_id"`
	PacketDigest  string                       `json:"packet_digest"`
	ItemDigest    string                       `json:"item_digest"`
	PromptDigest  string                       `json:"prompt_digest"`
	PayloadDigest string                       `json:"payload_digest"`
	Judge         distillQualityJudgeV1        `json:"judge"`
	Model         string                       `json:"model"`
	State         distillQualityVerdictStateV1 `json:"state"`
	Scores        []distillQualityPanelScoreV1 `json:"scores"`
	Rationale     string                       `json:"rationale,omitempty"`
}

type distillQualityPanelScoreV1 struct {
	Dimension distillQualityDimensionV1     `json:"dimension"`
	Label     distillQualityAdvisoryLabelV1 `json:"label"`
	// RawScore is preserved verbatim as a bounded JSON value. It is provider
	// output for auditability, not a value used by aggregation.
	RawScore  json.RawMessage `json:"raw_score,omitempty"`
	Rationale string          `json:"rationale,omitempty"`
}

// A human, after seeing the complete advisory aggregate, owns these labels.
type distillQualityHumanDecisionV1 string

const (
	distillQualityHumanAcceptV1      distillQualityHumanDecisionV1 = "accept"
	distillQualityHumanRejectV1      distillQualityHumanDecisionV1 = "reject"
	distillQualityHumanNeedsReviewV1 distillQualityHumanDecisionV1 = "needs_review"
)

type distillQualityProofLabelV1 string

const (
	distillQualityProofSupportedV1     distillQualityProofLabelV1 = "supported"
	distillQualityProofUnsupportedV1   distillQualityProofLabelV1 = "unsupported"
	distillQualityProofUncertainV1     distillQualityProofLabelV1 = "uncertain"
	distillQualityProofNotApplicableV1 distillQualityProofLabelV1 = "not_applicable"
)

type distillQualityHumanAdjudicationV1 struct {
	SchemaVersion  int                              `json:"schema_version"`
	RecordID       string                           `json:"record_id"`
	PacketID       string                           `json:"packet_id"`
	PacketDigest   string                           `json:"packet_digest"`
	ItemDigest     string                           `json:"item_digest"`
	PromptDigest   string                           `json:"prompt_digest"`
	PayloadDigest  string                           `json:"payload_digest"`
	AdvisoryDigest string                           `json:"advisory_digest"`
	AdjudicatorID  string                           `json:"adjudicator_id"`
	AdjudicatedAt  string                           `json:"adjudicated_at"`
	Decision       distillQualityHumanDecisionV1    `json:"decision"`
	Dimensions     []distillQualityHumanDimensionV1 `json:"dimensions"`
	Rationale      string                           `json:"rationale,omitempty"`
}

type distillQualityHumanDimensionV1 struct {
	Dimension  distillQualityDimensionV1  `json:"dimension"`
	ProofLabel distillQualityProofLabelV1 `json:"proof_label"`
	Rationale  string                     `json:"rationale,omitempty"`
}

// The aggregate is a deterministic presentation of advisory inputs.  It
// contains no panel-level verdict or numeric average by design.
type distillQualityPanelAggregateV1 struct {
	SchemaVersion int                     `json:"schema_version"`
	PacketID      string                  `json:"packet_id"`
	PacketDigest  string                  `json:"packet_digest"`
	ItemDigest    string                  `json:"item_digest"`
	PromptDigest  string                  `json:"prompt_digest"`
	PayloadDigest string                  `json:"payload_digest"`
	Judges        []distillQualityJudgeV1 `json:"judges"`
	// Verdicts is the complete bounded advisory evidence shown to the human.
	// It retains top-level abstain/invalid rationales as well as every score.
	Verdicts      []distillQualityPanelVerdictV1            `json:"verdicts"`
	MissingJudges []distillQualityJudgeV1                   `json:"missing_judges,omitempty"`
	Dimensions    []distillQualityPanelAggregateDimensionV1 `json:"dimensions"`
	Disagreement  bool                                      `json:"disagreement"`
	Critical      bool                                      `json:"critical"`
	Invalid       bool                                      `json:"invalid"`
}

type distillQualityPanelAggregateDimensionV1 struct {
	Dimension    distillQualityDimensionV1     `json:"dimension"`
	Signals      []distillQualityPanelSignalV1 `json:"signals"`
	Disagreement bool                          `json:"disagreement"`
	Critical     bool                          `json:"critical"`
	Invalid      bool                          `json:"invalid"`
}

type distillQualityPanelSignalV1 struct {
	Judge     distillQualityJudgeV1         `json:"judge"`
	State     distillQualityVerdictStateV1  `json:"state"`
	Label     distillQualityAdvisoryLabelV1 `json:"label"`
	RawScore  json.RawMessage               `json:"raw_score,omitempty"`
	Rationale string                        `json:"rationale,omitempty"`
}

func distillQualityPanelDigestV1(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func distillQualityPanelItemDigestV1(item distillQualityPanelItemV1) string {
	return distillQualityPanelDigestV1(struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	}{item.ID, item.Kind, item.Text})
}

func distillQualityPanelPromptDigestV1(prompt distillQualityPanelPromptV1) string {
	return distillQualityPanelDigestV1(struct {
		ID           string `json:"id"`
		Version      string `json:"version"`
		Instructions string `json:"instructions"`
	}{prompt.ID, prompt.Version, prompt.Instructions})
}

func distillQualityPanelFragmentDigestV1(fragment distillQualityPanelFragmentV1) string {
	return distillQualityPanelDigestV1(struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}{fragment.ID, fragment.Text})
}

func distillQualityPanelPayloadDigestV1(payload distillQualityPanelPayloadV1) string {
	type fragment struct{ ID, Text, Digest string }
	fragments := make([]fragment, len(payload.Fragments))
	for i, f := range payload.Fragments {
		fragments[i] = fragment{f.ID, f.Text, f.Digest}
	}
	return distillQualityPanelDigestV1(fragments)
}

func distillQualityPanelPacketDigestV1(packet distillQualityPanelPacketV1) string {
	return distillQualityPanelDigestV1(struct {
		SchemaVersion int    `json:"schema_version"`
		PacketID      string `json:"packet_id"`
		ItemDigest    string `json:"item_digest"`
		PromptDigest  string `json:"prompt_digest"`
		PayloadDigest string `json:"payload_digest"`
	}{packet.SchemaVersion, packet.PacketID, packet.Item.Digest, packet.Prompt.Digest, packet.Payload.Digest})
}

func validateDistillQualityPanelPacketV1(packet distillQualityPanelPacketV1) error {
	if packet.SchemaVersion != distillQualityPanelSchemaVersion {
		return fmt.Errorf("quality panel packet: unsupported schema_version %d", packet.SchemaVersion)
	}
	if err := validateDistillQualityPanelOpaqueIDV1("packet_id", packet.PacketID, distillQualityPanelMaxPacketIDBytes); err != nil {
		return err
	}
	if err := validateDistillQualityPanelItemV1(packet.Item); err != nil {
		return err
	}
	if err := validateDistillQualityPanelPromptV1(packet.Prompt); err != nil {
		return err
	}
	if err := validateDistillQualityPanelPayloadV1(packet.Payload); err != nil {
		return err
	}
	if !validSHA256Identity(packet.PacketDigest) || packet.PacketDigest != distillQualityPanelPacketDigestV1(packet) {
		return fmt.Errorf("quality panel packet: packet_digest does not match packet")
	}
	return nil
}

func validateDistillQualityPanelItemV1(item distillQualityPanelItemV1) error {
	if err := validateDistillQualityPanelOpaqueIDV1("item.id", item.ID, distillQualityPanelMaxOpaqueIDBytes); err != nil {
		return err
	}
	if !validPanelWordV1(item.Kind) {
		return fmt.Errorf("quality panel packet: invalid item.kind")
	}
	if err := validateDistillQualityPanelTextV1("item.text", item.Text, distillQualityPanelMaxItemTextBytes, false); err != nil {
		return err
	}
	if !validSHA256Identity(item.Digest) || item.Digest != distillQualityPanelItemDigestV1(item) {
		return fmt.Errorf("quality panel packet: item.digest does not match item")
	}
	return nil
}

func validateDistillQualityPanelPromptV1(prompt distillQualityPanelPromptV1) error {
	if err := validateDistillQualityPanelOpaqueIDV1("prompt.id", prompt.ID, distillQualityPanelMaxOpaqueIDBytes); err != nil {
		return err
	}
	if !validPanelWordV1(prompt.Version) {
		return fmt.Errorf("quality panel packet: invalid prompt.version")
	}
	if err := validateDistillQualityPanelTextV1("prompt.instructions", prompt.Instructions, distillQualityPanelMaxPromptBytes, false); err != nil {
		return err
	}
	if !validSHA256Identity(prompt.Digest) || prompt.Digest != distillQualityPanelPromptDigestV1(prompt) {
		return fmt.Errorf("quality panel packet: prompt.digest does not match prompt")
	}
	return nil
}

func validateDistillQualityPanelPayloadV1(payload distillQualityPanelPayloadV1) error {
	if len(payload.Fragments) == 0 || len(payload.Fragments) > distillQualityPanelMaxFragments {
		return fmt.Errorf("quality panel packet: fragments must contain 1..%d entries", distillQualityPanelMaxFragments)
	}
	seen := map[string]struct{}{}
	bytes := 0
	for _, fragment := range payload.Fragments {
		if err := validateDistillQualityPanelOpaqueIDV1("payload.fragments.id", fragment.ID, distillQualityPanelMaxOpaqueIDBytes); err != nil {
			return err
		}
		if _, ok := seen[fragment.ID]; ok {
			return fmt.Errorf("quality panel packet: duplicate payload fragment %q", fragment.ID)
		}
		seen[fragment.ID] = struct{}{}
		if err := validateDistillQualityPanelTextV1("payload.fragments.text", fragment.Text, distillQualityPanelMaxFragmentTextBytes, false); err != nil {
			return err
		}
		bytes += len(fragment.Text)
		if !validSHA256Identity(fragment.Digest) || fragment.Digest != distillQualityPanelFragmentDigestV1(fragment) {
			return fmt.Errorf("quality panel packet: fragment %q digest does not match", fragment.ID)
		}
	}
	if bytes > distillQualityPanelMaxPayloadBytes {
		return fmt.Errorf("quality panel packet: payload exceeds %d bytes", distillQualityPanelMaxPayloadBytes)
	}
	if !validSHA256Identity(payload.Digest) || payload.Digest != distillQualityPanelPayloadDigestV1(payload) {
		return fmt.Errorf("quality panel packet: payload.digest does not match payload")
	}
	return nil
}

func validateDistillQualityPanelVerdictV1(packet distillQualityPanelPacketV1, verdict distillQualityPanelVerdictV1) error {
	if err := validateDistillQualityPanelPacketV1(packet); err != nil {
		return err
	}
	if verdict.SchemaVersion != distillQualityPanelSchemaVersion {
		return fmt.Errorf("quality panel verdict: unsupported schema_version %d", verdict.SchemaVersion)
	}
	if !validDistillQualityJudgeV1(verdict.Judge) {
		return fmt.Errorf("quality panel verdict: unknown judge %q", verdict.Judge)
	}
	if !validPanelWordV1(verdict.Model) {
		return fmt.Errorf("quality panel verdict: invalid model")
	}
	if verdict.PacketID != packet.PacketID || verdict.PacketDigest != packet.PacketDigest || verdict.ItemDigest != packet.Item.Digest || verdict.PromptDigest != packet.Prompt.Digest || verdict.PayloadDigest != packet.Payload.Digest {
		return fmt.Errorf("quality panel verdict: packet identity or digest mismatch")
	}
	if err := validateDistillQualityPanelTextV1("verdict.rationale", verdict.Rationale, distillQualityPanelMaxRationaleBytes, true); err != nil {
		return err
	}
	switch verdict.State {
	case distillQualityVerdictCompletedV1:
		if len(verdict.Scores) == 0 {
			return fmt.Errorf("quality panel verdict: completed verdict needs scores")
		}
	case distillQualityVerdictAbstainV1, distillQualityVerdictInvalidV1:
		if len(verdict.Scores) != 0 {
			return fmt.Errorf("quality panel verdict: %s verdict cannot contain scores", verdict.State)
		}
	default:
		return fmt.Errorf("quality panel verdict: invalid state %q", verdict.State)
	}
	if len(verdict.Scores) > distillQualityPanelMaxScores {
		return fmt.Errorf("quality panel verdict: too many scores")
	}
	seen := map[distillQualityDimensionV1]struct{}{}
	for _, score := range verdict.Scores {
		if !validDistillQualityDimensionV1(score.Dimension) {
			return fmt.Errorf("quality panel verdict: invalid dimension %q", score.Dimension)
		}
		if _, ok := seen[score.Dimension]; ok {
			return fmt.Errorf("quality panel verdict: duplicate dimension %q", score.Dimension)
		}
		seen[score.Dimension] = struct{}{}
		if score.Label != distillQualityAdvisoryPassV1 && score.Label != distillQualityAdvisoryConcernV1 && score.Label != distillQualityAdvisoryCriticalV1 {
			return fmt.Errorf("quality panel verdict: completed score has invalid label %q", score.Label)
		}
		if len(score.RawScore) > distillQualityPanelMaxRawScoreBytes || (len(score.RawScore) > 0 && !json.Valid(score.RawScore)) {
			return fmt.Errorf("quality panel verdict: invalid raw_score for %q", score.Dimension)
		}
		if err := validateDistillQualityPanelTextV1("score.rationale", score.Rationale, distillQualityPanelMaxRationaleBytes, true); err != nil {
			return err
		}
	}
	return nil
}

func aggregateDistillQualityPanelV1(packet distillQualityPanelPacketV1, verdicts []distillQualityPanelVerdictV1) (distillQualityPanelAggregateV1, error) {
	if err := validateDistillQualityPanelPacketV1(packet); err != nil {
		return distillQualityPanelAggregateV1{}, err
	}
	if len(verdicts) > len(distillQualityPanelJudgesV1) {
		return distillQualityPanelAggregateV1{}, fmt.Errorf("quality panel: at most %d verdicts", len(distillQualityPanelJudgesV1))
	}
	agg := distillQualityPanelAggregateV1{SchemaVersion: distillQualityPanelSchemaVersion, PacketID: packet.PacketID, PacketDigest: packet.PacketDigest, ItemDigest: packet.Item.Digest, PromptDigest: packet.Prompt.Digest, PayloadDigest: packet.Payload.Digest}
	byDimension := map[distillQualityDimensionV1][]distillQualityPanelSignalV1{}
	seen := map[distillQualityJudgeV1]struct{}{}
	for _, verdict := range verdicts {
		if err := validateDistillQualityPanelVerdictV1(packet, verdict); err != nil {
			return distillQualityPanelAggregateV1{}, err
		}
		if _, ok := seen[verdict.Judge]; ok {
			return distillQualityPanelAggregateV1{}, fmt.Errorf("quality panel: duplicate judge %q", verdict.Judge)
		}
		seen[verdict.Judge] = struct{}{}
		agg.Judges = append(agg.Judges, verdict.Judge)
		agg.Verdicts = append(agg.Verdicts, verdict)
		if verdict.State == distillQualityVerdictInvalidV1 {
			agg.Invalid = true
		}
		if verdict.State != distillQualityVerdictCompletedV1 {
			continue
		}
		for _, score := range verdict.Scores {
			byDimension[score.Dimension] = append(byDimension[score.Dimension], distillQualityPanelSignalV1{Judge: verdict.Judge, State: verdict.State, Label: score.Label, RawScore: append(json.RawMessage(nil), score.RawScore...), Rationale: score.Rationale})
		}
	}
	sort.Slice(agg.Judges, func(i, j int) bool { return agg.Judges[i] < agg.Judges[j] })
	sort.Slice(agg.Verdicts, func(i, j int) bool { return agg.Verdicts[i].Judge < agg.Verdicts[j].Judge })
	for _, judge := range distillQualityPanelJudgesV1 {
		if _, ok := seen[judge]; !ok {
			agg.MissingJudges = append(agg.MissingJudges, judge)
		}
	}
	dimensions := make([]distillQualityDimensionV1, 0, len(byDimension))
	for dimension := range byDimension {
		dimensions = append(dimensions, dimension)
	}
	sort.Slice(dimensions, func(i, j int) bool { return dimensions[i] < dimensions[j] })
	for _, dimension := range dimensions {
		signals := byDimension[dimension]
		sort.Slice(signals, func(i, j int) bool { return signals[i].Judge < signals[j].Judge })
		labels := map[distillQualityAdvisoryLabelV1]struct{}{}
		dim := distillQualityPanelAggregateDimensionV1{Dimension: dimension, Signals: signals}
		for _, signal := range signals {
			labels[signal.Label] = struct{}{}
			if signal.Label == distillQualityAdvisoryCriticalV1 {
				dim.Critical = true
				agg.Critical = true
			}
		}
		dim.Disagreement = len(labels) > 1
		if dim.Disagreement {
			agg.Disagreement = true
		}
		agg.Dimensions = append(agg.Dimensions, dim)
	}
	return agg, nil
}

func distillQualityPanelAggregateDigestV1(aggregate distillQualityPanelAggregateV1) string {
	return distillQualityPanelDigestV1(aggregate)
}

// validateDistillQualityPanelAggregateV1 verifies the serializable aggregate
// shape without turning it into an authority.  A caller that has the original
// verdicts should use aggregateDistillQualityPanelV1, which recomputes it.
func validateDistillQualityPanelAggregateV1(packet distillQualityPanelPacketV1, aggregate distillQualityPanelAggregateV1) error {
	if err := validateDistillQualityPanelPacketV1(packet); err != nil {
		return err
	}
	if aggregate.SchemaVersion != distillQualityPanelSchemaVersion {
		return fmt.Errorf("quality panel aggregate: unsupported schema_version %d", aggregate.SchemaVersion)
	}
	if aggregate.PacketID != packet.PacketID || aggregate.PacketDigest != packet.PacketDigest || aggregate.ItemDigest != packet.Item.Digest || aggregate.PromptDigest != packet.Prompt.Digest || aggregate.PayloadDigest != packet.Payload.Digest {
		return fmt.Errorf("quality panel aggregate: packet identity or digest mismatch")
	}
	if len(aggregate.Verdicts) != len(aggregate.Judges) {
		return fmt.Errorf("quality panel aggregate: verdicts and judges length mismatch")
	}
	for i, verdict := range aggregate.Verdicts {
		if err := validateDistillQualityPanelVerdictV1(packet, verdict); err != nil {
			return err
		}
		if i > 0 && aggregate.Verdicts[i-1].Judge >= verdict.Judge {
			return fmt.Errorf("quality panel aggregate: verdicts must be sorted uniquely")
		}
	}
	seenJudges := map[distillQualityJudgeV1]struct{}{}
	for i, judge := range aggregate.Judges {
		if !validDistillQualityJudgeV1(judge) || (i > 0 && aggregate.Judges[i-1] >= judge) {
			return fmt.Errorf("quality panel aggregate: judges must be known and sorted uniquely")
		}
		seenJudges[judge] = struct{}{}
	}
	wantMissing := make([]distillQualityJudgeV1, 0, len(distillQualityPanelJudgesV1))
	for _, judge := range distillQualityPanelJudgesV1 {
		if _, ok := seenJudges[judge]; !ok {
			wantMissing = append(wantMissing, judge)
		}
	}
	if len(wantMissing) != len(aggregate.MissingJudges) {
		return fmt.Errorf("quality panel aggregate: missing_judges mismatch")
	}
	for i := range wantMissing {
		if wantMissing[i] != aggregate.MissingJudges[i] {
			return fmt.Errorf("quality panel aggregate: missing_judges mismatch")
		}
	}
	seenDimensions := map[distillQualityDimensionV1]struct{}{}
	anyDisagreement, anyCritical, anyInvalid := false, false, aggregate.Invalid
	for i, dimension := range aggregate.Dimensions {
		if !validDistillQualityDimensionV1(dimension.Dimension) || (i > 0 && aggregate.Dimensions[i-1].Dimension >= dimension.Dimension) {
			return fmt.Errorf("quality panel aggregate: dimensions must be known and sorted uniquely")
		}
		if _, ok := seenDimensions[dimension.Dimension]; ok {
			return fmt.Errorf("quality panel aggregate: duplicate dimension %q", dimension.Dimension)
		}
		seenDimensions[dimension.Dimension] = struct{}{}
		labels, signalJudges := map[distillQualityAdvisoryLabelV1]struct{}{}, map[distillQualityJudgeV1]struct{}{}
		critical := false
		for j, signal := range dimension.Signals {
			if !validDistillQualityJudgeV1(signal.Judge) || signal.State != distillQualityVerdictCompletedV1 || (j > 0 && dimension.Signals[j-1].Judge >= signal.Judge) {
				return fmt.Errorf("quality panel aggregate: invalid or unordered signal")
			}
			if _, ok := signalJudges[signal.Judge]; ok {
				return fmt.Errorf("quality panel aggregate: duplicate signal judge")
			}
			signalJudges[signal.Judge] = struct{}{}
			if signal.Label != distillQualityAdvisoryPassV1 && signal.Label != distillQualityAdvisoryConcernV1 && signal.Label != distillQualityAdvisoryCriticalV1 {
				return fmt.Errorf("quality panel aggregate: invalid signal label")
			}
			if len(signal.RawScore) > distillQualityPanelMaxRawScoreBytes || (len(signal.RawScore) > 0 && !json.Valid(signal.RawScore)) {
				return fmt.Errorf("quality panel aggregate: invalid raw_score")
			}
			if err := validateDistillQualityPanelTextV1("aggregate signal rationale", signal.Rationale, distillQualityPanelMaxRationaleBytes, true); err != nil {
				return err
			}
			labels[signal.Label] = struct{}{}
			critical = critical || signal.Label == distillQualityAdvisoryCriticalV1
		}
		if dimension.Disagreement != (len(labels) > 1) || dimension.Critical != critical || dimension.Invalid {
			return fmt.Errorf("quality panel aggregate: inconsistent dimension flags")
		}
		anyDisagreement = anyDisagreement || dimension.Disagreement
		anyCritical = anyCritical || dimension.Critical
	}
	if aggregate.Disagreement != anyDisagreement || aggregate.Critical != anyCritical || aggregate.Invalid != anyInvalid {
		return fmt.Errorf("quality panel aggregate: inconsistent aggregate flags")
	}
	// Recompute rather than trusting a stored panel conclusion. This also
	// verifies that every raw rationale/score displayed to the human belongs to
	// the corresponding per-dimension signal.
	expected, err := aggregateDistillQualityPanelV1(packet, aggregate.Verdicts)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, aggregate) {
		return fmt.Errorf("quality panel aggregate: does not match advisory verdicts")
	}
	return nil
}

func validateDistillQualityHumanAdjudicationV1(packet distillQualityPanelPacketV1, aggregate distillQualityPanelAggregateV1, record distillQualityHumanAdjudicationV1) error {
	if err := validateDistillQualityPanelPacketV1(packet); err != nil {
		return err
	}
	if err := validateDistillQualityPanelAggregateV1(packet, aggregate); err != nil {
		return err
	}
	if record.SchemaVersion != distillQualityPanelSchemaVersion {
		return fmt.Errorf("quality panel adjudication: unsupported schema_version %d", record.SchemaVersion)
	}
	if err := validateDistillQualityPanelOpaqueIDV1("record_id", record.RecordID, distillQualityPanelMaxOpaqueIDBytes); err != nil {
		return err
	}
	if err := validateDistillQualityPanelOpaqueIDV1("adjudicator_id", record.AdjudicatorID, distillQualityPanelMaxOpaqueIDBytes); err != nil {
		return err
	}
	if record.PacketID != packet.PacketID || record.PacketDigest != packet.PacketDigest || record.ItemDigest != packet.Item.Digest || record.PromptDigest != packet.Prompt.Digest || record.PayloadDigest != packet.Payload.Digest {
		return fmt.Errorf("quality panel adjudication: packet identity or digest mismatch")
	}
	if record.AdvisoryDigest != distillQualityPanelAggregateDigestV1(aggregate) || !validSHA256Identity(record.AdvisoryDigest) {
		return fmt.Errorf("quality panel adjudication: advisory_digest mismatch")
	}
	if _, err := time.Parse(time.RFC3339, record.AdjudicatedAt); err != nil {
		return fmt.Errorf("quality panel adjudication: invalid adjudicated_at: %w", err)
	}
	if record.Decision != distillQualityHumanAcceptV1 && record.Decision != distillQualityHumanRejectV1 && record.Decision != distillQualityHumanNeedsReviewV1 {
		return fmt.Errorf("quality panel adjudication: invalid decision %q", record.Decision)
	}
	if len(record.Dimensions) == 0 || len(record.Dimensions) > distillQualityPanelMaxHumanDimensions {
		return fmt.Errorf("quality panel adjudication: dimensions must contain 1..%d entries", distillQualityPanelMaxHumanDimensions)
	}
	seen := map[distillQualityDimensionV1]struct{}{}
	for _, dimension := range record.Dimensions {
		if !validDistillQualityDimensionV1(dimension.Dimension) {
			return fmt.Errorf("quality panel adjudication: invalid dimension %q", dimension.Dimension)
		}
		if _, ok := seen[dimension.Dimension]; ok {
			return fmt.Errorf("quality panel adjudication: duplicate dimension %q", dimension.Dimension)
		}
		seen[dimension.Dimension] = struct{}{}
		if !validDistillQualityProofLabelV1(dimension.ProofLabel) {
			return fmt.Errorf("quality panel adjudication: invalid proof_label %q", dimension.ProofLabel)
		}
		if err := validateDistillQualityPanelTextV1("human dimension rationale", dimension.Rationale, distillQualityPanelMaxHumanRationaleBytes, true); err != nil {
			return err
		}
	}
	return validateDistillQualityPanelTextV1("human rationale", record.Rationale, distillQualityPanelMaxHumanRationaleBytes, true)
}

// Strict JSON helpers reject unknown fields, trailing data, and oversized input.
func decodeDistillQualityPanelPacketJSONV1(data []byte) (distillQualityPanelPacketV1, error) {
	var packet distillQualityPanelPacketV1
	if err := decodeDistillQualityPanelJSONV1(data, &packet); err != nil {
		return packet, err
	}
	return packet, validateDistillQualityPanelPacketV1(packet)
}

func decodeDistillQualityPanelVerdictJSONV1(data []byte, packet distillQualityPanelPacketV1) (distillQualityPanelVerdictV1, error) {
	var verdict distillQualityPanelVerdictV1
	if err := decodeDistillQualityPanelJSONV1(data, &verdict); err != nil {
		return verdict, err
	}
	return verdict, validateDistillQualityPanelVerdictV1(packet, verdict)
}

func decodeDistillQualityPanelVerdictsJSONLV1(r io.Reader, packet distillQualityPanelPacketV1) ([]distillQualityPanelVerdictV1, error) {
	if err := validateDistillQualityPanelPacketV1(packet); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), distillQualityPanelMaxJSONLLineBytes)
	var verdicts []distillQualityPanelVerdictV1
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			return nil, fmt.Errorf("quality panel JSONL line %d: empty lines are not allowed", line)
		}
		verdict, err := decodeDistillQualityPanelVerdictJSONV1(scanner.Bytes(), packet)
		if err != nil {
			return nil, fmt.Errorf("quality panel JSONL line %d: %w", line, err)
		}
		verdicts = append(verdicts, verdict)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("quality panel JSONL: %w", err)
	}
	return verdicts, nil
}

func decodeDistillQualityPanelJSONV1(data []byte, target any) error {
	if len(data) == 0 || len(data) > distillQualityPanelMaxJSONBytes {
		return fmt.Errorf("quality panel JSON: invalid byte length")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("quality panel JSON: %w", err)
	}
	if decoder.More() {
		return fmt.Errorf("quality panel JSON: trailing data")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("quality panel JSON: trailing data")
	}
	return nil
}

func validateDistillQualityPanelOpaqueIDV1(field, value string, max int) error {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.ContainsAny(value, "/\\") || strings.Contains(strings.ToLower(value), "session") || strings.Contains(strings.ToLower(value), "transcript") || !validPanelWordV1(value) {
		return fmt.Errorf("quality panel: invalid %s", field)
	}
	return nil
}

func validateDistillQualityPanelTextV1(field, value string, max int, optional bool) error {
	if (value == "" && !optional) || len(value) > max || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("quality panel: invalid %s", field)
	}
	return nil
}

func validPanelWordV1(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == ':') {
			return false
		}
	}
	return true
}

func validDistillQualityJudgeV1(judge distillQualityJudgeV1) bool {
	return judge == distillQualityJudgeCopilotV1 || judge == distillQualityJudgeCursorV1 || judge == distillQualityJudgeClaudeV1
}
func validDistillQualityDimensionV1(d distillQualityDimensionV1) bool {
	return d == distillQualityDimensionAdmissionV1 || d == distillQualityDimensionFaithfulnessV1 || d == distillQualityDimensionAuthorityV1 || d == distillQualityDimensionTaxonomyV1 || d == distillQualityDimensionLocusV1 || d == distillQualityDimensionSafetyV1
}
func validDistillQualityProofLabelV1(label distillQualityProofLabelV1) bool {
	return label == distillQualityProofSupportedV1 || label == distillQualityProofUnsupportedV1 || label == distillQualityProofUncertainV1 || label == distillQualityProofNotApplicableV1
}
