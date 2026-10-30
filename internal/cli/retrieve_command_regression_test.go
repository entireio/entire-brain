package cli

import (
	"encoding/json"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/command/retrieve"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/workspace"
	"github.com/spf13/cobra"
)

type retrieveRegressionFailWriter struct{}

func (retrieveRegressionFailWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("retrieve regression output failure")
}

func retrieveRegressionWorkspace(t *testing.T) (Options, workspace.Manifest, map[string]string) {
	t.Helper()
	env := EntireEnv{PluginDataDir: t.TempDir()}
	manifest := workspace.Manifest{SchemaVersion: workspace.SchemaVersion, Name: "retrieval", Repos: []workspace.Repo{
		{RepoKey: "gh/acme/alpha", Name: "alpha"},
		{RepoKey: "gh/acme/beta", Name: "beta"},
	}}
	if err := workspace.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for i, repo := range manifest.Repos {
		brainDir, err := base.BrainDirForKey(env, repo.RepoKey)
		if err != nil {
			t.Fatal(err)
		}
		text := []string{"nebula alpha public record", "nebula beta public record"}[i]
		paths := normalizeFactPaths([]string{"architecture/retrieval.go"})
		fact := factRecord{ID: factRecordID(text, paths), Text: text, Paths: paths, Branch: "main", Status: factStatusActive, UpdatedAt: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
		if err := factstore.Write(brainDir, "main", []factRecord{fact}); err != nil {
			t.Fatal(err)
		}
		manifestBytes := []byte(`{"schema_version":3,"sources":{"sessions":{"default_branch":"main","sessions":[]}}}`)
		if err := os.WriteFile(filepath.Join(brainDir, bmanifest.FileName), manifestBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		ids[repo.RepoKey] = fact.ID
	}
	return Options{Version: "test", Env: env, Now: func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }}, manifest, ids
}

func TestRetrieveRootRouteSourceIsolationNoResultsAndLimit(t *testing.T) {
	opts, manifest, ids := retrieveRegressionWorkspace(t)
	// Workspace fan-out proves source isolation and per-member limiting with
	// durable records. A hidden canary in an unrelated file must never escape.
	firstBrain := mustBrainDirForKey(t, opts.Env, manifest.Repos[0].RepoKey)
	if err := os.WriteFile(filepath.Join(firstBrain, "hidden-canary.txt"), []byte("HIDDEN-RETRIEVAL-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(opts), "workspace", "search", manifest.Name, "nebula", "--source", "fact", "--branch", "main", "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("workspace search: %v\n%s", err, out)
	}
	if strings.Contains(out, "HIDDEN-RETRIEVAL-CANARY") {
		t.Fatalf("hidden content escaped: %s", out)
	}
	var payload struct {
		Results []workspaceRetrieveResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("groups=%d, want 2: %s", len(payload.Results), out)
	}
	for _, group := range payload.Results {
		if len(group.Results) != 1 || group.Results[0].Source != brainstore.RetrievalSourceFact || group.Results[0].ID != group.RepoKey+"/"+ids[group.RepoKey] {
			t.Fatalf("source/limit/qualification contract failed: %+v", group)
		}
	}

	empty, err := execute(t, NewRootCommand(opts), "workspace", "search", manifest.Name, "absent-token", "--source", "fact", "--branch", "main")
	if err != nil || empty != "" {
		t.Fatalf("empty text response = %q, err=%v", empty, err)
	}
}

func mustBrainDirForKey(t *testing.T, env EntireEnv, key string) string {
	t.Helper()
	dir, err := base.BrainDirForKey(env, key)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRetrieveCommandsRejectInvalidContractsBeforeReadingState(t *testing.T) {
	cmd := &cobra.Command{}
	wantErrors := map[string]string{
		"single zero limit":                  "--limit must be greater than 0",
		"workspace zero limit":               "--limit must be greater than zero",
		"workspace malformed source":         "source must be one of",
		"workspace missing manifest":         "missing",
		"workspace conversation vector":      "vector search was removed",
		"workspace fact conversation filter": "filters require --source conversation",
		"workspace fact abstract":            "--include-abstract requires --source conversation",
	}
	for name, err := range map[string]error{
		"single zero limit":                  retrieve.Run(cmd.Context(), cmd, retrieveCommandConfig(Options{}), "q", retrieval.ModeLexical, 0, "", retrievalOptions{}, false, false, "search"),
		"workspace zero limit":               runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 0}, retrieval.ModeLexical, "missing", "q"),
		"workspace malformed source":         runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 1, source: "private"}, retrieval.ModeLexical, "missing", "q"),
		"workspace missing manifest":         runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 1, source: brainstore.RetrievalSourceFact}, retrieval.ModeLexical, "missing", "q"),
		"workspace conversation vector":      runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 1, source: brainstore.RetrievalSourceConversation}, retrieval.ModeVector, "missing", "q"),
		"workspace fact conversation filter": runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 1, source: brainstore.RetrievalSourceFact, session: "secret-session"}, retrieval.ModeLexical, "missing", "q"),
		"workspace fact abstract":            runWorkspaceRetrieve(cmd, Options{}, workspaceRetrieveOptions{limit: 1, source: brainstore.RetrievalSourceFact, includeAbstract: true}, retrieval.ModeLexical, "missing", "q"),
	} {
		if err == nil || !strings.Contains(err.Error(), wantErrors[name]) {
			t.Errorf("%s: got %v, want %q", name, err, wantErrors[name])
		}
		if err != nil && strings.Contains(err.Error(), "secret-session") {
			t.Errorf("%s leaked hidden filter value: %v", name, err)
		}
	}
}

