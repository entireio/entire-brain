package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const (
	distillQualityHumanMaxBytesV1        = 16 << 20
	distillQualityAdjudicateDefaultV1    = 20
	distillQualityAdjudicateMaxV1        = 100
	distillQualityAdjudicateExcerptV1    = 2400
	distillQualityAdjudicateQueueAllV1   = "all"
	distillQualityAdjudicateQueueCalV1   = "calibration"
	distillQualityAdjudicateQueueCritV1  = "critical"
	distillQualityAdjudicateQueueInvV1   = "invalid"
	distillQualityAdjudicateQueueDisV1   = "disagreement"
	distillQualityAdjudicateQueueCleanV1 = "clean"
)

type distillQualityAdjudicateResultV1 struct {
	RunID           string `json:"run_id"`
	AdjudicatorID   string `json:"adjudicator_id"`
	Total           int    `json:"total"`
	AlreadyReviewed int    `json:"already_reviewed"`
	Presented       int    `json:"presented"`
	Reviewed        int    `json:"reviewed"`
	Remaining       int    `json:"remaining"`
	Quit            bool   `json:"quit"`
	OutputPath      string `json:"output_path"`
}

type distillQualityAdjudicateItemV1 struct {
	Packet            distillQualityPanelPacketV1
	Aggregate         distillQualityPanelAggregateV1
	Sources           []distillQualityPrivateSourceRecordV1
	CandidateAdmitted bool
	Stratum           string
}

type distillQualityAdjudicateMetadataV1 struct {
	CandidateAdmitted bool     `json:"candidate_admitted"`
	Stratum           string   `json:"stratum"`
	Roles             []string `json:"roles"`
	Authorities       []string `json:"authorities"`
	Cues              []string `json:"cues"`
	EvidenceBytes     int      `json:"evidence_bytes"`
	EvidenceTruncated bool     `json:"evidence_truncated"`
}

func newFactsDistillQualityAdjudicateCommand(opts Options) *cobra.Command {
	var bundleDir, adjudicatorID, queue string
	var limit int
	var fullEvidence bool
	cmd := &cobra.Command{
		Use:   "adjudicate",
		Short: "Review a small resumable batch of advisory packets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := runDistillQualityAdjudicationV1(bundleDir, adjudicatorID, queue, limit, fullEvidence, cmd.InOrStdin(), cmd.OutOrStdout(), opts.Now)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nSession complete: %d decisions saved from %d presented; %d of %d packets remain.\n", result.Reviewed, result.Presented, result.Remaining, result.Total)
			if result.AlreadyReviewed+result.Reviewed == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "The first decision will create: %s\n", result.OutputPath)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Adjudications: %s\n", result.OutputPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&bundleDir, "bundle", "", "Prepared Phase 2 quality bundle directory (required)")
	cmd.Flags().StringVar(&adjudicatorID, "adjudicator", "", "Stable human reviewer id (required)")
	cmd.Flags().StringVar(&queue, "queue", distillQualityAdjudicateQueueCalV1, "Queue: calibration, all, critical, invalid, disagreement, or clean")
	cmd.Flags().IntVar(&limit, "limit", distillQualityAdjudicateDefaultV1, "Maximum items presented in this session (1..100)")
	cmd.Flags().BoolVar(&fullEvidence, "full-evidence", false, "Show complete evidence immediately instead of a head/tail preview")
	_ = cmd.MarkFlagRequired("bundle")
	_ = cmd.MarkFlagRequired("adjudicator")
	return cmd
}

