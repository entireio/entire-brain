package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirstUserRequestCodex(t *testing.T) {
	lines := []string{
		`{"type":"session_meta","payload":{}}`,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n<cwd>/x</cwd>\n</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"Rename the plugin to entire-brain and update the README."}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"Sure."}]}}`,
	}
	got := firstUserRequest(strings.Join(lines, "\n"))
	if got != "Rename the plugin to entire-brain and update the README." {
		t.Fatalf("got %q", got)
	}
}

func TestFirstUserRequestClaudeAndWrapperSkip(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"content":[{"type":"text","text":"<command-name>/init</command-name>"}]}}`,
		`{"type":"user","message":{"content":"How does the history index dedupe decisions across sessions?"}}`,
	}
	got := firstUserRequest(strings.Join(lines, "\n"))
	if got != "How does the history index dedupe decisions across sessions?" {
		t.Fatalf("got %q", got)
	}
	// event_msg user_message form.
	em := `{"type":"event_msg","payload":{"type":"user_message","message":"Add a verify command for fact provenance."}}`
	if got := firstUserRequest(em); got != "Add a verify command for fact provenance." {
		t.Fatalf("event_msg got %q", got)
	}
	// No substantive request -> empty.
	only := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"<environment_context></environment_context>"}]}}`
	if got := firstUserRequest(only); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestClassifyQueryType(t *testing.T) {
	local := []factRecord{{Paths: []string{"architecture.data.flow"}}}
	cross := []factRecord{{Paths: []string{"workflow.testing.rules"}}, {Paths: []string{"preferences.coding.style"}}}

	if got := classifyQueryType("how does `MirrorCommittedMetadataRef` work in internal/cli/strategy", local); got != queryTypeCode {
		t.Errorf("structural code query should be code, got %q", got)
	}
	// Bare CamelCase (likely a product name) is not a code stratum on its own.
	if got := classifyQueryType("how does GitHub auth work conceptually", local); got != queryTypeConcept {
		t.Errorf("bare CamelCase should fall through to concept, got %q", got)
	}
	if got := classifyQueryType("what is the review etiquette", cross); got != queryTypeConvention {
		t.Errorf("cross-cutting relevant facts should be convention, got %q", got)
	}
	if got := classifyQueryType("add a verify command", local); got != queryTypeHowto {
		t.Errorf("imperative should be howto, got %q", got)
	}
	if got := classifyQueryType("how does provenance freshness work", local); got != queryTypeConcept {
		t.Errorf("plain NL should be concept, got %q", got)
	}
}

func TestGenerateEvalTasksFromProvenance(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	// Two facts citing session s1, one citing s2.
	mk := func(text, path, session string) factRecord {
		p := normalizeFactPaths([]string{path})
		return factRecord{ID: factRecordID(text, p), Paths: p, Text: text, Branch: "main", Status: factStatusActive,
			Provenance: []factAnchor{{SessionID: session}}, UpdatedAt: now}
	}
	facts := []factRecord{
		mk("Checkpoints v1.1 read from a custom ref", "architecture.data.flow", "s1"),
		mk("The mirror is best-effort", "constraints.invariants.general", "s1"),
		mk("Tests run with go test", "workflow.testing.rules", "s2"),
	}
	if err := writeFacts(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	// Transcripts with recoverable opening requests.
	writeTranscript := func(rel, req string) {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"` + req + `"}]}}`
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("sessions/main/s1.jsonl", "how do checkpoints v1.1 read metadata")
	writeTranscript("sessions/main/s2.jsonl", "what is the testing workflow")

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
			{SessionID: "s1", Branch: "main", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now},
			{SessionID: "s2", Branch: "main", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now},
		}}},
	}

	tasks, err := generateEvalTasks(brainDir, &manifest, "", 1, 0, 0)
	if err != nil {
		t.Fatalf("generateEvalTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	byID := map[string]evalTask{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	s1 := byID["s1"]
	if len(s1.Relevant) != 2 {
		t.Fatalf("s1 should have 2 relevant facts, got %d", len(s1.Relevant))
	}
	if s1.Task == "" || s1.Branch != "main" {
		t.Fatalf("s1 task/branch wrong: %+v", s1)
	}
	if s1.SourceSessionID != "s1" || s1.SourceTranscriptPath != "sessions/main/s1.jsonl" {
		t.Fatalf("s1 source anchor wrong: %+v", s1)
	}
	// s2's relevant fact is cross-cutting → convention stratum.
	if byID["s2"].QueryType != queryTypeConvention {
		t.Errorf("s2 should be convention, got %q", byID["s2"].QueryType)
	}
}

func TestGenerateEvalTasksScopesSessionFactsByResolvedBranch(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	mainPath := normalizeFactPaths([]string{"architecture.data.flow"})
	featurePath := normalizeFactPaths([]string{"workflow.testing.rules"})
	mainFact := factRecord{ID: factRecordID("main branch keeps checkpoint metadata", mainPath), Paths: mainPath, Text: "main branch keeps checkpoint metadata", Branch: "main", Status: factStatusActive, Provenance: []factAnchor{{SessionID: "same-session"}}, UpdatedAt: now}
	featureFact := factRecord{ID: factRecordID("feature branch updates testing workflow", featurePath), Paths: featurePath, Text: "feature branch updates testing workflow", Branch: "feature", Status: factStatusActive, Provenance: []factAnchor{{SessionID: "same-session"}}, UpdatedAt: now}
	if err := writeFacts(brainDir, "main", []factRecord{mainFact}); err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(brainDir, "feature", []factRecord{featureFact}); err != nil {
		t.Fatal(err)
	}

	writeTranscript := func(rel, req string) {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"` + req + `"}]}}`
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("sessions/main/same.jsonl", "how does main memory work")
	writeTranscript("sessions/feature/same.jsonl", "what is the feature testing workflow")

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
			{SessionID: "same-session", TranscriptPath: "sessions/main/same.jsonl", CreatedAt: now},
			{SessionID: "same-session", Branch: "feature", TranscriptPath: "sessions/feature/same.jsonl", CreatedAt: now.Add(time.Minute)},
		}}},
	}

	tasks, err := generateEvalTasks(brainDir, &manifest, "", 1, 0, 0)
	if err != nil {
		t.Fatalf("generateEvalTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected branch-separated tasks, got %d: %+v", len(tasks), tasks)
	}
	byBranch := map[string]evalTask{}
	for _, task := range tasks {
		byBranch[task.Branch] = task
	}
	if got := byBranch["main"]; got.ID != "main:same-session" || len(got.Relevant) != 1 || got.Relevant[0] != mainFact.ID {
		t.Fatalf("main task should use default branch facts only, got %+v", got)
	}
	if got := byBranch["feature"]; got.ID != "feature:same-session" || len(got.Relevant) != 1 || got.Relevant[0] != featureFact.ID {
		t.Fatalf("feature task should use feature facts only, got %+v", got)
	}

	mainOnly, err := generateEvalTasks(brainDir, &manifest, "main", 1, 0, 0)
	if err != nil {
		t.Fatalf("generateEvalTasks main: %v", err)
	}
	if len(mainOnly) != 1 || mainOnly[0].ID != "same-session" || mainOnly[0].Branch != "main" {
		t.Fatalf("single-branch filter should preserve legacy task id, got %+v", mainOnly)
	}
}

func TestGenerateSessionEvalTasksIncludesZeroFactSessions(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	writeTranscript := func(rel, req string) {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"` + req + `"}]}}`
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("sessions/main/s1.jsonl", "fix release wording")
	writeTranscript("sessions/main/s2.jsonl", "how does recall rank durable facts")

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
			{SessionID: "s1", Branch: "main", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now},
			{SessionID: "s2", Branch: "main", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now},
		}}},
	}

	tasks, err := generateSessionEvalTasks(brainDir, &manifest, "", 0)
	if err != nil {
		t.Fatalf("generateSessionEvalTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	byID := map[string]evalTask{}
	for _, task := range tasks {
		byID[task.ID] = task
		if len(task.Relevant) != 0 {
			t.Fatalf("session-sourced tasks should be unlabeled, got %+v", task)
		}
		if task.SourceSessionID == "" || task.SourceTranscriptPath == "" {
			t.Fatalf("missing source anchor: %+v", task)
		}
	}
	if byID["s1"].Task != "fix release wording" || byID["s1"].QueryType != queryTypeHowto {
		t.Fatalf("s1 task wrong: %+v", byID["s1"])
	}
	if byID["s2"].Task != "how does recall rank durable facts" || byID["s2"].QueryType != queryTypeConcept {
		t.Fatalf("s2 task wrong: %+v", byID["s2"])
	}
}

func TestGenerateSessionEvalTasksKeepsDuplicateRequests(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	writeTranscript := func(rel string) {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"review the current branch"}]}}`
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("sessions/main/s1.jsonl")
	writeTranscript("sessions/main/s2.jsonl")

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
			{SessionID: "s1", Branch: "main", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now},
			{SessionID: "s2", Branch: "main", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now.Add(time.Minute)},
		}}},
	}

	tasks, err := generateSessionEvalTasks(brainDir, &manifest, "", 0)
	if err != nil {
		t.Fatalf("generateSessionEvalTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("session-source generation must keep duplicate request sessions, got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].ID == tasks[1].ID {
		t.Fatalf("duplicate request tasks should preserve distinct session ids: %+v", tasks)
	}
}
