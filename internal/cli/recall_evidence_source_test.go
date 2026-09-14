package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRecallEvidenceSourceAnchors(t *testing.T) {
	_, brain := evidenceFixture(t)
	c, err := collectEvidence(context.Background(), brain, "main", "target", 10, 8192)
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
	changed, err := collectEvidence(context.Background(), brain, "main", "target", 10, 8192)
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

func TestRecallEvidenceMissingBranchAndSource(t *testing.T) {
	_, brain := evidenceFixture(t)
	c, err := collectEvidence(context.Background(), brain, "other", "target", 10, 8192)
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
	c, err = collectEvidence(context.Background(), brain, "main", "target", 10, 8192)
	if err != nil || c.State != "unavailable" || len(c.Warnings) == 0 || len(c.Spans) != 0 {
		t.Fatalf("missing source: %+v %v", c, err)
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
	if _, err := collectEvidence(context.Background(), brain, "main", "target", 10, 8192); err == nil {
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
	c, err := collectEvidence(context.Background(), brain, "main", "target", 10, 8192)
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

func TestRecallEvidenceDiverseNaturalLanguageCandidates(t *testing.T) {
	opts, brain := setupRecallIdentityTest(t)
	var sessions []exportSession
	for i := 0; i < 3; i++ {
		rel := fmt.Sprintf("sessions/kit-%d.jsonl", i)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(brain, rel)), 0700); err != nil {
			t.Fatal(err)
		}
		var body strings.Builder
		if i == 0 {
			for j := 0; j < 180; j++ {
				b, _ := json.Marshal(map[string]string{"role": "assistant", "content": fmt.Sprintf("Model kits painting advice %d: %s", j, strings.Repeat("generic materials and tools. ", 25))})
				body.Write(b)
				body.WriteByte('\n')
			}
		}
		text := fmt.Sprintf("I completed model kit number %d yesterday.", i)
		b, _ := json.Marshal(map[string]string{"role": "user", "content": text})
		body.Write(b)
		body.WriteByte('\n')
		if i == 2 {
			b, _ = json.Marshal(map[string]string{"role": "assistant", "content": "The model kit display code you asked me to remember is cedar-17."})
			body.Write(b)
			body.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(brain, rel), []byte(body.String()), 0600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, exportSession{SessionID: fmt.Sprintf("kit-%d", i), Branch: "main", TranscriptPath: rel, CreatedAt: opts.Now()})
	}
	m := exportManifest{SchemaVersion: brainManifestSchemaVersion, DefaultBranch: "main", Sources: &brainSources{Sessions: &sessionSourceManifest{DefaultBranch: "main", Sessions: sessions}}}
	if err := writeBrainManifestAndReadme(brain, m); err != nil {
		t.Fatal(err)
	}
	c, err := collectEvidence(context.Background(), brain, "main", "How many model kits have I completed?", 128, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionsMatched != 3 || !c.InputTruncated || len(c.Spans) > 128 {
		t.Fatalf("bad coverage/caps: matched=%d truncated=%v spans=%d", c.SessionsMatched, c.InputTruncated, len(c.Spans))
	}
	found := map[string]bool{}
	assistant := false
	cost := 0
	for _, s := range c.Spans {
		cost += evidenceCandidateCost(s)
		if strings.Contains(s.Text, "I completed") {
			found[s.SessionID] = true
		}
		if strings.Contains(s.Text, "cedar-17") {
			assistant = true
		}
	}
	if len(found) != 3 || !assistant || cost > evidenceCandidateBytes {
		t.Fatalf("lost distinct source evidence: users=%v assistant=%v cost=%d", found, assistant, cost)
	}
}

func TestRecallEvidenceConversationMatchesArePositive(t *testing.T) {
	long := "I finished a model tank. " + strings.Repeat("Weathering guidance. ", 400)
	if evidenceQueryScore(long, "How many model kits have I worked on?") <= 0 {
		t.Fatal("long transcript lost its valid partial match")
	}
	if evidenceQueryScore("I completed one project.", "Which projects have I completed?") < 2 {
		t.Fatal("lost singular form")
	}
	if evidenceQueryScore("unrelated ocean tides", "model kits") != 0 {
		t.Fatal("unrelated transcript matched")
	}
}

func TestRecallEvidenceCandidateCostIncludesLongMetadata(t *testing.T) {
	stamp := "2026-09-13T00:00:00." + strings.Repeat("1", evidenceCandidateBytes) + "Z"
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Fatal(err)
	}
	s := evidenceSpan{Line: 1, Role: "user", Timestamp: stamp, Text: "target"}
	if evidenceCandidateCost(s) <= evidenceCandidateBytes {
		t.Fatal("valid oversized timestamp bypassed candidate budget")
	}
}
