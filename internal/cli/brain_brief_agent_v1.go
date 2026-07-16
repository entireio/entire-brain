package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const (
	brainBriefAgentV1Marker      = "entire.brain_brief agent_v1"
	brainBriefAgentV1Schema      = 1
	brainBriefAgentV1BudgetBytes = 32 * 1024

	// This canonical description is the identity of the projection policy, not
	// merely a display label. Any change to selected fields, priority, overflow
	// behavior, or the byte budget must change this string and therefore the
	// config hash carried by every packet.
	brainBriefAgentV1ConfigSemantics = "mandatory=task,live,fact,fact_review,proposal_state_unavailable\n" +
		"optional_priority=edit_file,test_file,likely_file,action,symbol,test_suggestion,history\n" +
		"packing=section_prefix_then_lower_priority_residual\n" +
		"overflow=mandatory_error,optional_prefix_drop\n" +
		"trust=locus_drift_marker_from_unfiltered_values,stale_locus_safe_values_only\n" +
		"privacy=omit_structured_generated_at,host_roots,history_source_path,history_timestamp,session,checkpoint,transcript,provenance_anchor;filter_absolute_or_parent_structured_paths;preserve_natural_language_verbatim\n"
)

type brainBriefDeliveryPolicy string

const (
	brainBriefDeliveryAlways brainBriefDeliveryPolicy = "always"
	brainBriefDeliveryShadow brainBriefDeliveryPolicy = "shadow"
)

func (opts brainBriefOptions) resolvedDeliveryPolicy() brainBriefDeliveryPolicy {
	if opts.deliveryPolicy != "" {
		return opts.deliveryPolicy
	}
	return brainBriefDeliveryAlways
}

func brainBriefAgentV1ConfigIdentity(policy brainBriefDeliveryPolicy, requestedLimit, effectiveFactLimit int) string {
	sum := sha256.Sum256([]byte(brainBriefAgentV1ConfigCanonicalFor(policy, requestedLimit, effectiveFactLimit)))
	return fmt.Sprintf("sha256:%x", sum)
}

func brainBriefAgentV1ConfigCanonicalFor(policy brainBriefDeliveryPolicy, requestedLimit, effectiveFactLimit int) string {
	return fmt.Sprintf(
		"format=agent_v1\nschema=%d\ndelivery_policy=%s\nbyte_budget=%d\nrequested_limit=%d\neffective_fact_limit=%d\n",
		brainBriefAgentV1Schema, policy, brainBriefAgentV1BudgetBytes, requestedLimit, effectiveFactLimit,
	) + brainBriefAgentV1ConfigSemantics + "record_schema:\n" + brainBriefAgentV1RecordSchemaDescriptor()
}

type brainBriefAgentV1RecordSchema struct {
	tag    string
	fields []string
}

var brainBriefAgentV1RecordSchemas = []brainBriefAgentV1RecordSchema{
	{tag: "config", fields: []string{"schema", "packet_format", "delivery_policy", "byte_budget", "requested_limit", "effective_fact_limit", "config_sha256"}},
	{tag: "task", fields: []string{"value"}},
	{tag: "live", fields: []string{"branch", "head", "dirty", "changed"}},
	{tag: "fact", fields: []string{"rank", "id", "paths", "kind", "locus", "text", "status", "confidence", "locus_drift", "stale_locus"}},
	{tag: "fact_review", fields: []string{"fact_id", "review_id", "action", "confidence", "message", "related_ids"}},
	{tag: "trust_warning", fields: []string{"code", "message"}},
	{tag: "edit_file", fields: []string{"path"}},
	{tag: "test_file", fields: []string{"path"}},
	{tag: "likely_file", fields: []string{"path"}},
	{tag: "action", fields: []string{"rank", "file", "symbol", "action", "evidence"}},
	{tag: "symbol", fields: []string{"rank", "id", "kind", "name", "file", "start", "end", "signature", "confidence", "reason"}},
	{tag: "test_suggestion", fields: []string{"rank", "id", "kind", "name", "file", "start", "end", "signature", "reason"}},
	{tag: "history", fields: []string{"rank", "score", "matched_terms", "excerpt"}},
	{tag: "end", fields: []string{
		"facts", "fact_reviews", "facts_with_locus_drift", "trust_warnings", "edit_files", "test_files", "likely_files",
		"actions", "symbols", "test_suggestions", "history", "available_facts", "available_fact_reviews",
		"available_trust_warnings", "available_edit_files", "available_test_files", "available_likely_files",
		"available_actions", "available_symbols", "available_test_suggestions", "available_history", "truncated",
		"body_records", "body_bytes", "body_sha256",
	}},
}

