package cli

// Test-only retention of the compact brief serializers (COR-2317): the
// production CLI never offered compact_v1/v2/v3 (no flag or env selected
// them), so they left the shipped binary — but many brief behavior tests
// assert through these deterministic line-oriented projections, and those
// assertions keep their value. Everything below is reachable from tests only.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/spf13/cobra"
)

const (
	brainBriefPacketCompactV1 brainBriefPacketFormat = "compact_v1"
	brainBriefPacketCompactV2 brainBriefPacketFormat = "compact_v2"
	brainBriefPacketCompactV3 brainBriefPacketFormat = "compact_v3"
)

const brainBriefCompactV1Marker = "entire.brain_brief compact_v1"

// emitBrainBriefCompactV1 writes a deterministic, line-oriented projection of
// the full brief. Every data string is Go-quoted, so embedded newlines, tabs,
// quotes, and control bytes cannot manufacture packet records. Slice order is
// retained; map-backed records are sorted explicitly.
func emitBrainBriefCompactV1(cmd *cobra.Command, report brainBriefReport) error {
	if err := validateBrainBriefCompactV1Numbers(report); err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString(brainBriefCompactV1Marker)
	out.WriteByte('\n')
	compactV1Record(&out, "task", compactV1StringAlways("value", report.Task))

	compactV1Record(&out, "status",
		compactV1String("freshness", brainStatusFreshnessSeverity(report.Status)),
		compactV1String("repo_key", report.Status.Repo.Key),
		compactV1Int("brain_schema", report.Status.Brain.Schema),
		compactV1String("branch", report.Status.Live.Branch),
		compactV1String("head", report.Status.Live.Head),
		compactV1Bool("dirty", report.Status.Live.Dirty),
		compactV1IntAlways("changed_files", len(report.Status.Live.ChangedFiles)),
	)
	compactV1Record(&out, "sources",
		compactV1Bool("seed", report.Status.Sources.Seed),
		compactV1Bool("sessions", report.Status.Sources.Sessions),
		compactV1Bool("semantic", report.Status.Sources.Semantic),
		compactV1Bool("history", report.Status.Sources.History),
		compactV1Bool("facts", report.Status.Sources.Facts),
	)
	compactV1Record(&out, "live",
		compactV1Strings("staged", report.Status.Live.Staged),
		compactV1Strings("unstaged", report.Status.Live.Unstaged),
		compactV1Strings("untracked", report.Status.Live.Untracked),
		compactV1Strings("changed", report.Status.Live.ChangedFiles),
		compactV1String("diff_stat", strings.TrimSpace(report.Status.Live.DiffStat)),
	)
	for _, symbol := range report.Status.Live.ChangedSymbolHints {
		compactV1SemanticSymbol(&out, "live_symbol", symbol)
	}

	if facts := report.Status.Facts; facts != nil {
		fields := []string{
			compactV1IntAlways("facts", facts.Facts),
			compactV1IntAlways("distilled", facts.Distilled),
			compactV1IntAlways("authored", facts.Authored),
			// Omitted when zero: the packet has a byte budget and most brains
			// have no imported facts. But when there ARE some, leaving them out
			// made the origins fail to sum -- facts=400 distilled=0 authored=0
			// reads as an empty brain to the agent reading this packet.
			compactV1Int("imported", facts.Imported),
			compactV1IntAlways("superseded", facts.Superseded),
		}
		if verification := facts.Verification; verification != nil {
			fields = append(fields,
				compactV1IntAlways("verified_facts", verification.Facts),
				compactV1IntAlways("verified", verification.Verified),
				compactV1IntAlways("stale", verification.Stale),
				compactV1IntAlways("orphaned", verification.Orphaned),
				compactV1IntAlways("unverifiable_here", verification.UnverifiableHere),
				compactV1Int("sampled_of", verification.SampledOf),
			)
		}
		compactV1Record(&out, "facts_status", fields...)
	}

	if semantic := report.Status.Semantic; semantic != nil {
		var fields []string
		if provider := semantic.Provider; provider != nil {
			fields = append(fields,
				compactV1String("provider", provider.Name),
				compactV1String("provider_version", provider.Version),
				compactV1String("schema", provider.Schema),
				compactV1Strings("capabilities", provider.Capabilities),
			)
		}
		if coverage := semantic.Coverage; coverage != nil {
			fields = append(fields,
				compactV1IntAlways("files", coverage.Files),
				compactV1IntAlways("symbols", coverage.Symbols),
				compactV1IntAlways("relations", coverage.Relations),
				compactV1IntAlways("warnings", coverage.Warnings),
				compactV1IntAlways("partial_failures", coverage.PartialFailures),
			)
		}
		compactV1Record(&out, "semantic_status", fields...)

		if freshness := semantic.Freshness; freshness != nil {
			axisNames := make([]string, 0, len(freshness.Axes))
			for name := range freshness.Axes {
				axisNames = append(axisNames, name)
			}
			sort.Strings(axisNames)
			for _, name := range axisNames {
				axis := freshness.Axes[name]
				compactV1Record(&out, "freshness_axis",
					compactV1StringAlways("name", name),
					compactV1StringAlways("state", axis.State),
					compactV1String("detail", axis.Detail),
				)
			}
			for _, warning := range freshness.Warnings {
				compactV1SemanticWarning(&out, "freshness_warning", warning)
			}
		}
		if coverage := semantic.Coverage; coverage != nil {
			for _, warning := range coverage.WarningDetails {
				compactV1SemanticWarning(&out, "semantic_warning", warning)
			}
			for _, warning := range coverage.PartialFailureDetails {
				compactV1SemanticWarning(&out, "semantic_partial_failure", warning)
			}
		}
		for _, spot := range semantic.BlindSpots {
			compactV1Record(&out, "blind_spot",
				compactV1StringAlways("path", spot.Path),
				compactV1String("code", spot.Code),
				compactV1String("detail", spot.Detail),
			)
		}
	}

	seenLikely := make(map[string]struct{}, len(report.LikelyEditFiles)+len(report.LikelyTestFiles))
	for _, path := range report.LikelyEditFiles {
		compactV1Record(&out, "edit_file", compactV1StringAlways("path", path))
		seenLikely[path] = struct{}{}
	}
	for _, path := range report.LikelyTestFiles {
		compactV1Record(&out, "test_file", compactV1StringAlways("path", path))
		seenLikely[path] = struct{}{}
	}
	for _, path := range report.LikelyFiles {
		if _, seen := seenLikely[path]; seen {
			continue
		}
		compactV1Record(&out, "likely_file", compactV1StringAlways("path", path))
	}

	// Emitted ahead of the records it qualifies, not after them: agents cap the
	// bytes they read from a tool call, and a caveat that arrives after the
	// symbols is a caveat the reader may never see. Omitted entirely for an
	// exact answer, so an unstale packet is byte-identical to before.
	//
	// excluded_records uses the always-emitted form deliberately. Unreliable can
	// only name paths this answer returned records for, and an ancestor store has
	// already dropped the records for changed files -- so an absent unreliable
	// list is the common case even when the answer is incomplete, and on its own
	// it reads as "nothing to worry about". The count is what separates the two,
	// and a count omitted at zero would be indistinguishable from a renderer that
	// does not report it at all.
	if staleness := report.Semantic.Staleness; staleness != nil {
		compactV1Record(&out, "staleness",
			compactV1StringAlways("ancestor_commit", staleness.AncestorCommit),
			compactV1IntAlways("commits_behind", staleness.CommitsBehind),
			compactV1Strings("changed_files", staleness.ChangedFiles),
			compactV1Strings("unreliable", staleness.Unreliable),
			compactV1IntAlways("excluded_records", staleness.ExcludedRecords),
		)
	}

	for _, symbol := range report.Semantic.Context.Symbols {
		compactV1SemanticSymbol(&out, "symbol", symbol)
	}
	for i, relation := range report.Semantic.Context.Relations {
		compactV1SemanticRelation(&out, "relation", i, relation)
	}
	for _, neighbor := range report.Semantic.Context.Neighbors {
		compactV1SemanticSymbol(&out, "neighbor", neighbor)
	}
	for i, trace := range report.Semantic.RuntimeTraces {
		compactV1SemanticRelation(&out, "runtime_trace", i, trace)
	}
	for _, root := range report.Semantic.Tests.Roots {
		compactV1SemanticSymbol(&out, "test_root", root)
	}
	for _, suggestion := range report.Semantic.Tests.Suggestions {
		compactV1SemanticSymbol(&out, "test_suggestion", suggestion.Symbol,
			compactV1StringAlways("suggestion_reason", suggestion.Reason))
	}

	seenFacts := make(map[string]struct{}, len(report.Facts))
	for _, fact := range report.Facts {
		verifiedAnchors := 0
		for _, anchor := range fact.Provenance {
			if anchor.Verified {
				verifiedAnchors++
			}
		}
		compactV1Record(&out, "fact",
			compactV1StringAlways("id", fact.ID),
			compactV1Strings("paths", fact.Paths),
			compactV1String("kind", fact.Kind),
			compactV1Strings("locus", fact.Locus),
			compactV1StringAlways("text", fact.Text),
			compactV1String("branch", fact.Branch),
			compactV1String("origin", fact.Origin),
			compactV1String("status", fact.Status),
			compactV1String("confidence", fact.Confidence),
			compactV1Strings("related_ids", fact.RelatedIDs),
			compactV1String("superseded_by", fact.SupersededBy),
			compactV1IntAlways("anchors", len(fact.Provenance)),
			compactV1IntAlways("verified_anchors", verifiedAnchors),
			compactV1Strings("stale_locus", report.FactsLocusDrift[fact.ID]),
		)
		seenFacts[fact.ID] = struct{}{}
	}
	driftIDs := make([]string, 0, len(report.FactsLocusDrift))
	for id := range report.FactsLocusDrift {
		if _, seen := seenFacts[id]; !seen {
			driftIDs = append(driftIDs, id)
		}
	}
	sort.Strings(driftIDs)
	for _, id := range driftIDs {
		compactV1Record(&out, "fact_drift",
			compactV1StringAlways("id", id),
			compactV1Strings("stale_locus", report.FactsLocusDrift[id]),
		)
	}

	for _, action := range report.ActionChecklist {
		compactV1Record(&out, "action",
			compactV1String("file", action.File),
			compactV1String("symbol", action.Symbol),
			compactV1StringAlways("action", action.Action),
			compactV1String("evidence", action.Evidence),
		)
	}
	for _, pattern := range report.Patterns {
		fields := []string{
			compactV1StringAlways("id", pattern.ID),
			compactV1StringAlways("type", pattern.Type),
			compactV1String("scope", pattern.Scope),
			compactV1String("kind", pattern.Kind),
			compactV1StringAlways("title", pattern.Title),
			compactV1FloatAlways("strength", pattern.Strength),
			compactV1String("strength_label", pattern.StrengthLabel),
			compactV1IntAlways("support", pattern.Support),
			compactV1Int("repos", pattern.Repos),
			compactV1String("skill_status", pattern.SkillStatus),
			compactV1String("note", pattern.Note),
			compactV1String("intent_sig", pattern.IntentSig),
			compactV1String("gram", pattern.Gram),
			compactV1String("dossier_status", pattern.DossierStatus),
			compactV1String("verdict", pattern.Verdict),
			compactV1String("workspace", pattern.Workspace),
		}
		if reinforcement := pattern.Reinforcement; reinforcement != nil {
			fields = append(fields,
				compactV1IntAlways("success", reinforcement.Success),
				compactV1IntAlways("corrected", reinforcement.Corrected),
				compactV1IntAlways("neutral", reinforcement.Neutral),
			)
		}
		if example := pattern.Example; example != nil {
			fields = append(fields,
				compactV1String("example_path", example.Path),
				compactV1Int("example_line", example.Line),
			)
		}
		compactV1Record(&out, "pattern", fields...)
	}
	for _, consolidation := range report.Consolidations {
		fields := []string{
			compactV1StringAlways("pattern_id", consolidation.PatternID),
			compactV1StringAlways("type", consolidation.Type),
			compactV1StringAlways("title", consolidation.Title),
			compactV1StringAlways("trigger", consolidation.Trigger),
			compactV1FloatAlways("confidence", consolidation.Confidence),
			compactV1StringAlways("status", consolidation.Status),
			compactV1String("verdict", consolidation.Verdict),
			compactV1Strings("workflow", consolidation.Workflow),
			compactV1Strings("verification", consolidation.Verification),
			compactV1Strings("failure_modes", consolidation.FailureModes),
		}
		if anchor := consolidation.Anchor; anchor != nil {
			fields = append(fields,
				compactV1String("anchor_transcript", anchor.Transcript),
				compactV1Int("anchor_start", anchor.StartLine),
				compactV1Int("anchor_end", anchor.EndLine),
				compactV1String("anchor_outcome", anchor.Outcome),
			)
		}
		compactV1Record(&out, "consolidation", fields...)
	}
	for _, theme := range report.Themes {
		compactV1Record(&out, "theme",
			compactV1StringAlways("id", theme.ID),
			compactV1StringAlways("title", theme.Title),
			compactV1String("description", theme.Description),
			compactV1StringAlways("shape", theme.Shape),
			compactV1IntAlways("support", theme.Support),
			compactV1FloatAlways("strength", theme.Strength),
			compactV1StringAlways("status", theme.Status),
			compactV1String("verdict", theme.Verdict),
		)
	}
	for _, guidance := range report.Guidance {
		compactV1Record(&out, "guidance", compactV1StringAlways("text", guidance))
	}
	for _, warning := range report.Status.Warnings {
		compactV1Record(&out, "warning", compactV1StringAlways("source", "status"), compactV1StringAlways("text", warning))
	}
	for _, warning := range report.Status.Live.Warnings {
		compactV1Record(&out, "warning", compactV1StringAlways("source", "live"), compactV1StringAlways("text", warning))
	}
	for _, warning := range report.Warnings {
		compactV1Record(&out, "warning", compactV1StringAlways("source", "brief"), compactV1StringAlways("text", warning))
	}

	body := out.String()
	bodyHash := sha256.Sum256([]byte(body))
	bodyRecords := strings.Count(body, "\n") - 1 // exclude the version marker
	compactV1Record(&out, "end",
		compactV1IntAlways("symbols", len(report.Semantic.Context.Symbols)),
		compactV1IntAlways("relations", len(report.Semantic.Context.Relations)),
		compactV1IntAlways("neighbors", len(report.Semantic.Context.Neighbors)),
		compactV1IntAlways("runtime_traces", len(report.Semantic.RuntimeTraces)),
		compactV1IntAlways("test_roots", len(report.Semantic.Tests.Roots)),
		compactV1IntAlways("test_suggestions", len(report.Semantic.Tests.Suggestions)),
		compactV1IntAlways("facts", len(report.Facts)),
		compactV1IntAlways("actions", len(report.ActionChecklist)),
		compactV1IntAlways("patterns", len(report.Patterns)),
		compactV1IntAlways("consolidations", len(report.Consolidations)),
		compactV1IntAlways("themes", len(report.Themes)),
		compactV1IntAlways("guidance", len(report.Guidance)),
		compactV1IntAlways("warnings", brainBriefCompactV1WarningCount(report)),
		compactV1IntAlways("body_records", bodyRecords),
		compactV1StringAlways("body_sha256", fmt.Sprintf("sha256:%x", bodyHash)),
	)
	_, err := io.WriteString(cmd.OutOrStdout(), out.String())
	return err
}

