package cli

import (
	"context"
	"encoding/json"
	"github.com/entireio/entire-brain/internal/cli/command/retrieve"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestRecencyPublicRetrievalSurfaces(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	facts := []factRecord{
		{ID: "fact:old", Text: "alpha beta retrieval", Branch: "feature", Status: factStatusActive, CreatedAt: now.Add(-4 * 365 * 24 * time.Hour), UpdatedAt: now.Add(-3 * 365 * 24 * time.Hour)},
		{ID: "fact:fresh", Text: "alpha retrieval", Branch: "feature", Status: factStatusActive, CreatedAt: now.Add(-4 * 365 * 24 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour)},
	}
	if err := factstore.Write(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		want := "fact:old"
		if enabled {
			want = "fact:fresh"
		}
		for _, verb := range []string{"query", "search"} {
			args := []string{verb, "alpha beta", "--source", "fact", "--limit", "1", "--json"}
			if verb == "query" {
				args = append(args, "--keyword")
			}
			if enabled {
				args = append(args, "--recency", "--recency-half-life", "720h")
			}
			out, err := execute(t, NewRootCommand(opts), args...)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Results []unifiedResult `json:"results"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Results) != 1 || payload.Results[0].ID != want {
				t.Fatalf("CLI %s recency=%v: %s", verb, enabled, out)
			}

		}
	}
	// All three compatibility surfaces advertise the same opt-in contract.
	for _, cmd := range []*cobra.Command{retrieve.NewQuery(retrieveCommandConfig(opts)), retrieve.NewSearch(retrieveCommandConfig(opts)), retrieve.NewVsearch(retrieveCommandConfig(opts))} {
		if cmd.Flags().Lookup("recency") == nil || cmd.Flags().Lookup("recency-half-life") == nil {
			t.Fatalf("missing recency flags: %s", cmd.Name())
		}
	}
}

func TestRecencySurfaceValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cli  []string
		want string
	}{
		{"requires opt-in", []string{"--recency-half-life", "720h"}, "requires recency"},
		{"invalid", []string{"--recency", "--recency-half-life", "oops"}, "positive Go duration"},
		{"zero", []string{"--recency", "--recency-half-life", "0s"}, "positive Go duration"},
		{"negative", []string{"--recency", "--recency-half-life", "-1h"}, "positive Go duration"},
		{"conversation", []string{"--recency", "--source", "conversation"}, "not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execute(t, NewRootCommand(Options{}), append([]string{"query", "alpha"}, tc.cli...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CLI err=%v", err)
			}
		})
	}
	if _, err := retrieveUnifiedWithOptions("", t.TempDir(), "", "q", 1, retrieval.ModeLexical, retrievalOptions{Source: brainstore.RetrievalSourceConversation, Recency: true}); err == nil {
		t.Fatal("conversation must reject recency at retrieval boundary")
	}
}