func brainBriefAgentV1RecordSchemaDescriptor() string {
	var out strings.Builder
	for _, schema := range brainBriefAgentV1RecordSchemas {
		out.WriteString(schema.tag)
		out.WriteByte('(')
		out.WriteString(strings.Join(schema.fields, ","))
		out.WriteString(")\n")
	}
	return out.String()
}

type brainBriefAgentV1RecordKind uint8

const (
	agentV1Metadata brainBriefAgentV1RecordKind = iota
	agentV1Fact
	agentV1FactReview
	agentV1TrustWarning
	agentV1EditFile
	agentV1TestFile
	agentV1LikelyFile
	agentV1Action
	agentV1Symbol
	agentV1TestSuggestion
	agentV1History
)

type brainBriefAgentV1Record struct {
	kind       brainBriefAgentV1RecordKind
	line       string
	locusDrift bool
}

type brainBriefAgentV1Counts struct {
	facts               int
	factReviews         int
	factsWithLocusDrift int
	trustWarnings       int
	editFiles           int
	testFiles           int
	likelyFiles         int
	actions             int
	symbols             int
	testSuggestions     int
	history             int
}

func (counts *brainBriefAgentV1Counts) add(record brainBriefAgentV1Record) {
	switch record.kind {
	case agentV1Fact:
		counts.facts++
		if record.locusDrift {
			counts.factsWithLocusDrift++
		}
	case agentV1FactReview:
		counts.factReviews++
	case agentV1TrustWarning:
		counts.trustWarnings++
	case agentV1EditFile:
		counts.editFiles++
	case agentV1TestFile:
		counts.testFiles++
	case agentV1LikelyFile:
		counts.likelyFiles++
	case agentV1Action:
		counts.actions++
	case agentV1Symbol:
		counts.symbols++
	case agentV1TestSuggestion:
		counts.testSuggestions++
	case agentV1History:
		counts.history++
	}
}

func (counts brainBriefAgentV1Counts) truncated(available brainBriefAgentV1Counts) bool {
	return counts.facts != available.facts ||
		counts.factReviews != available.factReviews ||
		counts.factsWithLocusDrift != available.factsWithLocusDrift ||
		counts.trustWarnings != available.trustWarnings ||
		counts.editFiles != available.editFiles ||
		counts.testFiles != available.testFiles ||
		counts.likelyFiles != available.likelyFiles ||
		counts.actions != available.actions ||
		counts.symbols != available.symbols ||
		counts.testSuggestions != available.testSuggestions ||
		counts.history != available.history
}

func (counts brainBriefAgentV1Counts) profileCounts() brainBriefProfileCounts {
	return brainBriefProfileCounts{
		SemanticSymbols:     counts.symbols,
		TestSuggestions:     counts.testSuggestions,
		HistoryMatches:      counts.history,
		Facts:               counts.facts,
		FactsWithLocusDrift: counts.factsWithLocusDrift,
		Warnings:            counts.trustWarnings,
		Actions:             counts.actions,
		LikelyEditFiles:     counts.editFiles,
		LikelyTestFiles:     counts.testFiles,
		LikelyFiles:         counts.likelyFiles,
	}
}

type brainBriefAgentV1Projection struct {
	packet string
	counts brainBriefAgentV1Counts
}

type brainBriefAgentV1Emission struct {
	projection brainBriefAgentV1Projection
	shadow     *brainBriefAdmissionShadowArtifact
}

func emitBrainBriefAgentV1(cmd *cobra.Command, report brainBriefReport, policy brainBriefDeliveryPolicy, requestedLimit int) error {
	emission, err := prepareBrainBriefAgentV1(report, policy, requestedLimit)
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), emission.projection.packet)
	return err
}

func prepareBrainBriefAgentV1(report brainBriefReport, policy brainBriefDeliveryPolicy, requestedLimit int) (brainBriefAgentV1Emission, error) {
	return prepareBrainBriefAgentV1WithShadowConfig(report, policy, requestedLimit, defaultBrainBriefAdmissionShadowConfig())
}