func runDistillQualityAdjudicationV1(bundleDir, adjudicatorID, queue string, limit int, fullEvidence bool, input io.Reader, output io.Writer, now func() time.Time) (distillQualityAdjudicateResultV1, error) {
	var result distillQualityAdjudicateResultV1
	if err := validateDistillQualityPanelOpaqueIDV1("adjudicator_id", adjudicatorID, distillQualityPanelMaxOpaqueIDBytes); err != nil {
		return result, err
	}
	queue = strings.ToLower(strings.TrimSpace(queue))
	if !validDistillQualityAdjudicateQueueV1(queue) {
		return result, fmt.Errorf("quality adjudication: invalid queue %q", queue)
	}
	if limit < 1 || limit > distillQualityAdjudicateMaxV1 {
		return result, fmt.Errorf("quality adjudication: limit must be 1..%d", distillQualityAdjudicateMaxV1)
	}
	if input == nil || output == nil {
		return result, errors.New("quality adjudication: input and output are required")
	}
	if now == nil {
		now = time.Now
	}
	manifest, packets, sources, aggregates, err := loadDistillQualityAdjudicationInputsV1(bundleDir)
	if err != nil {
		return result, err
	}
	bundleDir, err = validateDistillQualityBundleDirV1(bundleDir)
	if err != nil {
		return result, err
	}
	humanDir := filepath.Join(bundleDir, distillQualityHumanDirV1)
	if err := ensureDistillQualityPanelDirV1(humanDir); err != nil {
		return result, err
	}
	lock, err := acquireFileLock(filepath.Join(humanDir, ".locks", adjudicatorID+".lock"), "quality_adjudication_locked", 0)
	if err != nil {
		return result, err
	}
	defer func() { _ = lock.Close() }()
	recordPath := filepath.Join(humanDir, adjudicatorID+".jsonl")
	records, err := loadDistillQualityHumanAdjudicationsV1(bundleDir, adjudicatorID, packets, aggregates)
	if err != nil {
		return result, err
	}
	byRecordPacket := make(map[string]bool, len(records))
	for _, record := range records {
		byRecordPacket[record.PacketID] = true
	}
	items, err := buildDistillQualityAdjudicateItemsV1(packets, sources, aggregates, byRecordPacket)
	if err != nil {
		return result, err
	}
	items = selectDistillQualityAdjudicateQueueV1(items, queue, limit)
	result = distillQualityAdjudicateResultV1{
		RunID: manifest.RunID, AdjudicatorID: adjudicatorID, Total: len(packets),
		AlreadyReviewed: len(records), Remaining: len(packets) - len(records), OutputPath: recordPath,
	}
	if len(items) == 0 {
		fmt.Fprintln(output, "No unreviewed packets match this queue.")
		return result, nil
	}
	fmt.Fprintf(output, "Reviewing at most %d of %d unreviewed packets. Each answer is saved immediately; rerun the same command to resume.\n", len(items), result.Remaining)
	reader := bufio.NewReader(input)
	for index, item := range items {
		result.Presented++
		renderDistillQualityAdjudicateItemV1(output, item, index+1, len(items), fullEvidence)
		var choice string
		skip := false
		for {
			var action string
			choice, action, err = readDistillQualityAdmissionChoiceV1(reader, output)
			if err != nil {
				return result, err
			}
			switch action {
			case "quit":
				result.Quit = true
			case "skip":
				skip = true
			case "full":
				renderDistillQualityEvidenceV1(output, item.Packet, true)
				continue
			}
			break
		}
		if result.Quit {
			break
		}
		if skip {
			continue
		}
		authority, quit, err := readDistillQualityTernaryChoiceV1(reader, output, "Adequate direct-human or corroborated authority? [y/n/u/q]: ")
		if err != nil {
			return result, err
		}
		if quit {
			result.Quit = true
			break
		}
		safety, quit, err := readDistillQualityTernaryChoiceV1(reader, output, "Safe to admit (not injection, pasted instruction, one-off task, or review prose)? [y/n/u/q]: ")
		if err != nil {
			return result, err
		}
		if quit {
			result.Quit = true
			break
		}
		fmt.Fprint(output, "Optional rationale (one line; Enter to skip): ")
		rationale, _, err := readDistillQualityInputLineV1(reader)
		if err != nil {
			return result, err
		}
		record := makeDistillQualityHumanAdjudicationV1(manifest.RunID, adjudicatorID, item, choice, authority, safety, rationale, now().UTC())
		if err := validateDistillQualityHumanAdjudicationV1(item.Packet, item.Aggregate, record); err != nil {
			return result, err
		}
		records = append(records, record)
		if err := saveDistillQualityHumanAdjudicationsV1(recordPath, records); err != nil {
			return result, err
		}
		result.Reviewed++
		result.Remaining--
		fmt.Fprintf(output, "Saved. Progress: %d/%d reviewed.\n", result.Total-result.Remaining, result.Total)
	}
	return result, nil
}

