package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDistillEvalEndToEndWithFakeAgents drives the whole loop — corpus → run →
// labels → judge → calibrate → score → second variant → compare — with the
// real distill extraction path (runDistillForBrain) behind the exec seam and
// canned agent runners. No real agent, no network.
func TestDistillEvalEndToEndWithFakeAgents(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	corpusDir := t.TempDir()

	writeBrain := func(root string) string {
		brainDir := filepath.Join(root, "brain")
		sessions := []exportSession{
			{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now},
			{SessionID: "s2", Branch: "main", LatestCheckpoint: "cp2", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now},
		}
		contents := map[string]string{
			"s1": "turn one\nturn two\n",
			"s2": "turn one\nturn two\nturn three\nturn four\n",
		}
		for _, s := range sessions {
			path := filepath.Join(brainDir, filepath.FromSlash(s.TranscriptPath))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(contents[s.SessionID]), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		manifest := exportManifest{
			SchemaVersion: brainManifestSchemaVersion,
			GeneratedAt:   now,
			DefaultBranch: "main",
			Sources: &brainSources{
				Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions},
			},
		}
		if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
			t.Fatal(err)
		}
		return brainDir
	}

	distRunner := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "project.tooling.stack\tfact one alpha\nproject.tooling.stack\tfact two beta\n", nil
	}

	restoreExec := distillEvalExec
	distillEvalExec = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		dataHome := envValue(env, "XDG_DATA_HOME")
		switch args[0] {
		case "refresh":
			if out := argValue(args, "-o"); out != "" {
				// Bundle pass: write directly into the named bundle dir, which
				// the harness swaps into the brain-dir location.
				if err := os.Rename(writeBrain(t.TempDir()), out); err != nil {
					t.Fatal(err)
				}
				return nil, nil
			}
			writeBrain(dataHome)
			return nil, nil
		case "distill":
			brainDir, err := findSandboxBrainDir(filepath.Dir(dataHome))
			if err != nil {
				t.Fatalf("locate sandbox brain: %v", err)
			}
			if cap := envValue(env, envFactsMaxPerChunk); cap != "" {
				os.Setenv(envFactsMaxPerChunk, cap)
				defer os.Unsetenv(envFactsMaxPerChunk)
			}
			maxChunk, _ := strconv.Atoi(argValue(args, "--max-chunk-bytes"))
			opts := distillCommandOptions{
				agent: "command", agentCommand: []string{"fake"}, run: distRunner,
				session:       argValue(args, "--session"),
				rawFactsOut:   argValue(args, "--raw-facts-out"),
				callTracePath: argValue(args, "--call-trace"),
				maxChunkBytes: maxChunk,
				timeout:       time.Minute,
			}
			if _, err := runDistillForBrain(ctx, t.TempDir(), brainDir, opts, now); err != nil {
				t.Fatalf("in-process distill: %v", err)
			}
			return nil, nil
		}
		t.Fatalf("unexpected exec: %v", args)
		return nil, nil
	}
	defer func() { distillEvalExec = restoreExec }()

	// 1. Corpus: both sessions, smallest (s1) golden.
	if _, err := runDistillEvalCommand(t, t.TempDir(), "corpus", "--out", corpusDir, "--sessions", "2", "--golden", "1"); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	corpusPath := filepath.Join(corpusDir, "corpus.json")

	// 2. Baseline run: 2 facts per session.
	if _, err := runDistillEvalCommand(t, t.TempDir(), "run", "--corpus", corpusPath, "--name", "base"); err != nil {
		t.Fatalf("run base: %v", err)
	}
	baseDir := filepath.Join(corpusDir, "runs", "base")

	// 3. Label sheet for the golden session, filled programmatically.
	labelsPath := filepath.Join(baseDir, "labels.jsonl")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "labels", "--run", baseDir, "--corpus", corpusPath, "--out", labelsPath); err != nil {
		t.Fatalf("labels: %v", err)
	}
	sheet, err := os.ReadFile(labelsPath)
	if err != nil {
		t.Fatal(err)
	}
	var filled []string
	verdicts := []string{"keep", "kill"}
	sc := bufio.NewScanner(strings.NewReader(string(sheet)))
	i := 0
	for sc.Scan() {
		var line distillEvalLabelLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		line.Verdict = verdicts[i%len(verdicts)]
		i++
		out, _ := json.Marshal(line)
		filled = append(filled, string(out))
	}
	if i != 2 {
		raw, _ := os.ReadFile(filepath.Join(baseDir, "raw-facts.jsonl"))
		corpusData, _ := os.ReadFile(corpusPath)
		t.Fatalf("golden sheet has %d facts, want 2 (one golden session, 2 facts)\nraw-facts.jsonl:\n%s\ncorpus.json:\n%s", i, raw, corpusData)
	}
	if err := os.WriteFile(labelsPath, []byte(strings.Join(filled, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 4. Judge with a fake Fable, then calibrate.
	restoreJudge := distillEvalJudgeRunnerFor
	distillEvalJudgeRunnerFor = func(agent string) distillAgentRunner {
		return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
			return "1 keep\n2 kill\n", nil
		}
	}
	defer func() { distillEvalJudgeRunnerFor = restoreJudge }()
	judgePath := filepath.Join(baseDir, "judge.jsonl")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "judge", "--run", baseDir, "--out", judgePath); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if _, err := runDistillEvalCommand(t, t.TempDir(), "calibrate", "--labels", labelsPath, "--judge", judgePath); err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "calibration.json")); err != nil {
		t.Fatalf("calibration.json missing: %v", err)
	}

	// 5. Score baseline.
	baseSummaryPath := filepath.Join(baseDir, "summary.json")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", baseDir, "--corpus", corpusPath, "--labels", labelsPath, "--judge", judgePath, "--out", baseSummaryPath); err != nil {
		t.Fatalf("score base: %v", err)
	}
	baseSummary := loadScoreSummary(t, baseSummaryPath)
	if baseSummary.Facts != 4 {
		t.Fatalf("baseline facts = %d, want 4 (2 sessions x 2 facts)", baseSummary.Facts)
	}
	if baseSummary.JudgeConfig == "" || baseSummary.Precision.Combined == nil {
		t.Fatalf("baseline summary incomplete: %+v", baseSummary)
	}

	// 6. Capped variant: --cap 1 halves every session's facts.
	if _, err := runDistillEvalCommand(t, t.TempDir(), "run", "--corpus", corpusPath, "--name", "capped", "--cap", "1"); err != nil {
		t.Fatalf("run capped: %v", err)
	}
	cappedDir := filepath.Join(corpusDir, "runs", "capped")
	cappedSummaryPath := filepath.Join(cappedDir, "summary.json")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", cappedDir, "--corpus", corpusPath, "--out", cappedSummaryPath); err != nil {
		t.Fatalf("score capped: %v", err)
	}
	if got := loadScoreSummary(t, cappedSummaryPath).Facts; got != 2 {
		t.Fatalf("capped facts = %d, want 2", got)
	}

	// 7. Compare: capped wins facts on every pair.
	out, err := runDistillEvalCommand(t, t.TempDir(), "compare", baseSummaryPath, cappedSummaryPath)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	var result distillEvalComparison
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode compare output: %v\n%s", err, out)
	}
	for _, c := range result.Paired {
		if c.Metric == "facts" {
			if c.N != 2 || c.Winner != "b" || c.MeanA != 2 || c.MeanB != 1 {
				t.Errorf("facts comparison wrong: %+v", c)
			}
		}
	}
}
