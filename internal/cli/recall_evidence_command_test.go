package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestRecallEvidenceCommandReturnsOriginalSourceWithoutModel(t *testing.T) {
	opts, brain := setupRecallIdentityTest(t)
	rel := "sessions/evidence.txt"
	if err := os.MkdirAll(filepath.Join(brain, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	text := "Release requires owner approval."
	if err := os.WriteFile(filepath.Join(brain, rel), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	m := exportManifest{SchemaVersion: brainManifestSchemaVersion, DefaultBranch: "main", Sources: &brainSources{Sessions: &sessionSourceManifest{DefaultBranch: "main", Sessions: []exportSession{{SessionID: "release", Branch: "main", TranscriptPath: rel}}}}}
	if err := writeBrainManifestAndReadme(brain, m); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	cmd := newRecallCommandWithEmbedder(opts, func() Embedder {
		t.Fatal("evidence recall initialized a model")
		return nil
	})
	output, err := execute(t, cmd, "release approval", "--evidence", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Evidence []struct{ Text string } `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Text != text {
		t.Fatalf("original source missing: %s", output)
	}
}

func TestRecallEvidenceBudgetAndStableOrdering(t *testing.T) {
	opts, _ := evidenceFixture(t)
	read := func(budget string) evidenceRecallResult {
		t.Helper()
		out, err := execute(t, newRecallCommand(opts), "target", "--evidence", "--evidence-bytes", budget, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var got evidenceRecallResult
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(got.Evidence)
		if len(encoded) != got.EvidenceBytes || got.EvidenceBytes > got.EvidenceByteBudget || got.ReturnedCount != len(got.Evidence) {
			t.Fatalf("incorrect output accounting: %s", out)
		}
		return got
	}
	full := read("8192")
	if full.ReturnedCount != 3 || full.Truncated || !reflect.DeepEqual(full, read("8192")) {
		t.Fatalf("unstable or incomplete fixture result: %+v", full)
	}
	empty := read("2")
	if empty.ReturnedCount != 0 || empty.Evidence == nil || !empty.Truncated || len(empty.OmittedIDs) != full.CandidateCount {
		t.Fatalf("empty output must explain omissions: %+v", empty)
	}
	spans := []evidenceSpan{{ID: "large", Text: strings.Repeat("x", 2000)}, {ID: "small", Text: "<tag> café ಕನ್ನಡ\r\n\"quoted\""}}
	one, _ := json.Marshal(spans[1:])
	packed, omitted, size, err := packDeterministicEvidence(spans, len(one))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(packed, spans[1:]) || !reflect.DeepEqual(omitted, []string{"large"}) || size != len(one) {
		t.Fatalf("must skip oversized blocks and preserve exact small text: %+v %v %d", packed, omitted, size)
	}
}

func TestRecallEvidenceRejectsUnsupportedOptions(t *testing.T) {
	opts, _ := evidenceFixture(t)
	for _, args := range [][]string{
		{"--agent", "codex"}, {"--agent", "none"}, {"--model", "model"}, {"--agent-command", "fake"},
		{"--expand"}, {"--scope", "local"}, {"--kind", "decision"}, {"--locus", "path"}, {"--all"},
		{"--eligible-before", "2026-01-01T00:00:00Z"}, {"--session-dates", "missing.json"},
		{"--exclude-session-id", "private"}, {"--read-only-semantic-cache"},
		{"--k", "0"}, {"--k", "129"}, {"--evidence-bytes", "1"}, {"--evidence-bytes", "1048577"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, err := execute(t, newRecallCommand(opts), append([]string{"target", "--evidence", "--json"}, args...)...)
			if err == nil || strings.Contains(out, "Target implementation") {
				t.Fatalf("invalid options emitted evidence: %s %v", out, err)
			}
		})
	}
	for _, args := range [][]string{{"--evidence"}, {" ", "--evidence"}, {"target", "--evidence-bytes", "8192"}} {
		if _, err := execute(t, newRecallCommand(opts), args...); err == nil {
			t.Fatalf("accepted invalid options: %v", args)
		}
	}
}

func TestRecallEvidenceLeavesOrdinaryRecallAndFactsUnchanged(t *testing.T) {
	opts, brain := evidenceFixture(t)
	factsBefore, err := loadFacts(brain, "main")
	if err != nil {
		t.Fatal(err)
	}
	before, err := execute(t, newRecallCommand(opts), "target", "--json", "--no-semantic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, `"facts"`) || strings.Contains(before, `"evidence"`) {
		t.Fatalf("ordinary recall envelope changed: %s", before)
	}
	if _, err := execute(t, newRecallCommand(opts), "target", "--evidence", "--json"); err != nil {
		t.Fatal(err)
	}
	after, err := execute(t, newRecallCommand(opts), "target", "--json", "--no-semantic")
	if err != nil {
		t.Fatal(err)
	}
	factsAfter, err := loadFacts(brain, "main")
	if err != nil || !reflect.DeepEqual(factsBefore, factsAfter) || before != after {
		t.Fatalf("evidence recall changed facts: %v", err)
	}
}

func TestRecallEvidenceExclusionsAndUnsafePaths(t *testing.T) {
	for _, mode := range []string{"excluded", "outside-root"} {
		t.Run(mode, func(t *testing.T) {
			opts, brain := evidenceFixture(t)
			if mode == "excluded" {
				stones := loadSessionTombstones(brain)
				stones.Excluded["evidence"] = sessionTombstone{At: opts.Now()}
				if err := saveSessionTombstones(brain, stones); err != nil {
					t.Fatal(err)
				}
			} else {
				m, err := loadBrainManifest(brain)
				if err != nil {
					t.Fatal(err)
				}
				m.Sources.Sessions.Sessions[0].TranscriptPath = "../outside.jsonl"
				if err := os.WriteFile(filepath.Join(brain, "..", "outside.jsonl"), []byte(`{"role":"user","content":"target private outside"}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := writeBrainManifestAndReadme(brain, *m); err != nil {
					t.Fatal(err)
				}
			}
			out, err := execute(t, newRecallCommand(opts), "target", "--evidence", "--json")
			if strings.Contains(out, "Target implementation") || strings.Contains(out, "target private outside") {
				t.Fatalf("private source escaped: %s", out)
			}
			if err == nil {
				var got evidenceRecallResult
				if err := json.Unmarshal([]byte(out), &got); err != nil || got.ReturnedCount != 0 {
					t.Fatalf("unsafe source returned: %s %v", out, err)
				}
			}
		})
	}
}

