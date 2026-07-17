package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const (
	brainBriefAgentV2Marker       = "entire.brain_brief agent_v2"
	brainBriefAgentV2Schema       = 2
	brainBriefAgentV2BudgetBytes  = 32 * 1024
	brainBriefAgentV2SizingSHA256 = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	brainBriefAgentV2MaxRelationTypeBytes = 64
	brainBriefAgentV2MaxResolutionBytes   = 64
	brainBriefAgentV2MaxEdgeIDBytes       = 1024

	// Agent V2 is a separately selected candidate. It retains the exact Agent
	// V1 task/live/fact/trust records and old optional-section order, then admits
	// typed semantic edges ahead of history only. No adaptive admission or default
	// switch is part of this identity.
	brainBriefAgentV2ConfigSemantics = "mandatory=task,live,fact,fact_review,proposal_state_unavailable\n" +
		"optional_priority=edit_file,test_file,likely_file,action,symbol,test_suggestion,semantic_relation,runtime_trace,history\n" +
		"packing=section_prefix_then_lower_priority_residual\n" +
		"overflow=mandatory_error,optional_prefix_drop\n" +
		"trust=locus_drift_marker_from_unfiltered_values,stale_locus_safe_values_only\n" +
		"semantic_edges=typed_relation_and_runtime_trace_with_finite_confidence,relation_type_ascii_upper_identifier_max64,resolution_ascii_identifier_max64,edge_id_bounded_whitespace_free_stable_identifier_max1024,optional_observed_type_from_exact_generated_reason_prefix_using_relation_type_grammar,no_free_form_runtime_reason,no_span_provenance\n" +
		"edge_id_privacy=pct_decode_once,backslash_as_slash,nested_pct_reject,decoded_grammar,ascii_casefold_components,absolute_slash_component_reject,drive=[A-Za-z]:/,home=..|~|$HOME|${HOME},schemes=file|http|https|mailto\n" +
		"privacy=omit_structured_generated_at,host_roots,history_source_path,history_timestamp,session,checkpoint,transcript,provenance_anchor,raw_status_warnings,raw_live_warnings,raw_report_warnings_except_proposal_state_unavailable,free_form_runtime_reason;filter_new_edge_fields_symmetrically_for_paths,credentials,controls,unicode_whitespace,and_invalid_grammar;preserve_existing_v1_natural_language_verbatim\n"
)

const brainBriefAgentV2RuntimeReasonPrefix = "runtime trace observed "

func brainBriefAgentV2ConfigIdentity(policy brainBriefDeliveryPolicy, requestedLimit, effectiveFactLimit int) string {
	sum := sha256.Sum256([]byte(brainBriefAgentV2ConfigCanonicalFor(policy, requestedLimit, effectiveFactLimit)))
	return fmt.Sprintf("sha256:%x", sum)
}

func brainBriefAgentV2ConfigCanonicalFor(policy brainBriefDeliveryPolicy, requestedLimit, effectiveFactLimit int) string {
	return fmt.Sprintf(
		"format=agent_v2\nschema=%d\ndelivery_policy=%s\nbyte_budget=%d\nrequested_limit=%d\neffective_fact_limit=%d\n",
		brainBriefAgentV2Schema, policy, brainBriefAgentV2BudgetBytes, requestedLimit, effectiveFactLimit,
	) + brainBriefAgentV2ConfigSemantics + "record_schema:\n" + brainBriefAgentV2RecordSchemaDescriptor()
}

var brainBriefAgentV2RecordSchemas = []brainBriefAgentV1RecordSchema{
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
	{tag: "semantic_relation", fields: []string{"rank", "type", "from", "to", "resolution", "confidence"}},
	{tag: "runtime_trace", fields: []string{"rank", "type", "from", "to", "observed_type", "confidence"}},
	{tag: "history", fields: []string{"rank", "score", "matched_terms", "excerpt"}},
	{tag: "end", fields: []string{
		"facts", "fact_reviews", "facts_with_locus_drift", "trust_warnings", "edit_files", "test_files", "likely_files",
		"actions", "symbols", "semantic_relations", "runtime_traces", "test_suggestions", "history",
		"available_facts", "available_fact_reviews", "available_facts_with_locus_drift", "available_trust_warnings",
		"available_edit_files", "available_test_files", "available_likely_files", "available_actions", "available_symbols",
		"available_semantic_relations", "available_runtime_traces", "available_test_suggestions", "available_history",
		"truncated", "body_records", "body_bytes", "body_sha256",
	}},
}

func brainBriefAgentV2RecordSchemaDescriptor() string {
	var out strings.Builder
	for _, schema := range brainBriefAgentV2RecordSchemas {
		out.WriteString(schema.tag)
		out.WriteByte('(')
		out.WriteString(strings.Join(schema.fields, ","))
		out.WriteString(")\n")
	}
	return out.String()
}

