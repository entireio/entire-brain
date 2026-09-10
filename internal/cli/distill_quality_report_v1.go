package cli

// Local-only renderer for the Phase 2 human review package.  The source map is
// intentionally read here, rather than copied into an advisory aggregate: it
// may contain session and transcript identities which must remain local.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// renderDistillQualityHumanReviewV1 has no provider interaction. Existing
// panel files are evidence: if one exists but is malformed, the report is not
// rendered. A completely absent provider file is instead shown as missing.
func renderDistillQualityHumanReviewV1(bundleDir string) (distillQualityReportResultV1, error) {
	// The shared loader verifies the sealed prepared manifest, public bundle,
	// packets, and private source map before any advisory file is considered.
	manifest, packets, sources, err := loadDistillQualityRunV1(bundleDir)
	if err != nil {
		return distillQualityReportResultV1{}, err
	}
	root, err := validateDistillQualityBundleDirV1(bundleDir)
	if err != nil {
		return distillQualityReportResultV1{}, err
	}
	packetByID := make(map[string]distillQualityPanelPacketV1, len(packets))
	for _, packet := range packets {
		packetByID[packet.PacketID] = packet
	}
	verdictsByPacket := make(map[string][]distillQualityPanelVerdictV1, len(packets))
	for _, judge := range distillQualityPanelJudgesV1 {
		verdicts, err := loadDistillQualityJudgeVerdictsV1(root, judge, packetByID)
		if err != nil {
			return distillQualityReportResultV1{}, err
		}
		if len(verdicts) == 0 {
			// The common loader distinguishes absent files by returning nil. An
			// existing but empty JSONL is corrupt panel evidence, never missing.
			if reportDistillQualityJudgeFileExistsV1(root, judge) {
				return distillQualityReportResultV1{}, fmt.Errorf("quality report judge %s: existing file contains no verdicts", judge)
			}
			continue
		}
		for _, verdict := range verdicts {
			verdictsByPacket[verdict.PacketID] = append(verdictsByPacket[verdict.PacketID], verdict)
		}
	}

	aggregates := make([]distillQualityPanelAggregateV1, 0, len(packets))
	templates := make([]distillQualityHumanTemplateV1, 0, len(packets))
	result := distillQualityReportResultV1{RunID: manifest.RunID, Packets: len(packets), ProofLabels: false}
	for _, packet := range packets {
		aggregate, err := aggregateDistillQualityPanelV1(packet, verdictsByPacket[packet.PacketID])
		if err != nil {
			return distillQualityReportResultV1{}, err
		}
		aggregates = append(aggregates, aggregate)
		if aggregate.Critical {
			result.Critical++
		}
		if aggregate.Disagreement {
			result.Disagreement++
		}
		if aggregate.Invalid {
			result.Invalid++
		}
		if len(aggregate.MissingJudges) > 0 {
			result.MissingJudges++
		}
		templates = append(templates, reportDistillQualityHumanTemplateV1(packet, aggregate))
	}
	aggregateBytes, err := marshalDistillQualityJSONLV1(aggregates)
	if err != nil {
		return distillQualityReportResultV1{}, err
	}
	templateBytes, err := marshalDistillQualityJSONLV1(templates)
	if err != nil {
		return distillQualityReportResultV1{}, err
	}
	review := reportRenderDistillQualityStartV1(manifest, result)
	audit := reportRenderDistillQualityMarkdownV1(manifest, packets, aggregates, sources)
	for _, output := range []struct {
		path string
		data []byte
	}{
		{filepath.Join(root, distillQualityAggregatesFileV1), aggregateBytes},
		{filepath.Join(root, distillQualityHumanTemplateFileV1), templateBytes},
		{filepath.Join(root, distillQualityHumanReviewFileV1), []byte(review)},
		{filepath.Join(root, distillQualityAuditArchiveFileV1), []byte(audit)},
	} {
		if err := reportWriteDistillQualityArtifactV1(output.path, output.data); err != nil {
			return distillQualityReportResultV1{}, err
		}
	}
	result.ReviewPath = filepath.Join(root, distillQualityHumanReviewFileV1)
	result.AuditPath = filepath.Join(root, distillQualityAuditArchiveFileV1)
	result.AggregatePath = filepath.Join(root, distillQualityAggregatesFileV1)
	result.TemplatePath = filepath.Join(root, distillQualityHumanTemplateFileV1)
	return result, nil
}

