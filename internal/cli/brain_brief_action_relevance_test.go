package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const brainBriefActionRelevanceTask = "Optimize brainBriefDeduplicateTestRoots allocations"

func TestBrainBriefActionChecklistRejectsUnrelatedHistoryIntent(t *testing.T) {
	repoDir, source := writeBrainBriefActionRelevanceFixture(t)
	report := brainBriefReport{
		Task: brainBriefActionRelevanceTask,
		History: brainBriefHistory{Matches: []brainTextMatch{
			{Excerpt: "The old scorer rebuilt normalized strings and caused excessive allocations. Repository query limit normalization uses normalizeLimit and MAX_QUERY_LIMIT."},
			{Excerpt: "Optimize allocations in the broad history pipeline; oversized query limits still require normalizeLimit."},
		}},
		LikelyEditFiles: []string{source},
		Guidance:        brainBriefActionRelevanceGuidance(),
	}

	// This is the exact false-action shape from the live packet: the current
	// source contains twelve limit-looking lines, but neither history excerpt is
	// evidence about the task's strong identifier. Even the second excerpt's two
	// prose overlaps cannot replace the task's explicit symbol anchor.
	legacyActions := brainBriefLimitNormalizationActions(repoDir, report.LikelyEditFiles)
	if got := len(legacyActions); got != 12 {
		t.Fatalf("fixture latent limit actions = %d, want 12: %+v", got, legacyActions)
	}
	for _, match := range report.History.Matches {
		if brainBriefActionHistoryRelevant(report.Task, match.Excerpt) {
			t.Fatalf("unrelated history passed relevance gate: %q", match.Excerpt)
		}
	}
	actions := brainBriefActionChecklist(repoDir, report, report.Task)
	if got := len(actions); got != 0 {
		t.Fatalf("false actions = %d, want 0: %+v", got, actions)
	}

	baseline := report
	baseline.ActionChecklist = legacyActions
	baseline.Guidance = append(append([]string(nil), baseline.Guidance...), brainBriefActionChecklistGuidance)
	candidate := report
	candidate.ActionChecklist = actions
	baselinePackets := renderBrainBriefAllPacketFormats(t, baseline)
	candidatePackets := renderBrainBriefAllPacketFormats(t, candidate)
	jsonDelta := len(baselinePackets["json"]) - len(candidatePackets["json"])
	if jsonDelta <= 0 {
		t.Fatalf("JSON packet did not shrink: baseline=%d candidate=%d", len(baselinePackets["json"]), len(candidatePackets["json"]))
	}
	t.Logf("false actions 12 -> 0; JSON packet %d -> %d bytes (%d-byte reduction)", len(baselinePackets["json"]), len(candidatePackets["json"]), jsonDelta)
}

func TestBrainBriefActionChecklistDirectTaskIntentPacketIsByteIdentical(t *testing.T) {
	repoDir, source := writeBrainBriefActionRelevanceFixture(t)
	report := brainBriefReport{
		Task:            "repository query limit normalization",
		LikelyEditFiles: []string{source},
		Guidance:        brainBriefActionRelevanceGuidance(),
		History: brainBriefHistory{Matches: []brainTextMatch{{
			Excerpt: "Unrelated historical prose must not perturb a task-selected action family.",
		}}},
	}
	want := brainBriefLimitNormalizationActions(repoDir, report.LikelyEditFiles)
	got := brainBriefActionChecklist(repoDir, report, report.Task)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("direct task actions changed\ngot:  %+v\nwant: %+v", got, want)
	}

	before := report
	before.ActionChecklist = want
	before.Guidance = append(append([]string(nil), before.Guidance...), brainBriefActionChecklistGuidance)
	after := report
	after.ActionChecklist = got
	after.Guidance = append(append([]string(nil), after.Guidance...), brainBriefActionChecklistGuidance)
	if beforePackets, afterPackets := renderBrainBriefAllPacketFormats(t, before), renderBrainBriefAllPacketFormats(t, after); !reflect.DeepEqual(afterPackets, beforePackets) {
		t.Fatalf("direct task packet bytes changed\nafter:  %#v\nbefore: %#v", afterPackets, beforePackets)
	}
}