const (
	agentV2SemanticRelation brainBriefAgentV1RecordKind = agentV1History + 1 + iota
	agentV2RuntimeTrace
)

type brainBriefAgentV2Counts struct {
	brainBriefAgentV1Counts
	semanticRelations int
	runtimeTraces     int
}

func (counts *brainBriefAgentV2Counts) add(record brainBriefAgentV1Record) {
	switch record.kind {
	case agentV2SemanticRelation:
		counts.semanticRelations++
	case agentV2RuntimeTrace:
		counts.runtimeTraces++
	default:
		counts.brainBriefAgentV1Counts.add(record)
	}
}

func (counts brainBriefAgentV2Counts) truncated(available brainBriefAgentV2Counts) bool {
	return counts.brainBriefAgentV1Counts.truncated(available.brainBriefAgentV1Counts) ||
		counts.semanticRelations != available.semanticRelations ||
		counts.runtimeTraces != available.runtimeTraces
}

func (counts brainBriefAgentV2Counts) profileCounts() brainBriefProfileCounts {
	profile := counts.brainBriefAgentV1Counts.profileCounts()
	profile.SemanticRelations = counts.semanticRelations
	profile.RuntimeTraces = counts.runtimeTraces
	return profile
}

type brainBriefAgentV2Projection struct {
	packet string
	counts brainBriefAgentV2Counts
}

func emitBrainBriefAgentV2(cmd *cobra.Command, report brainBriefReport, policy brainBriefDeliveryPolicy, requestedLimit int) error {
	_, err := emitBrainBriefAgentV2WithCounts(cmd, report, policy, requestedLimit)
	return err
}

func emitBrainBriefAgentV2WithCounts(
	cmd *cobra.Command,
	report brainBriefReport,
	policy brainBriefDeliveryPolicy,
	requestedLimit int,
) (brainBriefAgentV2Counts, error) {
	projection, err := buildBrainBriefAgentV2(report, policy, requestedLimit)
	if err != nil {
		return brainBriefAgentV2Counts{}, err
	}
	if _, err := io.WriteString(cmd.OutOrStdout(), projection.packet); err != nil {
		return brainBriefAgentV2Counts{}, err
	}
	return projection.counts, nil
}

func buildBrainBriefAgentV2(report brainBriefReport, policy brainBriefDeliveryPolicy, requestedLimit int) (brainBriefAgentV2Projection, error) {
	if policy == "" {
		policy = brainBriefDeliveryAlways
	}
	if policy != brainBriefDeliveryAlways {
		return brainBriefAgentV2Projection{}, fmt.Errorf("agent_v2 delivery policy must be always: %q", policy)
	}
	if requestedLimit <= 0 {
		requestedLimit = brainBriefDefaultLimit
	}
	effectiveFactLimit := brainBriefFactsCount(requestedLimit)
	if len(report.Facts) > effectiveFactLimit {
		return brainBriefAgentV2Projection{}, fmt.Errorf(
			"agent_v2 received %d facts, exceeds effective fact limit %d", len(report.Facts), effectiveFactLimit,
		)
	}
	if err := validateBrainBriefAgentV2Numbers(report); err != nil {
		return brainBriefAgentV2Projection{}, err
	}

	mandatory := []brainBriefAgentV1Record{
		agentV1Record(agentV1Metadata, "config",
			compactV1IntAlways("schema", brainBriefAgentV2Schema),
			compactV1StringAlways("packet_format", "agent_v2"),
			compactV1StringAlways("delivery_policy", string(policy)),
			compactV1IntAlways("byte_budget", brainBriefAgentV2BudgetBytes),
			compactV1IntAlways("requested_limit", requestedLimit),
			compactV1IntAlways("effective_fact_limit", effectiveFactLimit),
			compactV1StringAlways("config_sha256", brainBriefAgentV2ConfigIdentity(policy, requestedLimit, effectiveFactLimit)),
		),
	}
	mandatory = append(mandatory, brainBriefAgentSharedMandatoryRecords(report)...)
	optional := brainBriefAgentV2OptionalSections(report)
	packet, emitted, err := packBrainBriefAgentPacket(mandatory, optional, brainBriefAgentPacketProfile[brainBriefAgentV2Counts]{
		format: "agent_v2",
		marker: brainBriefAgentV2Marker,
		budget: brainBriefAgentV2BudgetBytes,
		add: func(counts *brainBriefAgentV2Counts, record brainBriefAgentV1Record) {
			counts.add(record)
		},
		sized:    brainBriefAgentV2SizedPacketBytes,
		finalize: finalizeBrainBriefAgentV2Body,
		validate: validateBrainBriefAgentV2Integrity,
	})
	if err != nil {
		return brainBriefAgentV2Projection{}, err
	}
	return brainBriefAgentV2Projection{packet: packet, counts: emitted}, nil
}

