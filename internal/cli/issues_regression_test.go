package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ashtom/entire-brain/internal/issues"
	"github.com/spf13/cobra"
)

type issueDisconnectEmbedder struct{ run func() }

func (*issueDisconnectEmbedder) ID() string                    { return "disconnect-test" }
func (*issueDisconnectEmbedder) Dim() int                      { return 2 }
func (*issueDisconnectEmbedder) Embed(string) []float32        { return []float32{1, 0} }
func (e *issueDisconnectEmbedder) EmbedQuery(string) []float32 { e.run(); return []float32{1, 0} }

func TestIssueRankingRejectsConcurrentDisconnect(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(fmt.Sprint(purge), func(t *testing.T) {
			dir := t.TempDir()
			seedIssueTest(t, dir)
			e := &issueDisconnectEmbedder{run: func() {
				if err := issueStore(dir).Disconnect(issueTestProject, purge); err != nil {
					t.Fatal(err)
				}
			}}
			hits, err := retrieveIssues(dir, "retry", 5, modeHybrid, e)
			if err != nil || len(hits) != 0 {
				t.Fatalf("disconnected evidence returned: %d, %v", len(hits), err)
			}
		})
	}
}

func TestIssueEmissionRejectsLatePurge(t *testing.T) {
	for _, surface := range []string{"query", "get", "brief", "workspace-query", "workspace-get", "mcp-get"} {
		t.Run(surface, func(t *testing.T) {
			f := newBrainBriefProfileFixture(t)
			s := seedIssueTest(t, f.brainDir)
			original := beforeRetrievalResponsePrivacyEmissionCheck
			called := false
			beforeRetrievalResponsePrivacyEmissionCheck = func() {
				if called {
					return
				}
				called = true
				if err := issueStore(f.brainDir).Disconnect(issueTestProject, true); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { beforeRetrievalResponsePrivacyEmissionCheck = original })
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			var err error
			switch surface {
			case "query":
				err = runRetrieve(context.Background(), cmd, f.opts, "retry", modeLexical, 3, "feature", retrievalOptions{Source: "issue"}, true, false, "query")
			case "get":
				_, err = runGet(context.Background(), cmd, f.opts, []string{s.Ref}, "feature", true, getOptions{}, "get")
			case "brief":
				err = runBrainBrief(context.Background(), cmd, f.opts, brainBriefOptions{limit: 3, json: true, issue: "COR-123"}, "retry")
			case "workspace-query", "workspace-get":
				const key = "gh/example/repo"
				if err := writeWorkspaceManifest(f.opts.Env, workspaceManifest{Name: "issue-race", Repos: []workspaceRepo{{RepoKey: key}}}); err != nil {
					t.Fatal(err)
				}
				if surface == "workspace-query" {
					err = runWorkspaceRetrieve(cmd, f.opts, workspaceRetrieveOptions{limit: 3, source: "issue", branch: "feature", json: true}, modeLexical, "issue-race", "retry")
				} else {
					err = runWorkspaceGet(cmd, f.opts, "issue-race", []string{key + "/" + s.Ref}, "feature", true, getOptions{})
				}
			case "mcp-get":
				raw, _ := json.Marshal(mcpToolCallParams{Name: "brain_get", Arguments: map[string]any{"id": s.Ref}})
				_, err = handleMCPToolCall(context.Background(), f.opts, raw)
			}
			if !called || err == nil || !strings.Contains(err.Error(), "issue evidence changed") || out.Len() != 0 {
				t.Fatalf("late purge leaked or was not checked: called=%v err=%v output=%q", called, err, out.String())
			}
		})
	}
}