func validateBrainBriefCompactV1Numbers(report brainBriefReport) error {
	return validateBrainBriefCompactNumbers(report, "compact_v1")
}

func validateBrainBriefCompactNumbers(report brainBriefReport, format string) error {
	check := func(path string, value float64) error {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s cannot encode non-finite %s", format, path)
		}
		return nil
	}
	checkRecords := func(path string, records []semanticRecord) error {
		for i, record := range records {
			if err := check(fmt.Sprintf("%s[%d].confidence", path, i), record.Confidence); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkRecords("status.live.changed_symbol_hints", report.Status.Live.ChangedSymbolHints); err != nil {
		return err
	}
	if err := checkRecords("semantic.context.symbols", report.Semantic.Context.Symbols); err != nil {
		return err
	}
	if err := checkRecords("semantic.context.relations", report.Semantic.Context.Relations); err != nil {
		return err
	}
	if err := checkRecords("semantic.context.neighbors", report.Semantic.Context.Neighbors); err != nil {
		return err
	}
	if err := checkRecords("semantic.runtime_traces", report.Semantic.RuntimeTraces); err != nil {
		return err
	}
	if err := checkRecords("semantic.tests.roots", report.Semantic.Tests.Roots); err != nil {
		return err
	}
	for i, suggestion := range report.Semantic.Tests.Suggestions {
		if err := check(fmt.Sprintf("semantic.tests.suggestions[%d].symbol.confidence", i), suggestion.Symbol.Confidence); err != nil {
			return err
		}
	}
	for i, pattern := range report.Patterns {
		if err := check(fmt.Sprintf("patterns[%d].strength", i), pattern.Strength); err != nil {
			return err
		}
	}
	for i, consolidation := range report.Consolidations {
		if err := check(fmt.Sprintf("consolidations[%d].confidence", i), consolidation.Confidence); err != nil {
			return err
		}
	}
	for i, theme := range report.Themes {
		if err := check(fmt.Sprintf("themes[%d].strength", i), theme.Strength); err != nil {
			return err
		}
	}
	return nil
}

func compactV1SemanticWarning(out *strings.Builder, tag string, warning semanticWarning) {
	compactV1Record(out, tag,
		compactV1String("code", warning.Code),
		compactV1String("severity", warning.Severity),
		compactV1String("path", warning.Path),
		compactV1String("effect", warning.Effect),
		compactV1String("detail", warning.Detail),
	)
}

func compactV1SemanticSymbol(out *strings.Builder, tag string, record semanticRecord, extra ...string) {
	name := displaySymbolName(record)
	fields := []string{
		compactV1String("id", record.ID),
		compactV1String("kind", record.Kind),
		compactV1String("name", name),
	}
	if record.Name != "" && record.Name != name {
		fields = append(fields, compactV1String("short_name", record.Name))
	}
	fields = append(fields,
		compactV1String("file", record.FilePath),
		compactV1Int("start", record.StartLine),
		compactV1Int("end", record.EndLine),
		compactV1String("path", record.Path),
		compactV1String("signature", record.Signature),
		compactV1String("language", record.Language),
		compactV1Int("score", record.Score),
		compactV1Float("confidence", record.Confidence),
		compactV1String("reason", record.Reason),
		compactV1Strings("warning_codes", record.WarningCodes),
	)
	fields = append(fields, extra...)
	compactV1Record(out, tag, fields...)
}

func compactV1SemanticRelation(out *strings.Builder, tag string, index int, record semanticRecord) {
	compactV1Record(out, tag,
		compactV1IntAlways("index", index),
		compactV1String("id", record.ID),
		compactV1String("type", record.Type),
		compactV1String("from", record.FromID),
		compactV1String("to", record.ToID),
		compactV1String("file", record.FilePath),
		compactV1Int("start", record.StartLine),
		compactV1Int("end", record.EndLine),
		compactV1String("path", record.Path),
		compactV1String("scope", record.RelationScope),
		compactV1String("resolution", record.Resolution),
		compactV1String("target_kind", record.TargetKind),
		compactV1Float("confidence", record.Confidence),
		compactV1String("reason", record.Reason),
		compactV1Strings("warning_codes", record.WarningCodes),
	)
	for evidenceIndex, evidence := range record.Evidence {
		compactV1Record(out, tag+"_evidence",
			compactV1IntAlways("owner_index", index),
			compactV1IntAlways("index", evidenceIndex),
			compactV1String("kind", evidence.Kind),
			compactV1String("file", evidence.FilePath),
			compactV1Int("start", evidence.StartLine),
			compactV1Int("end", evidence.EndLine),
			compactV1String("detail", evidence.Detail),
		)
	}
}

func brainBriefCompactV1WarningCount(report brainBriefReport) int {
	count := len(report.Status.Warnings) + len(report.Status.Live.Warnings) + len(report.Warnings)
	if semantic := report.Status.Semantic; semantic != nil {
		count += len(semantic.BlindSpots)
		if semantic.Freshness != nil {
			count += len(semantic.Freshness.Warnings)
		}
		if semantic.Coverage != nil {
			count += len(semantic.Coverage.WarningDetails) + len(semantic.Coverage.PartialFailureDetails)
		}
	}
	return count
}

func compactV1Record(out *strings.Builder, tag string, fields ...string) {
	out.WriteString(tag)
	for _, field := range fields {
		if field == "" {
			continue
		}
		out.WriteByte(' ')
		out.WriteString(field)
	}
	out.WriteByte('\n')
}

func compactV1String(key, value string) string {
	if value == "" {
		return ""
	}
	return compactV1StringAlways(key, value)
}

func compactV1StringAlways(key, value string) string {
	return key + "=" + strconv.Quote(value)
}

func compactV1Strings(key string, values []string) string {
	if len(values) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString(key)
	out.WriteString("=[")
	for i, value := range values {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(strconv.Quote(value))
	}
	out.WriteByte(']')
	return out.String()
}

func compactV1Bool(key string, value bool) string {
	return key + "=" + strconv.FormatBool(value)
}

func compactV1Int(key string, value int) string {
	if value == 0 {
		return ""
	}
	return compactV1IntAlways(key, value)
}

func compactV1IntAlways(key string, value int) string {
	return key + "=" + strconv.Itoa(value)
}

func compactV1Float(key string, value float64) string {
	if value == 0 {
		return ""
	}
	return compactV1FloatAlways(key, value)
}

func compactV1FloatAlways(key string, value float64) string {
	return key + "=" + strconv.FormatFloat(value, 'g', -1, 64)
}

const (
	brainBriefCompactV2Marker = "entire.brain_brief compact_v2"
	// The legend is part of the hashed packet body. "Previous" is scoped to
	// the same opcode, so unrelated physical rows between two records do not
	// reset a metadata repeat reference.
	brainBriefCompactV2Legend = "legend ~=absent ^=previous_same_opcode_record_same_column"
)

type compactV2FieldKind uint8

const (
	compactV2String compactV2FieldKind = iota
	compactV2Strings
	compactV2Bool
	compactV2Int
	compactV2Float
)

type compactV2Field struct {
	name      string
	kind      compactV2FieldKind
	reference bool
}

type compactV2Schema struct {
	tag            string
	opcode         byte
	fields         []compactV2Field
	fieldPositions map[string]int
	declaration    string
}

type compactV1RawField struct {
	key   string
	value string
}

type compactV1RawRecord struct {
	tag    string
	fields []compactV1RawField
	raw    string
}

type compactV2FamilyPlan struct {
	positional  bool
	declaration string
	rows        []string
}

func compactV2Natural(name string, kind compactV2FieldKind) compactV2Field {
	return compactV2Field{name: name, kind: kind}
}

func compactV2Metadata(name string, kind compactV2FieldKind) compactV2Field {
	return compactV2Field{name: name, kind: kind, reference: true}
}

func compactV2SchemaFor(tag string, opcode byte, fields ...compactV2Field) compactV2Schema {
	positions := make(map[string]int, len(fields))
	for i, field := range fields {
		positions[field.name] = i
	}
	schema := compactV2Schema{tag: tag, opcode: opcode, fields: fields, fieldPositions: positions}
	var declaration strings.Builder
	writeCompactV2Declaration(&declaration, schema)
	schema.declaration = declaration.String()
	return schema
}

// compactV2Schemas is the complete, versioned positional schema. Natural
// language columns deliberately do not permit repeat references: an agent must
// see task text, excerpts, guidance, actions, evidence, reasons, details,
// titles, descriptions, and workflow prose directly in every record.
var compactV2Schemas = []compactV2Schema{
	compactV2SchemaFor("task", 'q',
		compactV2Natural("value", compactV2String)),
	compactV2SchemaFor("status", 's',
		compactV2Metadata("freshness", compactV2String), compactV2Metadata("repo_key", compactV2String),
		compactV2Metadata("brain_schema", compactV2Int), compactV2Metadata("branch", compactV2String),
		compactV2Metadata("head", compactV2String), compactV2Metadata("dirty", compactV2Bool),
		compactV2Metadata("changed_files", compactV2Int)),
	compactV2SchemaFor("sources", 'u',
		compactV2Metadata("seed", compactV2Bool), compactV2Metadata("sessions", compactV2Bool),
		compactV2Metadata("semantic", compactV2Bool), compactV2Metadata("history", compactV2Bool),
		compactV2Metadata("facts", compactV2Bool)),
	compactV2SchemaFor("live", 'l',
		compactV2Metadata("staged", compactV2Strings), compactV2Metadata("unstaged", compactV2Strings),
		compactV2Metadata("untracked", compactV2Strings), compactV2Metadata("changed", compactV2Strings),
		compactV2Natural("diff_stat", compactV2String)),
	compactV2SemanticSymbolSchema("live_symbol", 'L', false),
	compactV2SchemaFor("facts_status", 'F',
		compactV2Metadata("facts", compactV2Int), compactV2Metadata("distilled", compactV2Int),
		compactV2Metadata("authored", compactV2Int), compactV2Metadata("superseded", compactV2Int),
		compactV2Metadata("branches", compactV2Int), compactV2Metadata("proposals", compactV2Int),
		compactV2Metadata("verified_facts", compactV2Int), compactV2Metadata("verified", compactV2Int),
		compactV2Metadata("stale", compactV2Int), compactV2Metadata("orphaned", compactV2Int),
		compactV2Metadata("unverifiable_here", compactV2Int), compactV2Metadata("sampled_of", compactV2Int)),
	compactV2SchemaFor("semantic_status", 'S',
		compactV2Metadata("provider", compactV2String), compactV2Metadata("provider_version", compactV2String),
		compactV2Metadata("schema", compactV2String), compactV2Metadata("capabilities", compactV2Strings),
		compactV2Metadata("files", compactV2Int), compactV2Metadata("symbols", compactV2Int),
		compactV2Metadata("relations", compactV2Int), compactV2Metadata("warnings", compactV2Int),
		compactV2Metadata("partial_failures", compactV2Int)),
	compactV2SchemaFor("freshness_axis", 'a',
		compactV2Metadata("name", compactV2String), compactV2Metadata("state", compactV2String),
		compactV2Natural("detail", compactV2String)),
	compactV2WarningSchema("freshness_warning", 'w'),
	compactV2WarningSchema("semantic_warning", 'W'),
	compactV2WarningSchema("semantic_partial_failure", 'E'),
	compactV2SchemaFor("blind_spot", 'b',
		compactV2Metadata("path", compactV2String), compactV2Metadata("code", compactV2String),
		compactV2Natural("detail", compactV2String)),
	compactV2SchemaFor("edit_file", 'e', compactV2Metadata("path", compactV2String)),
	compactV2SchemaFor("test_file", 't', compactV2Metadata("path", compactV2String)),
	compactV2SchemaFor("likely_file", 'p', compactV2Metadata("path", compactV2String)),
	// compact_v1 grew a staleness record, and v2/v3 refuse a v1 tag they have no
	// schema for -- so this entry is what keeps the ancestor disclosure reaching
	// agents on every compact format rather than failing the packet outright.
	compactV2SchemaFor("staleness", 'A',
		compactV2Metadata("ancestor_commit", compactV2String), compactV2Metadata("commits_behind", compactV2Int),
		compactV2Metadata("changed_files", compactV2Strings), compactV2Metadata("unreliable", compactV2Strings),
		compactV2Metadata("excluded_records", compactV2Int)),
	compactV2SemanticSymbolSchema("symbol", 'y', false),
	compactV2SemanticRelationSchema("relation", 'r'),
	compactV2EvidenceSchema("relation_evidence", 'R'),
	compactV2SemanticSymbolSchema("neighbor", 'n', false),
	compactV2SemanticRelationSchema("runtime_trace", 'x'),
	compactV2EvidenceSchema("runtime_trace_evidence", 'X'),
	compactV2SemanticSymbolSchema("test_root", 'o', false),
	compactV2SemanticSymbolSchema("test_suggestion", 'O', true),
	compactV2SchemaFor("fact", 'f',
		compactV2Metadata("id", compactV2String), compactV2Metadata("paths", compactV2Strings),
		compactV2Metadata("kind", compactV2String), compactV2Metadata("locus", compactV2Strings),
		compactV2Natural("text", compactV2String), compactV2Metadata("branch", compactV2String),
		compactV2Metadata("origin", compactV2String), compactV2Metadata("status", compactV2String),
		compactV2Metadata("confidence", compactV2String), compactV2Metadata("related_ids", compactV2Strings),
		compactV2Metadata("superseded_by", compactV2String), compactV2Metadata("anchors", compactV2Int),
		compactV2Metadata("verified_anchors", compactV2Int), compactV2Metadata("stale_locus", compactV2Strings)),
	compactV2SchemaFor("fact_drift", 'd',
		compactV2Metadata("id", compactV2String), compactV2Metadata("stale_locus", compactV2Strings)),
	compactV2SchemaFor("action", 'c',
		compactV2Metadata("file", compactV2String), compactV2Metadata("symbol", compactV2String),
		compactV2Natural("action", compactV2String), compactV2Natural("evidence", compactV2String)),
	compactV2SchemaFor("pattern", 'P',
		compactV2Metadata("id", compactV2String), compactV2Metadata("type", compactV2String),
		compactV2Metadata("scope", compactV2String), compactV2Metadata("kind", compactV2String),
		compactV2Natural("title", compactV2String), compactV2Metadata("strength", compactV2Float),
		compactV2Metadata("strength_label", compactV2String), compactV2Metadata("support", compactV2Int),
		compactV2Metadata("repos", compactV2Int), compactV2Metadata("skill_status", compactV2String),
		compactV2Natural("note", compactV2String), compactV2Natural("intent_sig", compactV2String),
		compactV2Natural("gram", compactV2String), compactV2Metadata("dossier_status", compactV2String),
		compactV2Metadata("verdict", compactV2String), compactV2Metadata("workspace", compactV2String),
		compactV2Metadata("success", compactV2Int), compactV2Metadata("corrected", compactV2Int),
		compactV2Metadata("neutral", compactV2Int), compactV2Metadata("example_path", compactV2String),
		compactV2Metadata("example_line", compactV2Int)),
	compactV2SchemaFor("consolidation", 'C',
		compactV2Metadata("pattern_id", compactV2String), compactV2Metadata("type", compactV2String),
		compactV2Natural("title", compactV2String), compactV2Natural("trigger", compactV2String),
		compactV2Metadata("confidence", compactV2Float), compactV2Metadata("status", compactV2String),
		compactV2Metadata("verdict", compactV2String), compactV2Natural("workflow", compactV2Strings),
		compactV2Natural("verification", compactV2Strings), compactV2Natural("failure_modes", compactV2Strings),
		compactV2Metadata("anchor_transcript", compactV2String), compactV2Metadata("anchor_start", compactV2Int),
		compactV2Metadata("anchor_end", compactV2Int), compactV2Metadata("anchor_outcome", compactV2String)),
	compactV2SchemaFor("theme", 'T',
		compactV2Metadata("id", compactV2String), compactV2Natural("title", compactV2String),
		compactV2Natural("description", compactV2String), compactV2Metadata("shape", compactV2String),
		compactV2Metadata("support", compactV2Int), compactV2Metadata("strength", compactV2Float),
		compactV2Metadata("status", compactV2String), compactV2Metadata("verdict", compactV2String)),
	compactV2SchemaFor("guidance", 'g', compactV2Natural("text", compactV2String)),
	compactV2SchemaFor("warning", '!',
		compactV2Metadata("source", compactV2String), compactV2Natural("text", compactV2String)),
}

func compactV2SemanticSymbolSchema(tag string, opcode byte, suggestion bool) compactV2Schema {
	fields := []compactV2Field{
		compactV2Metadata("id", compactV2String), compactV2Metadata("kind", compactV2String),
		compactV2Metadata("name", compactV2String), compactV2Metadata("short_name", compactV2String),
		compactV2Metadata("file", compactV2String), compactV2Metadata("start", compactV2Int),
		compactV2Metadata("end", compactV2Int), compactV2Metadata("path", compactV2String),
		compactV2Natural("signature", compactV2String), compactV2Metadata("language", compactV2String),
		compactV2Metadata("score", compactV2Int), compactV2Metadata("confidence", compactV2Float),
		compactV2Natural("reason", compactV2String), compactV2Metadata("warning_codes", compactV2Strings),
	}
	if suggestion {
		fields = append(fields, compactV2Natural("suggestion_reason", compactV2String))
	}
	return compactV2SchemaFor(tag, opcode, fields...)
}

func compactV2SemanticRelationSchema(tag string, opcode byte) compactV2Schema {
	return compactV2SchemaFor(tag, opcode,
		compactV2Metadata("index", compactV2Int), compactV2Metadata("id", compactV2String),
		compactV2Metadata("type", compactV2String), compactV2Metadata("from", compactV2String),
		compactV2Metadata("to", compactV2String), compactV2Metadata("file", compactV2String),
		compactV2Metadata("start", compactV2Int), compactV2Metadata("end", compactV2Int),
		compactV2Metadata("path", compactV2String), compactV2Metadata("scope", compactV2String),
		compactV2Metadata("resolution", compactV2String), compactV2Metadata("target_kind", compactV2String),
		compactV2Metadata("confidence", compactV2Float), compactV2Natural("reason", compactV2String),
		compactV2Metadata("warning_codes", compactV2Strings))
}

func compactV2EvidenceSchema(tag string, opcode byte) compactV2Schema {
	return compactV2SchemaFor(tag, opcode,
		compactV2Metadata("owner_index", compactV2Int), compactV2Metadata("index", compactV2Int),
		compactV2Metadata("kind", compactV2String), compactV2Metadata("file", compactV2String),
		compactV2Metadata("start", compactV2Int), compactV2Metadata("end", compactV2Int),
		compactV2Natural("detail", compactV2String))
}

func compactV2WarningSchema(tag string, opcode byte) compactV2Schema {
	return compactV2SchemaFor(tag, opcode,
		compactV2Metadata("code", compactV2String), compactV2Metadata("severity", compactV2String),
		compactV2Metadata("path", compactV2String), compactV2Natural("effect", compactV2String),
		compactV2Natural("detail", compactV2String))
}

// emitBrainBriefCompactV2 transcodes the exact compact_v1 semantic record
// stream. Keeping v1 as the projection source makes field selection, record
// ordering, duplicates, and privacy omissions identical by construction while
// v2 changes only their wire representation.
func emitBrainBriefCompactV2(cmd *cobra.Command, report brainBriefReport) error {
	if err := validateBrainBriefCompactNumbers(report, "compact_v2"); err != nil {
		return err
	}
	var v1 strings.Builder
	v1cmd := &cobra.Command{}
	v1cmd.SetOut(&v1)
	if err := emitBrainBriefCompactV1(v1cmd, report); err != nil {
		return err
	}
	packet, err := transcodeBrainBriefCompactV2(v1.String())
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), packet)
	return err
}