func prepareBrainBriefAgentV1WithShadowConfig(
	report brainBriefReport,
	policy brainBriefDeliveryPolicy,
	requestedLimit int,
	shadowConfig brainBriefAdmissionShadowConfig,
) (brainBriefAgentV1Emission, error) {
	if policy == "" {
		policy = brainBriefDeliveryAlways
	}
	if policy != brainBriefDeliveryAlways && policy != brainBriefDeliveryShadow {
		return brainBriefAgentV1Emission{}, fmt.Errorf("agent_v1 delivery policy must be always or shadow: %q", policy)
	}
	if requestedLimit <= 0 {
		requestedLimit = brainBriefDefaultLimit
	}
	emission := brainBriefAgentV1Emission{}
	if policy == brainBriefDeliveryShadow {
		effectiveFactLimit := brainBriefFactsCount(requestedLimit)
		packetConfig := brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, requestedLimit, effectiveFactLimit)
		artifact := evaluateBrainBriefAdmissionShadowSafe(
			report, requestedLimit, effectiveFactLimit, packetConfig, shadowConfig,
		)
		emission.shadow = &artifact
	}
	// Shadow is a control-plane observer, never a packet policy. Both modes use
	// the exact always-bound projection so enabling shadow cannot change bytes.
	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, requestedLimit)
	if err != nil {
		if emission.shadow != nil {
			markBrainBriefAdmissionShadowPacketBuildError(emission.shadow)
		}
		return emission, err
	}
	emission.projection = projection
	return emission, nil
}