func TestIssueLargeSnapshotMCPPagination(t *testing.T) {
	f := newBrainBriefProfileFixture(t)
	s := seedIssueTest(t, f.brainDir)
	r := s.Record
	r.Text = strings.Repeat("evidence🙂\"\n\x01 ", 13000)
	r.UpdatedAt = r.UpdatedAt.AddDate(0, 0, 1)
	importIssueTest(t, f.brainDir, 2, []issues.Record{r})
	current, _, _ := issueStore(f.brainDir).Get(r.Key())
	var assembled strings.Builder
	id := r.Key()
	var continuation string
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("pagination did not terminate")
		}
		raw, _ := json.Marshal(mcpToolCallParams{Name: "brain_get", Arguments: map[string]any{"id": id}})
		result, err := handleMCPToolCall(context.Background(), f.opts, raw)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(result)
		if len(b) > mcpToolResponseMaxBytes {
			t.Fatal("MCP page exceeds budget")
		}
		var wire struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(b, &wire); err != nil || len(wire.Content) != 1 {
			t.Fatal("invalid MCP page", err)
		}
		var payload struct {
			Results []unifiedResult `json:"results"`
		}
		if err := json.Unmarshal([]byte(wire.Content[0].Text), &payload); err != nil || len(payload.Results) != 1 {
			t.Fatal("snapshot dropped", wire, err)
		}
		hit := payload.Results[0]
		if hit.Issue.Snapshot != current.Ref || hit.Issue.Offset != assembled.Len() || !utf8.ValidString(hit.Text) {
			t.Fatal("invalid snapshot page", hit.Issue)
		}
		assembled.WriteString(hit.Text)
		if hit.Issue.NextID == "" {
			break
		}
		qualified := qualifyWorkspaceUnifiedResult("gh/example/repo", "workspace", hit)
		key, next, err := splitWorkspaceID(qualified.Issue.NextID)
		if err != nil || key != "gh/example/repo" || next != hit.Issue.NextID {
			t.Fatal("unresolvable workspace continuation", err)
		}
		id = hit.Issue.NextID
		if page == 0 {
			continuation = id
			// Refresh while the consumer is reading: continuation stays pinned.
			newer := r
			newer.Text = "new revision"
			newer.UpdatedAt = newer.UpdatedAt.AddDate(0, 0, 1)
			importIssueTest(t, f.brainDir, 3, []issues.Record{newer})
		}
	}
	if assembled.String() != r.Text {
		t.Fatal("pages did not reconstruct exact source text")
	}
	// The same opaque continuation works through CLI multi-get and workspace get.
	cliOut, err := execute(t, NewRootCommand(f.opts), "multi-get", continuation, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var cliPayload struct {
		Results []unifiedResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(cliOut), &cliPayload); err != nil || len(cliPayload.Results) != 1 || cliPayload.Results[0].Issue.Offset == 0 {
		t.Fatal("CLI continuation failed", cliOut, err)
	}
	const key = "gh/example/repo"
	if err := writeWorkspaceManifest(f.opts.Env, workspaceManifest{Name: "issue-pages", Repos: []workspaceRepo{{RepoKey: key}}}); err != nil {
		t.Fatal(err)
	}
	var workspaceOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&workspaceOut)
	if err := runWorkspaceGet(cmd, f.opts, "issue-pages", []string{key + "/" + continuation}, "feature", true, getOptions{}); err != nil {
		t.Fatal(err)
	}
	var workspacePayload struct {
		Results []workspaceGetResult `json:"results"`
	}
	if err := json.Unmarshal(workspaceOut.Bytes(), &workspacePayload); err != nil || len(workspacePayload.Results) != 1 || len(workspacePayload.Results[0].Results) != 1 {
		t.Fatal("workspace continuation failed", workspaceOut.String(), err)
	}
	workspacePage := workspacePayload.Results[0].Results[0]
	if workspacePage.Text != cliPayload.Results[0].Text || !strings.HasPrefix(workspacePage.Issue.NextID, key+"/") {
		t.Fatal("workspace continuation disagrees with CLI")
	}
	for _, bad := range []string{r.Key() + "#offset=1", current.Ref + "#offset=-1", current.Ref + "#offset=9", current.Ref + "#offset=9999999"} {
		if _, _, err := getIssueEvidence(f.brainDir, bad); err == nil {
			t.Fatal("invalid cursor accepted", bad)
		}
	}
}

func TestIssuePurgeWaitsForMCPResponseFrame(t *testing.T) {
	dir := t.TempDir()
	seedIssueTest(t, dir)
	policy, err := captureRetrievalPrivacyPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := &mcpResponsePrivacyState{}
	defer state.release()
	out := &mcpToolOutputBuffer{privacy: state}
	if err := writeRetrievalResponseBytes(out, []byte("issue evidence"), policy); err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); done <- issueStore(dir).Disconnect(issueTestProject, true) }()
	<-started
	select {
	case err := <-done:
		t.Fatalf("purge crossed unfinished MCP frame: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	state.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("purge did not finish after response lock released")
	}
}