type evidenceInspectWriter func([]byte) (int, error)

func (f evidenceInspectWriter) Write(b []byte) (int, error) { return f(b) }

func TestRecallEvidencePrivacyLockCoversOutputAndWriteErrors(t *testing.T) {
	_, brain := evidenceFixture(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetErr(io.Discard)
	writes := 0
	cmd.SetOut(evidenceInspectWriter(func(b []byte) (int, error) {
		writes++
		unlock, err := acquireBrainWriteLockTimeout(brain, 10*time.Millisecond)
		if err == nil {
			unlock()
			t.Fatal("source output emitted without privacy lock")
		}
		return 0, io.ErrClosedPipe
	}))
	if err := runRecallEvidence(cmd, brain, "main", "target", 10, 8192, true); !errors.Is(err, io.ErrClosedPipe) || writes != 1 {
		t.Fatalf("lost output error: %v writes=%d", err, writes)
	}
	unlock, err := acquireBrainWriteLockTimeout(brain, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("privacy lock leaked: %v", err)
	}
	unlock()
}

func TestRecallEvidenceCancellationEmitsNoOutput(t *testing.T) {
	_, brain := evidenceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	if err := runRecallEvidence(cmd, brain, "main", "target", 10, 8192, true); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("cancel emitted output: %v %s", err, output.String())
	}
}