func buildBrainBriefAgentV1(report brainBriefReport, policy brainBriefDeliveryPolicy, requestedLimit int) (brainBriefAgentV1Projection, error) {
	if policy == "" {
		policy = brainBriefDeliveryAlways
	}
	if policy != brainBriefDeliveryAlways {
		return brainBriefAgentV1Projection{}, fmt.Errorf("agent_v1 delivery policy must be always: %q", policy)
	}
	if requestedLimit <= 0 {
		requestedLimit = brainBriefDefaultLimit
	}
	effectiveFactLimit := brainBriefFactsCount(requestedLimit)
	if len(report.Facts) > effectiveFactLimit {
		return brainBriefAgentV1Projection{}, fmt.Errorf(
			"agent_v1 received %d facts, exceeds effective fact limit %d", len(report.Facts), effectiveFactLimit,
		)
	}
	if err := validateBrainBriefAgentV1Numbers(report); err != nil {
		return brainBriefAgentV1Projection{}, err
	}

	mandatory := []brainBriefAgentV1Record{
		agentV1Record(agentV1Metadata, "config",
			compactV1IntAlways("schema", brainBriefAgentV1Schema),
			compactV1StringAlways("packet_format", "agent_v1"),
			compactV1StringAlways("delivery_policy", string(policy)),
			compactV1IntAlways("byte_budget", brainBriefAgentV1BudgetBytes),
			compactV1IntAlways("requested_limit", requestedLimit),
			compactV1IntAlways("effective_fact_limit", effectiveFactLimit),
			compactV1StringAlways("config_sha256", brainBriefAgentV1ConfigIdentity(policy, requestedLimit, effectiveFactLimit)),
		),
		agentV1Record(agentV1Metadata, "task", compactV1StringAlways("value", report.Task)),
		agentV1Record(agentV1Metadata, "live",
			compactV1String("branch", report.Status.Live.Branch),
			compactV1String("head", report.Status.Live.Head),
			compactV1Bool("dirty", report.Status.Live.Dirty),
			compactV1Strings("changed", agentV1SafeStructuredValues(report.Status.Live.ChangedFiles)),
		),
	}

	for rank, fact := range report.Facts {
		rawDrift := report.FactsLocusDrift[fact.ID]
		drift := agentV1SafeStructuredValues(rawDrift)
		hasLocusDrift := len(rawDrift) > 0
		mandatory = append(mandatory, brainBriefAgentV1Record{
			kind: agentV1Fact,
			line: agentV1RecordLine("fact",
				compactV1IntAlways("rank", rank),
				compactV1StringAlways("id", fact.ID),
				compactV1Strings("paths", agentV1SafeStructuredValues(fact.Paths)),
				compactV1String("kind", fact.Kind),
				compactV1Strings("locus", agentV1SafeStructuredValues(fact.Locus)),
				compactV1StringAlways("text", fact.Text),
				compactV1String("status", fact.Status),
				compactV1String("confidence", fact.Confidence),
				compactV1Bool("locus_drift", hasLocusDrift),
				compactV1Strings("stale_locus", drift),
			),
			locusDrift: hasLocusDrift,
		})
		if notice, ok := report.FactsPendingReview[fact.ID]; ok {
			mandatory = append(mandatory, agentV1Record(agentV1FactReview, "fact_review",
				compactV1StringAlways("fact_id", fact.ID),
				compactV1StringAlways("review_id", notice.ReviewID),
				compactV1String("action", notice.Action),
				compactV1Float("confidence", notice.Confidence),
				compactV1StringAlways("message", notice.Message),
				compactV1Strings("related_ids", notice.RelatedIDs),
			))
		}
	}
	for _, warning := range report.Warnings {
		if warning == factReviewQueueUnavailableWarning {
			mandatory = append(mandatory, agentV1Record(agentV1TrustWarning, "trust_warning",
				compactV1StringAlways("code", "proposal_state_unavailable"),
				compactV1StringAlways("message", factReviewQueueUnavailableWarning),
			))
		}
	}

	optional := brainBriefAgentV1OptionalSections(report)
	var available brainBriefAgentV1Counts
	for _, record := range mandatory {
		available.add(record)
	}
	for _, section := range optional {
		for _, record := range section {
			available.add(record)
		}
	}

	selected := append([]brainBriefAgentV1Record(nil), mandatory...)
	var emitted brainBriefAgentV1Counts
	for _, record := range selected {
		emitted.add(record)
	}
	mandatoryPacket := finalizeBrainBriefAgentV1(selected, emitted, available)
	if len(mandatoryPacket) > brainBriefAgentV1BudgetBytes {
		return brainBriefAgentV1Projection{}, fmt.Errorf(
			"agent_v1 mandatory packet is %d bytes, exceeds %d-byte budget",
			len(mandatoryPacket), brainBriefAgentV1BudgetBytes,
		)
	}

	// Optional sections are admitted in a frozen priority order. Each section is
	// a prefix: if one complete record cannot fit, later records from that same
	// section are not allowed to leapfrog it. Lower-priority sections may still
	// contribute shorter records.
	for _, section := range optional {
		for _, record := range section {
			candidateRecords := append(append([]brainBriefAgentV1Record(nil), selected...), record)
			candidateCounts := emitted
			candidateCounts.add(record)
			if len(finalizeBrainBriefAgentV1(candidateRecords, candidateCounts, available)) > brainBriefAgentV1BudgetBytes {
				break
			}
			selected = candidateRecords
			emitted = candidateCounts
		}
	}

	packet := finalizeBrainBriefAgentV1(selected, emitted, available)
	if len(packet) > brainBriefAgentV1BudgetBytes {
		return brainBriefAgentV1Projection{}, fmt.Errorf(
			"agent_v1 packet is %d bytes, exceeds %d-byte budget",
			len(packet), brainBriefAgentV1BudgetBytes,
		)
	}
	if err := validateBrainBriefAgentV1Integrity(packet); err != nil {
		return brainBriefAgentV1Projection{}, err
	}
	return brainBriefAgentV1Projection{packet: packet, counts: emitted}, nil
}

func validateBrainBriefAgentV1Numbers(report brainBriefReport) error {
	check := func(path string, value float64) error {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("agent_v1 cannot encode non-finite %s", path)
		}
		return nil
	}
	for _, fact := range report.Facts {
		if notice, ok := report.FactsPendingReview[fact.ID]; ok {
			if err := check(fmt.Sprintf("facts_pending_review[%q].confidence", fact.ID), notice.Confidence); err != nil {
				return err
			}
		}
	}
	for i, symbol := range report.Semantic.Context.Symbols {
		if err := check(fmt.Sprintf("semantic.context.symbols[%d].confidence", i), symbol.Confidence); err != nil {
			return err
		}
	}
	return nil
}

