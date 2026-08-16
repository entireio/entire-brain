package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func largeCoverageResults(count int) []unifiedResult {
	results := make([]unifiedResult, count)
	for i := range results {
		sessionRef := conversationSessionIDPrefix + strings.Repeat("s", 64) + string(rune('a'+i%20))
		concepts := make([]string, conversationConceptsMaxTotal)
		matches := make([]conceptMatch, conversationConceptsMaxTotal)
		evidenceIDs := make([]string, conversationConceptsMaxTotal)
		for concept := range concepts {
			concepts[concept] = strings.Repeat(string(rune('a'+concept)), 500)
			evidenceIDs[concept] = conversationIDPrefix + strings.Repeat(string(rune('e'+concept)), 64) + string(rune('a'+i%20))
			matches[concept] = conceptMatch{
				Concept: concepts[concept], ConversationID: evidenceIDs[concept], Rank: i + concept + 1,
				MatchedTerms: []string{strings.Repeat(string(rune('k'+concept)), 500)}, Arm: "lexical",
			}
		}
		results[i] = unifiedResult{
			Source: retrievalSourceConversation, ID: sessionRef, Heading: "session_coverage", SessionRef: sessionRef,
			Text: strings.Repeat(`quoted "proof" \\ newline\n`, 500), VerificationRequired: true,
			Concepts: concepts, ConceptMatches: matches, EvidenceIDs: evidenceIDs,
			WorstRank: i + conversationConceptsMaxTotal, RankSum: 5*i + 15, Approximate: i%2 == 0,
		}
	}
	return results
}

func largeRetrievalExtras(results []unifiedResult) retrievalTransportExtras {
	previews := make([]sessionAbstractPreview, min(len(results), 10))
	for i := range previews {
		previews[i] = sessionAbstractPreview{
			SessionRef: results[i].SessionRef, AbstractStatus: abstractStatusCurrent,
			Overview: &abstractStatement{Text: strings.Repeat("preview ", 500), EvidenceIDs: results[i].EvidenceIDs},
		}
	}
	related := make([]relatedPatternRef, 20)
	for i := range related {
		related[i] = relatedPatternRef{Type: "pattern", ID: "pattern:" + strings.Repeat("p", 100), Title: strings.Repeat("optional title ", 100)}
	}
	return retrievalTransportExtras{AbstractPreviews: previews, Related: related}
}

func TestMultiConceptExactTransportBudgets(t *testing.T) {
	results := largeCoverageResults(conversationConceptResultMax)
	extras := largeRetrievalExtras(results)

	t.Run("cli json", func(t *testing.T) {
		payload, err := boundedRetrievalJSONPayload(context.Background(), strings.Repeat("query ", 80), strings.Repeat("branch", 80), results, extras, "query")
		if err != nil {
			t.Fatal(err)
		}
		actual, err := jsonOutputBytes(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(actual) > conversationConceptResponseMaxBytes || payload["response_truncated"] != true {
			t.Fatalf("CLI JSON bytes=%d marker=%v", len(actual), payload["response_truncated"])
		}
	})

	t.Run("mcp content-length frame", func(t *testing.T) {
		id := strings.Repeat("request-id", 30)
		ctx := context.WithValue(context.Background(), mcpResponseTransportContextKey{}, mcpResponseTransport{ID: id, FrameMode: mcpFrameContentLength})
		payload, err := boundedRetrievalJSONPayload(ctx, strings.Repeat("escaped query ", 30), "main", results, extras, "mcp:brain_query")
		if err != nil {
			t.Fatal(err)
		}
		inner, _ := jsonOutputBytes(payload)
		result := map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}
		var emitted bytes.Buffer
		if err := writeMCPMessage(&emitted, mcpMessage{JSONRPC: "2.0", ID: id, Result: result}, mcpFrameContentLength); err != nil {
			t.Fatal(err)
		}
		if emitted.Len() > conversationConceptResponseMaxBytes || payload["response_truncated"] != true {
			t.Fatalf("MCP frame bytes=%d marker=%v", emitted.Len(), payload["response_truncated"])
		}
	})

	t.Run("cli text", func(t *testing.T) {
		actual, err := boundedSingleRepoRetrievalText("query", results, extras)
		if err != nil {
			t.Fatal(err)
		}
		if len(actual) > conversationConceptResponseMaxBytes || !bytes.Contains(actual, []byte("response_truncated true")) {
			t.Fatalf("CLI text bytes=%d marker=%t", len(actual), bytes.Contains(actual, []byte("response_truncated true")))
		}
	})

	t.Run("workspace json and text", func(t *testing.T) {
		groups := []workspaceRetrieveResult{
			{RepoKey: "gh/acme/one", Name: "one", Results: append([]unifiedResult(nil), results...)},
			{RepoKey: "gh/acme/two", Name: "two", Results: append([]unifiedResult(nil), results...)},
		}
		payload, err := boundedWorkspaceRetrievalJSONPayload("product", "query", groups)
		if err != nil {
			t.Fatal(err)
		}
		actual, _ := jsonOutputBytes(payload)
		if len(actual) > conversationConceptResponseMaxBytes || payload["response_truncated"] != true {
			t.Fatalf("workspace JSON bytes=%d marker=%v", len(actual), payload["response_truncated"])
		}
		text, err := boundedWorkspaceRetrievalText("product", groups)
		if err != nil {
			t.Fatal(err)
		}
		if len(text) > conversationConceptResponseMaxBytes || !bytes.Contains(text, []byte("response_truncated true")) {
			t.Fatalf("workspace text bytes=%d", len(text))
		}
	})
}

