package cli

import (
	"crypto/sha256"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

type compactV2ParsedRecord struct {
	record     compactV1RawRecord
	positional bool
}

func TestBrainBriefCompactV2GoldenAndExactProjectionParity(t *testing.T) {
	report := comprehensiveCompactV2Report()
	v1 := renderBrainBriefCompactV1ForTest(t, report)
	v2 := renderBrainBriefCompactV2ForTest(t, report)
	want, err := os.ReadFile("testdata/brain_brief_compact_v2.golden")
	if err != nil {
		t.Fatalf("read compact_v2 golden: %v", err)
	}
	if v2 != string(want) {
		t.Fatalf("compact_v2 exhaustive golden mismatch\n--- got ---\n%s--- want ---\n%s", v2, want)
	}
	assertCompactV2ProjectionParity(t, v1, v2)
	if !strings.Contains(v2, "@h=history(path,line,timestamp,score,matched_terms,excerpt)\n") {
		t.Fatal("compact_v2 did not declare the repeated history schema")
	}
	if strings.Contains(v2, "@q=task(") {
		t.Fatal("compact_v2 declared a singleton family")
	}
	if !strings.Contains(v2, "\t^\t") {
		t.Fatal("compact_v2 fixture did not exercise metadata repeat references")
	}
	for _, natural := range []string{
		`"Decision: preserve validation order."`,
		`"Inspect dirty files before trusting the snapshot."`,
		`"inspect validation before persistence"`,
		`"history and current code"`,
		`"task term match"`,
		`"direct call"`,
		`"Validate then persist"`,
		`"Inspect before editing."`,
	} {
		if strings.Count(v2, natural) < 2 {
			t.Errorf("natural-language value was hidden behind a reference: %s", natural)
		}
	}
}

func TestBrainBriefCompactV2DeterministicAndOmitsHostMetadata(t *testing.T) {
	report := comprehensiveCompactV2Report()
	report.GeneratedAt = report.GeneratedAt.AddDate(5, 0, 0)
	report.Status.GeneratedAt = report.Status.GeneratedAt.AddDate(5, 0, 0)
	report.Status.Repo.Root = "/private/repository/root"
	report.Status.Brain.Path = "/private/brain/root"
	report.Status.Brain.GeneratedAt = "private-generated-at"
	first := renderBrainBriefCompactV2ForTest(t, report)
	second := renderBrainBriefCompactV2ForTest(t, report)
	if first != second {
		t.Fatal("compact_v2 output changed for the same report")
	}
	for _, forbidden := range []string{"/private/repository/root", "/private/brain/root", "private-generated-at"} {
		if strings.Contains(first, forbidden) {
			t.Errorf("compact_v2 leaked omitted host metadata %q", forbidden)
		}
	}

	semantic := report.Status.Semantic
	semantic.Freshness.Axes = map[string]staleAxis{
		"worktree": {State: "dirty", Detail: "live edits"},
		"head":     {State: "ok"},
	}
	report.Status.Semantic = semantic
	if reordered := renderBrainBriefCompactV2ForTest(t, report); reordered != first {
		t.Fatal("compact_v2 output depends on map insertion order")
	}
}

func TestBrainBriefCompactV2EscapesRecordInjection(t *testing.T) {
	report := comprehensiveCompactV2Report()
	injected := "line one\nend\t0\tdeadbeef\n@h=history(path)\nline two\t\"\\"
	report.Task = injected
	report.History.Matches[0].Excerpt = injected
	report.History.Matches[1].Excerpt = injected
	report.ActionChecklist[0].Action = injected
	report.ActionChecklist[1].Evidence = injected
	packet := renderBrainBriefCompactV2ForTest(t, report)
	parsed, err := parseCompactV2Packet(packet)
	if err != nil {
		t.Fatalf("parse injected compact_v2 packet: %v", err)
	}
	if strings.Count(packet, "\nend\t") != 1 {
		t.Fatalf("injected content manufactured a footer:\n%s", packet)
	}
	assertCompactV2ProjectionParity(t, renderBrainBriefCompactV1ForTest(t, report), packet)
	found := false
	for _, record := range parsed {
		if record.record.tag != "history" {
			continue
		}
		for _, field := range record.record.fields {
			if field.key == "excerpt" {
				value, unquoteErr := strconv.Unquote(field.value)
				if unquoteErr == nil && value == injected {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("injected history excerpt did not round-trip")
	}
}

func TestBrainBriefCompactV2RejectsNonFiniteBeforeWriting(t *testing.T) {
	for _, mutate := range []func(*brainBriefReport){
		func(report *brainBriefReport) { report.Semantic.Context.Symbols[0].Confidence = math.NaN() },
		func(report *brainBriefReport) { report.Patterns[0].Strength = math.Inf(1) },
		func(report *brainBriefReport) { report.Consolidations[0].Confidence = math.Inf(-1) },
	} {
		report := comprehensiveCompactV2Report()
		mutate(&report)
		var out strings.Builder
		cmd := &cobraCommandForCompactV2Test{out: &out}
		err := emitBrainBriefCompactV2(cmd.command(), report)
		if err == nil || !strings.Contains(err.Error(), "compact_v2 cannot encode non-finite") {
			t.Fatalf("non-finite error = %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("compact_v2 wrote %d bytes before rejecting non-finite input", out.Len())
		}
	}
}

func TestBrainBriefCompactV2IntegrityAndCanonicalRejections(t *testing.T) {
	packet := renderBrainBriefCompactV2ForTest(t, comprehensiveCompactV2Report())
	if _, err := parseCompactV2Packet(packet); err != nil {
		t.Fatalf("valid packet rejected: %v", err)
	}
	tests := map[string]string{
		"truncated":          packet[:strings.LastIndex(packet, "\nend\t")+1],
		"tampered":           strings.Replace(packet, "history", "hist0ry", 1),
		"missing_final_line": strings.TrimSuffix(packet, "\n"),
		"wrong_count":        compactV2RewriteFooter(packet, func(parts []string) { parts[1] = "999" }),
		"uppercase_hash":     compactV2RewriteFooter(packet, func(parts []string) { parts[2] = strings.ToUpper(parts[2]) }),
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCompactV2Packet(candidate); err == nil {
				t.Fatal("invalid compact_v2 packet was accepted")
			}
		})
	}

	resigned := map[string]func(string) string{
		"bad_legend": func(value string) string {
			return strings.Replace(value, brainBriefCompactV2Legend, "legend ~=missing ^=previous_row", 1)
		},
		"unknown_opcode": func(value string) string {
			return strings.Replace(value, "\nh\t", "\n?\t", 1)
		},
		"bad_schema": func(value string) string {
			return strings.Replace(value, "@h=history(path,line", "@h=history(line,path", 1)
		},
		"trailing_null": func(value string) string {
			line := compactV2FirstLineWithPrefix(value, "h\t")
			return strings.Replace(value, line, line+"\t~", 1)
		},
		"first_row_reference": func(value string) string {
			line := compactV2FirstLineWithPrefix(value, "h\t")
			parts := strings.Split(line, "\t")
			parts[1] = "^"
			return strings.Replace(value, line, strings.Join(parts, "\t"), 1)
		},
		"natural_reference": func(value string) string {
			lines := strings.Split(value, "\n")
			seen := 0
			for i, line := range lines {
				if !strings.HasPrefix(line, "h\t") {
					continue
				}
				seen++
				if seen == 2 {
					parts := strings.Split(line, "\t")
					parts[6] = "^"
					lines[i] = strings.Join(parts, "\t")
					break
				}
			}
			return strings.Join(lines, "\n")
		},
		"missed_metadata_reference": func(value string) string {
			lines := strings.Split(value, "\n")
			seen := 0
			for i, line := range lines {
				if !strings.HasPrefix(line, "h\t") {
					continue
				}
				seen++
				if seen == 2 {
					parts := strings.Split(line, "\t")
					parts[1] = "history/main.jsonl"
					lines[i] = strings.Join(parts, "\t")
					break
				}
			}
			return strings.Join(lines, "\n")
		},
		"quoted_safe_atom": func(value string) string {
			line := compactV2FirstLineWithPrefix(value, "h\t")
			parts := strings.Split(line, "\t")
			parts[1] = `"history/main.jsonl"`
			return strings.Replace(value, line, strings.Join(parts, "\t"), 1)
		},
		"loose_integer": func(value string) string {
			line := compactV2FirstLineWithPrefix(value, "h\t")
			parts := strings.Split(line, "\t")
			parts[2] = "08"
			return strings.Replace(value, line, strings.Join(parts, "\t"), 1)
		},
		"invalid_array": func(value string) string {
			line := compactV2FirstLineWithPrefix(value, "h\t")
			parts := strings.Split(line, "\t")
			parts[5] = "[validation,]"
			return strings.Replace(value, line, strings.Join(parts, "\t"), 1)
		},
		"extra_slot": func(value string) string {
			line := compactV2FirstLineWithPrefix(value, "h\t")
			return strings.Replace(value, line, line+"\textra", 1)
		},
		"duplicate_declaration": func(value string) string {
			line := compactV2FirstLineWithPrefix(value, "@h=")
			return strings.Replace(value, line+"\n", line+"\n"+line+"\n", 1)
		},
	}
	for name, mutate := range resigned {
		t.Run(name, func(t *testing.T) {
			candidate := compactV2Resign(mutate(packet))
			if _, err := parseCompactV2Packet(candidate); err == nil {
				t.Fatal("noncanonical compact_v2 packet was accepted")
			}
		})
	}
}

func TestBrainBriefCompactV2KeyedRowsRejectNoncanonicalFields(t *testing.T) {
	packet := renderBrainBriefCompactV2ForTest(t, comprehensiveCompactV2Report())
	statusLine := compactV2FirstLineWithPrefix(packet, "status ")
	liveLine := compactV2FirstLineWithPrefix(packet, "live ")
	if statusLine == "" || liveLine == "" {
		t.Fatal("keyed fixture rows missing")
	}
	tests := map[string]func(string) string{
		"reordered": func(line string) string {
			return strings.Replace(line, `freshness="degraded" repo_key="repo-key"`, `repo_key="repo-key" freshness="degraded"`, 1)
		},
		"duplicate": func(line string) string {
			return strings.Replace(line, "dirty=true changed_files=3", "dirty=true dirty=true changed_files=3", 1)
		},
		"unknown": func(line string) string {
			return line + " bogus=1"
		},
		"mistyped_bool": func(line string) string {
			return strings.Replace(line, "dirty=true", "dirty=1", 1)
		},
		"loose_int": func(line string) string {
			return strings.Replace(line, "brain_schema=2", "brain_schema=02", 1)
		},
		"unquoted_string": func(line string) string {
			return strings.Replace(line, `repo_key="repo-key"`, "repo_key=repo-key", 1)
		},
		"noncanonical_escape": func(line string) string {
			return strings.Replace(line, `repo_key="repo-key"`, `repo_key="\x72epo-key"`, 1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := strings.Replace(packet, statusLine, mutate(statusLine), 1)
			candidate = compactV2Resign(candidate)
			if _, err := parseCompactV2Packet(candidate); err == nil {
				t.Fatal("noncanonical keyed status row was accepted")
			}
		})
	}
	t.Run("mistyped_array", func(t *testing.T) {
		mutated := strings.Replace(liveLine, `staged=["internal/a.go"]`, "staged=[internal/a.go]", 1)
		candidate := compactV2Resign(strings.Replace(packet, liveLine, mutated, 1))
		if _, err := parseCompactV2Packet(candidate); err == nil {
			t.Fatal("noncanonical keyed array was accepted")
		}
	})
}

func TestBrainBriefCompactV2SparseTwoRecordFamiliesStayKeyed(t *testing.T) {
	report := sparseCompactV2Report(2)
	v1 := renderBrainBriefCompactV1ForTest(t, report)
	v2 := renderBrainBriefCompactV2ForTest(t, report)
	assertCompactV2ProjectionParity(t, v1, v2)
	for _, declaration := range []string{"@x=runtime_trace(", "@O=test_suggestion("} {
		if strings.Contains(v2, declaration) {
			t.Errorf("sparse family grew a positional declaration: %s", declaration)
		}
	}
	if len(v2) > len(v1) {
		t.Fatalf("sparse compact_v2 grew: v2=%d v1=%d", len(v2), len(v1))
	}
}

func TestBrainBriefCompactV2NeverGrowsAcrossPublicFixtures(t *testing.T) {
	reports := []brainBriefReport{
		{Task: "minimal"},
		comprehensiveCompactV1Report(),
		sparseCompactV2Report(1),
		sparseCompactV2Report(2),
		sparseCompactV2Report(3),
	}
	for count := 2; count <= 8; count++ {
		reports = append(reports, repeatedCompactV2Report(comprehensiveCompactV1Report(), count))
	}
	for i, report := range reports {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			v1 := renderBrainBriefCompactV1ForTest(t, report)
			v2 := renderBrainBriefCompactV2ForTest(t, report)
			assertCompactV2ProjectionParity(t, v1, v2)
			if len(v2) > len(v1) {
				t.Fatalf("compact_v2 grew on fixture %d: v2=%d v1=%d", i, len(v2), len(v1))
			}
		})
	}
}

func TestCompactV2FamilyEncodingTieIsKeyed(t *testing.T) {
	if compactV2UsePositional(2, 100, 100) {
		t.Fatal("equal family sizes must choose keyed encoding")
	}
	if !compactV2UsePositional(2, 100, 99) {
		t.Fatal("strictly smaller repeated positional family was not selected")
	}
	if compactV2UsePositional(1, 100, 1) {
		t.Fatal("singleton family must remain keyed")
	}
}

func TestCompactV2ParserRejectsNoncanonicalFamilyChoice(t *testing.T) {
	t.Run("larger_positional_instead_of_keyed", func(t *testing.T) {
		report := sparseCompactV2Report(2)
		v1 := renderBrainBriefCompactV1ForTest(t, report)
		packet := renderBrainBriefCompactV2ForTest(t, report)
		family := compactV2V1FamilyForTest(t, v1, "runtime_trace")
		schema := compactV2SchemaByTagForTest(t, "runtime_trace")
		keyed, positional := compactV2FamilyWiresForTest(t, schema, family)
		if !strings.Contains(packet, keyed) || strings.Contains(packet, positional) {
			t.Fatal("sparse fixture did not choose keyed runtime traces")
		}
		candidate := compactV2Resign(strings.Replace(packet, keyed, positional, 1))
		if _, err := parseCompactV2Packet(candidate); err == nil {
			t.Fatal("parser accepted a larger positional family")
		}
	})

	t.Run("larger_keyed_instead_of_positional", func(t *testing.T) {
		report := comprehensiveCompactV2Report()
		v1 := renderBrainBriefCompactV1ForTest(t, report)
		packet := renderBrainBriefCompactV2ForTest(t, report)
		family := compactV2V1FamilyForTest(t, v1, "history")
		schema := compactV2SchemaByTagForTest(t, "history")
		keyed, positional := compactV2FamilyWiresForTest(t, schema, family)
		if !strings.Contains(packet, positional) || strings.Contains(packet, keyed) {
			t.Fatal("comprehensive fixture did not choose positional history")
		}
		candidate := compactV2Resign(strings.Replace(packet, positional, keyed, 1))
		if _, err := parseCompactV2Packet(candidate); err == nil {
			t.Fatal("parser accepted a larger keyed family")
		}
	})
}

func TestBrainBriefCompactV2ReferenceAllowlistExcludesNaturalLanguage(t *testing.T) {
	denied := map[string]bool{
		"value": true, "diff_stat": true, "effect": true, "detail": true,
		"signature": true, "reason": true, "suggestion_reason": true,
		"excerpt": true, "text": true, "action": true, "evidence": true,
		"title": true, "note": true, "intent_sig": true, "gram": true,
		"trigger": true, "workflow": true, "verification": true,
		"failure_modes": true, "description": true,
	}
	for _, schema := range compactV2Schemas {
		for _, field := range schema.fields {
			if denied[field.name] && field.reference {
				t.Errorf("natural-language field permits repeat references: %s.%s", schema.tag, field.name)
			}
		}
	}
}

func TestBrainBriefCompactV2HighCardinalityByteMargin(t *testing.T) {
	report := repeatedCompactV2Report(comprehensiveCompactV1Report(), 20)
	v1 := renderBrainBriefCompactV1ForTest(t, report)
	v2 := renderBrainBriefCompactV2ForTest(t, report)
	var legacy strings.Builder
	legacyCmd := &cobraCommandForCompactV2Test{out: &legacy}
	if err := emitBrainBriefReport(legacyCmd.command(), report, true); err != nil {
		t.Fatalf("render legacy JSON: %v", err)
	}
	assertCompactV2ProjectionParity(t, v1, v2)
	if len(v2)*100 > len(v1)*78 {
		t.Fatalf("compact_v2 lacks byte margin: v2=%d v1=%d ratio=%.3f", len(v2), len(v1), float64(len(v2))/float64(len(v1)))
	}
	if len(v2)*100 > legacy.Len()*40 {
		t.Fatalf("compact_v2 misses the 60%% byte gate on the repeated fixture: v2=%d legacy=%d", len(v2), legacy.Len())
	}
	t.Logf("compact_v2 repeated fixture: %d bytes vs compact_v1 %d bytes (%.1f%% further reduction), legacy JSON %d bytes (%.1f%% total reduction)", len(v2), len(v1), 100*(1-float64(len(v2))/float64(len(v1))), legacy.Len(), 100*(1-float64(len(v2))/float64(legacy.Len())))
}

func comprehensiveCompactV2Report() brainBriefReport {
	return repeatedCompactV2Report(comprehensiveCompactV1Report(), 2)
}

func sparseCompactV2Report(count int) brainBriefReport {
	report := brainBriefReport{Task: "sparse family selection"}
	for i := 0; i < count; i++ {
		report.Semantic.RuntimeTraces = append(report.Semantic.RuntimeTraces, semanticRecord{Reason: fmt.Sprintf("trace reason %d", i)})
		report.Semantic.Tests.Suggestions = append(report.Semantic.Tests.Suggestions, semanticTestSuggestion{Reason: fmt.Sprintf("test reason %d", i)})
		report.Guidance = append(report.Guidance, fmt.Sprintf("guidance %d", i))
		report.Warnings = append(report.Warnings, fmt.Sprintf("warning %d", i))
	}
	return report
}

func compactV2SchemaByTagForTest(t *testing.T, tag string) compactV2Schema {
	t.Helper()
	for _, schema := range compactV2Schemas {
		if schema.tag == tag {
			return schema
		}
	}
	t.Fatalf("compact_v2 schema missing for %s", tag)
	return compactV2Schema{}
}

func compactV2V1FamilyForTest(t *testing.T, packet, tag string) []compactV1RawRecord {
	t.Helper()
	records, err := parseBrainBriefCompactV1Body(packet)
	if err != nil {
		t.Fatalf("parse compact_v1 family source: %v", err)
	}
	var family []compactV1RawRecord
	for _, record := range records {
		if record.tag == tag {
			family = append(family, record)
		}
	}
	if len(family) == 0 {
		t.Fatalf("compact_v1 family %s missing", tag)
	}
	return family
}

func compactV2FamilyWiresForTest(t *testing.T, schema compactV2Schema, family []compactV1RawRecord) (string, string) {
	t.Helper()
	var keyed, positional strings.Builder
	writeCompactV2Declaration(&positional, schema)
	var previous []string
	for _, record := range family {
		keyed.WriteString(record.raw)
		keyed.WriteByte('\n')
		row, cells, err := encodeCompactV2Row(schema, record, previous)
		if err != nil {
			t.Fatalf("encode positional family: %v", err)
		}
		positional.WriteString(row)
		previous = cells
	}
	return keyed.String(), positional.String()
}

func repeatedCompactV2Report(report brainBriefReport, count int) brainBriefReport {
	if count < 2 {
		return report
	}
	repeatSemanticRecords := func(values []semanticRecord) []semanticRecord {
		base := append([]semanticRecord(nil), values...)
		for i := 1; i < count; i++ {
			values = append(values, base...)
		}
		return values
	}
	report.Status.Live.ChangedSymbolHints = repeatSemanticRecords(report.Status.Live.ChangedSymbolHints)
	if semantic := report.Status.Semantic; semantic != nil {
		if semantic.Freshness != nil {
			base := append([]semanticWarning(nil), semantic.Freshness.Warnings...)
			for i := 1; i < count; i++ {
				semantic.Freshness.Warnings = append(semantic.Freshness.Warnings, base...)
			}
		}
		if semantic.Coverage != nil {
			warnings := append([]semanticWarning(nil), semantic.Coverage.WarningDetails...)
			failures := append([]semanticWarning(nil), semantic.Coverage.PartialFailureDetails...)
			for i := 1; i < count; i++ {
				semantic.Coverage.WarningDetails = append(semantic.Coverage.WarningDetails, warnings...)
				semantic.Coverage.PartialFailureDetails = append(semantic.Coverage.PartialFailureDetails, failures...)
			}
		}
		spots := append([]brainBlindSpot(nil), semantic.BlindSpots...)
		for i := 1; i < count; i++ {
			semantic.BlindSpots = append(semantic.BlindSpots, spots...)
		}
	}
	report.Semantic.Context.Symbols = repeatSemanticRecords(report.Semantic.Context.Symbols)
	report.Semantic.Context.Relations = repeatSemanticRecords(report.Semantic.Context.Relations)
	report.Semantic.Context.Neighbors = repeatSemanticRecords(report.Semantic.Context.Neighbors)
	report.Semantic.RuntimeTraces = repeatSemanticRecords(report.Semantic.RuntimeTraces)
	report.Semantic.Tests.Roots = repeatSemanticRecords(report.Semantic.Tests.Roots)
	baseSuggestions := append([]semanticTestSuggestion(nil), report.Semantic.Tests.Suggestions...)
	for i := 1; i < count; i++ {
		report.Semantic.Tests.Suggestions = append(report.Semantic.Tests.Suggestions, baseSuggestions...)
	}
	repeatStrings := func(values []string) []string {
		base := append([]string(nil), values...)
		for i := 1; i < count; i++ {
			values = append(values, base...)
		}
		return values
	}
	report.LikelyEditFiles = repeatStrings(report.LikelyEditFiles)
	report.LikelyTestFiles = repeatStrings(report.LikelyTestFiles)
	// Add two paths which are not edit/test paths so likely_file is exercised.
	report.LikelyFiles = append(report.LikelyFiles, "docs/other.md")
	report.LikelyFiles = repeatStrings(report.LikelyFiles)
	repeatHistory := append([]brainTextMatch(nil), report.History.Matches...)
	repeatFacts := append([]factRecord(nil), report.Facts...)
	repeatActions := append([]brainBriefAction(nil), report.ActionChecklist...)
	repeatPatterns := append([]patternView(nil), report.Patterns...)
	repeatConsolidations := append([]briefConsolidation(nil), report.Consolidations...)
	repeatThemes := append([]themeView(nil), report.Themes...)
	for i := 1; i < count; i++ {
		report.History.Matches = append(report.History.Matches, repeatHistory...)
		report.Facts = append(report.Facts, repeatFacts...)
		report.ActionChecklist = append(report.ActionChecklist, repeatActions...)
		report.Patterns = append(report.Patterns, repeatPatterns...)
		report.Consolidations = append(report.Consolidations, repeatConsolidations...)
		report.Themes = append(report.Themes, repeatThemes...)
	}
	report.Guidance = repeatStrings(report.Guidance)
	report.Status.Warnings = repeatStrings(report.Status.Warnings)
	report.Status.Live.Warnings = repeatStrings(report.Status.Live.Warnings)
	report.Warnings = repeatStrings(report.Warnings)
	if report.FactsLocusDrift == nil {
		report.FactsLocusDrift = map[string][]string{}
	}
	for i := 0; i < count; i++ {
		report.FactsLocusDrift[fmt.Sprintf("fact:orphan-%d", i)] = []string{"GoneSymbol"}
	}
	return report
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

func assertCompactV2ProjectionParity(t *testing.T, v1, v2 string) {
	t.Helper()
	want, err := parseBrainBriefCompactV1Body(v1)
	if err != nil {
		t.Fatalf("parse compact_v1 projection: %v", err)
	}
	got, err := parseCompactV2Packet(v2)
	if err != nil {
		t.Fatalf("parse compact_v2 projection: %v", err)
	}
	gotRecords := make([]compactV1RawRecord, len(got))
	for i := range got {
		gotRecords[i] = got[i].record
		gotRecords[i].raw = ""
		want[i].raw = ""
	}
	if !reflect.DeepEqual(gotRecords, want) {
		t.Fatalf("compact_v2 typed projection differs from compact_v1\n got: %#v\nwant: %#v", gotRecords, want)
	}
}

func parseCompactV2Records(t *testing.T, packet string) []compactV2ParsedRecord {
	t.Helper()
	records, err := parseCompactV2Packet(packet)
	if err != nil {
		t.Fatalf("compact_v2 parse: %v", err)
	}
	return records
}

func parseCompactV2Packet(packet string) ([]compactV2ParsedRecord, error) {
	if !strings.HasSuffix(packet, "\n") {
		return nil, fmt.Errorf("missing final newline")
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	if len(lines) < 4 || lines[0] != brainBriefCompactV2Marker {
		return nil, fmt.Errorf("marker mismatch")
	}
	if lines[1] != brainBriefCompactV2Legend {
		return nil, fmt.Errorf("legend mismatch")
	}
	footer := strings.Split(lines[len(lines)-1], "\t")
	if len(footer) != 3 || footer[0] != "end" {
		return nil, fmt.Errorf("footer shape")
	}
	count, err := strconv.Atoi(footer[1])
	if err != nil || count < 0 || strconv.Itoa(count) != footer[1] {
		return nil, fmt.Errorf("footer count")
	}
	if !compactV2LowerHexDigest(footer[2]) {
		return nil, fmt.Errorf("footer digest")
	}
	footerAt := strings.LastIndex(packet, "\nend\t")
	if footerAt < 0 {
		return nil, fmt.Errorf("footer boundary")
	}
	body := packet[:footerAt+1]
	digest := sha256.Sum256([]byte(body))
	if footer[2] != fmt.Sprintf("%x", digest) {
		return nil, fmt.Errorf("checksum mismatch")
	}

	byTag := make(map[string]compactV2Schema, len(compactV2Schemas))
	byOpcode := make(map[byte]compactV2Schema, len(compactV2Schemas))
	for _, schema := range compactV2Schemas {
		if _, exists := byTag[schema.tag]; exists {
			return nil, fmt.Errorf("duplicate static tag")
		}
		if _, exists := byOpcode[schema.opcode]; exists {
			return nil, fmt.Errorf("duplicate static opcode")
		}
		byTag[schema.tag] = schema
		byOpcode[schema.opcode] = schema
	}
	declared := map[byte]bool{}
	used := map[byte]bool{}
	previous := map[byte][]string{}
	var records []compactV2ParsedRecord
	for _, line := range lines[2 : len(lines)-1] {
		if strings.HasPrefix(line, "@") {
			if len(line) < 2 {
				return nil, fmt.Errorf("empty declaration")
			}
			schema, ok := byOpcode[line[1]]
			if !ok || declared[schema.opcode] {
				return nil, fmt.Errorf("unknown or duplicate declaration")
			}
			var want strings.Builder
			writeCompactV2Declaration(&want, schema)
			if strings.TrimSuffix(want.String(), "\n") != line {
				return nil, fmt.Errorf("noncanonical declaration")
			}
			declared[schema.opcode] = true
			continue
		}
		if line == "" {
			return nil, fmt.Errorf("empty body line")
		}
		if schema, ok := byOpcode[line[0]]; ok && declared[schema.opcode] && (len(line) == 1 || line[1] == '\t') {
			parsed, cells, err := parseCompactV2PositionalRecord(schema, line, previous[schema.opcode])
			if err != nil {
				return nil, err
			}
			previous[schema.opcode] = cells
			used[schema.opcode] = true
			records = append(records, compactV2ParsedRecord{record: parsed, positional: true})
			continue
		}
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			return nil, err
		}
		schema, ok := byTag[record.tag]
		if !ok {
			return nil, fmt.Errorf("unknown keyed tag")
		}
		if err := validateCompactV2KeyedRecord(record, schema); err != nil {
			return nil, err
		}
		records = append(records, compactV2ParsedRecord{record: record})
	}
	if len(records) != count {
		return nil, fmt.Errorf("body record count")
	}
	families := map[string][]compactV1RawRecord{}
	modeSet := map[string]bool{}
	actualMode := map[string]bool{}
	for _, record := range records {
		tag := record.record.tag
		if modeSet[tag] && actualMode[tag] != record.positional {
			return nil, fmt.Errorf("mixed family encoding for %s", tag)
		}
		modeSet[tag] = true
		actualMode[tag] = record.positional
		canonical := record.record
		canonical.raw = renderCompactV1RawRecord(canonical)
		families[tag] = append(families[tag], canonical)
	}
	for tag, family := range families {
		wantPositional, err := expectedCompactV2FamilyMode(byTag[tag], family)
		if err != nil {
			return nil, err
		}
		if actualMode[tag] != wantPositional {
			return nil, fmt.Errorf("noncanonical family encoding for %s", tag)
		}
	}
	for opcode := range declared {
		if !used[opcode] {
			return nil, fmt.Errorf("unused declaration")
		}
	}
	return records, nil
}

func validateCompactV2KeyedRecord(record compactV1RawRecord, schema compactV2Schema) error {
	positions := make(map[string]int, len(schema.fields))
	for i, field := range schema.fields {
		positions[field.name] = i
	}
	last := -1
	for _, field := range record.fields {
		position, ok := positions[field.key]
		if !ok {
			return fmt.Errorf("unknown keyed field %s.%s", record.tag, field.key)
		}
		if position <= last {
			return fmt.Errorf("duplicate or reordered keyed field %s.%s", record.tag, field.key)
		}
		canonical, err := canonicalCompactV1FieldValue(schema.fields[position].kind, field.value)
		if err != nil || canonical != field.value {
			return fmt.Errorf("noncanonical keyed field %s.%s", record.tag, field.key)
		}
		last = position
	}
	return nil
}

func canonicalCompactV1FieldValue(kind compactV2FieldKind, raw string) (string, error) {
	switch kind {
	case compactV2String:
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", err
		}
		return strconv.Quote(value), nil
	case compactV2Strings:
		values, err := parseCompactV1StringArray(raw)
		if err != nil {
			return "", err
		}
		var out strings.Builder
		out.WriteByte('[')
		for i, value := range values {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(strconv.Quote(value))
		}
		out.WriteByte(']')
		return out.String(), nil
	case compactV2Bool:
		if raw != "true" && raw != "false" {
			return "", fmt.Errorf("invalid bool")
		}
		return raw, nil
	case compactV2Int:
		value, err := strconv.Atoi(raw)
		if err != nil {
			return "", err
		}
		return strconv.Itoa(value), nil
	case compactV2Float:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return "", fmt.Errorf("invalid float")
		}
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("unknown field kind")
	}
}