func brainBriefAgentV1OptionalSections(report brainBriefReport) [][]brainBriefAgentV1Record {
	editFiles := make([]brainBriefAgentV1Record, 0, len(report.LikelyEditFiles))
	testFiles := make([]brainBriefAgentV1Record, 0, len(report.LikelyTestFiles))
	likelyFiles := make([]brainBriefAgentV1Record, 0, len(report.LikelyFiles))
	seenLikely := make(map[string]struct{}, len(report.LikelyEditFiles)+len(report.LikelyTestFiles))
	for _, path := range report.LikelyEditFiles {
		if !agentV1SafeStructuredValue(path) {
			continue
		}
		editFiles = append(editFiles, agentV1Record(agentV1EditFile, "edit_file", compactV1StringAlways("path", path)))
		seenLikely[path] = struct{}{}
	}
	for _, path := range report.LikelyTestFiles {
		if !agentV1SafeStructuredValue(path) {
			continue
		}
		testFiles = append(testFiles, agentV1Record(agentV1TestFile, "test_file", compactV1StringAlways("path", path)))
		seenLikely[path] = struct{}{}
	}
	for _, path := range report.LikelyFiles {
		if !agentV1SafeStructuredValue(path) {
			continue
		}
		if _, seen := seenLikely[path]; seen {
			continue
		}
		likelyFiles = append(likelyFiles, agentV1Record(agentV1LikelyFile, "likely_file", compactV1StringAlways("path", path)))
	}

	actions := make([]brainBriefAgentV1Record, 0, len(report.ActionChecklist))
	for rank, action := range report.ActionChecklist {
		actions = append(actions, agentV1Record(agentV1Action, "action",
			compactV1IntAlways("rank", rank),
			compactV1String("file", agentV1SafeStructuredField(action.File)),
			compactV1String("symbol", agentV1SafeStructuredField(action.Symbol)),
			compactV1StringAlways("action", action.Action),
			compactV1String("evidence", action.Evidence),
		))
	}

	symbols := make([]brainBriefAgentV1Record, 0, len(report.Semantic.Context.Symbols))
	for rank, symbol := range report.Semantic.Context.Symbols {
		symbols = append(symbols, agentV1Record(agentV1Symbol, "symbol",
			compactV1IntAlways("rank", rank),
			compactV1String("id", symbol.ID),
			compactV1String("kind", symbol.Kind),
			compactV1String("name", displaySymbolName(symbol)),
			compactV1String("file", agentV1SafeStructuredField(symbol.FilePath)),
			compactV1Int("start", symbol.StartLine),
			compactV1Int("end", symbol.EndLine),
			compactV1String("signature", symbol.Signature),
			compactV1Float("confidence", symbol.Confidence),
			compactV1String("reason", symbol.Reason),
		))
	}

	testSuggestions := make([]brainBriefAgentV1Record, 0, len(report.Semantic.Tests.Suggestions))
	for rank, suggestion := range report.Semantic.Tests.Suggestions {
		symbol := suggestion.Symbol
		testSuggestions = append(testSuggestions, agentV1Record(agentV1TestSuggestion, "test_suggestion",
			compactV1IntAlways("rank", rank),
			compactV1String("id", symbol.ID),
			compactV1String("kind", symbol.Kind),
			compactV1String("name", displaySymbolName(symbol)),
			compactV1String("file", agentV1SafeStructuredField(symbol.FilePath)),
			compactV1Int("start", symbol.StartLine),
			compactV1Int("end", symbol.EndLine),
			compactV1String("signature", symbol.Signature),
			compactV1StringAlways("reason", suggestion.Reason),
		))
	}

	history := make([]brainBriefAgentV1Record, 0, len(report.History.Matches))
	for rank, match := range report.History.Matches {
		// Path, line, and timestamp identify the backing transcript rather than
		// helping the coding agent reason. Rank plus exact excerpt preserves the
		// retrieval order and natural-language evidence without that provenance.
		history = append(history, agentV1Record(agentV1History, "history",
			compactV1IntAlways("rank", rank),
			compactV1Int("score", match.Score),
			compactV1Strings("matched_terms", match.MatchedTerms),
			compactV1StringAlways("excerpt", match.Excerpt),
		))
	}

	return [][]brainBriefAgentV1Record{
		editFiles,
		testFiles,
		likelyFiles,
		actions,
		symbols,
		testSuggestions,
		history,
	}
}

