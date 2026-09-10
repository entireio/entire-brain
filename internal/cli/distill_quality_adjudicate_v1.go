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
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ashtom/entire-brain/internal/tui"
)

const (
	distillQualityHumanMaxBytesV1        = 16 << 20
	distillQualityAdjudicateDefaultV1    = 20
	distillQualityAdjudicateMaxV1        = 100
	distillQualityAdjudicateQueueAllV1   = "all"
	distillQualityAdjudicateQueueCalV1   = "calibration"
	distillQualityAdjudicateQueueCritV1  = "critical"
	distillQualityAdjudicateQueueInvV1   = "invalid"
	distillQualityAdjudicateQueueDisV1   = "disagreement"
	distillQualityAdjudicateQueueCleanV1 = "clean"
	distillQualityAdjudicateQueueDeltaV1 = "delta"
)

type distillQualityAdjudicateResultV1 struct {
	RunID              string `json:"run_id"`
	AdjudicatorID      string `json:"adjudicator_id"`
	Total              int    `json:"total"`
	AlreadyReviewed    int    `json:"already_reviewed"`
	Presented          int    `json:"presented"`
	Reviewed           int    `json:"reviewed"`
	Remaining          int    `json:"remaining"`
	Quit               bool   `json:"quit"`
	OutputPath         string `json:"output_path"`
	PanelAgreements    int    `json:"panel_agreements,omitempty"`
	PanelDisagreements int    `json:"panel_disagreements,omitempty"`
	DirectAssessments  int    `json:"direct_assessments,omitempty"`
}

type distillQualityAdjudicateItemV1 struct {
	Packet            distillQualityPanelPacketV1
	Aggregate         distillQualityPanelAggregateV1
	Sources           []distillQualityPrivateSourceRecordV1
	CandidateAdmitted bool
	Roles             []string
	Authorities       []string
	Cues              []string
	Focus             []distillQualityReviewTurnV1
	Context           []distillQualityReviewTurnV1
}