func transcodeBrainBriefCompactV2(v1 string) (string, error) {
	records, err := parseBrainBriefCompactV1Body(v1)
	if err != nil {
		return "", err
	}
	schemasByTag := make(map[string]compactV2Schema, len(compactV2Schemas))
	for _, schema := range compactV2Schemas {
		if _, exists := schemasByTag[schema.tag]; exists {
			return "", fmt.Errorf("compact_v2 duplicate schema tag %q", schema.tag)
		}
		schemasByTag[schema.tag] = schema
	}
	for _, record := range records {
		if _, ok := schemasByTag[record.tag]; !ok {
			return "", fmt.Errorf("compact_v2 has no schema for %q", record.tag)
		}
	}
	plans, err := planCompactV2Families(records, schemasByTag)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	out.Grow(len(v1))
	out.WriteString(brainBriefCompactV2Marker)
	out.WriteByte('\n')
	out.WriteString(brainBriefCompactV2Legend)
	out.WriteByte('\n')
	positions := make(map[string]int, len(plans))
	for _, record := range records {
		plan := plans[record.tag]
		if !plan.positional {
			out.WriteString(record.raw)
			out.WriteByte('\n')
			continue
		}
		position := positions[record.tag]
		if position == 0 {
			out.WriteString(plan.declaration)
		}
		out.WriteString(plan.rows[position])
		positions[record.tag] = position + 1
	}
	body := out.String()
	digest := sha256.Sum256([]byte(body))
	fmt.Fprintf(&out, "end\t%d\t%x\n", len(records), digest)
	return out.String(), nil
}