func renderCompactV1RawRecord(record compactV1RawRecord) string {
	var out strings.Builder
	out.WriteString(record.tag)
	for _, field := range record.fields {
		out.WriteByte(' ')
		out.WriteString(field.key)
		out.WriteByte('=')
		out.WriteString(field.value)
	}
	return out.String()
}

func expectedCompactV2FamilyMode(schema compactV2Schema, family []compactV1RawRecord) (bool, error) {
	var declaration strings.Builder
	writeCompactV2Declaration(&declaration, schema)
	keyedBytes := 0
	positionalBytes := declaration.Len()
	var previous []string
	for _, record := range family {
		keyedBytes += len(record.raw) + 1
		row, cells, err := encodeCompactV2Row(schema, record, previous)
		if err != nil {
			return false, err
		}
		positionalBytes += len(row)
		previous = cells
	}
	return compactV2UsePositional(len(family), keyedBytes, positionalBytes), nil
}

func parseCompactV2PositionalRecord(schema compactV2Schema, line string, prior []string) (compactV1RawRecord, []string, error) {
	parts := strings.Split(line, "\t")
	if parts[0] != string(schema.opcode) || len(parts)-1 > len(schema.fields) {
		return compactV1RawRecord{}, nil, fmt.Errorf("positional width")
	}
	if len(parts) > 1 && parts[len(parts)-1] == "~" {
		return compactV1RawRecord{}, nil, fmt.Errorf("redundant trailing null")
	}
	cells := make([]string, len(schema.fields))
	for i := range cells {
		cells[i] = "~"
	}
	record := compactV1RawRecord{tag: schema.tag}
	for i, encoded := range parts[1:] {
		field := schema.fields[i]
		if encoded == "~" {
			continue
		}
		if encoded == "^" {
			if !field.reference || len(prior) != len(cells) || prior[i] == "~" {
				return compactV1RawRecord{}, nil, fmt.Errorf("invalid repeat reference")
			}
			cells[i] = prior[i]
		} else {
			raw, canonical, err := decodeCompactV2Cell(field.kind, encoded)
			if err != nil {
				return compactV1RawRecord{}, nil, err
			}
			if canonical != encoded {
				return compactV1RawRecord{}, nil, fmt.Errorf("noncanonical cell")
			}
			if field.reference && len(prior) == len(cells) && prior[i] == encoded && len(encoded) > 1 {
				return compactV1RawRecord{}, nil, fmt.Errorf("missed canonical repeat reference")
			}
			cells[i] = encoded
			record.fields = append(record.fields, compactV1RawField{key: field.name, value: raw})
			continue
		}
		raw, _, err := decodeCompactV2Cell(field.kind, cells[i])
		if err != nil {
			return compactV1RawRecord{}, nil, err
		}
		record.fields = append(record.fields, compactV1RawField{key: field.name, value: raw})
	}
	return record, cells, nil
}

