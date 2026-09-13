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

func TestRecallEvidenceSparseProtocol(t *testing.T) {
	r := evidenceTestRequest(t)
	wire := evidenceWireSelection{RequestSHA256: r.RequestSHA256, Selected: [][]string{{"b0", "applicable"}, {"b1", "applicable"}}, Supporting: [][]string{}, Links: [][]string{{"b0", "b1", "joint_evidence"}}}
	raw, _ := json.Marshal(wire)
	selection, err := validateEvidenceSelection(r, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if selection.Assessments[0].ID != r.Spans[0].ID || selection.Assessments[2].Relevance != "unrelated" {
		t.Fatal("aliases did not expand to original anchors")
	}
	one, _ := json.Marshal(r.Spans[:1])
	packet := packEvidence(r.Spans, &selection, len(one))
	if len(packet.Spans) != 0 {
		t.Fatal("joint evidence was partially returned")
	}
	packet = packEvidence(r.Spans, &selection, 8192)
	if len(packet.Spans) != 2 || packet.Spans[0].ID != r.Spans[0].ID || packet.Spans[0].Text != r.Spans[0].Text {
		t.Fatal("source changed during expansion/packing")
	}
	cases := map[string]func(*evidenceWireSelection){
		"unknown alias":     func(w *evidenceWireSelection) { w.Selected[0][0] = "b999" },
		"public ID on wire": func(w *evidenceWireSelection) { w.Selected[0][0] = r.Spans[0].ID },
		"duplicate":         func(w *evidenceWireSelection) { w.Supporting = [][]string{{"b0", "applicable"}} },
		"short tuple":       func(w *evidenceWireSelection) { w.Selected[0] = []string{"b0"} },
		"long tuple":        func(w *evidenceWireSelection) { w.Selected[0] = []string{"b0", "applicable", "extra"} },
		"unknown status":    func(w *evidenceWireSelection) { w.Selected[0][1] = "certain" },
		"null list":         func(w *evidenceWireSelection) { w.Supporting = nil },
		"backwards":         func(w *evidenceWireSelection) { w.Links[0] = []string{"b1", "b0", "joint_evidence"} },
		"missing endpoint":  func(w *evidenceWireSelection) { w.Links[0][1] = "b2" },
		"supporting joint": func(w *evidenceWireSelection) {
			w.Selected = w.Selected[:1]
			w.Supporting = [][]string{{"b1", "applicable"}}
		},
		"wrong binding": func(w *evidenceWireSelection) { w.RequestSHA256 = "forged" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var w evidenceWireSelection
			_ = json.Unmarshal(raw, &w)
			mutate(&w)
			b, _ := json.Marshal(w)
			if _, err := validateEvidenceSelection(r, string(b)); err == nil {
				t.Fatal("accepted invalid wire response")
			}
		})
	}
	for _, bad := range []string{string(raw) + " {}", strings.Replace(string(raw), "{", `{"selected":[],`, 1), strings.Replace(string(raw), "{", `{"extra":1,`, 1)} {
		if _, err := validateEvidenceSelection(r, bad); err == nil {
			t.Fatal("accepted malformed JSON")
		}
	}
	r.Spans[0].SourceSHA256 = "changed"
	if _, err := validateEvidenceSelection(r, string(raw)); err == nil {
		t.Fatal("accepted remapped catalogue")
	}
}

func TestRecallEvidenceCompactRequestBinding(t *testing.T) {
	r := evidenceTestRequest(t)
	raw, _ := json.Marshal(r)
	for _, s := range r.Spans {
		if strings.Contains(string(raw), s.ID) || strings.Contains(string(raw), s.SourceSHA256) || strings.Contains(string(raw), s.Path) {
			t.Fatal("repeated source anchors leaked onto model wire")
		}
	}
	if len(r.Sources) != 1 || len(r.Sources[0].Fields) != 2 {
		t.Fatal("metadata not grouped by source field")
	}
	for i, s := range r.Spans {
		found := false
		for _, source := range r.Sources {
			for _, field := range source.Fields {
				for _, block := range field.Blocks {
					if block[0] == evidenceAlias(i) {
						found = block[1] == s.Text
					}
				}
			}
		}
		if !found {
			t.Fatal("wire text/alias mismatch")
		}
	}
	changed := append([]evidenceSpan{}, r.Spans...)
	changed[0].Path = "sessions/new-version.jsonl"
	other := makeEvidenceRequest(r.Question, r.Branch, evidenceCandidates{Spans: changed, State: r.RetrievalState, InputTruncated: r.InputTruncated})
	if other.CatalogueSHA256 == r.CatalogueSHA256 || other.RequestSHA256 == r.RequestSHA256 {
		t.Fatal("anchor mutation not bound to request")
	}
	changed[0].Text = "caller mutation"
	if other.Spans[0].Text == changed[0].Text {
		t.Fatal("request aliases share mutable caller slice")
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
	c, err := collectEvidence(context.Background(), brain, "main", "How many model kits have I completed?", 128)
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