func TestMultiConceptLimitContractAcrossSurfaces(t *testing.T) {
	ropts := retrievalOptions{Source: retrievalSourceConversation, Concepts: []string{"second"}}
	for _, limit := range []int{0, conversationConceptResultMax + 1} {
		if _, err := retrieveConversationMultiConcept(t.TempDir(), "first", limit, modeLexical, ropts); err == nil || !strings.Contains(err.Error(), "between 1 and 50") {
			t.Fatalf("core limit %d: %v", limit, err)
		}
	}
	cmd := &cobra.Command{}
	if err := runRetrieve(context.Background(), cmd, Options{}, "first", modeLexical, 51, "", ropts, true, false, "query"); err == nil || !strings.Contains(err.Error(), "between 1 and 50") {
		t.Fatalf("CLI limit: %v", err)
	}
	if err := runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 51, source: retrievalSourceConversation, concepts: []string{"second"}}, modeLexical, "missing", "first"); err == nil || !strings.Contains(err.Error(), "between 1 and 50") {
		t.Fatalf("workspace limit: %v", err)
	}
	raw := json.RawMessage(`{"name":"brain_query","arguments":{"query":"first","source":"conversation","concepts":["second"],"limit":51}}`)
	if _, err := handleMCPToolCall(context.Background(), Options{}, raw); err == nil || !strings.Contains(err.Error(), "between 1 and 50") {
		t.Fatalf("MCP limit: %v", err)
	}

	huge := make([]string, 1<<16)
	if _, err := buildRetrievalOptions(retrievalSourceConversation, "", "", "", "", "", huge); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("oversized concept list: %v", err)
	}
	definitions, _ := json.Marshal(mcpToolDefinitions())
	if !bytes.Contains(definitions, []byte(`"maximum":50`)) || !bytes.Contains(definitions, []byte(`"required":["concepts"]`)) {
		t.Fatalf("MCP conditional multi-concept limit missing: %s", definitions)
	}
}

