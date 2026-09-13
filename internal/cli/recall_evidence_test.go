package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func evidenceFixture(t *testing.T) (Options, string) {
	t.Helper()
	opts, brain := setupRecallIdentityTest(t)
	rel := "sessions/main/evidence.jsonl"
	body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Keep target regression café ಕನ್ನಡ 🧠\r\n\r\nTarget needs approval too."},{"type":"tool_result","content":"DO NOT SEND TOOL SECRET"}]}}` + "\n" + `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"DO NOT SEND THOUGHTS"},{"type":"text","text":"Target implementation reported."}]}}` + "\n"
	if err := os.MkdirAll(filepath.Join(brain, "sessions", "main"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brain, filepath.FromSlash(rel)), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	m := exportManifest{SchemaVersion: brainManifestSchemaVersion, DefaultBranch: "main", Sources: &brainSources{Sessions: &sessionSourceManifest{DefaultBranch: "main", Sessions: []exportSession{{SessionID: "evidence", Branch: "main", TranscriptPath: rel, CreatedAt: opts.Now()}}}}}
	if err := writeBrainManifestAndReadme(brain, m); err != nil {
		t.Fatal(err)
	}
	return opts, brain
}

func evidenceValidResponse(r evidenceRequest) evidenceSelection {
	out := evidenceSelection{RequestSHA256: r.RequestSHA256, Assessments: []evidenceAssessment{}, Links: []evidenceLink{}}
	for _, s := range r.Spans {
		out.Assessments = append(out.Assessments, evidenceAssessment{s.ID, "direct", "applicable"})
	}
	return out
}

func evidenceTestRequest(t *testing.T) evidenceRequest {
	t.Helper()
	_, brain := evidenceFixture(t)
	c, err := collectEvidence(context.Background(), brain, "main", "target", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spans) != 3 {
		t.Fatalf("spans=%+v", c)
	}
	sort.SliceStable(c.Spans, func(i, j int) bool {
		if c.Spans[i].Line != c.Spans[j].Line {
			return c.Spans[i].Line < c.Spans[j].Line
		}
		return c.Spans[i].StartByte < c.Spans[j].StartByte
	})
	return makeEvidenceRequest("target", "main", c)
}

func TestRecallEvidenceSourceAnchors(t *testing.T) {
	_, brain := evidenceFixture(t)
	c, err := collectEvidence(context.Background(), brain, "main", "target", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range c.Spans {
		data, err := os.ReadFile(filepath.Join(brain, filepath.FromSlash(s.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if evidenceHash(data) != s.SourceSHA256 {
			t.Fatal("source hash mismatch")
		}
		fields, _ := evidenceFields(data, s.Path)
		found := false
		for _, f := range fields {
			if f.line == s.Line && f.pointer == s.JSONPointer {
				found = true
				if evidenceHash([]byte(f.text)) != s.ContentSHA256 || f.text[s.StartByte:s.EndByte] != s.Text {
					t.Fatal("source byte mismatch")
				}
			}
		}
		if !found || strings.Contains(s.Text, "DO NOT SEND") {
			t.Fatalf("bad field: %+v", s)
		}
	}
	old := c.Spans[0].ID
	p := filepath.Join(brain, filepath.FromSlash(c.Spans[0].Path))
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := collectEvidence(context.Background(), brain, "main", "target", 10)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Spans[0].ID == old {
		t.Fatal("changed file retained versioned span ID")
	}
}

func TestRecallEvidenceBlocksKeepFences(t *testing.T) {
	text := "café ಕನ್ನಡ\r\n\r\n```go\r\nx := 1\r\n\r\nx++\r\n```\r\n\r\n| A | B |\n| 1 | 2 |\n"
	ranges := evidenceBlocks(text)
	if len(ranges) != 3 || !strings.Contains(text[ranges[1][0]:ranges[1][1]], "\r\n\r\nx++") {
		t.Fatalf("blocks=%v", ranges)
	}
}

func TestRecallEvidenceStrictValidation(t *testing.T) {
	r := evidenceTestRequest(t)
	tests := map[string]func(*evidenceSelection){
		"request":    func(s *evidenceSelection) { s.RequestSHA256 = "wrong" },
		"duplicate":  func(s *evidenceSelection) { s.Assessments[1] = s.Assessments[0] },
		"missing":    func(s *evidenceSelection) { s.Assessments = s.Assessments[:1] },
		"unknown":    func(s *evidenceSelection) { s.Assessments[0].ID = "invented" },
		"bad status": func(s *evidenceSelection) { s.Assessments[0].Status = "truth" },
		"null links": func(s *evidenceSelection) { s.Links = nil },
		"backwards":  func(s *evidenceSelection) { s.Links = []evidenceLink{{r.Spans[1].ID, r.Spans[0].ID, "replaces"}} },
		"future replacement": func(s *evidenceSelection) {
			s.Assessments[1].Status = "future"
			s.Links = []evidenceLink{{r.Spans[0].ID, r.Spans[1].ID, "replaces"}}
		},
		"support requirement": func(s *evidenceSelection) {
			s.Assessments[1].Relevance = "supporting"
			s.Links = []evidenceLink{{r.Spans[0].ID, r.Spans[1].ID, "adds_requirement"}}
		},
		"conflicting relations": func(s *evidenceSelection) {
			s.Links = []evidenceLink{{r.Spans[0].ID, r.Spans[1].ID, "replaces"}, {r.Spans[0].ID, r.Spans[2].ID, "conflicts"}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := evidenceValidResponse(r)
			mutate(&s)
			b, _ := json.Marshal(s)
			if _, err := validateEvidenceJudgments(r, string(b)); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	s := evidenceValidResponse(r)
	b, _ := json.Marshal(s)
	if _, err := validateEvidenceJudgments(r, string(b)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{string(b) + " {}", strings.Replace(string(b), "{", `{"extra":true,`, 1), strings.Replace(string(b), "{", `{"request_sha256":"forged",`, 1)} {
		if _, err := validateEvidenceJudgments(r, bad); err == nil {
			t.Fatal("accepted malformed envelope")
		}
	}
}

func TestRecallEvidenceWholeGroupsAndOptionalCorroboration(t *testing.T) {
	r := evidenceTestRequest(t)
	s := evidenceValidResponse(r)
	one, _ := json.Marshal(r.Spans[:1])
	budget := len(one)
	s.Links = []evidenceLink{{r.Spans[0].ID, r.Spans[1].ID, "adds_requirement"}}
	p := packEvidence(r.Spans, &s, budget)
	for _, span := range p.Spans {
		if span.ID == r.Spans[0].ID || span.ID == r.Spans[1].ID {
			t.Fatal("partial required group")
		}
	}
	s.Links[0].Kind = "conflicts"
	p = packEvidence(r.Spans, &s, budget)
	for _, span := range p.Spans {
		if span.ID == r.Spans[0].ID || span.ID == r.Spans[1].ID {
			t.Fatal("partial conflict group")
		}
	}
	s.Links[0].Kind = "corroborates"
	p = packEvidence(r.Spans, &s, budget)
	if len(p.Spans) != 1 || p.Spans[0].ID != r.Spans[0].ID {
		t.Fatal("optional context crowded out direct evidence")
	}
	s.Links[0].Kind = "replaces"
	p = packEvidence(r.Spans, &s, 8192)
	for _, span := range p.Spans {
		if span.ID == r.Spans[0].ID {
			t.Fatal("returned replaced block")
		}
	}
	p = packEvidence(r.Spans, nil, 2)
	if len(p.Spans) != 0 || len(p.OmittedIDs) != len(r.Spans) || p.Bytes != 2 {
		t.Fatal("empty budget accounting")
	}
}

func TestRecallEvidenceFallbackAndOptIn(t *testing.T) {
	opts, brain := evidenceFixture(t)
	for _, mode := range []string{"failure", "invalid", "valid"} {
		t.Run(mode, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&bytes.Buffer{})
			calls := 0
			run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
				calls++
				if dir == opts.Env.RepoRoot || timeout != 180*time.Second {
					t.Fatal("provider not isolated/bounded")
				}
				if bytes.Contains(input, []byte("DO NOT SEND")) {
					t.Fatal("private content in request")
				}
				if mode == "failure" {
					return "", errors.New("provider secret should not leak")
				}
				if mode == "invalid" {
					return `{}`, nil
				}
				var r evidenceRequest
				if err := json.Unmarshal(input, &r); err != nil {
					t.Fatal(err)
				}
				wire := evidenceWireSelection{RequestSHA256: r.RequestSHA256, Selected: [][]string{}, Supporting: [][]string{}, Links: [][]string{}}
				for _, source := range r.Sources {
					for _, field := range source.Fields {
						for _, block := range field.Blocks {
							wire.Selected = append(wire.Selected, []string{block[0], "applicable"})
						}
					}
				}
				b, _ := json.Marshal(wire)
				return string(b), nil
			}
			err := runRecallEvidence(cmd, opts, opts.Env.RepoRoot, brain, "main", "target", 10, 8192, "command", "", []string{"fake"}, true, run)
			if err != nil {
				t.Fatal(err)
			}
			var got evidenceRecallResult
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := "fallback"
			if mode == "valid" {
				want = "model"
			}
			if calls != 1 || got.SelectionMode != want || got.ReturnedCount != 3 || strings.Contains(buf.String(), "provider secret") {
				t.Fatalf("bad result: %s", buf.String())
			}
			encoded, _ := json.Marshal(got.Evidence)
			if len(encoded) != got.EvidenceBytes {
				t.Fatal("byte accounting")
			}
		})
	}
	output, err := execute(t, newRecallCommand(opts), "target", "--json", "--no-semantic")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, `"evidence"`) || !strings.Contains(output, `"facts"`) {
		t.Fatal("default recall changed")
	}
	output, err = execute(t, newRecallCommand(opts), "target", "--evidence", "--agent", "none", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, `"selection_mode": "deterministic"`) {
		t.Fatal(output)
	}
}

func TestRecallEvidenceMissingBranchAndSource(t *testing.T) {
	_, brain := evidenceFixture(t)
	c, err := collectEvidence(context.Background(), brain, "other", "target", 10)
	if err != nil || len(c.Spans) != 0 {
		t.Fatalf("cross-branch evidence: %+v %v", c, err)
	}
	m, err := loadBrainManifest(brain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(brain, filepath.FromSlash(m.Sources.Sessions.Sessions[0].TranscriptPath))); err != nil {
		t.Fatal(err)
	}
	c, err = collectEvidence(context.Background(), brain, "main", "target", 10)
	if err != nil || c.State != "unavailable" || len(c.Warnings) == 0 || len(c.Spans) != 0 {
		t.Fatalf("missing source: %+v %v", c, err)
	}
}

func TestRecallEvidencePrivacyAndNoEgress(t *testing.T) {
	opts, brain := evidenceFixture(t)
	stones := loadSessionTombstones(brain)
	stones.Excluded["evidence"] = sessionTombstone{At: opts.Now()}
	if err := saveSessionTombstones(brain, stones); err != nil {
		t.Fatal(err)
	}
	c, err := collectEvidence(context.Background(), brain, "main", "target", 10)
	if err != nil || len(c.Spans) != 0 {
		t.Fatalf("excluded source returned: %+v %v", c, err)
	}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	for _, agent := range []string{"codex", "claude-code", "command", "unknown"} {
		if _, err := evidenceSelectorArgs(agent, "", []string{"fake"}); err == nil {
			t.Fatalf("egress allowed: %s", agent)
		}
	}
	if _, err := evidenceSelectorArgs("ollama", "model", nil); err != nil {
		t.Fatal(err)
	}
}

func TestRecallEvidencePrivacyLockHeldDuringProvider(t *testing.T) {
	opts, brain := evidenceFixture(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		unlock, err := acquireBrainWriteLockTimeout(brain, 10*time.Millisecond)
		if err == nil {
			unlock()
			t.Fatal("provider ran without privacy lock")
		}
		return "", errors.New("offline")
	}
	if err := runRecallEvidence(cmd, opts, opts.Env.RepoRoot, brain, "main", "target", 10, 8192, "command", "", []string{"fake"}, true, run); err != nil {
		t.Fatal(err)
	}
}

func TestRecallEvidenceRejectsAmbiguousIdentity(t *testing.T) {
	_, brain := evidenceFixture(t)
	m, err := loadBrainManifest(brain)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := m.Sources.Sessions.Sessions[0]
	duplicate.TranscriptPath = "sessions/other.txt"
	m.Sources.Sessions.Sessions = append(m.Sources.Sessions.Sessions, duplicate)
	if err := writeBrainManifestAndReadme(brain, *m); err != nil {
		t.Fatal(err)
	}
	if _, err := collectEvidence(context.Background(), brain, "main", "target", 10); err == nil {
		t.Fatal("ambiguous source accepted")
	}
}

func TestRecallEvidenceCandidateCapAndMalformedContent(t *testing.T) {
	_, brain := evidenceFixture(t)
	m, err := loadBrainManifest(brain)
	if err != nil {
		t.Fatal(err)
	}
	rel := m.Sources.Sessions.Sessions[0].TranscriptPath
	line := `{"role":"user","content":"target ` + strings.Repeat("x", evidenceInputBytes) + `"}` + "\n" + `{"role":"user","content":"target small complete block"}` + "\n{broken\n"
	if err := os.WriteFile(filepath.Join(brain, filepath.FromSlash(rel)), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := collectEvidence(context.Background(), brain, "main", "target", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !c.InputTruncated || c.State != "partial" || len(c.Spans) != 1 || c.Spans[0].Text != "target small complete block" {
		t.Fatalf("cap silently lost coverage: %+v", c)
	}
}

func TestRecallEvidenceDocumentPointerAndUnknownFormat(t *testing.T) {
	data := []byte(`{"messages":[{"info":{"role":"user"},"parts":[{"type":"text","text":"  preserve café\r\n"},{"type":"tool","state":{"output":"hidden"}}]}]}`)
	fields, partial := evidenceFields(data, "session.json")
	if partial || len(fields) != 1 || fields[0].pointer != "/messages/0/parts/0/text" || fields[0].text != "  preserve café\r\n" {
		t.Fatalf("document extraction: %+v %v", fields, partial)
	}
	fields, partial = evidenceFields([]byte(`{"unrecognized":"not conversation evidence"}`), "session.jsonl")
	if !partial || len(fields) != 0 {
		t.Fatal("unsupported data became evidence")
	}
}

func TestRecallEvidenceBoundsAndCancellation(t *testing.T) {
	r := evidenceTestRequest(t)
	if _, err := validateEvidenceJudgments(r, strings.Repeat("[", 20)+"0"+strings.Repeat("]", 20)); err == nil {
		t.Fatal("accepted excessive nesting")
	}
	opts, brain := evidenceFixture(t)
	cmd := &cobra.Command{}
	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		cancel()
		return "", context.Canceled
	}
	if err := runRecallEvidence(cmd, opts, opts.Env.RepoRoot, brain, "main", "target", 10, 8192, "command", "", []string{"fake"}, true, run); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancel emitted fallback: %v %s", err, out.String())
	}
}
