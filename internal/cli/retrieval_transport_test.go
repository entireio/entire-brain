package cli

import (
	"bytes"
	"context"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/command/brainreview"
	"github.com/entireio/entire-brain/internal/cli/command/dash"
	"github.com/entireio/entire-brain/internal/cli/command/patternscmd"
	"github.com/entireio/entire-brain/internal/cli/command/retrieve"
	"github.com/entireio/entire-brain/internal/cli/command/viz"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/workspace"
	"github.com/spf13/cobra"
)

func largeCoverageResults(count int) []unifiedResult {
	results := make([]unifiedResult, count)
	for i := range results {
		sessionRef := brainstore.ConversationSessionIDPrefix + strings.Repeat("s", 64) + string(rune('a'+i%20))
		concepts := make([]string, retrieval.ConversationConceptsMaxTotal)
		matches := make([]conceptMatch, retrieval.ConversationConceptsMaxTotal)
		evidenceIDs := make([]string, retrieval.ConversationConceptsMaxTotal)
		for concept := range concepts {
			concepts[concept] = strings.Repeat(string(rune('a'+concept)), 500)
			evidenceIDs[concept] = conversationIDPrefix + strings.Repeat(string(rune('e'+concept)), 64) + string(rune('a'+i%20))
			matches[concept] = conceptMatch{
				Concept: concepts[concept], ConversationID: evidenceIDs[concept], Rank: i + concept + 1,
				MatchedTerms: []string{strings.Repeat(string(rune('k'+concept)), 500)}, Arm: "lexical",
			}
		}
		results[i] = unifiedResult{
			Source: brainstore.RetrievalSourceConversation, ID: sessionRef, Heading: "session_coverage", SessionRef: sessionRef,
			Text: strings.Repeat(`quoted "proof" \\ newline\n`, 500), VerificationRequired: true,
			Concepts: concepts, ConceptMatches: matches, EvidenceIDs: evidenceIDs,
			WorstRank: i + retrieval.ConversationConceptsMaxTotal, RankSum: 5*i + 15, Approximate: i%2 == 0,
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
	results := largeCoverageResults(retrieval.ConversationConceptResultMax)
	extras := largeRetrievalExtras(results)

	t.Run("cli json", func(t *testing.T) {
		payload, err := boundedRetrievalJSONPayload(strings.Repeat("query ", 80), strings.Repeat("branch", 80), results, extras)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := cmdutil.JSONOutputBytes(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(actual) > conversationConceptResponseMaxBytes || payload["response_truncated"] != true {
			t.Fatalf("CLI JSON bytes=%d marker=%v", len(actual), payload["response_truncated"])
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
		actual, err := cmdutil.JSONOutputBytes(payload)
		if err != nil {
			t.Fatal(err)
		}
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
	ropts := retrievalOptions{Source: brainstore.RetrievalSourceConversation, Concepts: []string{"second"}}
	for _, limit := range []int{0, retrieval.ConversationConceptResultMax + 1} {
		if _, err := retrieveConversationMultiConcept(t.TempDir(), "first", limit, retrieval.ModeLexical, ropts); err == nil || !strings.Contains(err.Error(), "between 1 and 50") {
			t.Fatalf("core limit %d: %v", limit, err)
		}
	}
	cmd := &cobra.Command{}
	if err := retrieve.Run(context.Background(), cmd, retrieveCommandConfig(Options{}), "first", retrieval.ModeLexical, 51, "", ropts, true, false, "query"); err == nil || !strings.Contains(err.Error(), "between 1 and 50") {
		t.Fatalf("CLI limit: %v", err)
	}
	if err := runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 51, source: brainstore.RetrievalSourceConversation, concepts: []string{"second"}}, retrieval.ModeLexical, "missing", "first"); err == nil || !strings.Contains(err.Error(), "between 1 and 50") {
		t.Fatalf("workspace limit: %v", err)
	}
	huge := make([]string, 1<<16)
	if _, err := retrieve.BuildRetrievalOptions(brainstore.RetrievalSourceConversation, "", "", "", "", "", huge); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("oversized concept list: %v", err)
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
	results, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, retrieval.ModeLexical, retrievalOptions{
		Source: brainstore.RetrievalSourceConversation, Concepts: []string{"beta cache eviction"},
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
		stones := brainstore.LoadSessionTombstones(brainDir)
		stones.Excluded["sess-one"] = sessionTombstone{At: time.Now().UTC(), Reason: "race regression"}
		if err := brainstore.SaveSessionTombstones(brainDir, stones); err != nil {
			panic(err)
		}
	}
	filtered, _, err := revalidateRetrievalResponsePrivacy(brainDir, results)
	if err == nil || !strings.Contains(err.Error(), brainstore.PrivacyDirtyCode) || filtered != nil || !called {
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
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
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
		SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now, RepoKey: "gh/example/repo", DefaultBranch: "feature",
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
	manifestOnDisk, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := historystore.LoadIndex(brainDir, manifestOnDisk.Sources.History)
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
		stones := brainstore.LoadSessionTombstones(fixture.BrainDir)
		stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "late emission race"}
		if err := brainstore.SaveSessionTombstones(fixture.BrainDir, stones); err != nil {
			panic(err)
		}
	}
	t.Cleanup(func() { beforeRetrievalResponsePrivacyEmissionCheck = original })
	return &called
}

func requireLatePrivacyEmissionFailure(t *testing.T, err error, out *bytes.Buffer, called *bool) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), brainstore.PrivacyDirtyCode) {
		t.Fatalf("late tombstone error = %v, want %s", err, brainstore.PrivacyDirtyCode)
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
		err := retrieve.Run(context.Background(), cmd, retrieveCommandConfig(fixture.Options), "final privacy gate", retrieval.ModeLexical, 10, "", retrievalOptions{Source: brainstore.RetrievalSourceConversation}, true, false, "query")
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
			_, err := retrieve.RunGet(context.Background(), cmd, retrieveCommandConfig(fixture.Options), ids, "", jsonOut, getOptions{}, "get")
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
			if err := workspace.DefaultStore(fixture.Options.Env).WriteManifest(workspace.Manifest{Name: workspaceName, Repos: []workspace.Repo{{RepoKey: fixture.RepoKey}}}); err != nil {
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
				err = runWorkspaceRetrieve(cmd, fixture.Options, workspaceRetrieveOptions{limit: 10, source: brainstore.RetrievalSourceConversation, json: true}, retrieval.ModeLexical, workspaceName, "final privacy gate")
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
			defer close(result)
			result <- brainstore.WithBrainWriteLock(fixture.BrainDir, func() error {
				stones, _, err := brainstore.LoadSessionTombstonesChecked(fixture.BrainDir)
				if err != nil {
					return err
				}
				stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "linearization race"}
				return brainstore.SaveSessionTombstones(fixture.BrainDir, stones)
			})
		}()
	}
	t.Cleanup(func() {
		defer func() { beforeRetrievalResponseWrite = original }()
		if called {
			if err := awaitRetrievalResult(t, "tombstone cleanup", result); err != nil {
				t.Errorf("tombstone cleanup: %v", err)
			}
		}
	})
	return result
}

