package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type evalCommandFailWriter struct{}

func (evalCommandFailWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("eval output failure") }

func evalCommandFixture(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC) }}
	storage, err := repoStoragePaths(newHistoryEvalCommand(opts).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir, manifest, index := historyEvalFixture(t)
	for _, session := range manifest.Sources.Sessions.Sessions {
		data, err := os.ReadFile(filepath.Join(sourceDir, filepath.FromSlash(session.TranscriptPath)))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(storage.BrainDir, filepath.FromSlash(session.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(storage.BrainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	indexBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, filepath.FromSlash(historyIndexPath)), append(indexBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.RepoKey = "gh/example/eval"
	manifest.Sources.History = &historySourceManifest{GeneratedAt: index.GeneratedAt, IndexPath: historyIndexPath, Records: len(index.Records)}
	if err := writeBrainManifestAndReadme(storage.BrainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

func TestHistoryEvalRootGenerateThenSubstringAndBM25(t *testing.T) {
	opts, _ := evalCommandFixture(t)
	tasksPath := filepath.Join(t.TempDir(), "tasks.json")
	out, err := execute(t, NewRootCommand(opts), "history-eval-gen", "--out", tasksPath, "--midtask=false")
	if err != nil || !strings.Contains(out, "wrote 2 tasks") {
		t.Fatalf("eval-gen file: err=%v out=%q", err, out)
	}
	tasks, err := loadEvalTasks(tasksPath)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("generated tasks=%+v err=%v", tasks, err)
	}
	for _, task := range tasks {
		wantLabels := map[string][]string{"add retry backoff to the fetcher": {"r1", "r2"}, "why does the exporter skip unchanged sessions": {"r4", "r5"}}
		if task.LabelSource != evalLabelSourceProvenanceSilver || !reflect.DeepEqual(task.Relevant, wantLabels[task.Task]) {
			t.Fatalf("task lost provenance labels: %+v", task)
		}
	}

	for _, arm := range []string{"substring", "bm25"} {
		jsonOut, err := execute(t, NewRootCommand(opts), "history-eval", "--tasks", tasksPath, "--arm", arm, "--k", "2", "--json")
		if err != nil {
			t.Fatalf("%s JSON: %v\n%s", arm, err, jsonOut)
		}
		var summary evalSummary
		if err := json.Unmarshal([]byte(jsonOut), &summary); err != nil || summary.Arm != arm || len(summary.Results) != 2 {
			t.Fatalf("%s summary=%+v decode=%v\n%s", arm, summary, err, jsonOut)
		}
		for _, result := range summary.Results {
			if !result.Labeled || result.RelevanceSource != evalRelevanceExplicitLabel || result.LabelSource != evalLabelSourceProvenanceSilver {
				t.Fatalf("%s result lost proof provenance: %+v", arm, result)
			}
		}
		text, err := execute(t, NewRootCommand(opts), "history-eval", "--tasks", tasksPath, "--arm", arm, "--k", "2")
		if err != nil || !strings.Contains(text, "relevance") || !strings.Contains(text, "provenance") || strings.Contains(text, "add retry backoff") {
			t.Fatalf("%s text contract: err=%v\n%s", arm, err, text)
		}
	}
}

func TestHistoryEvalCommandsMalformedEmptyAndOutputFailure(t *testing.T) {
	opts, brainDir := evalCommandFixture(t)
	if _, err := execute(t, NewRootCommand(opts), "history-eval"); err == nil || !strings.Contains(err.Error(), "--tasks") {
		t.Fatalf("missing tasks error=%v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, NewRootCommand(opts), "history-eval", "--tasks", bad); err == nil {
		t.Fatal("malformed tasks unexpectedly accepted")
	}
	validTasks := filepath.Join(t.TempDir(), "valid.json")
	if err := os.WriteFile(validTasks, []byte(`[{"id":"t1","task":"retry backoff","relevant":["r1"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, NewRootCommand(opts), "history-eval", "--tasks", validTasks, "--arm", "invalid"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("invalid arm error = %v", err)
	}

	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions = nil
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, NewRootCommand(opts), "history-eval-gen", "--midtask=false"); err == nil || !strings.Contains(err.Error(), "no sessions qualified") {
		t.Fatalf("empty generation error=%v", err)
	}

	cmd := newHistoryEvalGenCommand(opts)
	cmd.SetOut(evalCommandFailWriter{})
	// Restore the fixture so generation reaches stdout transport.
	_, _, index := historyEvalFixture(t)
	manifest.Sources.Sessions.Sessions = []exportSession{{SessionID: "session-aaaa-1111", Branch: "main", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: opts.Now()}}
	manifest.Sources.History = &historySourceManifest{GeneratedAt: index.GeneratedAt, IndexPath: historyIndexPath, Records: len(index.Records)}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs([]string{"--midtask=false"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "eval output failure") {
		t.Fatalf("stdout failure=%v", err)
	}
}

func TestFactsEvalCompareRootTextJSONAndWriteFailure(t *testing.T) {
	opts, _ := evalCommandFixture(t)
	makeSummary := func(arm string, precision float64) evalSummary {
		results := []evalTaskResult{
			{ID: "task-a", Task: "retry backoff", QueryType: queryTypeHowto, Labeled: true, RelevanceSource: evalRelevanceExplicitLabel, LabelSource: evalLabelSourceProvenanceSilver, Precision: precision, Recall: precision, Tokens: 20, RelevantSurfaced: 1, Surfaced: 1},
			{ID: "task-b", Task: "export cache", QueryType: queryTypeConcept, Labeled: true, RelevanceSource: evalRelevanceExplicitLabel, LabelSource: evalLabelSourceProvenanceSilver, Precision: precision, Recall: precision, Tokens: 30, RelevantSurfaced: 1, Surfaced: 1},
		}
		s := summarizeEval(results)
		s.Retriever, s.Arm = evalRetrieverHistory, arm
		return s
	}
	writeSummary := func(name string, summary evalSummary) string {
		path := filepath.Join(t.TempDir(), name)
		data, err := json.Marshal(summary)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	aPath := writeSummary("substring.json", makeSummary("substring", 0.5))
	bPath := writeSummary("bm25.json", makeSummary("bm25", 1.0))

	jsonOut, err := execute(t, NewRootCommand(opts), "facts", "eval-compare", "--a", aPath, "--b", bPath, "--json")
	if err != nil {
		t.Fatalf("compare JSON: %v\n%s", err, jsonOut)
	}
	var payload struct {
		N                int                `json:"n"`
		ARetriever       string             `json:"a_retriever"`
		BRetriever       string             `json:"b_retriever"`
		ReleaseClaimable bool               `json:"release_claimable"`
		Metrics          []metricComparison `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil || payload.N != 2 || payload.ARetriever != evalRetrieverHistory || payload.BRetriever != evalRetrieverHistory || payload.ReleaseClaimable || len(payload.Metrics) == 0 {
		t.Fatalf("compare payload=%+v decode=%v\n%s", payload, err, jsonOut)
	}

	foundPrecision := false
	for _, metric := range payload.Metrics {
		if metric.Metric == "precision" {
			foundPrecision = true
			if metric.N != 2 || metric.MeanA != 0.5 || metric.MeanB != 1 || metric.Delta != 0.5 {
				t.Fatalf("comparison direction/count lost: %+v", metric)
			}
		}
	}
	if !foundPrecision {
		t.Fatal("comparison omitted precision")
	}
	text, err := execute(t, NewRootCommand(opts), "facts", "eval-compare", "--a", aPath, "--b", bPath)
	if err != nil || !strings.Contains(text, "paired A/B over 2 shared tasks") || !strings.Contains(text, "A=history B=history") || !strings.Contains(text, "n=2 is small") {
		t.Fatalf("compare text: %v\n%s", err, text)
	}

	cmd := newFactsEvalCompareCommand(opts)
	cmd.SetOut(evalCommandFailWriter{})
	cmd.SetArgs([]string{"--a", aPath, "--b", bPath, "--json"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "eval output failure") {
		t.Fatalf("compare write failure=%v", err)
	}
	if _, err := execute(t, NewRootCommand(opts), "facts", "eval-compare", "--a", aPath, "--b", filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing comparison summary unexpectedly accepted")
	}
}