func validateBrainBriefAgentV2Numbers(report brainBriefReport) error {
	if err := validateBrainBriefAgentV1Numbers(report); err != nil {
		return fmt.Errorf("agent_v2%s", strings.TrimPrefix(err.Error(), "agent_v1"))
	}
	check := func(path string, value float64) error {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("agent_v2 cannot encode non-finite %s", path)
		}
		return nil
	}
	for i, relation := range report.Semantic.Context.Relations {
		if err := check(fmt.Sprintf("semantic.context.relations[%d].confidence", i), relation.Confidence); err != nil {
			return err
		}
	}
	for i, trace := range report.Semantic.RuntimeTraces {
		if err := check(fmt.Sprintf("semantic.runtime_traces[%d].confidence", i), trace.Confidence); err != nil {
			return err
		}
	}
	return nil
}

func brainBriefAgentV2OptionalSections(report brainBriefReport) [][]brainBriefAgentV1Record {
	v1 := brainBriefAgentV1OptionalSections(report)
	relations := make([]brainBriefAgentV1Record, 0, len(report.Semantic.Context.Relations))
	for rank, relation := range report.Semantic.Context.Relations {
		relations = append(relations, agentV1Record(agentV2SemanticRelation, "semantic_relation",
			compactV1IntAlways("rank", rank),
			compactV1String("type", brainBriefAgentV2SafeRelationType(relation.Type)),
			compactV1String("from", brainBriefAgentV2SafeEdgeID(relation.FromID)),
			compactV1String("to", brainBriefAgentV2SafeEdgeID(relation.ToID)),
			compactV1String("resolution", brainBriefAgentV2SafeResolution(relation.Resolution)),
			compactV1Float("confidence", relation.Confidence),
		))
	}
	runtimeTraces := make([]brainBriefAgentV1Record, 0, len(report.Semantic.RuntimeTraces))
	for rank, trace := range report.Semantic.RuntimeTraces {
		runtimeTraces = append(runtimeTraces, agentV1Record(agentV2RuntimeTrace, "runtime_trace",
			compactV1IntAlways("rank", rank),
			compactV1String("type", brainBriefAgentV2SafeRelationType(trace.Type)),
			compactV1String("from", brainBriefAgentV2SafeEdgeID(trace.FromID)),
			compactV1String("to", brainBriefAgentV2SafeEdgeID(trace.ToID)),
			compactV1String("observed_type", brainBriefAgentV2ObservedType(trace.Reason)),
			compactV1Float("confidence", trace.Confidence),
		))
	}
	return [][]brainBriefAgentV1Record{
		v1[brainBriefAgentOptionalEditFiles],
		v1[brainBriefAgentOptionalTestFiles],
		v1[brainBriefAgentOptionalLikelyFiles],
		v1[brainBriefAgentOptionalActions],
		v1[brainBriefAgentOptionalSymbols],
		v1[brainBriefAgentOptionalTestSuggestions],
		relations,
		runtimeTraces,
		v1[brainBriefAgentOptionalHistory],
	}
}

// Runtime trace reasons are synthesized from caller-controlled observed_type
// input. V2 exposes that value only when the producer-generated prefix is exact,
// then treats the suffix as structured input. Generic fallback and free-form
// prose are omitted rather than becoming a new unredacted release boundary.
func brainBriefAgentV2ObservedType(reason string) string {
	if !strings.HasPrefix(reason, brainBriefAgentV2RuntimeReasonPrefix) {
		return ""
	}
	observedType := strings.TrimPrefix(reason, brainBriefAgentV2RuntimeReasonPrefix)
	if observedType == "" || observedType == "edge" {
		return ""
	}
	return brainBriefAgentV2SafeRelationType(observedType)
}

func brainBriefAgentV2SafeRelationType(value string) string {
	if len(value) == 0 || len(value) > brainBriefAgentV2MaxRelationTypeBytes || value[0] < 'A' || value[0] > 'Z' {
		return ""
	}
	for index := 1; index < len(value); index++ {
		ch := value[index]
		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '_' {
			return ""
		}
	}
	return value
}