// Each blocked writer is released and joined even when an assertion fails.
func retrievalWriterCleanup(t *testing.T, release, finished chan struct{}) func() {
	t.Helper()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		awaitRetrievalSignal(t, "writer cleanup", finished)
	})
	return unblock
}

func awaitRetrievalSignal(t *testing.T, name string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func awaitRetrievalResult(t *testing.T, name string, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

func TestRetrievalWriteLinearizesBeforeConcurrentTombstone(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	policy, err := brainstore.CaptureRetrievalPrivacyPolicy(fixture.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	mutation := installConcurrentLockedTombstone(t, fixture)
	out := &blockingRetrievalWriter{started: make(chan struct{}), release: make(chan struct{})}
	writeDone := make(chan error, 1)
	finished := make(chan struct{})
	unblock := retrievalWriterCleanup(t, out.release, finished)
	go func() {
		defer close(finished)
		writeDone <- writeRetrievalResponseBytes(out, []byte("private response\n"), policy)
	}()
	awaitRetrievalSignal(t, "response write", out.started)
	select {
	case err := <-mutation:
		t.Fatalf("tombstone linearized while the response write lock was held: %v", err)
	default:
	}
	unblock()
	if err := awaitRetrievalResult(t, "writeDone", writeDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitRetrievalResult(t, "mutation", mutation); err != nil {
		t.Fatalf("tombstone after response: %v", err)
	}
	if got := out.String(); got != "private response\n" {
		t.Fatalf("response=%q", got)
	}
	guard, err := bmanifest.LoadSessionReadGuard(fixture.BrainDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !guard.BlocksSession(fixture.SessionID) {
		t.Fatal("tombstone did not commit after response linearization")
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
	finished := make(chan struct{})
	unblock := retrievalWriterCleanup(t, out.release, finished)
	go func() {
		defer close(finished)
		done <- run(cmd)
	}()
	awaitRetrievalSignal(t, "response write", out.started)
	select {
	case err := <-mutation:
		t.Fatalf("tombstone linearized while the command response write lock was held: %v", err)
	default:
	}
	unblock()
	if err := awaitRetrievalResult(t, "done", done); err != nil {
		t.Fatal(err)
	}
	if err := awaitRetrievalResult(t, "mutation", mutation); err != nil {
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
			return dash.Run(context.Background(), cmd, dashConfig(fixture.Options), dash.Flags{JSON: true, Limit: 10}, fixture.Options.Env.RepoRoot)
		})
	})

	t.Run("workspace aggregate", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		const workspaceName = "privacy-aggregate-race"
		if err := workspace.DefaultStore(fixture.Options.Env).WriteManifest(workspace.Manifest{
			Name: workspaceName, Repos: []workspace.Repo{{RepoKey: fixture.RepoKey}},
		}); err != nil {
			t.Fatal(err)
		}
		requireCommandWriteLinearizesBeforeTombstone(t, fixture, func(cmd *cobra.Command) error {
			return runWorkspacePatternsList(context.Background(), cmd, fixture.Options, workspaceName, patternscmd.ListOptions{AsJSON: true})
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
		err := dash.Run(context.Background(), cmd, dashConfig(fixture.Options), dash.Flags{JSON: true, Limit: 10}, fixture.Options.Env.RepoRoot)
		requireLatePrivacyEmissionFailure(t, err, &out, called)
	})

	t.Run("workspace aggregate", func(t *testing.T) {
		fixture := writeRetrievalEmissionPrivacyFixture(t)
		const workspaceName = "privacy-aggregate-late"
		if err := workspace.DefaultStore(fixture.Options.Env).WriteManifest(workspace.Manifest{
			Name: workspaceName, Repos: []workspace.Repo{{RepoKey: fixture.RepoKey}},
		}); err != nil {
			t.Fatal(err)
		}
		called := installLateRetrievalTombstone(t, fixture)
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		err := runWorkspacePatternsList(context.Background(), cmd, fixture.Options, workspaceName, patternscmd.ListOptions{AsJSON: true})
		requireLatePrivacyEmissionFailure(t, err, &out, called)
	})
}

func TestWorkspaceAggregateRejectsAlreadyDirtyMember(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	const workspaceName = "privacy-dirty-member"
	if err := workspace.DefaultStore(fixture.Options.Env).WriteManifest(workspace.Manifest{
		Name: workspaceName, Repos: []workspace.Repo{{RepoKey: fixture.RepoKey}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WithBrainWriteLock(fixture.BrainDir, func() error {
		stones, _, err := brainstore.LoadSessionTombstonesChecked(fixture.BrainDir)
		if err != nil {
			return err
		}
		stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "already dirty"}
		return brainstore.SaveSessionTombstones(fixture.BrainDir, stones)
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := runWorkspacePatternsList(context.Background(), cmd, fixture.Options, workspaceName, patternscmd.ListOptions{AsJSON: true})
	if err == nil || !strings.Contains(err.Error(), brainstore.PrivacyDirtyCode) {
		t.Fatalf("workspace dirty-member error=%v, want %s", err, brainstore.PrivacyDirtyCode)
	}
	if out.Len() != 0 {
		t.Fatalf("dirty workspace emitted stale bytes: %q", out.String())
	}
}

func TestPatternBearingSurfacesRejectAlreadyDirtyDerivedStateWithoutOutput(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	if err := brainstore.WithBrainWriteLock(fixture.BrainDir, func() error {
		stones, _, err := brainstore.LoadSessionTombstonesChecked(fixture.BrainDir)
		if err != nil {
			return err
		}
		stones.Excluded[fixture.SessionID] = sessionTombstone{At: time.Now().UTC(), Reason: "already dirty"}
		return brainstore.SaveSessionTombstones(fixture.BrainDir, stones)
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
			return brainreview.RunBrainReview(context.Background(), cmd, brainReviewConfig(fixture.Options), regressionDetectorOptions{Limit: 3, JSON: true, Patterns: true}, "privacy state")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{Use: "test"}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			err := tc.run(cmd)
			if err == nil || !strings.Contains(err.Error(), brainstore.PrivacyDirtyCode) {
				t.Fatalf("dirty pattern surface error = %v, want %s", err, brainstore.PrivacyDirtyCode)
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
			return retrieve.Run(context.Background(), cmd, retrieveCommandConfig(fixture.Options), "privacy state", retrieval.ModeLexical, 5, "", retrievalOptions{Source: brainstore.RetrievalSourceConversation}, true, true, "query")
		}},
		{name: "get pattern", run: func(cmd *cobra.Command) error {
			_, err := retrieve.RunGet(context.Background(), cmd, retrieveCommandConfig(fixture.Options), []string{"pattern:missing"}, "", true, getOptions{}, "get")
			return err
		}},
		{name: "review patterns", run: func(cmd *cobra.Command) error {
			return brainreview.RunBrainReview(context.Background(), cmd, brainReviewConfig(fixture.Options), regressionDetectorOptions{Limit: 3, JSON: true, Patterns: true}, "privacy state")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{Use: "test"}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			err := tc.run(cmd)
			if err == nil || !strings.Contains(err.Error(), brainstore.SourceStaleCode) {
				t.Fatalf("missing requested corpus error = %v, want %s", err, brainstore.SourceStaleCode)
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
	manifest, err := bmanifest.Load(fixture.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := bmanifest.LoadSessionReadGuard(fixture.BrainDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	srv := &viz.Server{
		RepoDir: fixture.Options.Env.RepoRoot, BrainDir: fixture.BrainDir, Branch: "feature", Manifest: manifest,
		Guard: guard, GuardObserved: !guard.Empty(), Deps: vizDeps(),
	}
	handler, err := srv.Mux()
	if err != nil {
		t.Fatal(err)
	}
	mutation := installConcurrentLockedTombstone(t, fixture)
	out := &blockingHTTPResponseWriter{
		header: make(http.Header), started: make(chan struct{}), release: make(chan struct{}),
	}
	done := make(chan struct{})
	unblock := retrievalWriterCleanup(t, out.release, done)
	go func() {
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
		close(done)
	}()
	awaitRetrievalSignal(t, "response write", out.started)
	select {
	case err := <-mutation:
		t.Fatalf("tombstone linearized while the HTTP response write lock was held: %v", err)
	default:
	}
	unblock()
	awaitRetrievalSignal(t, "HTTP response", done)
	if err := awaitRetrievalResult(t, "mutation", mutation); err != nil {
		t.Fatalf("tombstone after HTTP response: %v", err)
	}
	if out.status != http.StatusOK || out.body.Len() == 0 {
		t.Fatalf("HTTP response status=%d body=%q", out.status, out.body.String())
	}
}

func TestVizAPIResponseRejectsLateTombstone(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	manifest, err := bmanifest.Load(fixture.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := bmanifest.LoadSessionReadGuard(fixture.BrainDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	srv := &viz.Server{
		RepoDir: fixture.Options.Env.RepoRoot, BrainDir: fixture.BrainDir, Branch: "feature", Manifest: manifest,
		Guard: guard, GuardObserved: !guard.Empty(), Deps: vizDeps(),
	}
	handler, err := srv.Mux()
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