func TestWorkspaceQualificationIsRecursive(t *testing.T) {
	repoKey := "gh/acme/service"
	result := unifiedResult{
		ID: "conversation-session:s", SessionRef: "conversation-session:s", TargetID: "conversation:t",
		RelatedIDs: []string{"fact:r"}, EvidenceIDs: []string{"conversation:e"},
		ConceptMatches: []conceptMatch{{ConversationID: "conversation:e"}},
		Turns:          []conversationTurn{{ConversationID: "conversation:t"}},
		Abstract: &sessionAbstract{
			SessionRef: "conversation-session:s",
			Overview:   abstractStatement{EvidenceIDs: []string{"conversation:e"}},
			Decisions:  []abstractStatement{{EvidenceIDs: []string{"conversation:d"}}},
		},
	}
	qualified := qualifyWorkspaceUnifiedResult(repoKey, "workspace", result)
	for name, got := range map[string]string{
		"id": qualified.ID, "session": qualified.SessionRef, "target": qualified.TargetID,
		"related": qualified.RelatedIDs[0], "evidence": qualified.EvidenceIDs[0],
		"match": qualified.ConceptMatches[0].ConversationID, "turn": qualified.Turns[0].ConversationID,
		"abstract session": qualified.Abstract.SessionRef, "abstract evidence": qualified.Abstract.Overview.EvidenceIDs[0],
		"abstract decision": qualified.Abstract.Decisions[0].EvidenceIDs[0],
	} {
		if !strings.HasPrefix(got, repoKey+"/") {
			t.Errorf("%s not qualified: %q", name, got)
		}
	}
	preview := qualifyWorkspacePreview(repoKey, "workspace", sessionAbstractPreview{
		SessionRef: "conversation-session:s", Overview: &abstractStatement{EvidenceIDs: []string{"conversation:e"}},
	})
	if !strings.HasPrefix(preview.SessionRef, repoKey+"/") || !strings.HasPrefix(preview.Overview.EvidenceIDs[0], repoKey+"/") {
		t.Fatalf("preview not recursively qualified: %+v", preview)
	}
	// The input structs must remain unmodified so single-repo serialization is
	// byte-compatible with the pre-workspace contract.
	if result.ID != "conversation-session:s" || result.Abstract.Overview.EvidenceIDs[0] != "conversation:e" {
		t.Fatalf("qualification mutated single-repo value: %+v", result)
	}
}

func TestMultiConceptTextProofParity(t *testing.T) {
	result := largeCoverageResults(1)
	cliText, err := boundedSingleRepoRetrievalText("deployment", result, retrievalTransportExtras{})
	if err != nil {
		t.Fatal(err)
	}
	workspaceText, err := boundedWorkspaceRetrievalText("product", []workspaceRetrieveResult{{RepoKey: "gh/acme/service", Results: result}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proof concepts=", "worst_rank=", "rank_sum=", "approximate=true", "rank=1", "arm=lexical", "evidence_ids=", "response_truncated false"} {
		if !bytes.Contains(cliText, []byte(want)) || !bytes.Contains(workspaceText, []byte(want)) {
			t.Errorf("missing proof field %q\nCLI:\n%s\nworkspace:\n%s", want, cliText, workspaceText)
		}
	}
	if !bytes.Contains(workspaceText, []byte("gh/acme/service/conversation:")) {
		t.Fatalf("workspace proof ids are not qualified:\n%s", workspaceText)
	}
}

func TestMultiConceptPreEmissionPrivacyRaceFailsClosed(t *testing.T) {
	brainDir := writeMultiConceptFixture(t)
	results, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, modeLexical, retrievalOptions{
		Source: retrievalSourceConversation, Concepts: []string{"beta cache eviction"},
	})
	if err != nil || len(results) == 0 {
		t.Fatalf("rank fixture: %v %+v", err, results)
	}
	originalHook := beforeRetrievalResponsePrivacyRecheck
	defer func() { beforeRetrievalResponsePrivacyRecheck = originalHook }()
	called := false
	beforeRetrievalResponsePrivacyRecheck = func() {
		if called {
			return
		}
		called = true
		stones := loadSessionTombstones(brainDir)
		stones.Excluded["sess-one"] = sessionTombstone{At: time.Now().UTC(), Reason: "race regression"}
		if err := saveSessionTombstones(brainDir, stones); err != nil {
			panic(err)
		}
	}
	filtered, _, err := revalidateRetrievalResponsePrivacy(brainDir, results)
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) || filtered != nil || !called {
		t.Fatalf("late tombstone must fail before emission: called=%v results=%+v err=%v", called, filtered, err)
	}
}

type retrievalEmissionPrivacyFixture struct {
	BrainDir  string
	RepoKey   string
	SessionID string
	RecordID  string
	Options   Options
}