// planCompactV2Families compares the exact canonical bytes for each complete
// record family. A repeated family uses positional rows only when declaration
// plus row bytes are strictly smaller than its keyed v1 rows. Equal sizes are
// keyed, which freezes a deterministic tie rule and prevents declarations from
// ever making a family grow.
func planCompactV2Families(records []compactV1RawRecord, schemasByTag map[string]compactV2Schema) (map[string]compactV2FamilyPlan, error) {
	type familyState struct {
		plan            compactV2FamilyPlan
		previous        []string
		keyedBytes      int
		positionalBytes int
		records         int
	}
	counts := make(map[string]int, len(schemasByTag))
	for _, record := range records {
		counts[record.tag]++
	}
	states := make(map[string]familyState, len(schemasByTag))
	for _, record := range records {
		schema := schemasByTag[record.tag]
		state := states[record.tag]
		if state.records == 0 {
			state.plan.declaration = schema.declaration
			state.plan.rows = make([]string, 0, counts[record.tag])
			state.positionalBytes = len(schema.declaration)
		}
		row, cells, err := encodeCompactV2Row(schema, record, state.previous)
		if err != nil {
			return nil, err
		}
		state.plan.rows = append(state.plan.rows, row)
		state.previous = cells
		state.keyedBytes += len(record.raw) + 1
		state.positionalBytes += len(row)
		state.records++
		states[record.tag] = state
	}

	plans := make(map[string]compactV2FamilyPlan, len(states))
	for tag, state := range states {
		state.plan.positional = compactV2UsePositional(state.records, state.keyedBytes, state.positionalBytes)
		plans[tag] = state.plan
	}
	return plans, nil
}