func reportRenderDistillQualityStartV1(manifest distillQualityRunManifestV1, result distillQualityReportResultV1) string {
	var out strings.Builder
	out.WriteString("# Phase 2 Distillation Quality: Human Review\n\n")
	out.WriteString("You do **not** need to read the complete audit archive. Review one item at a time with:\n\n")
	out.WriteString("```sh\n")
	out.WriteString("entire brain facts distill-quality adjudicate \\\n  --bundle <this-directory> \\\n  --adjudicator <your-id>\n")
	out.WriteString("```\n\n")
	out.WriteString("The default is a balanced 20-item calibration batch in a full-screen terminal workspace. Each screen shows only the exact statement under review, what the automatic filter decided, and every available Copilot, Cursor, and Claude decision with a short reason; unavailable judges are omitted. At the bottom, answer whether you agree with the judges' majority using `y` or `n`, or left/right plus Enter. If there is no majority, answer whether the filter itself was correct. Up/down scrolls, `x` skips, and `q` stops. Every answer is saved immediately and the same command resumes without repeating it. Human disagreements with the panel are retained for follow-up. Use `--plain` for the line-oriented accessibility or non-TTY fallback.\n\n")
	fmt.Fprintf(&out, "Run `%s` contains %d packets: %d critical, %d with judge disagreement, %d with invalid judge output, and %d with missing judges. These are attention-routing counts, not human error labels.\n\n", manifest.RunID, result.Packets, result.Critical, result.Disagreement, result.Invalid, result.MissingJudges)
	out.WriteString("After calibration, continue in bounded sessions with `--queue critical|invalid|disagreement|clean|all --limit 1..100`. Model consensus never supplies a default decision.\n\n")
	out.WriteString("The exhaustive evidence trail is in [`human-review-audit.md`](human-review-audit.md). It exists for audit and spot-checking; it is not a linear review task.\n")
	return out.String()
}

func reportDistillQualityJudgeFileExistsV1(root string, judge distillQualityJudgeV1) bool {
	_, err := os.Lstat(filepath.Join(root, distillQualityPanelDirV1, string(judge)+".jsonl"))
	return err == nil
}

func reportDistillQualityHumanTemplateV1(packet distillQualityPanelPacketV1, aggregate distillQualityPanelAggregateV1) distillQualityHumanTemplateV1 {
	dimensions := make(map[string]*distillQualityProofLabelV1, len(reportDistillQualityDimensionsV1()))
	for _, dimension := range reportDistillQualityDimensionsV1() {
		dimensions[string(dimension)] = nil // Explicitly a human-only, unset proof label.
	}
	return distillQualityHumanTemplateV1{
		Contract:       distillQualityRunContractV1,
		SchemaVersion:  distillQualityPanelSchemaVersion,
		PacketID:       packet.PacketID,
		PacketDigest:   packet.PacketDigest,
		AdvisoryDigest: distillQualityPanelAggregateDigestV1(aggregate),
		Decision:       nil,
		Dimensions:     dimensions,
	}
}

func reportDistillQualityDimensionsV1() []distillQualityDimensionV1 {
	return []distillQualityDimensionV1{
		distillQualityDimensionAdmissionV1,
		distillQualityDimensionFaithfulnessV1,
		distillQualityDimensionAuthorityV1,
		distillQualityDimensionTaxonomyV1,
		distillQualityDimensionLocusV1,
		distillQualityDimensionSafetyV1,
	}
}