func writeRetrievalEmissionPrivacyFixture(t *testing.T) retrievalEmissionPrivacyFixture {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	rel := "sessions/feature/20260809T120000Z_privacy-race.jsonl"
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"How should the final privacy gate work?"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Buffer the complete response and recheck the tombstone policy before writing."}]}}`,
	}, "\n") + "\n"
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "gh/example/repo", DefaultBranch: "feature",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "feature", Sessions: []exportSession{{
			SessionID: "privacy-race-session", Branch: "feature", Agent: "claude", LatestCheckpoint: "cp", TranscriptPath: rel, CreatedAt: now,
		}}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}
	manifestOnDisk, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifestOnDisk.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	recordID := ""
	for _, record := range index.Records {
		if record.Kind == conversationKind {
			recordID = record.ID
			break
		}
	}
	if recordID == "" {
		t.Fatal("fixture did not produce a conversation record")
	}
	return retrievalEmissionPrivacyFixture{BrainDir: brainDir, RepoKey: manifest.RepoKey, SessionID: "privacy-race-session", RecordID: recordID, Options: opts}
}

func installLateRetrievalTombstone(t *testing.T, fixture retrievalEmissionPrivacyFixture) *bool {
	t.Helper()
	original := beforeRetrievalResponsePrivacyEmissionCheck
	called := false
	beforeRetrievalResponsePrivacyEmissionCheck = func() {
		if called {
			return
		}
		called = true
		stones := loadSessionTombstones(fixture.BrainDir)
		stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "late emission race"}
		if err := saveSessionTombstones(fixture.BrainDir, stones); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() { beforeRetrievalResponsePrivacyEmissionCheck = original })
	return &called
}

func requireLatePrivacyEmissionFailure(t *testing.T, err error, out *bytes.Buffer, called *bool) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
		t.Fatalf("late tombstone error = %v, want %s", err, memoryErrPrivacyDirty)
	}
	if !*called {
		t.Fatal("final privacy emission seam was not reached")
	}
	if out.Len() != 0 {
		t.Fatalf("stale bytes escaped before the final privacy check: %q", out.String())
	}
}

func TestAllRetrievalEmissionPathsRejectLateTombstone(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_BRIEF_CONVERSATION", "1")

	t.Run("ordinary search", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		called := installLateRetrievalTombstone(t, fixture)
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		err := runRetrieve(context.Background(), cmd, fixture.Options, "final privacy gate", modeLexical, 10, "", retrievalOptions{Source: retrievalSourceConversation}, true, false, "query")
		requireLatePrivacyEmissionFailure(t, err, &out, called)
	})

	for _, jsonOut := range []bool{false, true} {
		name := "get text"
		if jsonOut {
			name = "multi-get json"
		}
		t.Run(name, func(t *testing.T) {
			fixture := writeRetrievalEmissionPrivacyFixture(t)
			called := installLateRetrievalTombstone(t, fixture)
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			ids := []string{fixture.RecordID}
			if jsonOut {
				ids = append(ids, "history:missing")
			}
			err := runGet(context.Background(), cmd, fixture.Options, ids, "", jsonOut, getOptions{}, "get")
			requireLatePrivacyEmissionFailure(t, err, &out, called)
		})
	}

	t.Run("brief", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		called := installLateRetrievalTombstone(t, fixture)
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		err := runBrainBrief(context.Background(), cmd, fixture.Options, brainBriefOptions{limit: 3, json: true}, "final privacy gate")
		requireLatePrivacyEmissionFailure(t, err, &out, called)
	})

	for _, get := range []bool{false, true} {
		name := "workspace search"
		if get {
			name = "workspace get"
		}
		t.Run(name, func(t *testing.T) {
			fixture := writeRetrievalEmissionPrivacyFixture(t)
			workspaceName := "privacy-race"
			if err := writeWorkspaceManifest(fixture.Options.Env, workspaceManifest{Name: workspaceName, Repos: []workspaceRepo{{RepoKey: fixture.RepoKey}}}); err != nil {
				t.Fatal(err)
			}
			called := installLateRetrievalTombstone(t, fixture)
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			var err error
			if get {
				err = runWorkspaceGet(cmd, fixture.Options, workspaceName, []string{fmt.Sprintf("%s/%s", fixture.RepoKey, fixture.RecordID)}, "", true, getOptions{})
			} else {
				err = runWorkspaceRetrieve(cmd, fixture.Options, workspaceRetrieveOptions{limit: 10, source: retrievalSourceConversation, json: true}, modeLexical, workspaceName, "final privacy gate")
			}
			requireLatePrivacyEmissionFailure(t, err, &out, called)
		})
	}
}