func compactV2UsePositional(recordCount, keyedBytes, positionalBytes int) bool {
	return recordCount > 1 && positionalBytes < keyedBytes
}

func encodeCompactV2Row(schema compactV2Schema, record compactV1RawRecord, previous []string) (string, []string, error) {
	cells, last, err := compactV2Cells(schema, record)
	if err != nil {
		return "", nil, err
	}
	var out strings.Builder
	rowBytes := 3 + last // opcode, tabs, and trailing newline
	for i := 0; i <= last; i++ {
		rowBytes += len(cells[i])
	}
	out.Grow(rowBytes)
	out.WriteByte(schema.opcode)
	for i := 0; i <= last; i++ {
		out.WriteByte('\t')
		cell := cells[i]
		if schema.fields[i].reference && cell != "~" && len(previous) == len(cells) && previous[i] == cell && len(cell) > 1 {
			out.WriteByte('^')
		} else {
			out.WriteString(cell)
		}
	}
	out.WriteByte('\n')
	return out.String(), cells, nil
}

func writeCompactV2Declaration(out *strings.Builder, schema compactV2Schema) {
	out.WriteByte('@')
	out.WriteByte(schema.opcode)
	out.WriteByte('=')
	out.WriteString(schema.tag)
	out.WriteByte('(')
	for i, field := range schema.fields {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(field.name)
	}
	out.WriteString(")\n")
}