func TestRunRetrieveRealFactTextJSONNoResultsAndWriterFailure(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	paths := normalizeFactPaths([]string{"architecture/retrieval.go"})
	visible := factRecord{ID: factRecordID("orion visible retrieval fact", paths), Text: "orion visible retrieval fact", Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: opts.Now()}
	if err := factstore.Write(storage.BrainDir, "feature", []factRecord{visible}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, "hidden.txt"), []byte("HIDDEN-ORION-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidAbstract := &cobra.Command{}
	if err := retrieve.Run(invalidAbstract.Context(), invalidAbstract, retrieveCommandConfig(opts), "orion", retrieval.ModeLexical, 1, "feature", retrievalOptions{Source: brainstore.RetrievalSourceFact, IncludeAbstract: true}, false, false, "search"); err == nil || !strings.Contains(err.Error(), "conversation") {
		t.Fatalf("fact abstract error = %v", err)
	}

	for _, jsonOut := range []bool{false, true} {
		cmd := &cobra.Command{}
		var out strings.Builder
		cmd.SetOut(&out)
		err := retrieve.Run(cmd.Context(), cmd, retrieveCommandConfig(opts), "orion", retrieval.ModeLexical, 1, "feature", retrievalOptions{Source: brainstore.RetrievalSourceFact}, jsonOut, false, "search")
		if err != nil || !strings.Contains(out.String(), visible.ID) || strings.Contains(out.String(), "HIDDEN-ORION-CANARY") {
			t.Fatalf("json=%v result contract: err=%v output=%s", jsonOut, err, out.String())
		}
	}

	empty := &cobra.Command{}
	var emptyOut strings.Builder
	empty.SetOut(&emptyOut)
	if err := retrieve.Run(empty.Context(), empty, retrieveCommandConfig(opts), "not-present", retrieval.ModeLexical, 2, "feature", retrievalOptions{Source: brainstore.RetrievalSourceFact}, false, false, "search"); err != nil || !strings.Contains(emptyOut.String(), `no results for "not-present"`) {
		t.Fatalf("no-results contract: err=%v output=%s", err, emptyOut.String())
	}

	failing := &cobra.Command{}
	failing.SetOut(retrieveRegressionFailWriter{})
	if err := retrieve.Run(failing.Context(), failing, retrieveCommandConfig(opts), "orion", retrieval.ModeLexical, 1, "feature", retrievalOptions{Source: brainstore.RetrievalSourceFact}, true, false, "search"); err == nil || !strings.Contains(err.Error(), "output failure") {
		t.Fatalf("writer error = %v", err)
	}
}

func TestWorkspaceRetrieveMalformedMemberAndOutputFailure(t *testing.T) {
	opts, manifest, _ := retrieveRegressionWorkspace(t)
	badBrain := mustBrainDirForKey(t, opts.Env, manifest.Repos[1].RepoKey)
	if err := os.WriteFile(filepath.Join(badBrain, bmanifest.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	err := runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 2, source: brainstore.RetrievalSourceFact}, retrieval.ModeLexical, manifest.Name, "nebula")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), manifest.Repos[0].RepoKey) || strings.Contains(out.String(), "HIDDEN") {
		t.Fatalf("healthy member was not preserved: %s", out.String())
	}

	cmd.SetOut(retrieveRegressionFailWriter{})
	err = runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 2, source: brainstore.RetrievalSourceFact, json: true}, retrieval.ModeLexical, manifest.Name, "nebula")
	if err == nil || !strings.Contains(err.Error(), "output failure") {
		t.Fatalf("writer error = %v", err)
	}
}