type blockingRetrievalWriter struct {
	bytes.Buffer
	started chan struct{}
	release chan struct{}
}

func (w *blockingRetrievalWriter) Write(p []byte) (int, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
	}
	<-w.release
	return w.Buffer.Write(p)
}

func installConcurrentLockedTombstone(t *testing.T, fixture retrievalEmissionPrivacyFixture) <-chan error {
	t.Helper()
	original := beforeRetrievalResponseWrite
	result := make(chan error, 1)
	called := false
	beforeRetrievalResponseWrite = func() {
		if called {
			return
		}
		called = true
		go func() {
			result <- withBrainWriteLock(fixture.BrainDir, func() error {
				stones, _, err := loadSessionTombstonesChecked(fixture.BrainDir)
				if err != nil {
					return err
				}
				stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "linearization race"}
				return saveSessionTombstones(fixture.BrainDir, stones)
			})
		}()
	}
	t.Cleanup(func() { beforeRetrievalResponseWrite = original })
	return result
}

func TestRetrievalWriteLinearizesBeforeConcurrentTombstone(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	policy, err := captureRetrievalPrivacyPolicy(fixture.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	mutation := installConcurrentLockedTombstone(t, fixture)
	out := &blockingRetrievalWriter{started: make(chan struct{}), release: make(chan struct{})}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writeRetrievalResponseBytes(out, []byte("private response\n"), policy)
	}()
	<-out.started
	select {
	case err := <-mutation:
		t.Fatalf("tombstone linearized while the response write lock was held: %v", err)
	default:
	}
	close(out.release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-mutation; err != nil {
		t.Fatalf("tombstone after response: %v", err)
	}
	if got := out.String(); got != "private response\n" {
		t.Fatalf("response=%q", got)
	}
	guard, err := loadSessionReadGuard(fixture.BrainDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !guard.blocksSession(fixture.SessionID) {
		t.Fatal("tombstone did not commit after response linearization")
	}
}

func TestMCPRetrievalRetainsPrivacyLockThroughOuterFrameWrite(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	mutation := installConcurrentLockedTombstone(t, fixture)
	out := &blockingRetrievalWriter{started: make(chan struct{}), release: make(chan struct{})}
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_search","arguments":{"query":"final privacy gate","source":"conversation"}}}`)
	runDone := make(chan error, 1)
	go func() {
		runDone <- runMCP(context.Background(), strings.NewReader(input), out, fixture.Options)
	}()
	<-out.started
	select {
	case err := <-mutation:
		t.Fatalf("tombstone linearized before the complete MCP frame write: %v", err)
	default:
	}
	close(out.release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-mutation; err != nil {
		t.Fatalf("tombstone after MCP response: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 1 || responses[0]["error"] != nil {
		t.Fatalf("MCP response=%+v", responses)
	}
}

func requireCommandWriteLinearizesBeforeTombstone(t *testing.T, fixture retrievalEmissionPrivacyFixture, run func(*cobra.Command) error) {
	t.Helper()
	mutation := installConcurrentLockedTombstone(t, fixture)
	out := &blockingRetrievalWriter{started: make(chan struct{}), release: make(chan struct{})}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	done := make(chan error, 1)
	go func() { done <- run(cmd) }()
	<-out.started
	select {
	case err := <-mutation:
		t.Fatalf("tombstone linearized while the command response write lock was held: %v", err)
	default:
	}
	close(out.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-mutation; err != nil {
		t.Fatalf("tombstone after command response: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("command emitted no response")
	}
}

func TestTranscriptDerivedCLIResponsesLinearizeBeforeConcurrentTombstone(t *testing.T) {
	t.Run("handoff", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		requireCommandWriteLinearizesBeforeTombstone(t, fixture, func(cmd *cobra.Command) error {
			return runBrainHandoff(context.Background(), cmd, fixture.Options, 1, true)
		})
	})

	t.Run("dash json", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		requireCommandWriteLinearizesBeforeTombstone(t, fixture, func(cmd *cobra.Command) error {
			return runDash(context.Background(), cmd, fixture.Options, dashFlags{json: true, limit: 10}, fixture.Options.Env.RepoRoot)
		})
	})

	t.Run("workspace aggregate", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		const workspaceName = "privacy-aggregate-race"
		if err := writeWorkspaceManifest(fixture.Options.Env, workspaceManifest{
			Name: workspaceName, Repos: []workspaceRepo{{RepoKey: fixture.RepoKey}},
		}); err != nil {
			t.Fatal(err)
		}
		requireCommandWriteLinearizesBeforeTombstone(t, fixture, func(cmd *cobra.Command) error {
			return runWorkspacePatternsList(context.Background(), cmd, fixture.Options, workspaceName, patternsListOptions{asJSON: true})
		})
	})
}

func TestTranscriptDerivedCLIResponsesRejectLateTombstone(t *testing.T) {
	t.Run("handoff", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		called := installLateRetrievalTombstone(t, fixture)
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		err := runBrainHandoff(context.Background(), cmd, fixture.Options, 1, true)
		requireLatePrivacyEmissionFailure(t, err, &out, called)
	})

	t.Run("dash json", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		called := installLateRetrievalTombstone(t, fixture)
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		err := runDash(context.Background(), cmd, fixture.Options, dashFlags{json: true, limit: 10}, fixture.Options.Env.RepoRoot)
		requireLatePrivacyEmissionFailure(t, err, &out, called)
	})

	t.Run("workspace aggregate", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		const workspaceName = "privacy-aggregate-late"
		if err := writeWorkspaceManifest(fixture.Options.Env, workspaceManifest{
			Name: workspaceName, Repos: []workspaceRepo{{RepoKey: fixture.RepoKey}},
		}); err != nil {
			t.Fatal(err)
		}
		called := installLateRetrievalTombstone(t, fixture)
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		err := runWorkspacePatternsList(context.Background(), cmd, fixture.Options, workspaceName, patternsListOptions{asJSON: true})
		requireLatePrivacyEmissionFailure(t, err, &out, called)
	})
}