func reportWriteDistillQualityArtifactV1(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("quality report artifact %s is not a regular file", filepath.Base(path))
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

func reportRenderDistillQualityMarkdownV1(manifest distillQualityRunManifestV1, packets []distillQualityPanelPacketV1, aggregates []distillQualityPanelAggregateV1, sources []distillQualityPrivateSourceRecordV1) string {
	byPacket := make(map[string]distillQualityPanelAggregateV1, len(aggregates))
	for _, aggregate := range aggregates {
		byPacket[aggregate.PacketID] = aggregate
	}
	byPublicID := make(map[string][]distillQualityPrivateSourceRecordV1)
	for _, source := range sources {
		byPublicID[source.PublicID] = append(byPublicID[source.PublicID], source)
	}
	for id := range byPublicID {
		sort.Slice(byPublicID[id], func(i, j int) bool {
			return reportDistillQualitySourceKeyV1(byPublicID[id][i]) < reportDistillQualitySourceKeyV1(byPublicID[id][j])
		})
	}
	ordered := append([]distillQualityPanelPacketV1(nil), packets...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := byPacket[ordered[i].PacketID], byPacket[ordered[j].PacketID]
		return reportDistillQualityAttentionRankV1(left) < reportDistillQualityAttentionRankV1(right)
	})
	var out strings.Builder
	out.WriteString("# Phase 2 Distillation Quality: Audit Archive\n\n")
	out.WriteString("> **Audit archive — do not review this file linearly.** Use the resumable one-item-at-a-time reviewer instead:\n>\n> `entire brain facts distill-quality adjudicate --bundle <this-directory> --adjudicator <your-id>`\n>\n> It starts with a balanced 20-item calibration batch, saves every answer immediately, and resumes without repeating completed items. This document remains the complete immutable evidence trail.\n\n")
	fmt.Fprintf(&out, "Run `%s` · %d packets · advisory only; no proof labels have been assigned.\n\n", manifest.RunID, len(packets))
	critical, disagreement, invalid, missing := 0, 0, 0, 0
	for _, aggregate := range aggregates {
		if aggregate.Critical {
			critical++
		}
		if aggregate.Disagreement {
			disagreement++
		}
		if aggregate.Invalid {
			invalid++
		}
		if len(aggregate.MissingJudges) > 0 {
			missing++
		}
	}
	out.WriteString("## Attention first\n\n")
	fmt.Fprintf(&out, "- Critical: **%d**\n- Judge disagreement: **%d**\n- Invalid judge verdicts: **%d**\n- Packets with missing judges: **%d**\n\n", critical, disagreement, invalid, missing)
	out.WriteString("Read each advisory score as input for human adjudication. It is not a proof label and must not be copied as one. The JSONL template deliberately contains null decisions and null proof labels.\n\n")
	for index, packet := range ordered {
		aggregate := byPacket[packet.PacketID]
		fmt.Fprintf(&out, "## %d. `%s`\n\n", index+1, reportDistillQualityMarkdownCodeV1(packet.PacketID))
		fmt.Fprintf(&out, "**Attention:** %s\n\n", reportDistillQualityAttentionTextV1(aggregate))
		out.WriteString("### Redacted review packet\n\n```json\n")
		item, _ := json.MarshalIndent(packet.Item, "", "  ")
		out.Write(item)
		out.WriteString("\n```\n\n")
		out.WriteString("### Redacted evidence\n\n")
		for _, fragment := range packet.Payload.Fragments {
			fmt.Fprintf(&out, "Fragment `%s`:\n\n    %s\n\n", reportDistillQualityMarkdownCodeV1(fragment.ID), reportDistillQualityIndentedV1(fragment.Text))
		}
		out.WriteString("### Advisory panel\n\n")
		if len(aggregate.Verdicts) == 0 {
			out.WriteString("No judge files were available.\n\n")
		}
		for _, verdict := range aggregate.Verdicts {
			fmt.Fprintf(&out, "- **%s** (`%s`): %s", verdict.Judge, reportDistillQualityMarkdownCodeV1(verdict.Model), verdict.State)
			if verdict.Rationale != "" {
				fmt.Fprintf(&out, " — %s", verdict.Rationale)
			}
			out.WriteString("\n")
			for _, score := range verdict.Scores {
				fmt.Fprintf(&out, "  - `%s`: **%s**", score.Dimension, score.Label)
				if len(score.RawScore) > 0 {
					fmt.Fprintf(&out, " · raw score `%s`", reportDistillQualityInlineJSONV1(score.RawScore))
				}
				if score.Rationale != "" {
					fmt.Fprintf(&out, " · %s", score.Rationale)
				}
				out.WriteString("\n")
			}
		}
		if len(aggregate.MissingJudges) > 0 {
			fmt.Fprintf(&out, "\nMissing judges: `%s`.\n", strings.Join(reportDistillQualityJudgeStringsV1(aggregate.MissingJudges), "`, `"))
		}
		out.WriteString("\n### Local source references (private)\n\n")
		for _, source := range byPublicID[packet.Item.ID] {
			fmt.Fprintf(&out, "- session `%s`, branch `%s`, checkpoint `%s`, transcript `%s`, lines %d–%d, turns `%s`\n", reportDistillQualityMarkdownCodeV1(source.SessionID), reportDistillQualityMarkdownCodeV1(source.Branch), reportDistillQualityMarkdownCodeV1(reportDistillQualityEmptyV1(source.CheckpointID)), reportDistillQualityMarkdownCodeV1(filepath.ToSlash(source.TranscriptPath)), source.StartLine, source.EndLine, reportDistillQualityMarkdownCodeV1(strings.Join(source.TurnIDs, "`, `")))
		}
		out.WriteString("\n")
	}
	return out.String()
}

