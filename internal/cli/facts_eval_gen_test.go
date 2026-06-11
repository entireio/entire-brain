package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	mk := func(text, path, session string, line int) factRecord {
		p := normalizeFactPaths([]string{path})
		return factRecord{ID: factRecordID(text, p), Paths: p, Text: text, Branch: "main", Status: factStatusActive,
			Provenance: []factAnchor{{SessionID: session, Line: line}}, UpdatedAt: now}
	}
	facts := []factRecord{
		mk("Checkpoints v1.1 read from a custom ref", "architecture.data.flow", "s1", 3),
		mk("The mirror is best-effort", "constraints.invariants.general", "s1", 7),
		mk("Tests run with go test", "workflow.testing.rules", "s2", 5),
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
	if !reflect.DeepEqual(s1.SourceLines, []int{3, 7}) {
		t.Fatalf("s1 source lines wrong: %+v", s1.SourceLines)
	}
	if s1.LabelSource != evalLabelSourceProvenanceSilver {
		t.Fatalf("s1 label source = %q, want %q", s1.LabelSource, evalLabelSourceProvenanceSilver)
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
	if got := byBranch["main"]; !strings.HasPrefix(got.ID, "main:same-session:") || len(got.Relevant) != 1 || got.Relevant[0] != mainFact.ID {
		t.Fatalf("main task should use default branch facts only, got %+v", got)
	}
	if got := byBranch["feature"]; !strings.HasPrefix(got.ID, "feature:same-session:") || len(got.Relevant) != 1 || got.Relevant[0] != featureFact.ID {
		t.Fatalf("feature task should use feature facts only, got %+v", got)
	}
	if byBranch["main"].ID == byBranch["feature"].ID {
		t.Fatalf("branch collision ids should be unique: %+v", byBranch)
	}

	mainOnly, err := generateEvalTasks(brainDir, &manifest, "main", 1, 0, 0)
	if err != nil {
		t.Fatalf("generateEvalTasks main: %v", err)
	}
	if len(mainOnly) != 1 || mainOnly[0].ID != "same-session" || mainOnly[0].Branch != "main" {
		t.Fatalf("single-branch filter should preserve legacy task id, got %+v", mainOnly)
	}
}

func TestGenerateEvalTasksScopesDuplicateSessionIDByTranscript(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	writeTranscript := func(rel, req string) {
		t.Helper()
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"` + req + `"}]}}`
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const sessionID = "shared-session"
	firstTranscript := "sessions/main/shared-one.jsonl"
	secondTranscript := "sessions/main/shared-two.jsonl"
	writeTranscript(firstTranscript, "review the current branch")
	writeTranscript(secondTranscript, "review the current branch")

	firstPaths := normalizeFactPaths([]string{"architecture.data.flow"})
	secondPaths := normalizeFactPaths([]string{"workflow.testing.rules"})
	firstFact := factRecord{
		ID:         factRecordID("first shared session fact", firstPaths),
		Paths:      firstPaths,
		Text:       "first shared session fact",
		Branch:     "main",
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: sessionID, Transcript: firstTranscript, Line: 2}},
		UpdatedAt:  now,
	}
	secondFact := factRecord{
		ID:         factRecordID("second shared session fact", secondPaths),
		Paths:      secondPaths,
		Text:       "second shared session fact",
		Branch:     "main",
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: sessionID, Transcript: secondTranscript, Line: 5}},
		UpdatedAt:  now,
	}
	if err := writeFacts(brainDir, "main", []factRecord{firstFact, secondFact}); err != nil {
		t.Fatal(err)
	}

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
			{SessionID: sessionID, Branch: "main", TranscriptPath: firstTranscript, CreatedAt: now},
			{SessionID: sessionID, Branch: "main", TranscriptPath: secondTranscript, CreatedAt: now.Add(time.Minute)},
		}}},
	}

	tasks, err := generateEvalTasks(brainDir, &manifest, "main", 1, 0, 0)
	if err != nil {
		t.Fatalf("generateEvalTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected transcript-scoped tasks, got %d: %+v", len(tasks), tasks)
	}
	byTranscript := map[string]evalTask{}
	for _, task := range tasks {
		byTranscript[task.SourceTranscriptPath] = task
	}
	if got := byTranscript[firstTranscript]; len(got.Relevant) != 1 || got.Relevant[0] != firstFact.ID || !reflect.DeepEqual(got.SourceLines, []int{2}) {
		t.Fatalf("first transcript labels leaked or lost: %+v", got)
	}
	if got := byTranscript[secondTranscript]; len(got.Relevant) != 1 || got.Relevant[0] != secondFact.ID || !reflect.DeepEqual(got.SourceLines, []int{5}) {
		t.Fatalf("second transcript labels leaked or lost: %+v", got)
	}
	if byTranscript[firstTranscript].ID == byTranscript[secondTranscript].ID {
		t.Fatalf("duplicate session IDs should disambiguate task IDs: %+v", tasks)
	}
}

func TestGenerateSessionEvalTasksDisambiguatesSameBranchShortIDCollisions(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	writeTranscript := func(rel, req string) {
		t.Helper()
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"` + req + `"}]}}`
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript("sessions/main/one.jsonl", "inspect alpha collision")
	writeTranscript("sessions/main/two.jsonl", "inspect beta collision")
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
			{SessionID: "abcdefghijkl-session-one", Branch: "main", TranscriptPath: "sessions/main/one.jsonl", CreatedAt: now},
			{SessionID: "abcdefghijkl-session-two", Branch: "main", TranscriptPath: "sessions/main/two.jsonl", CreatedAt: now.Add(time.Minute)},
		}}},
	}

	tasks, err := generateSessionEvalTasks(brainDir, &manifest, "main", 0)
	if err != nil {
		t.Fatalf("generateSessionEvalTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected two collision tasks, got %d: %+v", len(tasks), tasks)
	}
	ids := map[string]struct{}{}
	for _, task := range tasks {
		if !strings.HasPrefix(task.ID, "main:abcdefghijkl:") {
			t.Fatalf("collision task ID should include branch and hash, got %+v", task)
		}
		ids[task.ID] = struct{}{}
	}
	if len(ids) != 2 {
		t.Fatalf("same-branch short ID collision should produce unique IDs: %+v", tasks)
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

func TestFactsEvalGenHelpIncludesOllamaModel(t *testing.T) {
	cmd := newFactsEvalGenCommand(Options{})
	out, err := execute(t, cmd, "--help")
	if err != nil {
		t.Fatalf("help: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ollama") {
		t.Fatalf("help should mention ollama support:\n%s", out)
	}
	if !strings.Contains(out, "--model") {
		t.Fatalf("help should include --model:\n%s", out)
	}
}

func TestFactsEvalGenRefineOllamaNoEgressSendsModel(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	const wantModel = "llama3.2:test"

	type ollamaRequest struct {
		Model  string `json:"model"`
		System string `json:"system"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	seen := make(chan ollamaRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/api/generate" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		var req ollamaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen <- req
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"1 yes\n"}`))
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	repoDir := t.TempDir()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   t.TempDir(),
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	transcriptRel := "sessions/main/s1.jsonl"
	transcriptPath := filepath.Join(storage.BrainDir, filepath.FromSlash(transcriptRel))
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"how does local refine work"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		RepoRoot:      repoDir,
		RepoKey:       storage.Key,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "s1", Branch: "main", TranscriptPath: transcriptRel, CreatedAt: now},
		}}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	fact := factRecord{
		ID:         factRecordID("local refine sends the selected ollama model", paths),
		Paths:      paths,
		Text:       "local refine sends the selected ollama model",
		Branch:     "main",
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "s1"}},
		UpdatedAt:  now,
	}
	if err := writeFacts(storage.BrainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "facts", "eval-gen", "--refine", "--agent", "ollama", "--model", wantModel, "--min-facts", "1")
	if err != nil {
		t.Fatalf("eval-gen refine ollama: %v\n%s", err, out)
	}
	var tasks []evalTask
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatalf("parse eval-gen output: %v\n%s", err, out)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected one refined task, got %d: %+v\n%s", len(tasks), tasks, out)
	}
	if len(tasks[0].Relevant) != 1 || tasks[0].Relevant[0] != fact.ID {
		t.Fatalf("refined task relevant ids wrong: %+v", tasks[0])
	}
	if tasks[0].LabelSource != evalLabelSourceJudgeRefined {
		t.Fatalf("refined label source = %q, want %q", tasks[0].LabelSource, evalLabelSourceJudgeRefined)
	}

	select {
	case req := <-seen:
		if req.Model != wantModel {
			t.Fatalf("ollama model = %q, want %q", req.Model, wantModel)
		}
		if req.Stream {
			t.Fatal("ollama refine must request non-streaming output")
		}
		if !strings.Contains(req.System, "You assess whether retrieved repository facts are relevant") {
			t.Fatalf("unexpected ollama system prompt: %q", req.System)
		}
		if !strings.Contains(req.Prompt, "TASK: how does local refine work") || !strings.Contains(req.Prompt, fact.Text) {
			t.Fatalf("unexpected ollama prompt: %q", req.Prompt)
		}
	default:
		t.Fatal("ollama server did not receive a refine request")
	}
}