func TestWorkspaceAggregateRejectsAlreadyDirtyMember(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	const workspaceName = "privacy-dirty-member"
	if err := writeWorkspaceManifest(fixture.Options.Env, workspaceManifest{
		Name: workspaceName, Repos: []workspaceRepo{{RepoKey: fixture.RepoKey}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(fixture.BrainDir, func() error {
		stones, _, err := loadSessionTombstonesChecked(fixture.BrainDir)
		if err != nil {
			return err
		}
		stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "already dirty"}
		return saveSessionTombstones(fixture.BrainDir, stones)
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := runWorkspacePatternsList(context.Background(), cmd, fixture.Options, workspaceName, patternsListOptions{asJSON: true})
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
		t.Fatalf("workspace dirty-member error=%v, want %s", err, memoryErrPrivacyDirty)
	}
	if out.Len() != 0 {
		t.Fatalf("dirty workspace emitted stale bytes: %q", out.String())
	}
}

func TestPatternBearingSurfacesRejectAlreadyDirtyDerivedStateWithoutOutput(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	if err := withBrainWriteLock(fixture.BrainDir, func() error {
		stones, _, err := loadSessionTombstonesChecked(fixture.BrainDir)
		if err != nil {
			return err
		}
		stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "already dirty"}
		return saveSessionTombstones(fixture.BrainDir, stones)
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		run  func(*cobra.Command) error
	}{
		{name: "overview", run: func(cmd *cobra.Command) error {
			return runBrainOverview(context.Background(), cmd, fixture.Options, fixture.Options.Env.RepoRoot, true, 3)
		}},
		{name: "brief", run: func(cmd *cobra.Command) error {
			return runBrainBrief(context.Background(), cmd, fixture.Options, brainBriefOptions{limit: 3, json: true}, "privacy state")
		}},
		{name: "handoff", run: func(cmd *cobra.Command) error {
			return runBrainHandoff(context.Background(), cmd, fixture.Options, 1, true)
		}},
		{name: "review patterns", run: func(cmd *cobra.Command) error {
			return runBrainReview(context.Background(), cmd, fixture.Options, regressionDetectorOptions{limit: 3, json: true, patterns: true}, "privacy state")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{Use: "test"}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			err := tc.run(cmd)
			if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
				t.Fatalf("dirty pattern surface error = %v, want %s", err, memoryErrPrivacyDirty)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("dirty pattern surface emitted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestExplicitPatternRequestsReportUnavailableCorpusWithoutOutput(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	tests := []struct {
		name string
		run  func(*cobra.Command) error
	}{
		{name: "retrieve patterns", run: func(cmd *cobra.Command) error {
			return runRetrieve(context.Background(), cmd, fixture.Options, "privacy state", modeLexical, 5, "", retrievalOptions{Source: retrievalSourceConversation}, true, true, "query")
		}},
		{name: "get pattern", run: func(cmd *cobra.Command) error {
			return runGet(context.Background(), cmd, fixture.Options, []string{"pattern:missing"}, "", true, getOptions{}, "get")
		}},
		{name: "review patterns", run: func(cmd *cobra.Command) error {
			return runBrainReview(context.Background(), cmd, fixture.Options, regressionDetectorOptions{limit: 3, json: true, patterns: true}, "privacy state")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{Use: "test"}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			err := tc.run(cmd)
			if err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) {
				t.Fatalf("missing requested corpus error = %v, want %s", err, memoryErrSourceStale)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("missing requested corpus emitted output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

type blockingHTTPResponseWriter struct {
	header  http.Header
	status  int
	body    bytes.Buffer
	started chan struct{}
	release chan struct{}
}

func (w *blockingHTTPResponseWriter) Header() http.Header { return w.header }

func (w *blockingHTTPResponseWriter) WriteHeader(status int) { w.status = status }

func (w *blockingHTTPResponseWriter) Write(p []byte) (int, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
	}
	<-w.release
	return w.body.Write(p)
}

func TestVizAPIResponseLinearizesBeforeConcurrentTombstone(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	manifest, err := loadBrainManifest(fixture.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := loadSessionReadGuard(fixture.BrainDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	srv := &vizServer{
		repoDir: fixture.Options.Env.RepoRoot, brainDir: fixture.BrainDir, branch: "feature", manifest: manifest,
		guard: guard, guardObserved: !guard.empty(),
	}
	handler, err := srv.mux()
	if err != nil {
		t.Fatal(err)
	}
	mutation := installConcurrentLockedTombstone(t, fixture)
	out := &blockingHTTPResponseWriter{
		header: make(http.Header), started: make(chan struct{}), release: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
		close(done)
	}()
	<-out.started
	select {
	case err := <-mutation:
		t.Fatalf("tombstone linearized while the HTTP response write lock was held: %v", err)
	default:
	}
	close(out.release)
	<-done
	if err := <-mutation; err != nil {
		t.Fatalf("tombstone after HTTP response: %v", err)
	}
	if out.status != http.StatusOK || out.body.Len() == 0 {
		t.Fatalf("HTTP response status=%d body=%q", out.status, out.body.String())
	}
}

func TestVizAPIResponseRejectsLateTombstone(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	manifest, err := loadBrainManifest(fixture.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := loadSessionReadGuard(fixture.BrainDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	srv := &vizServer{
		repoDir: fixture.Options.Env.RepoRoot, brainDir: fixture.BrainDir, branch: "feature", manifest: manifest,
		guard: guard, guardObserved: !guard.empty(),
	}
	handler, err := srv.mux()
	if err != nil {
		t.Fatal(err)
	}
	called := installLateRetrievalTombstone(t, fixture)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if !*called {
		t.Fatal("final HTTP privacy emission seam was not reached")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q, want 503", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), fixture.SessionID) {
		t.Fatalf("late tombstone leaked stale session response: %q", rec.Body.String())
	}
}
