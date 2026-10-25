package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLabelFixtureRun(t *testing.T, corpusDir string) (runDir, corpusPath string) {
	t.Helper()
	corpus := distillEvalCorpus{
		RepoKey: "test/repo", CreatedAt: time.Now(), CorpusSHA256: "abc",
		Sessions: []distillEvalCorpusEntry{
			{SessionID: "s-golden", Branch: "main", TranscriptPath: "sessions/main/s-golden.jsonl", Golden: true},
			{SessionID: "s-plain", Branch: "main", TranscriptPath: "sessions/main/s-plain.jsonl"},
		},
	}
	corpusPath = filepath.Join(corpusDir, "corpus.json")
	data, _ := json.Marshal(corpus)
	if err := os.WriteFile(corpusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	runDir = filepath.Join(corpusDir, "runs", "base")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := strings.Join([]string{
		`{"type":"chunk","session_id":"s-golden","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":1,"cap":6}`,
		`{"type":"fact","session_id":"s-golden","branch":"main","chunk_index":1,"fact_id":"f1","kind":"gotcha","paths":["a.b.c"],"text":"golden fact","checkpoint_id":"cp"}`,
		`{"type":"fact","session_id":"s-plain","branch":"main","chunk_index":1,"fact_id":"f2","kind":"decision","paths":["d.e.f"],"text":"plain fact","checkpoint_id":"cp"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(runDir, "raw-facts.jsonl"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return runDir, corpusPath
}

func TestDistillEvalLabelsSheetCoversGoldenFactsOnly(t *testing.T) {
	runDir, corpusPath := writeLabelFixtureRun(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "labels.jsonl")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "labels", "--run", runDir, "--corpus", corpusPath, "--out", out); err != nil {
		t.Fatalf("labels: %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var sheet []distillEvalLabelLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line distillEvalLabelLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad sheet line %q: %v", sc.Text(), err)
		}
		sheet = append(sheet, line)
	}
	if len(sheet) != 1 {
		t.Fatalf("sheet has %d lines, want 1 (golden sessions only): %+v", len(sheet), sheet)
	}
	l := sheet[0]
	if l.FactID != "f1" || l.SessionID != "s-golden" || l.TranscriptPath != "sessions/main/s-golden.jsonl" ||
		l.Kind != "gotcha" || l.Text != "golden fact" || l.Verdict != "" {
		t.Errorf("sheet line wrong: %+v", l)
	}
}

func TestLoadDistillEvalLabelsRejectsBadVerdict(t *testing.T) {
	for _, bad := range []string{"Keep", "kil", ""} {
		path := filepath.Join(t.TempDir(), "labels.jsonl")
		content := `{"fact_id":"f1","verdict":"keep"}` + "\n" +
			`{"fact_id":"f2","verdict":"` + bad + `"}` + "\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadDistillEvalLabels(path)
		if err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("verdict %q: expected error naming line 2, got %v", bad, err)
		}
	}
}

func TestLoadDistillEvalLabelsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.jsonl")
	content := `{"fact_id":"f1","verdict":"keep","reason":"useful"}` + "\n" +
		`{"fact_id":"f2","verdict":"kill","reason":"restates code"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	labels, err := loadDistillEvalLabels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 || labels["f1"].Verdict != "keep" || labels["f2"].Verdict != "kill" || labels["f2"].Reason != "restates code" {
		t.Errorf("labels wrong: %+v", labels)
	}
}