func brainBriefAgentV2SafeResolution(value string) string {
	if len(value) == 0 || len(value) > brainBriefAgentV2MaxResolutionBytes || !isASCIIAlpha(value[0]) {
		return ""
	}
	for index := 1; index < len(value); index++ {
		ch := value[index]
		if !isASCIIAlpha(ch) && (ch < '0' || ch > '9') && ch != '_' && ch != '-' {
			return ""
		}
	}
	// Of the shared credential languages, only a GitHub token can satisfy the
	// resolution grammar above. Keep that exact matcher rather than running the
	// four regexes whose required spaces, punctuation, or lowercase prefix make
	// them unreachable here.
	if reGitHubTok.MatchString(value) {
		return ""
	}
	return value
}

func brainBriefAgentV2SafeEdgeID(value string) string {
	if len(value) == 0 || len(value) > brainBriefAgentV2MaxEdgeIDBytes || !utf8.ValidString(value) {
		return ""
	}
	for index, ch := range value {
		if !brainBriefAgentV2EdgeIDRuneAllowed(ch, index == 0) {
			return ""
		}
	}
	if !brainBriefAgentV2EdgeIDComponentsSafe(value) {
		return ""
	}
	if !agentV1SafeStructuredValue(value) || sanitizeSemanticWarningText(value, "") == "<redacted>" {
		return ""
	}
	// Whitespace-free IDs cannot contain the private-key or Bearer languages.
	// JWTs, GitHub tokens, and secret assignments remain grammar-reachable, so
	// retain those exact shared matchers without a heuristic prefilter.
	if reJWT.MatchString(value) || reGitHubTok.MatchString(value) || reSecretEnv.MatchString(value) {
		return ""
	}
	return value
}

func brainBriefAgentV2EdgeIDRuneAllowed(ch rune, first bool) bool {
	if first && ch != '_' && !unicode.IsLetter(ch) && !unicode.IsDigit(ch) {
		return false
	}
	if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
		return true
	}
	switch ch {
	case ':', '/', '.', '_', '-', '$', '#', '@', '+', '*', '=', '?', '!', '%', '&', '|', '^', '~', '(', ')', '[', ']', '{', '}', '<', '>', ',', '\'', '`':
		return true
	default:
		return false
	}
}

// brainBriefAgentV2EdgeIDComponentsSafe inspects the decoded meaning of an
// otherwise grammar-valid opaque edge ID. The original value remains the wire
// value. One bounded percent-decoding pass prevents a harmless-looking prefix
// from hiding a host path or URI, while a remaining escape after that pass
// fails closed instead of allowing recursive encoding to bypass inspection.
func brainBriefAgentV2EdgeIDComponentsSafe(value string) bool {
	if len(value) == 0 || len(value) > brainBriefAgentV2MaxEdgeIDBytes {
		return false
	}
	if strings.IndexByte(value, '%') < 0 && strings.IndexByte(value, '\\') < 0 {
		return brainBriefAgentV2EdgeIDInspectionSafe(value)
	}
	var inspected [brainBriefAgentV2MaxEdgeIDBytes]byte
	inspectedBytes := 0
	for index := 0; index < len(value); {
		decoded := value[index]
		if decoded == '%' && index+2 < len(value) {
			high, highOK := brainBriefAgentV2HexNibble(value[index+1])
			low, lowOK := brainBriefAgentV2HexNibble(value[index+2])
			if highOK && lowOK {
				decoded = high<<4 | low
				index += 3
			} else {
				index++
			}
		} else {
			index++
		}
		if decoded == '\\' {
			decoded = '/'
		}
		inspected[inspectedBytes] = decoded
		inspectedBytes++
	}

	view := inspected[:inspectedBytes]
	if !utf8.Valid(view) {
		return false
	}
	for index := 0; index < len(view); {
		if view[index] == '%' && index+2 < len(view) {
			_, highOK := brainBriefAgentV2HexNibble(view[index+1])
			_, lowOK := brainBriefAgentV2HexNibble(view[index+2])
			if highOK && lowOK {
				return false
			}
		}
		ch, width := utf8.DecodeRune(view[index:])
		if !brainBriefAgentV2EdgeIDRuneAllowed(ch, index == 0) {
			return false
		}
		index += width
	}

	return brainBriefAgentV2EdgeIDInspectionSafe(view)
}

type brainBriefAgentV2EdgeIDView interface {
	~string | ~[]byte
}

func brainBriefAgentV2EdgeIDInspectionSafe[T brainBriefAgentV2EdgeIDView](view T) bool {
	for index := 0; index < len(view); index++ {
		if !brainBriefAgentV2EdgeIDComponentStart(view, index) {
			continue
		}
		if view[index] == '/' {
			return false
		}
		if index+2 < len(view) && isASCIIAlpha(view[index]) && view[index+1] == ':' && view[index+2] == '/' {
			return false
		}
		if brainBriefAgentV2FoldComponent(view, index, "..") ||
			brainBriefAgentV2FoldComponent(view, index, "~") ||
			brainBriefAgentV2FoldComponent(view, index, "$home") ||
			brainBriefAgentV2FoldComponent(view, index, "${home}") {
			return false
		}
		if brainBriefAgentV2FoldPrefix(view, index, "file:") ||
			brainBriefAgentV2FoldPrefix(view, index, "http:") ||
			brainBriefAgentV2FoldPrefix(view, index, "https:") ||
			brainBriefAgentV2FoldPrefix(view, index, "mailto:") {
			return false
		}
	}
	return true
}