func finalizeBrainBriefAgentV1(records []brainBriefAgentV1Record, emitted, available brainBriefAgentV1Counts) string {
	var body strings.Builder
	body.WriteString(brainBriefAgentV1Marker)
	body.WriteByte('\n')
	for _, record := range records {
		body.WriteString(record.line)
	}
	bodyText := body.String()
	bodyHash := sha256.Sum256([]byte(bodyText))
	end := agentV1RecordLine("end",
		compactV1IntAlways("facts", emitted.facts),
		compactV1IntAlways("fact_reviews", emitted.factReviews),
		compactV1IntAlways("facts_with_locus_drift", emitted.factsWithLocusDrift),
		compactV1IntAlways("trust_warnings", emitted.trustWarnings),
		compactV1IntAlways("edit_files", emitted.editFiles),
		compactV1IntAlways("test_files", emitted.testFiles),
		compactV1IntAlways("likely_files", emitted.likelyFiles),
		compactV1IntAlways("actions", emitted.actions),
		compactV1IntAlways("symbols", emitted.symbols),
		compactV1IntAlways("test_suggestions", emitted.testSuggestions),
		compactV1IntAlways("history", emitted.history),
		compactV1IntAlways("available_facts", available.facts),
		compactV1IntAlways("available_fact_reviews", available.factReviews),
		compactV1IntAlways("available_trust_warnings", available.trustWarnings),
		compactV1IntAlways("available_edit_files", available.editFiles),
		compactV1IntAlways("available_test_files", available.testFiles),
		compactV1IntAlways("available_likely_files", available.likelyFiles),
		compactV1IntAlways("available_actions", available.actions),
		compactV1IntAlways("available_symbols", available.symbols),
		compactV1IntAlways("available_test_suggestions", available.testSuggestions),
		compactV1IntAlways("available_history", available.history),
		compactV1Bool("truncated", emitted.truncated(available)),
		compactV1IntAlways("body_records", len(records)),
		compactV1IntAlways("body_bytes", len(bodyText)),
		compactV1StringAlways("body_sha256", fmt.Sprintf("sha256:%x", bodyHash)),
	)
	return bodyText + end
}

func agentV1Record(kind brainBriefAgentV1RecordKind, tag string, fields ...string) brainBriefAgentV1Record {
	return brainBriefAgentV1Record{kind: kind, line: agentV1RecordLine(tag, fields...)}
}

func agentV1RecordLine(tag string, fields ...string) string {
	var out strings.Builder
	compactV1Record(&out, tag, fields...)
	return out.String()
}

func validateBrainBriefAgentV1PacketSchema(packet string) error {
	if !strings.HasSuffix(packet, "\n") {
		return fmt.Errorf("agent_v1 packet has no final newline")
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	if len(lines) < 2 || lines[0] != brainBriefAgentV1Marker {
		return fmt.Errorf("agent_v1 marker mismatch")
	}
	schemas := make(map[string]brainBriefAgentV1RecordSchema, len(brainBriefAgentV1RecordSchemas))
	for _, schema := range brainBriefAgentV1RecordSchemas {
		if _, duplicate := schemas[schema.tag]; duplicate {
			return fmt.Errorf("agent_v1 duplicate record schema %q", schema.tag)
		}
		schemas[schema.tag] = schema
	}
	for _, line := range lines[1:] {
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			return fmt.Errorf("agent_v1 record: %w", err)
		}
		schema, ok := schemas[record.tag]
		if !ok {
			return fmt.Errorf("agent_v1 has no schema for record %q", record.tag)
		}
		positions := make(map[string]int, len(schema.fields))
		for position, field := range schema.fields {
			positions[field] = position
		}
		last := -1
		for _, field := range record.fields {
			position, ok := positions[field.key]
			if !ok {
				return fmt.Errorf("agent_v1 %s has unknown field %q", record.tag, field.key)
			}
			if position <= last {
				return fmt.Errorf("agent_v1 %s fields are not in schema order", record.tag)
			}
			last = position
		}
	}
	return nil
}