func TestBrainBriefActionChecklistHistoryCanRevealIntentWithStrongIdentifier(t *testing.T) {
	repoDir, source := writeBrainBriefActionRelevanceFixture(t)
	report := brainBriefReport{
		Task:            "Repair listNodes regression",
		LikelyEditFiles: []string{source},
		History: brainBriefHistory{Matches: []brainTextMatch{{
			Excerpt: "The listNodes contract requires normalizeLimit before applying the SQL LIMIT.",
		}}},
	}
	if !brainBriefActionHistoryRelevant(report.Task, report.History.Matches[0].Excerpt) {
		t.Fatal("strong identifier overlap did not establish history relevance")
	}
	if got := brainBriefActionChecklist(repoDir, report, report.Task); len(got) != 12 {
		t.Fatalf("strong-identifier history actions = %d, want 12: %+v", len(got), got)
	}
}

func TestBrainBriefActionChecklistHistoryCanRevealIntentWithTermOverlap(t *testing.T) {
	repoDir, source := writeBrainBriefActionRelevanceFixture(t)
	report := brainBriefReport{
		Task:            "Repair storage pagination behavior",
		LikelyEditFiles: []string{source},
		History: brainBriefHistory{Matches: []brainTextMatch{{
			Excerpt: "Storage pagination must normalize every oversized query limit before execution.",
		}}},
	}
	if !brainBriefActionHistoryRelevant(report.Task, report.History.Matches[0].Excerpt) {
		t.Fatal("significant-term overlap did not establish history relevance")
	}
	if got := brainBriefActionChecklist(repoDir, report, report.Task); len(got) != 12 {
		t.Fatalf("term-overlap history actions = %d, want 12: %+v", len(got), got)
	}
}

func TestBrainBriefActionChecklistGenericTermOverlapCannotRevealIntent(t *testing.T) {
	cases := []struct {
		task, excerpt string
	}{
		{
			task:    "Repair repository query behavior",
			excerpt: "Repository query handling must normalize every oversized limit before execution.",
		},
		{
			task:    "Audit SQL query behavior",
			excerpt: "SQL query handling must normalize every oversized limit before execution.",
		},
	}
	for _, tt := range cases {
		if brainBriefActionHistoryRelevant(tt.task, tt.excerpt) {
			t.Fatalf("generic overlap unexpectedly established relevance: task=%q excerpt=%q", tt.task, tt.excerpt)
		}
	}
}

func TestBrainBriefActionChecklistNoActionPacketIsByteIdentical(t *testing.T) {
	report := brainBriefReport{
		Task:     "Document the release workflow",
		Guidance: brainBriefActionRelevanceGuidance(),
		History: brainBriefHistory{Matches: []brainTextMatch{{
			Excerpt: "A release note with no source-code action invariant.",
		}}},
	}
	before := renderBrainBriefAllPacketFormats(t, report)
	report.ActionChecklist = brainBriefActionChecklist(t.TempDir(), report, report.Task)
	if len(report.ActionChecklist) != 0 {
		t.Fatalf("no-action result = %#v, want empty", report.ActionChecklist)
	}
	after := renderBrainBriefAllPacketFormats(t, report)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("no-action packet bytes changed\nafter:  %#v\nbefore: %#v", after, before)
	}
}

func writeBrainBriefActionRelevanceFixture(t *testing.T) (repoDir, source string) {
	t.Helper()
	repoDir = t.TempDir()
	source = "internal/cli/semantic.go"
	abs := filepath.Join(repoDir, filepath.FromSlash(source))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("export class SemanticStore {\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&body, "  list%02d(limit = 50) {\n", i)
		body.WriteString("    return this.db.query(\"SELECT * FROM symbols LIMIT ?\", [limit]);\n")
		body.WriteString("  }\n")
	}
	body.WriteString("}\n")
	if err := os.WriteFile(abs, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return repoDir, source
}

func brainBriefActionRelevanceGuidance() []string {
	return []string{
		"Treat the brain as an indexed snapshot, not live memory.",
		"Use likely_edit_files and history matches before broad text search; use likely_test_files for validation context.",
		"Use the live-state overlay before trusting semantic results for files changed in this session.",
		"Inspect full diffs or source files when the task intersects dirty files or when confidence is low.",
	}
}