func compactV2Cells(schema compactV2Schema, record compactV1RawRecord) ([]string, int, error) {
	cells := make([]string, len(schema.fields))
	for i := range cells {
		cells[i] = "~"
	}
	last := -1
	for _, field := range record.fields {
		position, ok := schema.fieldPositions[field.key]
		if !ok {
			return nil, -1, fmt.Errorf("compact_v2 %s has unknown field %q", record.tag, field.key)
		}
		if position <= last {
			return nil, -1, fmt.Errorf("compact_v2 %s fields are not in schema order", record.tag)
		}
		encoded, err := encodeCompactV2Cell(schema.fields[position].kind, field.value)
		if err != nil {
			return nil, -1, fmt.Errorf("compact_v2 %s.%s: %w", record.tag, field.key, err)
		}
		cells[position] = encoded
		last = position
	}
	return cells, last, nil
}

func encodeCompactV2Cell(kind compactV2FieldKind, raw string) (string, error) {
	switch kind {
	case compactV2String:
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", fmt.Errorf("invalid quoted string")
		}
		if compactV2SafeAtom(value) {
			return value, nil
		}
		return raw, nil
	case compactV2Strings:
		return encodeCompactV2StringArray(raw)
	case compactV2Bool:
		switch raw {
		case "true":
			return "1", nil
		case "false":
			return "0", nil
		default:
			return "", fmt.Errorf("invalid boolean")
		}
	case compactV2Int:
		if _, err := strconv.Atoi(raw); err != nil {
			return "", fmt.Errorf("invalid integer")
		}
		return raw, nil
	case compactV2Float:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("invalid finite float")
		}
		return raw, nil
	default:
		return "", fmt.Errorf("unknown field kind")
	}
}

func compactV2SafeAtom(value string) bool {
	if value == "" || value == "~" || value == "^" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' && i > 0 {
			continue
		}
		switch c {
		case '_', '.', '/', ':', '@', '+', '-':
			continue
		default:
			return false
		}
	}
	return true
}

func encodeCompactV2StringArray(raw string) (string, error) {
	values, err := parseCompactV1StringArray(raw)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.Grow(len(raw))
	out.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			out.WriteByte(',')
		}
		if compactV2SafeAtom(value) {
			out.WriteString(value)
		} else {
			out.WriteString(strconv.Quote(value))
		}
	}
	out.WriteByte(']')
	return out.String(), nil
}

func parseCompactV1StringArray(raw string) ([]string, error) {
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, fmt.Errorf("invalid string array")
	}
	if raw == "[]" {
		return nil, nil
	}
	var values []string
	for cursor := 1; cursor < len(raw)-1; {
		if raw[cursor] != '"' {
			return nil, fmt.Errorf("invalid quoted array element")
		}
		end, err := scanCompactQuoted(raw, cursor)
		if err != nil || end > len(raw)-1 {
			return nil, fmt.Errorf("invalid quoted array element")
		}
		value, err := strconv.Unquote(raw[cursor:end])
		if err != nil {
			return nil, fmt.Errorf("invalid quoted array element")
		}
		values = append(values, value)
		cursor = end
		if cursor == len(raw)-1 {
			break
		}
		if raw[cursor] != ',' {
			return nil, fmt.Errorf("invalid array separator")
		}
		cursor++
		if cursor == len(raw)-1 {
			return nil, fmt.Errorf("trailing array separator")
		}
	}
	return values, nil
}

