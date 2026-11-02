package cli

import (
	"bytes"
	"context"
	"github.com/entireio/entire-brain/internal/cli/command/brainreview"
	"github.com/entireio/entire-brain/internal/cli/command/retrieve"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
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

// Emission must not contend with brain writers: a response whose policies
// validated writes even while another process holds the brain write lock. The
// retired behavior acquired that lock per response, so any long-running writer
// (promote, distill) failed every concurrent reader with brain_locked. A
// regression here fails after the 10s lock timeout rather than hanging.
func TestRetrievalWriteSucceedsWhileBrainWriteLockHeld(t *testing.T) {
	fixture := writeRetrievalEmissionPrivacyFixture(t)
	unlock, err := brainstore.AcquireBrainWriteLock(fixture.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var out bytes.Buffer
	if err := writeRetrievalResponseBytes(&out, []byte("response\n")); err != nil {
		t.Fatalf("emission failed under a held brain write lock: %v", err)
	}
	if got := out.String(); got != "response\n" {
		t.Fatalf("response=%q", got)
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