func decodeCompactV2Cell(kind compactV2FieldKind, encoded string) (raw, canonical string, err error) {
	switch kind {
	case compactV2String:
		if strings.HasPrefix(encoded, "\"") {
			value, unquoteErr := strconv.Unquote(encoded)
			if unquoteErr != nil || compactV2SafeAtom(value) {
				return "", "", fmt.Errorf("invalid or noncanonical quoted string")
			}
			return strconv.Quote(value), strconv.Quote(value), nil
		}
		if !compactV2SafeAtom(encoded) {
			return "", "", fmt.Errorf("unsafe atom")
		}
		return strconv.Quote(encoded), encoded, nil
	case compactV2Strings:
		values, parseErr := parseCompactV2StringArray(encoded)
		if parseErr != nil {
			return "", "", parseErr
		}
		var v1, v2 strings.Builder
		v1.WriteByte('[')
		v2.WriteByte('[')
		for i, value := range values {
			if i > 0 {
				v1.WriteByte(',')
				v2.WriteByte(',')
			}
			v1.WriteString(strconv.Quote(value))
			if compactV2SafeAtom(value) {
				v2.WriteString(value)
			} else {
				v2.WriteString(strconv.Quote(value))
			}
		}
		v1.WriteByte(']')
		v2.WriteByte(']')
		return v1.String(), v2.String(), nil
	case compactV2Bool:
		if encoded == "1" {
			return "true", "1", nil
		}
		if encoded == "0" {
			return "false", "0", nil
		}
		return "", "", fmt.Errorf("invalid boolean")
	case compactV2Int:
		value, parseErr := strconv.Atoi(encoded)
		if parseErr != nil {
			return "", "", fmt.Errorf("invalid integer")
		}
		canonical = strconv.Itoa(value)
		return canonical, canonical, nil
	case compactV2Float:
		value, parseErr := strconv.ParseFloat(encoded, 64)
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return "", "", fmt.Errorf("invalid float")
		}
		canonical = strconv.FormatFloat(value, 'g', -1, 64)
		return canonical, canonical, nil
	default:
		return "", "", fmt.Errorf("unknown field kind")
	}
}

