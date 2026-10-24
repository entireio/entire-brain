package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJudgeFixtureRun(t *testing.T) string {
	t.Helper()
	runDir := t.TempDir()
	raw := `{"type":"chunk","session_id":"s1","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":3,"cap":6}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f1","kind":"gotcha","paths":["a.b.c"],"text":"first","checkpoint_id":"cp"}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f2","kind":"decision","paths":["d.e.f"],"text":"second","checkpoint_id":"cp"}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f3","kind":"invariant","paths":["g.h.i"],"text":"third","checkpoint_id":"cp"}
`
	if err := os.WriteFile(filepath.Join(runDir, "raw-facts.jsonl"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return runDir
}

func readJudgeLines(t *testing.T, path string) map[string]distillEvalJudgeLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]distillEvalJudgeLine{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line distillEvalJudgeLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad judge line %q: %v", sc.Text(), err)
		}
		out[line.FactID] = line
	}
	return out
}

func TestDistillEvalJudgeParsesVerdictsAndDefaultsToKill(t *testing.T) {
	runDir := writeJudgeFixtureRun(t)
	calls := 0
	restore := distillEvalJudgeRunnerFor
	distillEvalJudgeRunnerFor = func(agent string) distillAgentRunner {
		return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
			calls++
			return "1 keep\n3 banana\n", nil
		}
	}
	defer func() { distillEvalJudgeRunnerFor = restore }()

	out := filepath.Join(runDir, "judge.jsonl")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "judge", "--run", runDir, "--agent", "claude-code", "--model", "claude-fable-5", "--out", out); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if calls != 1 {
		t.Errorf("one session should mean one judge call, got %d", calls)
	}
	lines := readJudgeLines(t, out)
	if len(lines) != 3 {
		t.Fatalf("got %d judge lines, want 3", len(lines))
	}
	if lines["f1"].Verdict != "keep" || lines["f2"].Verdict != "kill" || lines["f3"].Verdict != "kill" {
		t.Errorf("verdicts wrong: %+v", lines)
	}
	if lines["f1"].JudgeAgent != "claude-code" || lines["f1"].JudgeModel != "claude-fable-5" {
		t.Errorf("judge identity missing: %+v", lines["f1"])
	}
}

func TestDistillEvalJudgeUsesCache(t *testing.T) {
	runDir := writeJudgeFixtureRun(t)
	calls := 0
	restore := distillEvalJudgeRunnerFor
	distillEvalJudgeRunnerFor = func(agent string) distillAgentRunner {
		return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
			calls++
			return "1 keep\n2 keep\n3 keep\n", nil
		}
	}
	defer func() { distillEvalJudgeRunnerFor = restore }()

	cache := filepath.Join(t.TempDir(), "judge-cache.json")
	for i := 0; i < 2; i++ {
		out := filepath.Join(runDir, "judge.jsonl")
		if _, err := runDistillEvalCommand(t, t.TempDir(), "judge", "--run", runDir, "--judge-cache", cache, "--out", out); err != nil {
			t.Fatalf("judge pass %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("second pass must be served from cache: %d calls", calls)
	}
	lines := readJudgeLines(t, filepath.Join(runDir, "judge.jsonl"))
	if len(lines) != 3 || lines["f2"].Verdict != "keep" {
		t.Errorf("cached verdicts wrong: %+v", lines)
	}
}

func TestDistillEvalCalibrateAgreement(t *testing.T) {
	dir := t.TempDir()
	labels := `{"fact_id":"f1","verdict":"keep"}
{"fact_id":"f2","verdict":"kill"}
{"fact_id":"f3","verdict":"kill"}
`
	judge := `{"fact_id":"f1","verdict":"keep","judge_agent":"claude-code","judge_model":"claude-fable-5"}
{"fact_id":"f2","verdict":"keep","judge_agent":"claude-code","judge_model":"claude-fable-5"}
{"fact_id":"f3","verdict":"kill","judge_agent":"claude-code","judge_model":"claude-fable-5"}
{"fact_id":"f4","verdict":"kill","judge_agent":"claude-code","judge_model":"claude-fable-5"}
`
	labelsPath := filepath.Join(dir, "labels.jsonl")
	judgePath := filepath.Join(dir, "judge.jsonl")
	if err := os.WriteFile(labelsPath, []byte(labels), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(judgePath, []byte(judge), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runDistillEvalCommand(t, t.TempDir(), "calibrate", "--labels", labelsPath, "--judge", judgePath); err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "calibration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cal distillEvalCalibration
	if err := json.Unmarshal(data, &cal); err != nil {
		t.Fatal(err)
	}
	// f4 is judge-only: excluded. n=3; agreement 2/3; kills {f2,f3} → 1/2; keeps {f1} → 1.
	if cal.N != 3 {
		t.Fatalf("n = %d, want 3", cal.N)
	}
	almost := func(got, want float64) bool { d := got - want; return d < 1e-9 && d > -1e-9 }
	if !almost(cal.Agreement, 2.0/3.0) || !almost(cal.AgreementOnKills, 0.5) || !almost(cal.AgreementOnKeeps, 1.0) {
		t.Errorf("calibration wrong: %+v", cal)
	}
}