func reportDistillQualityAttentionRankV1(aggregate distillQualityPanelAggregateV1) int {
	if aggregate.Critical {
		return 0
	}
	if aggregate.Invalid {
		return 1
	}
	if aggregate.Disagreement {
		return 2
	}
	if len(aggregate.MissingJudges) > 0 {
		return 3
	}
	return 4
}
func reportDistillQualityAttentionTextV1(aggregate distillQualityPanelAggregateV1) string {
	var parts []string
	if aggregate.Critical {
		parts = append(parts, "critical advisory issue")
	}
	if aggregate.Invalid {
		parts = append(parts, "invalid advisory verdict")
	}
	if aggregate.Disagreement {
		parts = append(parts, "judge disagreement")
	}
	if len(aggregate.MissingJudges) > 0 {
		parts = append(parts, "missing judges: "+strings.Join(reportDistillQualityJudgeStringsV1(aggregate.MissingJudges), ", "))
	}
	if len(parts) == 0 {
		return "no panel-level attention flag"
	}
	return strings.Join(parts, "; ")
}
func reportDistillQualityJudgeStringsV1(judges []distillQualityJudgeV1) []string {
	out := make([]string, len(judges))
	for i, judge := range judges {
		out[i] = string(judge)
	}
	return out
}
func reportDistillQualitySourceKeyV1(source distillQualityPrivateSourceRecordV1) string {
	return strings.Join([]string{source.SessionID, source.Branch, source.CheckpointID, source.TranscriptPath, fmt.Sprintf("%010d", source.StartLine)}, "\x00")
}
func reportDistillQualityEmptyV1(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
func reportDistillQualityInlineJSONV1(value json.RawMessage) string {
	return strings.ReplaceAll(strings.ReplaceAll(string(value), "`", "'"), "\n", " ")
}
func reportDistillQualityMarkdownCodeV1(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "`", "'"), "\n", " ")
}
func reportDistillQualityIndentedV1(value string) string {
	return strings.ReplaceAll(value, "\n", "\n    ")
}