func validateBrainBriefAgentV1Integrity(packet string) error {
	if len(packet) > brainBriefAgentV1BudgetBytes {
		return fmt.Errorf("agent_v1 packet is %d bytes, exceeds %d-byte budget", len(packet), brainBriefAgentV1BudgetBytes)
	}
	if err := validateBrainBriefAgentV1PacketSchema(packet); err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	records := make([]compactV1RawRecord, 0, len(lines)-1)
	for _, line := range lines[1:] {
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			return err
		}
		records = append(records, record)
	}
	if len(records) < 2 || records[0].tag != "config" || records[len(records)-1].tag != "end" {
		return fmt.Errorf("agent_v1 config or end record missing")
	}
	end := records[len(records)-1]
	body := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	bodyRecords, err := agentV1RecordInt(end, "body_records")
	if err != nil {
		return err
	}
	if bodyRecords != len(records)-1 {
		return fmt.Errorf("agent_v1 body_records = %d, want %d", bodyRecords, len(records)-1)
	}
	bodyBytes, err := agentV1RecordInt(end, "body_bytes")
	if err != nil {
		return err
	}
	if bodyBytes != len(body) {
		return fmt.Errorf("agent_v1 body_bytes = %d, want %d", bodyBytes, len(body))
	}
	bodyHash, err := agentV1RecordString(end, "body_sha256")
	if err != nil {
		return err
	}
	wantBodyHash := sha256.Sum256([]byte(body))
	if bodyHash != fmt.Sprintf("sha256:%x", wantBodyHash) {
		return fmt.Errorf("agent_v1 body_sha256 mismatch")
	}

	config := records[0]
	format, err := agentV1RecordString(config, "packet_format")
	if err != nil || format != "agent_v1" {
		return fmt.Errorf("agent_v1 config packet_format mismatch")
	}
	policyString, err := agentV1RecordString(config, "delivery_policy")
	if err != nil {
		return err
	}
	policy := brainBriefDeliveryPolicy(policyString)
	if policy != brainBriefDeliveryAlways {
		return fmt.Errorf("agent_v1 config delivery policy must be always")
	}
	budget, err := agentV1RecordInt(config, "byte_budget")
	if err != nil || budget != brainBriefAgentV1BudgetBytes {
		return fmt.Errorf("agent_v1 config byte budget mismatch")
	}
	requestedLimit, err := agentV1RecordInt(config, "requested_limit")
	if err != nil {
		return err
	}
	effectiveFactLimit, err := agentV1RecordInt(config, "effective_fact_limit")
	if err != nil {
		return err
	}
	configHash, err := agentV1RecordString(config, "config_sha256")
	if err != nil {
		return err
	}
	if want := brainBriefAgentV1ConfigIdentity(policy, requestedLimit, effectiveFactLimit); configHash != want {
		return fmt.Errorf("agent_v1 config_sha256 mismatch")
	}
	return nil
}

func agentV1RecordField(record compactV1RawRecord, key string) (string, error) {
	for _, field := range record.fields {
		if field.key == key {
			return field.value, nil
		}
	}
	return "", fmt.Errorf("agent_v1 %s.%s missing", record.tag, key)
}

func agentV1RecordString(record compactV1RawRecord, key string) (string, error) {
	raw, err := agentV1RecordField(record, key)
	if err != nil {
		return "", err
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return "", fmt.Errorf("agent_v1 %s.%s: %w", record.tag, key, err)
	}
	return value, nil
}

func agentV1RecordInt(record compactV1RawRecord, key string) (int, error) {
	raw, err := agentV1RecordField(record, key)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("agent_v1 %s.%s: %w", record.tag, key, err)
	}
	return value, nil
}

func agentV1SafeStructuredField(value string) string {
	if !agentV1SafeStructuredValue(value) {
		return ""
	}
	return value
}

func agentV1SafeStructuredValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if agentV1SafeStructuredValue(value) {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// agentV1SafeStructuredValue accepts repository-relative paths and symbol-like
// loci while rejecting every common host-absolute or parent-traversal shape.
// Natural-language fields are deliberately not passed through this filter:
// task/fact/history prose must remain exact and is documented as unredacted.
func agentV1SafeStructuredValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	normalized := strings.ReplaceAll(trimmed, `\`, "/")
	lower := strings.ToLower(normalized)
	if strings.HasPrefix(normalized, "/") || normalized == "~" || strings.HasPrefix(normalized, "~/") ||
		strings.HasPrefix(normalized, "$HOME/") || strings.HasPrefix(normalized, "${HOME}/") ||
		strings.HasPrefix(lower, "file:") || strings.Contains(normalized, "://") {
		return false
	}
	if len(normalized) >= 2 && normalized[1] == ':' &&
		((normalized[0] >= 'A' && normalized[0] <= 'Z') || (normalized[0] >= 'a' && normalized[0] <= 'z')) {
		return false
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