func loadDistillQualityAdjudicationInputsV1(bundleDir string) (distillQualityRunManifestV1, []distillQualityPanelPacketV1, []distillQualityPrivateSourceRecordV1, map[string]distillQualityPanelAggregateV1, error) {
	manifest, packets, sources, err := loadDistillQualityRunV1(bundleDir)
	if err != nil {
		return manifest, nil, nil, nil, err
	}
	packetByID := make(map[string]distillQualityPanelPacketV1, len(packets))
	verdictsByPacket := make(map[string][]distillQualityPanelVerdictV1, len(packets))
	for _, packet := range packets {
		packetByID[packet.PacketID] = packet
	}
	for _, judge := range distillQualityPanelJudgesV1 {
		verdicts, err := loadDistillQualityJudgeVerdictsV1(bundleDir, judge, packetByID)
		if err != nil {
			return manifest, nil, nil, nil, err
		}
		for _, verdict := range verdicts {
			verdictsByPacket[verdict.PacketID] = append(verdictsByPacket[verdict.PacketID], verdict)
		}
	}
	aggregates := make(map[string]distillQualityPanelAggregateV1, len(packets))
	for _, packet := range packets {
		aggregate, err := aggregateDistillQualityPanelV1(packet, verdictsByPacket[packet.PacketID])
		if err != nil {
			return manifest, nil, nil, nil, err
		}
		aggregates[packet.PacketID] = aggregate
	}
	return manifest, packets, sources, aggregates, nil
}

func loadDistillQualityHumanAdjudicationsV1(bundleDir, adjudicatorID string, packets []distillQualityPanelPacketV1, aggregates map[string]distillQualityPanelAggregateV1) ([]distillQualityHumanAdjudicationV1, error) {
	data, err := readDistillQualityFileV1(filepath.Join(bundleDir, distillQualityHumanDirV1), adjudicatorID+".jsonl", distillQualityHumanMaxBytesV1, false)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	packetByID := make(map[string]distillQualityPanelPacketV1, len(packets))
	for _, packet := range packets {
		packetByID[packet.PacketID] = packet
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), distillQualityPanelMaxJSONBytes)
	seen := map[string]bool{}
	var records []distillQualityHumanAdjudicationV1
	for line := 1; scanner.Scan(); line++ {
		var record distillQualityHumanAdjudicationV1
		if err := decodeDistillQualityStrictJSONV1(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("quality human adjudication line %d: %w", line, err)
		}
		packet, ok := packetByID[record.PacketID]
		if !ok {
			return nil, fmt.Errorf("quality human adjudication line %d references unknown packet", line)
		}
		if record.AdjudicatorID != adjudicatorID || seen[record.PacketID] {
			return nil, fmt.Errorf("quality human adjudication line %d has mismatched reviewer or duplicate packet", line)
		}
		if err := validateDistillQualityHumanAdjudicationV1(packet, aggregates[record.PacketID], record); err != nil {
			return nil, fmt.Errorf("quality human adjudication line %d: %w", line, err)
		}
		seen[record.PacketID] = true
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].PacketID < records[j].PacketID })
	return records, nil
}

func saveDistillQualityHumanAdjudicationsV1(path string, records []distillQualityHumanAdjudicationV1) error {
	sort.Slice(records, func(i, j int) bool { return records[i].PacketID < records[j].PacketID })
	data, err := marshalDistillQualityJSONLV1(records)
	if err != nil {
		return err
	}
	if len(data) > distillQualityHumanMaxBytesV1 {
		return errors.New("quality human adjudications exceed the bounded store")
	}
	return writeFileAtomic(path, data, 0o600)
}