func parseBrainBriefCompactV1Body(packet string) ([]compactV1RawRecord, error) {
	if !strings.HasSuffix(packet, "\n") {
		return nil, fmt.Errorf("compact_v1 packet has no final newline")
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	if len(lines) < 3 || lines[0] != brainBriefCompactV1Marker {
		return nil, fmt.Errorf("compact_v1 marker mismatch")
	}
	end, err := parseCompactV1RawRecord(lines[len(lines)-1])
	if err != nil || end.tag != "end" {
		return nil, fmt.Errorf("compact_v1 end record missing")
	}
	records := make([]compactV1RawRecord, 0, len(lines)-2)
	for _, line := range lines[1 : len(lines)-1] {
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func parseCompactV1RawRecord(line string) (compactV1RawRecord, error) {
	if line == "" {
		return compactV1RawRecord{}, fmt.Errorf("empty compact_v1 record")
	}
	tagEnd := strings.IndexByte(line, ' ')
	if tagEnd < 0 {
		return compactV1RawRecord{tag: line, raw: line}, nil
	}
	record := compactV1RawRecord{tag: line[:tagEnd], raw: line}
	for cursor := tagEnd + 1; cursor < len(line); {
		keyStart := cursor
		for cursor < len(line) && line[cursor] != '=' && line[cursor] != ' ' {
			cursor++
		}
		if cursor == keyStart || cursor >= len(line) || line[cursor] != '=' {
			return compactV1RawRecord{}, fmt.Errorf("invalid compact_v1 field in %q", record.tag)
		}
		key := line[keyStart:cursor]
		cursor++
		valueStart := cursor
		if cursor >= len(line) {
			return compactV1RawRecord{}, fmt.Errorf("missing compact_v1 value in %q", record.tag)
		}
		var err error
		switch line[cursor] {
		case '"':
			cursor, err = scanCompactQuoted(line, cursor)
		case '[':
			cursor, err = scanCompactArray(line, cursor)
		default:
			for cursor < len(line) && line[cursor] != ' ' {
				cursor++
			}
		}
		if err != nil || cursor == valueStart {
			return compactV1RawRecord{}, fmt.Errorf("invalid compact_v1 value in %q", record.tag)
		}
		record.fields = append(record.fields, compactV1RawField{key: key, value: line[valueStart:cursor]})
		if cursor == len(line) {
			break
		}
		if line[cursor] != ' ' || cursor+1 == len(line) || line[cursor+1] == ' ' {
			return compactV1RawRecord{}, fmt.Errorf("invalid compact_v1 field separator in %q", record.tag)
		}
		cursor++
	}
	return record, nil
}

func scanCompactQuoted(value string, start int) (int, error) {
	for cursor := start + 1; cursor < len(value); cursor++ {
		switch value[cursor] {
		case '\\':
			cursor++
			if cursor >= len(value) {
				return 0, fmt.Errorf("unterminated quoted value")
			}
		case '"':
			return cursor + 1, nil
		case '\n', '\r':
			return 0, fmt.Errorf("unescaped line break")
		}
	}
	return 0, fmt.Errorf("unterminated quoted value")
}

func scanCompactArray(value string, start int) (int, error) {
	for cursor := start + 1; cursor < len(value); cursor++ {
		switch value[cursor] {
		case '"':
			end, err := scanCompactQuoted(value, cursor)
			if err != nil {
				return 0, err
			}
			cursor = end - 1
		case ']':
			return cursor + 1, nil
		case '\n', '\r':
			return 0, fmt.Errorf("unescaped line break")
		}
	}
	return 0, fmt.Errorf("unterminated array value")
}

const (
	brainBriefCompactV3Marker = "entire.brain_brief c3"
	// V3 retains v2's record schemas, body grammar, and strictly-shorter family
	// selection. It has a compact marker, permits the existing exact-value
	// repeat marker in every column, and uses an unpadded base64url checksum.
	brainBriefCompactV3Legend = brainBriefCompactV2Legend
)

// compactV3Schemas copies the frozen v2 positional schema and widens the
// repeat-reference allowlist. Field order, types, opcodes, and declarations
// remain byte-for-byte identical. A repeat marker is emitted only for an exact
// prior value in the same opcode and column; unique values remain inline.
var compactV3Schemas = compactV3AllReferenceSchemas()

// compactV3SchemasByTag is immutable after initialization and safe for
// concurrent emitters. Keeping the versioned lookup here also validates the
// frozen schema once instead of rebuilding the same map for every packet.
var compactV3SchemasByTag = compactV3SchemaLookup()

func compactV3AllReferenceSchemas() []compactV2Schema {
	schemas := make([]compactV2Schema, len(compactV2Schemas))
	for i, schema := range compactV2Schemas {
		schemas[i] = schema
		schemas[i].fields = append([]compactV2Field(nil), schema.fields...)
		for j := range schemas[i].fields {
			schemas[i].fields[j].reference = true
		}
	}
	return schemas
}

func compactV3SchemaLookup() map[string]compactV2Schema {
	byTag := make(map[string]compactV2Schema, len(compactV3Schemas))
	for _, schema := range compactV3Schemas {
		if _, exists := byTag[schema.tag]; exists {
			panic(fmt.Sprintf("compact_v3 duplicate schema tag %q", schema.tag))
		}
		byTag[schema.tag] = schema
	}
	return byTag
}

// emitBrainBriefCompactV3 transcodes the same compact_v1 canonical solving
// projection as compact_v2. V3 remains opt-in; no default selects it.
func emitBrainBriefCompactV3(cmd *cobra.Command, report brainBriefReport) error {
	if err := validateBrainBriefCompactNumbers(report, "compact_v3"); err != nil {
		return err
	}
	var v1 strings.Builder
	v1cmd := &cobra.Command{}
	v1cmd.SetOut(&v1)
	if err := emitBrainBriefCompactV1(v1cmd, report); err != nil {
		return err
	}
	packet, err := transcodeBrainBriefCompactV3(v1.String())
	if err != nil {
		return err
	}
	_, err = io.WriteString(cmd.OutOrStdout(), packet)
	return err
}

func transcodeBrainBriefCompactV3(v1 string) (string, error) {
	records, err := parseBrainBriefCompactV1Body(v1)
	if err != nil {
		return "", fmt.Errorf("compact_v3: %w", err)
	}
	for _, record := range records {
		if _, ok := compactV3SchemasByTag[record.tag]; !ok {
			return "", fmt.Errorf("compact_v3 has no schema for %q", record.tag)
		}
	}
	plans, err := planCompactV2Families(records, compactV3SchemasByTag)
	if err != nil {
		return "", fmt.Errorf("compact_v3: %w", err)
	}

	var out strings.Builder
	out.Grow(len(v1))
	out.WriteString(brainBriefCompactV3Marker)
	out.WriteByte('\n')
	out.WriteString(brainBriefCompactV3Legend)
	out.WriteByte('\n')
	positions := make(map[string]int, len(plans))
	for _, record := range records {
		plan := plans[record.tag]
		if !plan.positional {
			out.WriteString(record.raw)
			out.WriteByte('\n')
			continue
		}
		position := positions[record.tag]
		if position == 0 {
			out.WriteString(plan.declaration)
		}
		out.WriteString(plan.rows[position])
		positions[record.tag] = position + 1
	}
	body := out.String()
	digest := sha256.Sum256([]byte(body))
	fmt.Fprintf(&out, "end\t%d\t%s\n", len(records), base64.RawURLEncoding.EncodeToString(digest[:]))
	return out.String(), nil
}

func renderBrainBriefCompactV3ForTest(t *testing.T, report brainBriefReport) string {
	t.Helper()
	var out strings.Builder
	cmd := &cobraCommandForCompactV2Test{out: &out}
	if err := emitBrainBriefCompactV3(cmd.command(), report); err != nil {
		t.Fatalf("render compact_v3: %v", err)
	}
	return out.String()
}

func renderBrainBriefCompactV2ForTest(t *testing.T, report brainBriefReport) string {
	t.Helper()
	var out strings.Builder
	cmd := &cobraCommandForCompactV2Test{out: &out}
	if err := emitBrainBriefCompactV2(cmd.command(), report); err != nil {
		t.Fatalf("render compact_v2: %v", err)
	}
	return out.String()
}

func renderBrainBriefCompactV1ForTest(t *testing.T, report brainBriefReport) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := emitBrainBriefCompactV1(cmd, report); err != nil {
		t.Fatalf("render compact_v1: %v", err)
	}
	return out.String()
}

// This tiny wrapper keeps test setup local without changing production command
// construction helpers.
type cobraCommandForCompactV2Test struct {
	out *strings.Builder
}

func (c *cobraCommandForCompactV2Test) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(c.out)
	return cmd
}

func compactPacketByteProxy(packet string) int {
	if packet == "" {
		return 0
	}
	return (len(packet) + 3) / 4
}

func comprehensiveCompactV1Report() brainBriefReport {
	symbol := semanticRecord{
		ID: "sym:validate", Kind: "function", Name: "Validate", QualifiedName: "pkg.Validate",
		FilePath: "internal/a.go", StartLine: 10, EndLine: 20, Signature: "func Validate(token string) error",
		Language: "go", Score: 9, Confidence: 0.95, Reason: "task term match", WarningCodes: []string{"partial"},
	}
	relation := semanticRecord{
		ID: "rel:1", Type: "CALLS", FromID: "sym:validate", ToID: "sym:persist", FilePath: "internal/a.go",
		StartLine: 16, EndLine: 16, RelationScope: "file", Resolution: "exact", TargetKind: "symbol", Confidence: 0.9,
		Evidence: []semanticEvidence{{Kind: "call", FilePath: "internal/a.go", StartLine: 16, EndLine: 16, Detail: "direct call"}},
	}
	runtimeTrace := semanticRecord{
		ID: "runtime:1", Type: "RUNTIME_TRACE", FromID: "sym:validate", ToID: "sym:persist", Reason: "observed edge",
		Evidence: []semanticEvidence{{Kind: "trace", FilePath: "trace.ndjson", StartLine: 2, EndLine: 2, Detail: "captured locally"}},
	}
	return brainBriefReport{
		GeneratedAt: time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC),
		Task:        "repair validation ordering",
		Status: brainStatusReport{
			Repo:    brainStatusRepo{Root: "/repo", Key: "repo-key"},
			Brain:   brainStatusBrain{Path: "/brain", Schema: 2, GeneratedAt: "2026-07-16T09:00:00Z"},
			Sources: brainStatusSources{Seed: true, Sessions: true, Semantic: true, History: true, Facts: true},
			Facts: &brainStatusFacts{
				Facts: 4, Distilled: 3, Authored: 1, Superseded: 1,
				Verification: &verifySummary{Facts: 3, Verified: 2, Stale: 1, Orphaned: 0, UnverifiableHere: 0, SampledOf: 4},
			},
			Semantic: &brainStatusSemantic{
				Provider: &brainStatusSemanticProvider{Name: "entire-graph", Version: "1.2", Schema: "1.1", Snapshot: "/snapshot", Store: "/store", Capabilities: []string{"relations", "tests"}},
				Coverage: &brainStatusSemanticCoverage{
					Files: 12, Symbols: 30, Relations: 22, Warnings: 1, PartialFailures: 1,
					WarningDetails:        []semanticWarning{{Code: "WARN", Severity: "warning", Path: "warn.go", Effect: "partial", Detail: "unsupported syntax"}},
					PartialFailureDetails: []semanticWarning{{Code: "FAIL", Severity: "error", Path: "broken.go", Effect: "missing symbols", Detail: "parse failed"}},
				},
				Freshness: &staleReport{
					Severity: "degraded",
					Axes: map[string]staleAxis{
						"head":     {State: "ok"},
						"worktree": {State: "dirty", Detail: "live edits"},
					},
					Warnings: []semanticWarning{{Code: "STALE", Severity: "warning", Detail: "worktree differs"}},
				},
				BlindSpots: []brainBlindSpot{{Path: "generated.go", Code: "ignored", Detail: "excluded by policy"}},
			},
			Live: brainLiveState{
				Branch: "feature", Head: "abc123", Dirty: true,
				Staged: []string{"internal/a.go"}, Unstaged: []string{"internal/a_test.go"}, Untracked: []string{"notes.txt"},
				ChangedFiles: []string{"internal/a.go", "internal/a_test.go", "notes.txt"}, DiffStat: "2 files changed",
				ChangedSymbolHints: []semanticRecord{symbol}, Warnings: []string{"live warning"},
			},
			Warnings: []string{"status warning"},
		},
		Semantic: brainBriefSemantic{
			Context: semanticContextResult{
				Symbols: []semanticRecord{symbol}, Relations: []semanticRecord{relation},
				Neighbors: []semanticRecord{{ID: "sym:helper", Kind: "function", Name: "Helper", QualifiedName: "pkg.Helper", FilePath: "internal/helper.go", StartLine: 4, EndLine: 8}},
			},
			RuntimeTraces: []semanticRecord{runtimeTrace},
			Tests: semanticTestsResult{
				Roots:       []semanticRecord{{ID: "test:root", Kind: "function", Name: "TestValidate", FilePath: "internal/a_test.go", StartLine: 10, EndLine: 30}},
				Suggestions: []semanticTestSuggestion{{Symbol: semanticRecord{ID: "test:suggestion", Kind: "function", Name: "TestValidateOrder", FilePath: "internal/a_test.go", StartLine: 32, EndLine: 44}, Reason: "covers the boundary"}},
			},
			// The all-sections fixture carries an ancestor-served answer so the
			// exhaustive golden and the per-tag counts cover the staleness
			// disclosure the same way they cover every other retained record.
			Staleness: &semanticContextStaleness{
				AncestorCommit: "ancestor1", CommitsBehind: 2,
				ChangedFiles: []string{"internal/a.go", "internal/b.go"},
				Unreliable:   []string{"internal/a.go"},
				// Non-zero on purpose: zero here would let the renderer omit the
				// count without the golden noticing.
				ExcludedRecords: 12,
			},
		},
		Facts: []factRecord{{
			ID: "fact:1", Paths: []string{"architecture.validation.order"}, Kind: "invariant", Locus: []string{"pkg.Validate"},
			Text: "Validation runs before persistence.", Branch: "feature", Origin: "distilled", Status: "active", Confidence: "high",
			Provenance: []factAnchor{{SessionID: "private-session", CheckpointID: "private-checkpoint", Verified: true}}, RelatedIDs: []string{"fact:2"},
		}},
		FactsLocusDrift: map[string][]string{"fact:1": {"OldValidate"}, "fact:orphan": {"GoneSymbol"}},
		ActionChecklist: []brainBriefAction{{File: "internal/a.go", Symbol: "pkg.Validate", Action: "inspect validation before persistence", Evidence: "history and current code"}},
		LikelyEditFiles: []string{"internal/a.go"}, LikelyTestFiles: []string{"internal/a_test.go"}, LikelyFiles: []string{"internal/a.go", "internal/a_test.go", "docs/order.md"},
		Patterns: []patternView{{
			ID: "pattern:1", Type: "procedure", Scope: "repo", Kind: "workflow", Title: "Validate then persist", Strength: 0.88, StrengthLabel: "strong", Support: 5,
			Reinforcement: &reinforcementCounts{Success: 4, Corrected: 1}, Example: &episodeAnchor{Path: "patterns/episodes.ndjson", Line: 12}, Note: "Reuse the validated sequence.",
			IntentSig: "validation ordering", Gram: "validate>persist", DossierStatus: "current", Verdict: "accepted",
		}},
		Consolidations: []briefConsolidation{{
			PatternID: "pattern:1", Type: "procedure", Title: "Validate then persist", Trigger: "when validation changes", Confidence: 0.88, Status: "current", Verdict: "accepted",
			Workflow: []string{"validate input", "persist state"}, Verification: []string{"run validation tests"}, FailureModes: []string{"persisting first"},
			Anchor: &patterncore.DossierAnchor{SessionID: "private-session", CheckpointID: "private-checkpoint", Transcript: "sessions/redacted.jsonl", StartLine: 10, EndLine: 14, Outcome: "success"},
		}},
		Themes:   []themeView{{ID: "theme:1", Title: "Read before edit", Description: "Inspect before editing.", Shape: "read_only", Support: 4, Strength: 0.75, Status: "current", Verdict: "accepted"}},
		Guidance: []string{"Inspect dirty files before trusting the snapshot."},
		Warnings: []string{"brief warning"},
	}
}