func parseCompactV2StringArray(encoded string) ([]string, error) {
	if len(encoded) < 2 || encoded[0] != '[' || encoded[len(encoded)-1] != ']' {
		return nil, fmt.Errorf("invalid compact_v2 array")
	}
	if encoded == "[]" {
		return nil, nil
	}
	var values []string
	for cursor := 1; cursor < len(encoded)-1; {
		if encoded[cursor] == '"' {
			end, err := scanCompactQuoted(encoded, cursor)
			if err != nil || end > len(encoded)-1 {
				return nil, fmt.Errorf("invalid compact_v2 array string")
			}
			value, err := strconv.Unquote(encoded[cursor:end])
			if err != nil || compactV2SafeAtom(value) {
				return nil, fmt.Errorf("noncanonical compact_v2 array string")
			}
			values = append(values, value)
			cursor = end
		} else {
			end := cursor
			for end < len(encoded)-1 && encoded[end] != ',' {
				end++
			}
			value := encoded[cursor:end]
			if !compactV2SafeAtom(value) {
				return nil, fmt.Errorf("unsafe compact_v2 array atom")
			}
			values = append(values, value)
			cursor = end
		}
		if cursor == len(encoded)-1 {
			break
		}
		if encoded[cursor] != ',' {
			return nil, fmt.Errorf("invalid compact_v2 array separator")
		}
		cursor++
		if cursor == len(encoded)-1 {
			return nil, fmt.Errorf("trailing compact_v2 array separator")
		}
	}
	return values, nil
}

func compactV2LowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

func compactV2FirstLineWithPrefix(packet, prefix string) string {
	for _, line := range strings.Split(packet, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func compactV2RewriteFooter(packet string, mutate func([]string)) string {
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	parts := strings.Split(lines[len(lines)-1], "\t")
	mutate(parts)
	lines[len(lines)-1] = strings.Join(parts, "\t")
	return strings.Join(lines, "\n") + "\n"
}

func compactV2Resign(packet string) string {
	footerAt := strings.LastIndex(packet, "\nend\t")
	if footerAt < 0 {
		return packet
	}
	body := packet[:footerAt+1]
	footerEnd := strings.IndexByte(packet[footerAt+1:], '\n')
	if footerEnd < 0 {
		return packet
	}
	footer := strings.Split(packet[footerAt+1:footerAt+1+footerEnd], "\t")
	if len(footer) != 3 {
		return packet
	}
	digest := sha256.Sum256([]byte(body))
	footer[2] = fmt.Sprintf("%x", digest)
	return body + strings.Join(footer, "\t") + "\n"
}
