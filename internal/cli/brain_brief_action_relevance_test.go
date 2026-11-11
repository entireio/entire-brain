package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBrainBriefActionChecklistTaskWithoutIntentYieldsNoActions(t *testing.T) {
	repoDir, source := writeBrainBriefActionRelevanceFixture(t)
	report := brainBriefReport{
		Task:            "Optimize brainBriefDeduplicateTestRoots allocations",
		LikelyEditFiles: []string{source},
		Guidance:        brainBriefActionRelevanceGuidance(),
	}

	// The fixture contains twelve latent limit-looking lines; only the task may
	// activate that scanner, and this task carries no limit intent.
	legacyActions := brainBriefLimitNormalizationActions(repoDir, report.LikelyEditFiles)
	if got := len(legacyActions); got != 12 {
		t.Fatalf("fixture latent limit actions = %d, want 12: %+v", got, legacyActions)
	}
	actions := brainBriefActionChecklist(repoDir, report, report.Task)
	if got := len(actions); got != 0 {
		t.Fatalf("false actions = %d, want 0: %+v", got, actions)
	}
}

func TestBrainBriefActionChecklistDirectTaskIntentPacketIsByteIdentical(t *testing.T) {
	repoDir, source := writeBrainBriefActionRelevanceFixture(t)
	report := brainBriefReport{
		Task:            "repository query limit normalization",
		LikelyEditFiles: []string{source},
		Guidance:        brainBriefActionRelevanceGuidance(),
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

func TestBrainBriefActionChecklistNoActionPacketIsByteIdentical(t *testing.T) {
	report := brainBriefReport{
		Task:     "Document the release workflow",
		Guidance: brainBriefActionRelevanceGuidance(),
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
		"Use likely_edit_files before broad text search; use likely_test_files for validation context.",
		"Use the live-state overlay before trusting semantic results for files changed in this session.",
		"Inspect full diffs or source files when the task intersects dirty files or when confidence is low.",
	}
}
