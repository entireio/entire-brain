package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCompareSummary(t *testing.T, dir, name string, s distillEvalSummary) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func compareFixtureSummary(corpusSHA, judgeConfig string, facts []int) distillEvalSummary {
	ids := []string{"s1", "s2", "s3", "s4"}
	var sessions []distillEvalSessionResult
	total := 0
	for i, n := range facts {
		total += n
		sessions = append(sessions, distillEvalSessionResult{
			SessionID: ids[i], Facts: n, Chunks: 1, Tokens: 100, AgentCalls: 1,
		})
	}
	p := 0.5
	return distillEvalSummary{
		Variant:      distillEvalVariant{Name: "v"},
		CorpusSHA256: corpusSHA,
		JudgeConfig:  judgeConfig,
		Sessions:     sessions,
		Facts:        total,
		Precision:    distillEvalPrecision{Combined: &p, GoldenN: 1},
		CapShare:     0.5,
		Redundancy:   distillEvalRedundancy{NearDupRate: 0.2},
	}
}

func TestDistillEvalCompareRefusesCorpusMismatch(t *testing.T) {
	dir := t.TempDir()
	a := writeCompareSummary(t, dir, "a.json", compareFixtureSummary("sha-A", "", []int{1, 1, 1, 1}))
	b := writeCompareSummary(t, dir, "b.json", compareFixtureSummary("sha-B", "", []int{1, 1, 1, 1}))
	_, err := runDistillEvalCommand(t, t.TempDir(), "compare", a, b)
	if err == nil || !strings.Contains(err.Error(), "corpus") {
		t.Fatalf("expected corpus mismatch error, got %v", err)
	}
	if _, err := runDistillEvalCommand(t, t.TempDir(), "compare", a, b, "--allow-corpus-mismatch"); err != nil {
		t.Fatalf("override should pass: %v", err)
	}
}

func TestDistillEvalCompareRefusesJudgeMismatch(t *testing.T) {
	dir := t.TempDir()
	a := writeCompareSummary(t, dir, "a.json", compareFixtureSummary("sha", "claude-code/claude-fable-5", []int{1, 1, 1, 1}))
	b := writeCompareSummary(t, dir, "b.json", compareFixtureSummary("sha", "claude-code/other-model", []int{1, 1, 1, 1}))
	_, err := runDistillEvalCommand(t, t.TempDir(), "compare", a, b)
	if err == nil || !strings.Contains(err.Error(), "judge") {
		t.Fatalf("expected judge mismatch error, got %v", err)
	}
	if _, err := runDistillEvalCommand(t, t.TempDir(), "compare", a, b, "--allow-judge-mismatch"); err != nil {
		t.Fatalf("override should pass: %v", err)
	}
}

func TestDistillEvalCompareRefusesGoldenOnlyMismatch(t *testing.T) {
	dir := t.TempDir()
	sa := compareFixtureSummary("sha", "", []int{1, 1, 1, 1})
	sa.GoldenOnly = true
	a := writeCompareSummary(t, dir, "a.json", sa)
	b := writeCompareSummary(t, dir, "b.json", compareFixtureSummary("sha", "", []int{1, 1, 1, 1}))
	_, err := runDistillEvalCommand(t, t.TempDir(), "compare", a, b)
	if err == nil || !strings.Contains(err.Error(), "golden") {
		t.Fatalf("expected golden-only coverage error, got %v", err)
	}
	if _, err := runDistillEvalCommand(t, t.TempDir(), "compare", a, b, "--allow-corpus-mismatch"); err != nil {
		t.Fatalf("override should pass: %v", err)
	}
}

func TestDistillEvalComparePairedVolume(t *testing.T) {
	dir := t.TempDir()
	a := writeCompareSummary(t, dir, "a.json", compareFixtureSummary("sha", "", []int{4, 2, 2, 0}))
	b := writeCompareSummary(t, dir, "b.json", compareFixtureSummary("sha", "", []int{2, 1, 1, 0}))
	out, err := runDistillEvalCommand(t, t.TempDir(), "compare", a, b)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	var result distillEvalComparison
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode output %q: %v", out, err)
	}
	var factsCmp *metricComparison
	for i := range result.Paired {
		if result.Paired[i].Metric == "facts" {
			factsCmp = &result.Paired[i]
		}
	}
	if factsCmp == nil {
		t.Fatalf("no facts comparison in %+v", result.Paired)
	}
	// Zero-fact session included: all four sessions pair.
	if factsCmp.N != 4 {
		t.Errorf("facts N = %d, want 4", factsCmp.N)
	}
	if factsCmp.Winner != "b" {
		t.Errorf("facts winner = %q, want b (fewer facts is better)", factsCmp.Winner)
	}
	if factsCmp.MeanA != 2.0 || factsCmp.MeanB != 1.0 {
		t.Errorf("means wrong: %+v", factsCmp)
	}
	if len(result.Paired) != 3 {
		t.Errorf("want 3 paired metrics (facts, tokens, agent_calls), got %d", len(result.Paired))
	}
	foundPooled := false
	for _, p := range result.Pooled {
		if p.Metric == "precision.combined" {
			foundPooled = true
			if p.Note != "pooled (no paired test)" {
				t.Errorf("pooled note = %q", p.Note)
			}
		}
	}
	if !foundPooled {
		t.Errorf("pooled precision delta missing: %+v", result.Pooled)
	}
}