type distillQualityReviewTurnV1 struct {
	Role string
	Text string
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

// distillQualityReviewFocusV1 recovers the exact turn the deterministic
// selector judged. Candidate cards contain one trigger-role turn plus, at most,
// adjacent opposite-role context. Filtered packets contain one turn. Refusing
// an ambiguous shape is safer than asking a human to infer the proposition
// from a transcript dump.
func distillQualityReviewFocusV1(packet distillQualityPanelPacketV1, metadata distillQualityAdjudicateMetadataV1) ([]distillQualityReviewTurnV1, []distillQualityReviewTurnV1, error) {
	if metadata.EvidenceTruncated {
		return nil, nil, errors.New("cannot isolate a statement from truncated evidence")
	}
	if len(metadata.Roles) != 1 || (metadata.Roles[0] != "user" && metadata.Roles[0] != "assistant") {
		return nil, nil, errors.New("requires exactly one user or assistant trigger role")
	}
	var evidence strings.Builder
	for _, fragment := range packet.Payload.Fragments {
		evidence.WriteString(fragment.Text)
	}
	turns, err := splitDistillQualityReviewTurnsV1(evidence.String())
	if err != nil {
		return nil, nil, err
	}
	if !metadata.CandidateAdmitted && len(turns) != 1 {
		return nil, nil, errors.New("filtered evidence must contain exactly one statement")
	}
	if metadata.CandidateAdmitted {
		if len(turns) > 3 {
			return nil, nil, errors.New("selected evidence contains too many speaker turns")
		}
		for index := 1; index < len(turns); index++ {
			if turns[index].Role == turns[index-1].Role {
				return nil, nil, errors.New("selected evidence contains adjacent same-role turns")
			}
		}
	}
	trigger := -1
	for index, turn := range turns {
		if turn.Role != metadata.Roles[0] {
			continue
		}
		if trigger >= 0 {
			return nil, nil, fmt.Errorf("trigger role %q appears more than once", metadata.Roles[0])
		}
		trigger = index
	}
	if trigger < 0 {
		return nil, nil, fmt.Errorf("trigger role %q is absent", metadata.Roles[0])
	}
	focusIndexes := map[int]bool{trigger: true}
	if metadata.CandidateAdmitted && stringSliceContainsV1(metadata.Cues, string(distillCandidateCueAcceptanceV1)) {
		if trigger == 0 || turns[trigger-1].Role != "assistant" {
			return nil, nil, errors.New("accepted proposal has no preceding assistant statement")
		}
		focusIndexes[trigger-1] = true
	}
	var focus, context []distillQualityReviewTurnV1
	for index, turn := range turns {
		if focusIndexes[index] {
			focus = append(focus, turn)
		} else {
			context = append(context, turn)
		}
	}
	return focus, context, nil
}

func splitDistillQualityReviewTurnsV1(value string) ([]distillQualityReviewTurnV1, error) {
	type marker struct {
		start, textStart int
		role             string
	}
	var markers []marker
	for start := 0; start < len(value); {
		lineEnd := strings.IndexByte(value[start:], '\n')
		if lineEnd < 0 {
			lineEnd = len(value)
		} else {
			lineEnd += start
		}
		line := value[start:lineEnd]
		for _, role := range []string{"user", "assistant"} {
			prefix := role + ": "
			if strings.HasPrefix(line, prefix) {
				markers = append(markers, marker{start: start, textStart: start + len(prefix), role: role})
				break
			}
		}
		if lineEnd == len(value) {
			break
		}
		start = lineEnd + 1
	}
	if len(markers) == 0 || markers[0].start != 0 {
		return nil, errors.New("evidence does not begin with a recognized speaker")
	}
	turns := make([]distillQualityReviewTurnV1, 0, len(markers))
	for index, current := range markers {
		end := len(value)
		if index+1 < len(markers) {
			end = markers[index+1].start
		}
		text := strings.TrimSpace(value[current.textStart:end])
		if text == "" {
			return nil, fmt.Errorf("%s evidence turn is empty", current.role)
		}
		turns = append(turns, distillQualityReviewTurnV1{Role: current.role, Text: text})
	}
	return turns, nil
}

func stringSliceContainsV1(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// distillQualityAdjudicationSessionV1 owns the immutable review inputs, the
// per-reviewer lock, and the append-only human decisions for one invocation.
// Both the full-screen reviewer and the plain fallback use this boundary so
// they cannot drift on queue selection, validation, persistence, or resume.
type distillQualityAdjudicationSessionV1 struct {
	manifest   distillQualityRunManifestV1
	items      []distillQualityAdjudicateItemV1
	records    []distillQualityHumanAdjudicationV1
	result     distillQualityAdjudicateResultV1
	recordPath string
	lock       *fileLock
}

func (session *distillQualityAdjudicationSessionV1) close() error {
	if session == nil || session.lock == nil {
		return nil
	}
	return session.lock.Close()
}

func (session *distillQualityAdjudicationSessionV1) save(item distillQualityAdjudicateItemV1, admission, authority, safety, rationale string, at time.Time) error {
	record := makeDistillQualityHumanAdjudicationV1(session.manifest.RunID, session.result.AdjudicatorID, item, admission, authority, safety, rationale, at.UTC())
	return session.saveRecord(item, record)
}

// saveFilterAssessment records the single proposition the admission review can
// establish: whether the deterministic filter made the right keep/drop choice.
// It deliberately leaves authority, safety, and fact-quality proof dimensions
// unclaimed; those require different evidence and later adjudication.
func (session *distillQualityAdjudicationSessionV1) saveFilterAssessment(item distillQualityAdjudicateItemV1, filterCorrect bool, rationale string, at time.Time) error {
	record := makeDistillQualityHumanFilterAssessmentV1(session.manifest.RunID, session.result.AdjudicatorID, item, filterCorrect, rationale, at.UTC())
	return session.saveRecord(item, record)
}

func (session *distillQualityAdjudicationSessionV1) saveRecord(item distillQualityAdjudicateItemV1, record distillQualityHumanAdjudicationV1) error {
	if err := validateDistillQualityHumanAdjudicationV1(item.Packet, item.Aggregate, record); err != nil {
		return err
	}
	for _, existing := range session.records {
		if existing.PacketID == record.PacketID {
			return fmt.Errorf("quality adjudication: packet %q was already reviewed", record.PacketID)
		}
	}
	next := append([]distillQualityHumanAdjudicationV1(nil), session.records...)
	next = append(next, record)
	if err := saveDistillQualityHumanAdjudicationsV1(session.recordPath, next); err != nil {
		return err
	}
	session.records = next
	session.result.Reviewed++
	session.result.Remaining--
	return nil
}

func newFactsDistillQualityAdjudicateCommand(opts Options) *cobra.Command {
	var bundleDir, adjudicatorID, queue, themeName string
	var limit int
	var fullEvidence, plain bool
	cmd := &cobra.Command{
		Use:   "adjudicate",
		Short: "Compare filter decisions with a readable advisory panel",
		Long: `adjudicate opens a full-screen human review workspace over a bounded
Phase 2 quality batch. Each screen shows one statement, the deterministic
filter decision, and each available Copilot/Cursor/Claude decision with its
reason. A single agree/disagree answer at the bottom is saved immediately; a
later invocation resumes without repeating it. Unavailable judges are omitted.

On a non-TTY, or with --plain, it uses the deterministic line-oriented fallback.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			theme, err := resolveDashTheme(themeName)
			if err != nil {
				return err
			}
			var result distillQualityAdjudicateResultV1
			if !plain && commandInputIsTTY(cmd) && commandOutputIsTTY(cmd) {
				result, err = runDistillQualityAdjudicationTUIV1(bundleDir, adjudicatorID, queue, limit, cmd.InOrStdin(), cmd.OutOrStdout(), theme, opts.Now)
			} else {
				result, err = runDistillQualityAdjudicationV1(bundleDir, adjudicatorID, queue, limit, fullEvidence, cmd.InOrStdin(), cmd.OutOrStdout(), opts.Now)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nSession complete: %d decisions saved from %d presented; %d of %d packets remain.\n", result.Reviewed, result.Presented, result.Remaining, result.Total)
			if result.PanelAgreements+result.PanelDisagreements+result.DirectAssessments > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Panel comparison: %d agreed, %d flagged for disagreement follow-up, %d decided directly because the judges had no majority.\n", result.PanelAgreements, result.PanelDisagreements, result.DirectAssessments)
			}
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
	cmd.Flags().StringVar(&queue, "queue", distillQualityAdjudicateQueueCalV1, "Queue: calibration, delta, all, critical, invalid, disagreement, or clean")
	cmd.Flags().IntVar(&limit, "limit", distillQualityAdjudicateDefaultV1, "Maximum items presented in this session (1..100)")
	cmd.Flags().StringVar(&themeName, "theme", "", "Color theme: "+strings.Join(tui.ThemeNames(), ", ")+" (or set ENTIRE_BRAIN_THEME)")
	cmd.Flags().BoolVar(&plain, "plain", false, "Use the line-oriented reviewer instead of the full-screen TUI")
	cmd.Flags().BoolVar(&fullEvidence, "full-evidence", false, "In --plain mode, show surrounding context and local source details")
	_ = cmd.MarkFlagRequired("bundle")
	_ = cmd.MarkFlagRequired("adjudicator")
	return cmd
}

func runDistillQualityAdjudicationV1(bundleDir, adjudicatorID, queue string, limit int, fullEvidence bool, input io.Reader, output io.Writer, now func() time.Time) (distillQualityAdjudicateResultV1, error) {
	var result distillQualityAdjudicateResultV1
	if input == nil || output == nil {
		return result, errors.New("quality adjudication: input and output are required")
	}
	if now == nil {
		now = time.Now
	}
	session, err := openDistillQualityAdjudicationSessionV1(bundleDir, adjudicatorID, queue, limit)
	if err != nil {
		return result, err
	}
	defer func() { _ = session.close() }()
	result = session.result
	items := session.items
	if len(items) == 0 {
		fmt.Fprintln(output, "No unreviewed packets match this queue.")
		return result, nil
	}
	fmt.Fprintf(output, "Reviewing at most %d of %d unreviewed packets. Each answer is saved immediately; rerun the same command to resume.\n", len(items), result.Remaining)
	reader := bufio.NewReader(input)
	for index, item := range items {
		session.result.Presented++
		renderDistillQualityAdjudicateItemV1(output, item, index+1, len(items), fullEvidence)
		var choice string
		skip := false
		for {
			var action string
			choice, action, err = readDistillQualityAdmissionChoiceV1(reader, output)
			if err != nil {
				return session.result, err
			}
			switch action {
			case "quit":
				session.result.Quit = true
			case "skip":
				skip = true
			case "full":
				renderDistillQualityReviewEvidenceV1(output, item, true)
				continue
			}
			break
		}
		if session.result.Quit {
			break
		}
		if skip {
			continue
		}
		authority, quit, err := readDistillQualityTernaryChoiceV1(reader, output, "Did the user say this themselves, or explicitly approve it? [y/n/u/q]: ")
		if err != nil {
			return session.result, err
		}
		if quit {
			session.result.Quit = true
			break
		}
		safety, quit, err := readDistillQualityTernaryChoiceV1(reader, output, "Is this safe evidence—not one-off, pasted/generated, or system/tool/review text? [y/n/u/q]: ")
		if err != nil {
			return session.result, err
		}
		if quit {
			session.result.Quit = true
			break
		}
		fmt.Fprint(output, "Optional rationale (one line; Enter to skip): ")
		rationale, _, err := readDistillQualityInputLineV1(reader)
		if err != nil {
			return session.result, err
		}
		if err := session.save(item, choice, authority, safety, rationale, now()); err != nil {
			return session.result, err
		}
		fmt.Fprintf(output, "Saved. Progress: %d/%d reviewed.\n", session.result.Total-session.result.Remaining, session.result.Total)
	}
	return session.result, nil
}

func openDistillQualityAdjudicationSessionV1(bundleDir, adjudicatorID, queue string, limit int) (*distillQualityAdjudicationSessionV1, error) {
	if err := validateDistillQualityPanelOpaqueIDV1("adjudicator_id", adjudicatorID, distillQualityPanelMaxOpaqueIDBytes); err != nil {
		return nil, err
	}
	queue = strings.ToLower(strings.TrimSpace(queue))
	if !validDistillQualityAdjudicateQueueV1(queue) {
		return nil, fmt.Errorf("quality adjudication: invalid queue %q", queue)
	}
	if limit < 1 || limit > distillQualityAdjudicateMaxV1 {
		return nil, fmt.Errorf("quality adjudication: limit must be 1..%d", distillQualityAdjudicateMaxV1)
	}
	manifest, packets, sources, aggregates, err := loadDistillQualityAdjudicationInputsV1(bundleDir)
	if err != nil {
		return nil, err
	}
	bundleDir, err = validateDistillQualityBundleDirV1(bundleDir)
	if err != nil {
		return nil, err
	}
	humanDir := filepath.Join(bundleDir, distillQualityHumanDirV1)
	if err := ensureDistillQualityPanelDirV1(humanDir); err != nil {
		return nil, err
	}
	lock, err := acquireFileLock(filepath.Join(humanDir, ".locks", adjudicatorID+".lock"), "quality_adjudication_locked", 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*distillQualityAdjudicationSessionV1, error) {
		_ = lock.Close()
		return nil, err
	}
	recordPath := filepath.Join(humanDir, adjudicatorID+".jsonl")
	records, err := loadDistillQualityHumanAdjudicationsV1(bundleDir, adjudicatorID, packets, aggregates)
	if err != nil {
		return fail(err)
	}
	byRecordPacket := make(map[string]bool, len(records))
	for _, record := range records {
		byRecordPacket[record.PacketID] = true
	}
	items, err := buildDistillQualityAdjudicateItemsV1(packets, sources, aggregates, byRecordPacket)
	if err != nil {
		return fail(err)
	}
	if queue == distillQualityAdjudicateQueueDeltaV1 {
		metadata, err := loadDistillQualityReuseMetadataV1(bundleDir)
		if err != nil {
			return fail(fmt.Errorf("quality adjudication delta queue: %w", err))
		}
		if err := validateDistillQualityReuseTargetV1(metadata, manifest, packets); err != nil {
			return fail(fmt.Errorf("quality adjudication delta queue: %w", err))
		}
		if metadata.TargetPacketCount != len(packets) {
			return fail(errors.New("quality adjudication delta queue: reuse metadata packet count mismatch"))
		}
		knownPackets := make(map[string]bool, len(packets))
		for _, packet := range packets {
			knownPackets[packet.PacketID] = true
		}
		for _, id := range append(append([]string(nil), metadata.ReusedPacketIDs...), metadata.DeltaPacketIDs...) {
			if !knownPackets[id] {
				return fail(fmt.Errorf("quality adjudication delta queue: metadata references unknown packet %q", id))
			}
		}
		delta := make(map[string]bool, len(metadata.DeltaPacketIDs))
		for _, id := range metadata.DeltaPacketIDs {
			delta[id] = true
		}
		filtered := items[:0]
		for _, item := range items {
			if delta[item.Packet.PacketID] {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	items = selectDistillQualityAdjudicateQueueV1(items, queue, limit)
	result := distillQualityAdjudicateResultV1{
		RunID: manifest.RunID, AdjudicatorID: adjudicatorID, Total: len(packets),
		AlreadyReviewed: len(records), Remaining: len(packets) - len(records), OutputPath: recordPath,
	}
	return &distillQualityAdjudicationSessionV1{
		manifest: manifest, items: items, records: records, result: result,
		recordPath: recordPath, lock: lock,
	}, nil
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
		focus, context, err := distillQualityReviewFocusV1(packet, metadata)
		if err != nil {
			return nil, fmt.Errorf("quality adjudication packet %q focus: %w", packet.PacketID, err)
		}
		items = append(items, distillQualityAdjudicateItemV1{
			Packet: packet, Aggregate: aggregates[packet.PacketID], Sources: byPublicID[packet.Item.ID],
			CandidateAdmitted: metadata.CandidateAdmitted,
			Roles:             append([]string(nil), metadata.Roles...), Authorities: append([]string(nil), metadata.Authorities...),
			Cues: append([]string(nil), metadata.Cues...), Focus: focus, Context: context,
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
			queue == distillQualityAdjudicateQueueDeltaV1 ||
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
	return queue == distillQualityAdjudicateQueueCalV1 || queue == distillQualityAdjudicateQueueAllV1 || queue == distillQualityAdjudicateQueueCritV1 || queue == distillQualityAdjudicateQueueInvV1 || queue == distillQualityAdjudicateQueueDisV1 || queue == distillQualityAdjudicateQueueCleanV1 || queue == distillQualityAdjudicateQueueDeltaV1
}

func renderDistillQualityAdjudicateItemV1(output io.Writer, item distillQualityAdjudicateItemV1, index, total int, fullEvidence bool) {
	decision := "FILTERED BEFORE EXTRACTION"
	if item.CandidateAdmitted {
		decision = "SELECTED FOR EXTRACTION"
	}
	fmt.Fprintf(output, "\n=== Item %d/%d ===\n\nYOUR TASK\nDecide whether the statement below should be remembered for future project work. Do not judge the surrounding conversation.\n\nWHAT THE AUTOMATIC FILTER DID\n%s\n%s\n", index, total, decision, distillQualityTerminalTextV1(distillQualitySelectorReasonV1(item)))
	renderDistillQualityReviewEvidenceV1(output, item, fullEvidence)
	fmt.Fprintln(output, "\nINDEPENDENT MODEL ADVICE (advisory only; no answer is preselected)")
	for _, verdict := range item.Aggregate.Verdicts {
		if verdict.State != distillQualityVerdictCompletedV1 {
			reason := distillQualityTerminalTextV1(distillQualityHumanVerdictNoteV1(verdict.Rationale))
			if strings.TrimSpace(reason) == "" {
				reason = "no usable verdict"
			}
			fmt.Fprintf(output, "%-9s %-11s %s\n", verdict.Judge, strings.ToUpper(string(verdict.State)), reason)
			continue
		}
		labels := map[distillQualityDimensionV1]string{}
		why := ""
		for _, score := range verdict.Scores {
			_, labels[score.Dimension] = distillQualityReviewScoreSummaryV1(score)
			if score.Dimension == distillQualityDimensionAdmissionV1 {
				why = distillQualityHumanJudgeRationaleV1(item.Packet, score.Rationale)
			}
		}
		fmt.Fprintf(output, "%-9s remember: %-7s user endorsed: %-7s safe evidence: %-7s\n", verdict.Judge, emptyDistillQualityAdvisoryV1(labels[distillQualityDimensionAdmissionV1]), emptyDistillQualityAdvisoryV1(labels[distillQualityDimensionAuthorityV1]), emptyDistillQualityAdvisoryV1(labels[distillQualityDimensionSafetyV1]))
		if why != "" {
			fmt.Fprintf(output, "          Why: %s\n", distillQualityTerminalTextV1(why))
		}
	}
	if fullEvidence && len(item.Sources) > 0 {
		source := item.Sources[0]
		fmt.Fprintf(output, "Source: %s:%d-%d (%s", distillQualityTerminalTextV1(filepath.ToSlash(source.TranscriptPath)), source.StartLine, source.EndLine, distillQualityTerminalTextV1(source.Branch))
		if len(item.Sources) > 1 {
			fmt.Fprintf(output, "; +%d more", len(item.Sources)-1)
		}
		fmt.Fprintln(output, ")")
	}
}

func renderDistillQualityReviewEvidenceV1(output io.Writer, item distillQualityAdjudicateItemV1, full bool) {
	fmt.Fprintln(output, "\nSTATEMENT UNDER REVIEW")
	acceptance := stringSliceContainsV1(item.Cues, string(distillCandidateCueAcceptanceV1))
	for _, turn := range item.Focus {
		label := "HUMAN STATEMENT"
		if acceptance && turn.Role == "assistant" {
			label = "ASSISTANT PROPOSAL"
		} else if acceptance && turn.Role == "user" {
			label = "HUMAN CONFIRMATION"
		} else if turn.Role == "assistant" {
			label = "ASSISTANT STATEMENT"
		}
		fmt.Fprintf(output, "%s\n%s\n", label, distillQualityTerminalTextV1(turn.Text))
	}
	if !full {
		if len(item.Context) > 0 {
			fmt.Fprintf(output, "\nSurrounding context hidden (%d turn(s)); enter v if the statement is ambiguous.\n", len(item.Context))
		}
		return
	}
	fmt.Fprintln(output, "\nSURROUNDING CONTEXT (NOT THE STATEMENT UNDER REVIEW)")
	if len(item.Context) == 0 {
		fmt.Fprintln(output, "None.")
		return
	}
	for _, turn := range item.Context {
		fmt.Fprintf(output, "%s CONTEXT\n%s\n", strings.ToUpper(turn.Role), distillQualityTerminalTextV1(turn.Text))
	}
}

func emptyDistillQualityAdvisoryV1(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func readDistillQualityAdmissionChoiceV1(reader *bufio.Reader, output io.Writer) (string, string, error) {
	for {
		fmt.Fprint(output, "Should this be remembered for future project work? [y/n/u/s/v/q]: ")
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

func makeDistillQualityHumanFilterAssessmentV1(runID, adjudicatorID string, item distillQualityAdjudicateItemV1, filterCorrect bool, rationale string, at time.Time) distillQualityHumanAdjudicationV1 {
	shouldAdmit := item.CandidateAdmitted == filterCorrect
	admission := "no"
	decision := distillQualityHumanRejectV1
	if shouldAdmit {
		admission = "yes"
		decision = distillQualityHumanAcceptV1
	}
	identity := distillQualitySHA256V1([]byte(strings.Join([]string{runID, adjudicatorID, item.Packet.PacketID}, "\x00")))
	return distillQualityHumanAdjudicationV1{
		SchemaVersion: distillQualityPanelSchemaVersion,
		RecordID:      "human-v1:" + strings.TrimPrefix(identity, "sha256:"),
		PacketID:      item.Packet.PacketID, PacketDigest: item.Packet.PacketDigest,
		ItemDigest: item.Packet.Item.Digest, PromptDigest: item.Packet.Prompt.Digest, PayloadDigest: item.Packet.Payload.Digest,
		AdvisoryDigest: distillQualityPanelAggregateDigestV1(item.Aggregate), AdjudicatorID: adjudicatorID,
		AdjudicatedAt: at.Truncate(time.Second).Format(time.RFC3339), Decision: decision,
		Dimensions: []distillQualityHumanDimensionV1{{
			Dimension: distillQualityDimensionAdmissionV1, ProofLabel: distillQualityAdmissionProofV1(admission, item.CandidateAdmitted),
		}},
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

func distillQualityTerminalTextV1(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n':
			out.WriteRune(r)
		case r == '\t':
			out.WriteString("    ")
		case unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r):
			out.WriteRune('�')
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