func brainBriefAgentV2HexNibble(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func brainBriefAgentV2EdgeIDComponentDelimiter(value byte) bool {
	switch value {
	case ':', '/', '$', '#', '@', '+', '*', '=', '?', '!', '%', '&', '|', '^', '~',
		'(', ')', '[', ']', '{', '}', '<', '>', ',', '\'', '`':
		return true
	default:
		return false
	}
}

func brainBriefAgentV2EdgeIDComponentStart[T brainBriefAgentV2EdgeIDView](value T, index int) bool {
	return index == 0 || brainBriefAgentV2EdgeIDComponentDelimiter(value[index-1])
}

func brainBriefAgentV2EdgeIDComponentEnd[T brainBriefAgentV2EdgeIDView](value T, index int) bool {
	return index == len(value) || brainBriefAgentV2EdgeIDComponentDelimiter(value[index])
}

func brainBriefAgentV2FoldPrefix[T brainBriefAgentV2EdgeIDView](value T, index int, prefix string) bool {
	if index+len(prefix) > len(value) {
		return false
	}
	for offset := 0; offset < len(prefix); offset++ {
		got := value[index+offset]
		if got >= 'A' && got <= 'Z' {
			got += 'a' - 'A'
		}
		if got != prefix[offset] {
			return false
		}
	}
	return true
}

func brainBriefAgentV2FoldComponent[T brainBriefAgentV2EdgeIDView](value T, index int, component string) bool {
	return brainBriefAgentV2FoldPrefix(value, index, component) &&
		brainBriefAgentV2EdgeIDComponentEnd(value, index+len(component))
}

func finalizeBrainBriefAgentV2Body(bodyText string, bodyRecords int, emitted, available brainBriefAgentV2Counts) string {
	bodyHash := sha256.Sum256([]byte(bodyText))
	end := brainBriefAgentV2EndRecordLine(
		emitted, available, bodyRecords, len(bodyText), fmt.Sprintf("sha256:%x", bodyHash),
	)
	var packet strings.Builder
	packet.Grow(len(bodyText) + len(end))
	packet.WriteString(bodyText)
	packet.WriteString(end)
	return packet.String()
}

func brainBriefAgentV2SizedPacketBytes(bodyBytes, bodyRecords int, emitted, available brainBriefAgentV2Counts) int {
	return bodyBytes + brainBriefAgentV2EncodeEndRecord(
		nil, emitted, available, bodyRecords, bodyBytes, brainBriefAgentV2SizingSHA256,
	)
}

func brainBriefAgentV2EndRecordLine(
	emitted, available brainBriefAgentV2Counts,
	bodyRecords, bodyBytes int,
	bodySHA256 string,
) string {
	endBytes := brainBriefAgentV2EncodeEndRecord(
		nil, emitted, available, bodyRecords, bodyBytes, brainBriefAgentV2SizingSHA256,
	)
	var end strings.Builder
	end.Grow(endBytes)
	brainBriefAgentV2EncodeEndRecord(&end, emitted, available, bodyRecords, bodyBytes, bodySHA256)
	return end.String()
}

func brainBriefAgentV2EncodeEndRecord(
	out *strings.Builder,
	emitted, available brainBriefAgentV2Counts,
	bodyRecords, bodyBytes int,
	bodySHA256 string,
) int {
	encoder := brainBriefAgentV1EndRecordEncoder{out: out}
	encoder.string("end")
	encoder.integer("facts", emitted.facts)
	encoder.integer("fact_reviews", emitted.factReviews)
	encoder.integer("facts_with_locus_drift", emitted.factsWithLocusDrift)
	encoder.integer("trust_warnings", emitted.trustWarnings)
	encoder.integer("edit_files", emitted.editFiles)
	encoder.integer("test_files", emitted.testFiles)
	encoder.integer("likely_files", emitted.likelyFiles)
	encoder.integer("actions", emitted.actions)
	encoder.integer("symbols", emitted.symbols)
	encoder.integer("semantic_relations", emitted.semanticRelations)
	encoder.integer("runtime_traces", emitted.runtimeTraces)
	encoder.integer("test_suggestions", emitted.testSuggestions)
	encoder.integer("history", emitted.history)
	encoder.integer("available_facts", available.facts)
	encoder.integer("available_fact_reviews", available.factReviews)
	encoder.integer("available_facts_with_locus_drift", available.factsWithLocusDrift)
	encoder.integer("available_trust_warnings", available.trustWarnings)
	encoder.integer("available_edit_files", available.editFiles)
	encoder.integer("available_test_files", available.testFiles)
	encoder.integer("available_likely_files", available.likelyFiles)
	encoder.integer("available_actions", available.actions)
	encoder.integer("available_symbols", available.symbols)
	encoder.integer("available_semantic_relations", available.semanticRelations)
	encoder.integer("available_runtime_traces", available.runtimeTraces)
	encoder.integer("available_test_suggestions", available.testSuggestions)
	encoder.integer("available_history", available.history)
	encoder.boolean("truncated", emitted.truncated(available))
	encoder.integer("body_records", bodyRecords)
	encoder.integer("body_bytes", bodyBytes)
	encoder.quotedASCII("body_sha256", bodySHA256)
	encoder.byte('\n')
	return encoder.bytes
}

func validateBrainBriefAgentV2PacketSchema(packet string) error {
	return validateBrainBriefAgentPacketSchema("agent_v2", brainBriefAgentV2Marker, brainBriefAgentV2RecordSchemas, packet)
}

func validateBrainBriefAgentV2Integrity(packet string) error {
	if len(packet) > brainBriefAgentV2BudgetBytes {
		return fmt.Errorf("agent_v2 packet is %d bytes, exceeds %d-byte budget", len(packet), brainBriefAgentV2BudgetBytes)
	}
	if err := validateBrainBriefAgentV2PacketSchema(packet); err != nil {
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
	if len(records) < 4 || records[0].tag != "config" || records[1].tag != "task" || records[2].tag != "live" || records[len(records)-1].tag != "end" {
		return fmt.Errorf("agent_v2 config, task, live, or end record missing")
	}
	if err := validateBrainBriefAgentV2RecordOrder(records); err != nil {
		return err
	}

	end := records[len(records)-1]
	body := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	bodyRecords, err := agentV2RecordInt(end, "body_records")
	if err != nil {
		return err
	}
	if bodyRecords != len(records)-1 {
		return fmt.Errorf("agent_v2 body_records = %d, want %d", bodyRecords, len(records)-1)
	}
	bodyBytes, err := agentV2RecordInt(end, "body_bytes")
	if err != nil {
		return err
	}
	if bodyBytes != len(body) {
		return fmt.Errorf("agent_v2 body_bytes = %d, want %d", bodyBytes, len(body))
	}
	bodyHash, err := agentV2RecordString(end, "body_sha256")
	if err != nil {
		return err
	}
	wantBodyHash := sha256.Sum256([]byte(body))
	if bodyHash != fmt.Sprintf("sha256:%x", wantBodyHash) {
		return fmt.Errorf("agent_v2 body_sha256 mismatch")
	}

	actual, err := brainBriefAgentV2CountBodyRecords(records[:len(records)-1])
	if err != nil {
		return err
	}
	emitted, err := brainBriefAgentV2FooterCounts(end, "")
	if err != nil {
		return err
	}
	if actual != emitted {
		return fmt.Errorf("agent_v2 footer emitted counts do not match body")
	}
	available, err := brainBriefAgentV2FooterCounts(end, "available_")
	if err != nil {
		return err
	}
	if !brainBriefAgentV2CountsContain(available, emitted) {
		return fmt.Errorf("agent_v2 available counts are less than emitted counts")
	}
	if available.facts != emitted.facts || available.factReviews != emitted.factReviews ||
		available.factsWithLocusDrift != emitted.factsWithLocusDrift || available.trustWarnings != emitted.trustWarnings {
		return fmt.Errorf("agent_v2 mandatory counts must be fully emitted")
	}
	truncated, err := agentV2RecordBool(end, "truncated")
	if err != nil {
		return err
	}
	if truncated != emitted.truncated(available) {
		return fmt.Errorf("agent_v2 truncated footer mismatch")
	}

	config := records[0]
	schema, err := agentV2RecordInt(config, "schema")
	if err != nil || schema != brainBriefAgentV2Schema {
		return fmt.Errorf("agent_v2 config schema mismatch")
	}
	format, err := agentV2RecordString(config, "packet_format")
	if err != nil || format != "agent_v2" {
		return fmt.Errorf("agent_v2 config packet_format mismatch")
	}
	policyString, err := agentV2RecordString(config, "delivery_policy")
	if err != nil {
		return err
	}
	policy := brainBriefDeliveryPolicy(policyString)
	if policy != brainBriefDeliveryAlways {
		return fmt.Errorf("agent_v2 config delivery policy must be always")
	}
	budget, err := agentV2RecordInt(config, "byte_budget")
	if err != nil || budget != brainBriefAgentV2BudgetBytes {
		return fmt.Errorf("agent_v2 config byte budget mismatch")
	}
	requestedLimit, err := agentV2RecordInt(config, "requested_limit")
	if err != nil {
		return err
	}
	if requestedLimit <= 0 {
		return fmt.Errorf("agent_v2 config requested_limit must be positive")
	}
	effectiveFactLimit, err := agentV2RecordInt(config, "effective_fact_limit")
	if err != nil {
		return err
	}
	if effectiveFactLimit != brainBriefFactsCount(requestedLimit) {
		return fmt.Errorf("agent_v2 config effective_fact_limit mismatch")
	}
	configHash, err := agentV2RecordString(config, "config_sha256")
	if err != nil {
		return err
	}
	if want := brainBriefAgentV2ConfigIdentity(policy, requestedLimit, effectiveFactLimit); configHash != want {
		return fmt.Errorf("agent_v2 config_sha256 mismatch")
	}
	return nil
}

func validateBrainBriefAgentV2RecordOrder(records []compactV1RawRecord) error {
	optionalOrder := map[string]int{
		"edit_file": 0, "test_file": 1, "likely_file": 2, "action": 3, "symbol": 4,
		"test_suggestion": 5, "semantic_relation": 6, "runtime_trace": 7, "history": 8,
	}
	nextRank := map[string]int{}
	lastOptional := -1
	optionalStarted := false
	trustStarted := false
	previousFactID := ""
	for index, record := range records {
		if index < 3 || index == len(records)-1 {
			continue
		}
		if order, ok := optionalOrder[record.tag]; ok {
			previousFactID = ""
			optionalStarted = true
			if order < lastOptional {
				return fmt.Errorf("agent_v2 optional records are not in configured priority order")
			}
			lastOptional = order
		} else {
			if optionalStarted {
				return fmt.Errorf("agent_v2 mandatory record %q follows optional records", record.tag)
			}
			switch record.tag {
			case "fact":
				if trustStarted {
					return fmt.Errorf("agent_v2 fact follows trust warning")
				}
				factID, err := agentV2RecordString(record, "id")
				if err != nil {
					return err
				}
				previousFactID = factID
			case "fact_review":
				if trustStarted {
					return fmt.Errorf("agent_v2 fact review follows trust warning")
				}
				factID, err := agentV2RecordString(record, "fact_id")
				if err != nil {
					return err
				}
				if previousFactID == "" || factID != previousFactID {
					return fmt.Errorf("agent_v2 fact review does not immediately follow its fact")
				}
				previousFactID = ""
			case "trust_warning":
				previousFactID = ""
				trustStarted = true
				code, err := agentV2RecordString(record, "code")
				if err != nil {
					return err
				}
				message, err := agentV2RecordString(record, "message")
				if err != nil {
					return err
				}
				if code != "proposal_state_unavailable" || message != factReviewQueueUnavailableWarning {
					return fmt.Errorf("agent_v2 trust warning is not an approved safe warning")
				}
			default:
				return fmt.Errorf("agent_v2 unexpected body record %q", record.tag)
			}
		}
		if record.tag == "fact" || record.tag == "action" || record.tag == "symbol" ||
			record.tag == "semantic_relation" || record.tag == "runtime_trace" ||
			record.tag == "test_suggestion" || record.tag == "history" {
			rank, err := agentV2RecordInt(record, "rank")
			if err != nil {
				return err
			}
			if rank != nextRank[record.tag] {
				return fmt.Errorf("agent_v2 %s rank = %d, want %d", record.tag, rank, nextRank[record.tag])
			}
			nextRank[record.tag]++
		}
		if record.tag == "semantic_relation" || record.tag == "runtime_trace" {
			if err := validateBrainBriefAgentV2StructuredEdge(record); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBrainBriefAgentV2StructuredEdge(record compactV1RawRecord) error {
	type fieldRule struct {
		name string
		safe func(string) string
	}
	rules := []fieldRule{
		{name: "type", safe: brainBriefAgentV2SafeRelationType},
		{name: "from", safe: brainBriefAgentV2SafeEdgeID},
		{name: "to", safe: brainBriefAgentV2SafeEdgeID},
	}
	if record.tag == "semantic_relation" {
		rules = append(rules, fieldRule{name: "resolution", safe: brainBriefAgentV2SafeResolution})
	} else {
		rules = append(rules, fieldRule{name: "observed_type", safe: brainBriefAgentV2SafeRelationType})
	}
	for _, rule := range rules {
		for _, field := range record.fields {
			if field.key != rule.name {
				continue
			}
			value, err := agentV2RecordString(record, rule.name)
			if err != nil {
				return err
			}
			if rule.safe(value) != value {
				return fmt.Errorf("agent_v2 %s.%s is unsafe", record.tag, rule.name)
			}
			break
		}
	}
	return nil
}

func brainBriefAgentV2CountBodyRecords(records []compactV1RawRecord) (brainBriefAgentV2Counts, error) {
	var counts brainBriefAgentV2Counts
	for _, record := range records {
		switch record.tag {
		case "fact":
			locusDrift, err := agentV2RecordBool(record, "locus_drift")
			if err != nil {
				return brainBriefAgentV2Counts{}, err
			}
			counts.add(brainBriefAgentV1Record{kind: agentV1Fact, locusDrift: locusDrift})
		case "fact_review":
			counts.add(brainBriefAgentV1Record{kind: agentV1FactReview})
		case "trust_warning":
			counts.add(brainBriefAgentV1Record{kind: agentV1TrustWarning})
		case "edit_file":
			counts.add(brainBriefAgentV1Record{kind: agentV1EditFile})
		case "test_file":
			counts.add(brainBriefAgentV1Record{kind: agentV1TestFile})
		case "likely_file":
			counts.add(brainBriefAgentV1Record{kind: agentV1LikelyFile})
		case "action":
			counts.add(brainBriefAgentV1Record{kind: agentV1Action})
		case "symbol":
			counts.add(brainBriefAgentV1Record{kind: agentV1Symbol})
		case "semantic_relation":
			counts.add(brainBriefAgentV1Record{kind: agentV2SemanticRelation})
		case "runtime_trace":
			counts.add(brainBriefAgentV1Record{kind: agentV2RuntimeTrace})
		case "test_suggestion":
			counts.add(brainBriefAgentV1Record{kind: agentV1TestSuggestion})
		case "history":
			counts.add(brainBriefAgentV1Record{kind: agentV1History})
		case "config", "task", "live":
		default:
			return brainBriefAgentV2Counts{}, fmt.Errorf("agent_v2 cannot count body record %q", record.tag)
		}
	}
	return counts, nil
}

func brainBriefAgentV2FooterCounts(record compactV1RawRecord, prefix string) (brainBriefAgentV2Counts, error) {
	read := func(key string) (int, error) { return agentV2RecordInt(record, prefix+key) }
	var counts brainBriefAgentV2Counts
	fields := []struct {
		key string
		out *int
	}{
		{"facts", &counts.facts},
		{"fact_reviews", &counts.factReviews},
		{"facts_with_locus_drift", &counts.factsWithLocusDrift},
		{"trust_warnings", &counts.trustWarnings},
		{"edit_files", &counts.editFiles},
		{"test_files", &counts.testFiles},
		{"likely_files", &counts.likelyFiles},
		{"actions", &counts.actions},
		{"symbols", &counts.symbols},
		{"semantic_relations", &counts.semanticRelations},
		{"runtime_traces", &counts.runtimeTraces},
		{"test_suggestions", &counts.testSuggestions},
		{"history", &counts.history},
	}
	for _, field := range fields {
		value, err := read(field.key)
		if err != nil {
			return brainBriefAgentV2Counts{}, err
		}
		*field.out = value
	}
	return counts, nil
}

func brainBriefAgentV2CountsContain(available, emitted brainBriefAgentV2Counts) bool {
	return available.facts >= emitted.facts &&
		available.factReviews >= emitted.factReviews &&
		available.factsWithLocusDrift >= emitted.factsWithLocusDrift &&
		available.trustWarnings >= emitted.trustWarnings &&
		available.editFiles >= emitted.editFiles &&
		available.testFiles >= emitted.testFiles &&
		available.likelyFiles >= emitted.likelyFiles &&
		available.actions >= emitted.actions &&
		available.symbols >= emitted.symbols &&
		available.semanticRelations >= emitted.semanticRelations &&
		available.runtimeTraces >= emitted.runtimeTraces &&
		available.testSuggestions >= emitted.testSuggestions &&
		available.history >= emitted.history
}

func agentV2RecordField(record compactV1RawRecord, key string) (string, error) {
	return brainBriefAgentRecordField("agent_v2", record, key)
}

func agentV2RecordString(record compactV1RawRecord, key string) (string, error) {
	return brainBriefAgentRecordString("agent_v2", record, key)
}

func agentV2RecordInt(record compactV1RawRecord, key string) (int, error) {
	return brainBriefAgentRecordInt("agent_v2", record, key)
}

func agentV2RecordBool(record compactV1RawRecord, key string) (bool, error) {
	return brainBriefAgentRecordBool("agent_v2", record, key)
}
