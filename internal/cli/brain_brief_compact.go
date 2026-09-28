package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// brainBriefPacketFormat is an internal render choice. MCP exposes the stable
// names legacy_json, compact_v1, compact_v2, and compact_v3; the CLI keeps its
// existing text/json flags.
type brainBriefPacketFormat string

const (
	brainBriefPacketText       brainBriefPacketFormat = "text"
	brainBriefPacketLegacyJSON brainBriefPacketFormat = "json"
	brainBriefPacketCompactV1  brainBriefPacketFormat = "compact_v1"
	brainBriefPacketCompactV2  brainBriefPacketFormat = "compact_v2"
	brainBriefPacketCompactV3  brainBriefPacketFormat = "compact_v3"
)

const brainBriefCompactV1Marker = "entire.brain_brief compact_v1"

func (opts brainBriefOptions) resolvedPacketFormat() brainBriefPacketFormat {
	if opts.packetFormat != "" {
		return opts.packetFormat
	}
	if opts.json {
		return brainBriefPacketLegacyJSON
	}
	return brainBriefPacketText
}

func emitBrainBriefPacket(cmd *cobra.Command, report brainBriefReport, format brainBriefPacketFormat) error {
	switch format {
	case brainBriefPacketText:
		return emitBrainBriefReport(cmd, report, false)
	case brainBriefPacketLegacyJSON:
		return emitBrainBriefReport(cmd, report, true)
	case brainBriefPacketCompactV1:
		return emitBrainBriefCompactV1(cmd, report)
	case brainBriefPacketCompactV2:
		return emitBrainBriefCompactV2(cmd, report)
	case brainBriefPacketCompactV3:
		return emitBrainBriefCompactV3(cmd, report)
	default:
		return fmt.Errorf("unsupported brain brief packet format: %q", format)
	}
}

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
			compactV1IntAlways("superseded", facts.Superseded),
			compactV1IntAlways("branches", facts.Branches),
			compactV1IntAlways("proposals", facts.Proposals),
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

	for _, match := range report.History.Matches {
		compactV1Record(&out, "history",
			compactV1StringAlways("path", match.Path),
			compactV1IntAlways("line", match.Line),
			compactV1String("timestamp", match.Timestamp),
			compactV1Int("score", match.Score),
			compactV1Strings("matched_terms", match.MatchedTerms),
			compactV1StringAlways("excerpt", match.Excerpt),
		)
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
		if notice, ok := report.FactsPendingReview[fact.ID]; ok {
			// Reuse the existing warning family so every compact version retains
			// the trust notice without changing its positional fact schema.
			compactV1Record(&out, "warning", compactV1StringAlways("source", fact.ID),
				compactV1StringAlways("text", factReviewNoticeLine(notice)+" "+notice.Message))
		}
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
		compactV1IntAlways("history", len(report.History.Matches)),
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
	for _, fact := range report.Facts {
		if _, ok := report.FactsPendingReview[fact.ID]; ok {
			count++
		}
	}
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