func buildDistillQualityAdjudicateItemsV1(packets []distillQualityPanelPacketV1, sources []distillQualityPrivateSourceRecordV1, aggregates map[string]distillQualityPanelAggregateV1, reviewed map[string]bool) ([]distillQualityAdjudicateItemV1, error) {
	byPublicID := make(map[string][]distillQualityPrivateSourceRecordV1)
	for _, source := range sources {
		byPublicID[source.PublicID] = append(byPublicID[source.PublicID], source)
	}
	var items []distillQualityAdjudicateItemV1
	for _, packet := range packets {
		if reviewed[packet.PacketID] {
			continue
		}
		var metadata distillQualityAdjudicateMetadataV1
		if err := decodeDistillQualityStrictJSONV1([]byte(packet.Item.Text), &metadata); err != nil {
			return nil, fmt.Errorf("quality adjudication packet %q metadata: %w", packet.PacketID, err)
		}
		items = append(items, distillQualityAdjudicateItemV1{
			Packet: packet, Aggregate: aggregates[packet.PacketID], Sources: byPublicID[packet.Item.ID],
			CandidateAdmitted: metadata.CandidateAdmitted, Stratum: metadata.Stratum,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := reportDistillQualityAttentionRankV1(items[i].Aggregate), reportDistillQualityAttentionRankV1(items[j].Aggregate)
		if left != right {
			return left < right
		}
		return items[i].Packet.PacketID < items[j].Packet.PacketID
	})
	return items, nil
}

func selectDistillQualityAdjudicateQueueV1(items []distillQualityAdjudicateItemV1, queue string, limit int) []distillQualityAdjudicateItemV1 {
	if queue == distillQualityAdjudicateQueueCalV1 {
		buckets := make([][]distillQualityAdjudicateItemV1, 5)
		for _, item := range items {
			bucket := 4
			switch {
			case item.Aggregate.Invalid:
				bucket = 0
			case item.Aggregate.Critical && item.CandidateAdmitted:
				bucket = 1
			case item.Aggregate.Critical:
				bucket = 2
			case item.Aggregate.Disagreement:
				bucket = 3
			}
			buckets[bucket] = append(buckets[bucket], item)
		}
		selected := make([]distillQualityAdjudicateItemV1, 0, limit)
		for len(selected) < limit {
			advanced := false
			for index := range buckets {
				if len(buckets[index]) == 0 || len(selected) >= limit {
					continue
				}
				selected = append(selected, buckets[index][0])
				buckets[index] = buckets[index][1:]
				advanced = true
			}
			if !advanced {
				break
			}
		}
		return selected
	}
	selected := make([]distillQualityAdjudicateItemV1, 0, limit)
	for _, item := range items {
		match := queue == distillQualityAdjudicateQueueAllV1 ||
			queue == distillQualityAdjudicateQueueCritV1 && item.Aggregate.Critical ||
			queue == distillQualityAdjudicateQueueInvV1 && item.Aggregate.Invalid ||
			queue == distillQualityAdjudicateQueueDisV1 && item.Aggregate.Disagreement ||
			queue == distillQualityAdjudicateQueueCleanV1 && !item.Aggregate.Critical && !item.Aggregate.Invalid && !item.Aggregate.Disagreement && len(item.Aggregate.MissingJudges) == 0
		if match {
			selected = append(selected, item)
			if len(selected) == limit {
				break
			}
		}
	}
	return selected
}

func validDistillQualityAdjudicateQueueV1(queue string) bool {
	return queue == distillQualityAdjudicateQueueCalV1 || queue == distillQualityAdjudicateQueueAllV1 || queue == distillQualityAdjudicateQueueCritV1 || queue == distillQualityAdjudicateQueueInvV1 || queue == distillQualityAdjudicateQueueDisV1 || queue == distillQualityAdjudicateQueueCleanV1
}

func renderDistillQualityAdjudicateItemV1(output io.Writer, item distillQualityAdjudicateItemV1, index, total int, fullEvidence bool) {
	decision := "FILTERED"
	if item.CandidateAdmitted {
		decision = "ADMITTED"
	}
	fmt.Fprintf(output, "\n=== Item %d/%d · %s ===\nCandidate: %s · Stratum: %s\nAttention: %s\n", index, total, item.Packet.PacketID, decision, item.Stratum, reportDistillQualityAttentionTextV1(item.Aggregate))
	renderDistillQualityEvidenceV1(output, item.Packet, fullEvidence)
	fmt.Fprintln(output, "\nAdvisory scores (input only; you own the decision):")
	fmt.Fprintln(output, "judge     state      admission  authority  safety")
	for _, verdict := range item.Aggregate.Verdicts {
		labels := map[distillQualityDimensionV1]string{}
		for _, score := range verdict.Scores {
			labels[score.Dimension] = string(score.Label)
		}
		fmt.Fprintf(output, "%-9s %-10s %-10s %-10s %-10s\n", verdict.Judge, verdict.State, emptyDistillQualityAdvisoryV1(labels[distillQualityDimensionAdmissionV1]), emptyDistillQualityAdvisoryV1(labels[distillQualityDimensionAuthorityV1]), emptyDistillQualityAdvisoryV1(labels[distillQualityDimensionSafetyV1]))
		if verdict.Rationale != "" {
			fmt.Fprintf(output, "  %s: %s\n", verdict.Judge, verdict.Rationale)
		}
		for _, score := range verdict.Scores {
			fmt.Fprintf(output, "  %s/%s [%s] raw=%s · %s\n", verdict.Judge, score.Dimension, score.Label, reportDistillQualityInlineJSONV1(score.RawScore), score.Rationale)
		}
	}
	if len(item.Sources) > 0 {
		source := item.Sources[0]
		fmt.Fprintf(output, "Source: %s:%d-%d (%s", filepath.ToSlash(source.TranscriptPath), source.StartLine, source.EndLine, source.Branch)
		if len(item.Sources) > 1 {
			fmt.Fprintf(output, "; +%d more", len(item.Sources)-1)
		}
		fmt.Fprintln(output, ")")
	}
}

func renderDistillQualityEvidenceV1(output io.Writer, packet distillQualityPanelPacketV1, full bool) {
	var evidence strings.Builder
	for _, fragment := range packet.Payload.Fragments {
		fmt.Fprintf(&evidence, "[%s]\n%s\n", fragment.ID, fragment.Text)
	}
	value := strings.TrimSpace(evidence.String())
	runes := []rune(value)
	if full || len(runes) <= distillQualityAdjudicateExcerptV1 {
		fmt.Fprintf(output, "\nEvidence:\n%s\n", value)
		return
	}
	half := distillQualityAdjudicateExcerptV1 / 2
	fmt.Fprintf(output, "\nEvidence preview:\n%s\n\n[… %d runes hidden; enter v at the decision prompt to show all …]\n\n%s\n", string(runes[:half]), len(runes)-2*half, string(runes[len(runes)-half:]))
}

func emptyDistillQualityAdvisoryV1(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func readDistillQualityAdmissionChoiceV1(reader *bufio.Reader, output io.Writer) (string, string, error) {
	for {
		fmt.Fprint(output, "Should this evidence be admitted as durable knowledge? [y/n/u/s/v/q]: ")
		value, eof, err := readDistillQualityInputLineV1(reader)
		if err != nil {
			return "", "", err
		}
		switch strings.ToLower(value) {
		case "y", "yes":
			return "yes", "decision", nil
		case "n", "no":
			return "no", "decision", nil
		case "u", "unclear":
			return "unclear", "decision", nil
		case "s", "skip":
			return "", "skip", nil
		case "v", "view":
			return "", "full", nil
		case "q", "quit":
			return "", "quit", nil
		}
		if eof {
			return "", "quit", nil
		}
		fmt.Fprintln(output, "Enter y, n, u, s, v, or q.")
	}
}

func readDistillQualityTernaryChoiceV1(reader *bufio.Reader, output io.Writer, prompt string) (string, bool, error) {
	for {
		fmt.Fprint(output, prompt)
		value, eof, err := readDistillQualityInputLineV1(reader)
		if err != nil {
			return "", false, err
		}
		switch strings.ToLower(value) {
		case "y", "yes":
			return "yes", false, nil
		case "n", "no":
			return "no", false, nil
		case "u", "unclear":
			return "unclear", false, nil
		case "q", "quit":
			return "", true, nil
		}
		if eof {
			return "", true, nil
		}
		fmt.Fprintln(output, "Enter y, n, u, or q.")
	}
}

func readDistillQualityInputLineV1(reader *bufio.Reader) (string, bool, error) {
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return "", false, errors.New("quality adjudication: invalid terminal input")
	}
	return value, errors.Is(err, io.EOF), nil
}

func makeDistillQualityHumanAdjudicationV1(runID, adjudicatorID string, item distillQualityAdjudicateItemV1, admission, authority, safety, rationale string, at time.Time) distillQualityHumanAdjudicationV1 {
	decision := distillQualityHumanNeedsReviewV1
	if admission == "yes" {
		decision = distillQualityHumanAcceptV1
	} else if admission == "no" {
		decision = distillQualityHumanRejectV1
	}
	dimensions := []distillQualityHumanDimensionV1{
		{Dimension: distillQualityDimensionAdmissionV1, ProofLabel: distillQualityAdmissionProofV1(admission, item.CandidateAdmitted)},
		{Dimension: distillQualityDimensionFaithfulnessV1, ProofLabel: distillQualityProofNotApplicableV1},
		{Dimension: distillQualityDimensionAuthorityV1, ProofLabel: distillQualityChoiceProofV1(authority)},
		{Dimension: distillQualityDimensionTaxonomyV1, ProofLabel: distillQualityProofNotApplicableV1},
		{Dimension: distillQualityDimensionLocusV1, ProofLabel: distillQualityProofNotApplicableV1},
		{Dimension: distillQualityDimensionSafetyV1, ProofLabel: distillQualityChoiceProofV1(safety)},
	}
	identity := distillQualitySHA256V1([]byte(strings.Join([]string{runID, adjudicatorID, item.Packet.PacketID}, "\x00")))
	return distillQualityHumanAdjudicationV1{
		SchemaVersion: distillQualityPanelSchemaVersion,
		RecordID:      "human-v1:" + strings.TrimPrefix(identity, "sha256:"),
		PacketID:      item.Packet.PacketID, PacketDigest: item.Packet.PacketDigest,
		ItemDigest: item.Packet.Item.Digest, PromptDigest: item.Packet.Prompt.Digest, PayloadDigest: item.Packet.Payload.Digest,
		AdvisoryDigest: distillQualityPanelAggregateDigestV1(item.Aggregate), AdjudicatorID: adjudicatorID,
		AdjudicatedAt: at.Truncate(time.Second).Format(time.RFC3339), Decision: decision, Dimensions: dimensions,
		Rationale: strings.TrimSpace(rationale),
	}
}

func distillQualityAdmissionProofV1(choice string, candidateAdmitted bool) distillQualityProofLabelV1 {
	if choice == "unclear" {
		return distillQualityProofUncertainV1
	}
	if (choice == "yes") == candidateAdmitted {
		return distillQualityProofSupportedV1
	}
	return distillQualityProofUnsupportedV1
}

func distillQualityChoiceProofV1(choice string) distillQualityProofLabelV1 {
	switch choice {
	case "yes":
		return distillQualityProofSupportedV1
	case "no":
		return distillQualityProofUnsupportedV1
	default:
		return distillQualityProofUncertainV1
	}
}