func TestWorkspaceRetrieveGraphGroupSuccessAndMalformedState(t *testing.T) {
	opts, manifest, _ := retrieveRegressionWorkspace(t)
	edge := workspace.CrossEdge{
		Endpoint: "external:http:get /nebula", Type: "route", FromRepo: manifest.Repos[0].RepoKey, ToRepo: manifest.Repos[1].RepoKey,
		FromSymbol:   workspace.SymbolRef{RepoKey: manifest.Repos[0].RepoKey, ID: "symbol:alpha.callNebula", Kind: "function", Name: "callNebula", FilePath: "alpha/client.go"},
		ToSymbol:     workspace.SymbolRef{RepoKey: manifest.Repos[1].RepoKey, ID: "symbol:beta.serveNebula", Kind: "function", Name: "serveNebula", FilePath: "beta/server.go"},
		RelationKind: "cross_repo_route_call", SharedCount: 3,
	}
	if _, err := writeWorkspaceGraphPayload(opts.Env, workspaceGraphPayload{Generation: workspaceGraphTestGeneration(t, opts.Env, manifest), Workspace: manifest.Name, GeneratedAt: opts.Now(), CrossEdges: []workspace.CrossEdge{edge}}); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	if err := runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 3, json: true, branch: "main"}, retrieval.ModeLexical, manifest.Name, "nebula route"); err != nil {
		t.Fatal(err)
	}
	graphID := workspace.GraphCrossEdgeID(edge)
	for _, want := range []string{graphID, `"source": "workspace_graph"`, "cross_repo_route_call", "alpha/client.go", "beta/server.go"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("graph output missing %q: %s", want, out.String())
		}
	}

	wsDir, err := workspace.DefaultStore(opts.Env).Dir(manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, workspaceGraphName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 3, json: true, branch: "main"}, retrieval.ModeLexical, manifest.Name, "nebula"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"name": "workspace graph"`) || !strings.Contains(out.String(), `"error":`) || !strings.Contains(out.String(), "nebula alpha public record") {
		t.Fatalf("malformed graph must be isolated from member results: %s", out.String())
	}
}

func TestWorkspaceRetrieveMultiConceptJSONAndTextProofs(t *testing.T) {
	sourceBrain := writeMultiConceptFixture(t)
	env := EntireEnv{PluginDataDir: t.TempDir()}
	repoKey := "test/mc"
	targetBrain := mustBrainDirForKey(t, env, repoKey)
	if err := os.MkdirAll(filepath.Dir(targetBrain), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sourceBrain, targetBrain); err != nil {
		t.Fatal(err)
	}
	manifest := workspace.Manifest{SchemaVersion: workspace.SchemaVersion, Name: "concepts", Repos: []workspace.Repo{{RepoKey: repoKey, Name: "multi-concept"}}}
	if err := workspace.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}
	opts := Options{Version: "test", Env: env, Now: func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }}
	retrieveOpts := workspaceRetrieveOptions{
		limit: 10, source: brainstore.RetrievalSourceConversation,
		concepts: []string{"beta cache eviction", "gamma flag"}, includeAbstract: true,
	}
	for _, jsonOut := range []bool{false, true} {
		cmd := &cobra.Command{}
		var out strings.Builder
		cmd.SetOut(&out)
		retrieveOpts.json = jsonOut
		if err := runWorkspaceRetrieve(cmd, opts, retrieveOpts, retrieval.ModeLexical, manifest.Name, "alpha rollout"); err != nil {
			t.Fatalf("json=%v: %v", jsonOut, err)
		}
		got := out.String()
		for _, want := range []string{repoKey + "/" + brainstore.ConversationSessionIDPrefix, repoKey + "/" + conversationIDPrefix + "one1", "beta cache eviction", "gamma flag"} {
			if !strings.Contains(got, want) {
				t.Fatalf("json=%v missing proof %q: %s", jsonOut, want, got)
			}
		}
		if strings.Contains(got, "sess-three") || strings.Contains(got, "HIDDEN") {
			t.Fatalf("json=%v admitted incomplete or hidden session: %s", jsonOut, got)
		}
		if len(got) > conversationConceptResponseMaxBytes {
			t.Fatalf("json=%v response exceeded budget: %d", jsonOut, len(got))
		}
		if jsonOut && !strings.Contains(got, `"abstract_previews"`) {
			t.Fatalf("JSON omitted abstract preview status: %s", got)
		}
	}
}

func TestWorkspaceRetrieveCorruptMemberCorpusIsReported(t *testing.T) {
	opts, manifest, ids := retrieveRegressionWorkspace(t)
	badBrain := mustBrainDirForKey(t, opts.Env, manifest.Repos[1].RepoKey)
	if err := os.WriteFile(filepath.Join(badBrain, filepath.FromSlash(factstore.FileRelPath("main"))), []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	if err := runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 3, source: brainstore.RetrievalSourceFact, branch: "main"}, retrieval.ModeLexical, manifest.Name, "nebula"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), manifest.Repos[0].RepoKey+"/"+ids[manifest.Repos[0].RepoKey]) || !strings.Contains(out.String(), manifest.Repos[1].RepoKey+" error ") || !strings.Contains(out.String(), "facts.ndjson") {
		t.Fatalf("corrupt member must be isolated and named: %s", out.String())
	}
}

func TestWorkspaceRetrieveLimitTruncatesEachMember(t *testing.T) {
	opts, manifest, firstIDs := retrieveRegressionWorkspace(t)
	secondIDs := map[string]string{}
	for _, repo := range manifest.Repos {
		dir := mustBrainDirForKey(t, opts.Env, repo.RepoKey)
		facts, err := factstore.Load(dir, "main")
		if err != nil {
			t.Fatal(err)
		}
		second := factFor(t, "nebula companion public record "+repo.Name, []string{"architecture.retrieval.contract"}, opts.Now())
		second.Branch = "main"
		secondIDs[repo.RepoKey] = second.ID
		if err := factstore.Write(dir, "main", append(facts, second)); err != nil {
			t.Fatal(err)
		}
	}
	results := func(limit string) []workspaceRetrieveResult {
		t.Helper()
		out, err := execute(t, NewRootCommand(opts), "workspace", "search", manifest.Name, "nebula", "--source", "fact", "--branch", "main", "--limit", limit, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Results []workspaceRetrieveResult `json:"results"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Results
	}
	full, limited := results("10"), results("1")
	if len(full) != 2 || len(limited) != 2 {
		t.Fatalf("member counts full=%d limited=%d", len(full), len(limited))
	}
	for i, group := range full {
		if len(group.Results) != 2 {
			t.Fatalf("fixture does not exercise truncation: %+v", group)
		}
		got := map[string]bool{}
		for _, r := range group.Results {
			got[r.ID] = true
		}
		if !got[group.RepoKey+"/"+firstIDs[group.RepoKey]] || !got[group.RepoKey+"/"+secondIDs[group.RepoKey]] {
			t.Fatalf("wrong full identities: %+v", group)
		}
		if limited[i].RepoKey != group.RepoKey || len(limited[i].Results) != 1 || limited[i].Results[0].ID != group.Results[0].ID {
			t.Fatalf("limit changed member/ranking rather than truncating: full=%+v limited=%+v", group, limited[i])
		}
	}
}
