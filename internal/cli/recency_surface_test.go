package cli

import (
	"context"
	"encoding/json"
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
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
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

			selected := facts[0]
			if enabled {
				selected = facts[1]
			}
			if payload.Results[0].CreatedAt != selected.CreatedAt.Format(time.RFC3339) || payload.Results[0].UpdatedAt != selected.UpdatedAt.Format(time.RFC3339) {
				t.Fatalf("CLI lost timestamp semantics: %+v", payload.Results[0])
			}
			margs := map[string]any{"query": "alpha beta", "source": "fact", "limit": 1}
			if verb == "query" {
				margs["keyword"] = true
			}
			if enabled {
				margs["recency"] = true
				margs["recency_half_life"] = "720h"
			}
			raw, err := json.Marshal(map[string]any{"name": "brain_" + verb, "arguments": margs})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handleMCPToolCall(context.Background(), opts, raw)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(map[string]any{"result": result})
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(wire, &response); err != nil {
				t.Fatal(err)
			}
			mp := mcpTextJSONPayload(t, response)
			row := firstPayloadObject(t, mp, "results")

			if row["created_at"] != selected.CreatedAt.Format(time.RFC3339) || row["updated_at"] != selected.UpdatedAt.Format(time.RFC3339) {
				t.Fatalf("MCP lost timestamp semantics: %+v", row)
			}
			if row["id"] != want {
				t.Fatalf("MCP %s recency=%v: %+v", verb, enabled, mp)
			}
		}
	}
	// All three compatibility surfaces advertise the same opt-in contract.
	for _, cmd := range []*cobra.Command{newQueryCommand(opts), newSearchCommand(opts), newVsearchCommand(opts)} {
		if cmd.Flags().Lookup("recency") == nil || cmd.Flags().Lookup("recency-half-life") == nil {
			t.Fatalf("missing recency flags: %s", cmd.Name())
		}
	}
	for _, tool := range []string{"brain_query", "brain_search", "brain_vsearch"} {
		if err := validateMCPToolArguments(tool, map[string]any{"query": "alpha", "recency": true, "recency_half_life": "720h"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecencySurfaceValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cli  []string
		mcp  map[string]any
		want string
	}{
		{"requires opt-in", []string{"--recency-half-life", "720h"}, map[string]any{"recency_half_life": "720h"}, "requires recency"},
		{"invalid", []string{"--recency", "--recency-half-life", "oops"}, map[string]any{"recency": true, "recency_half_life": "oops"}, "positive Go duration"},
		{"zero", []string{"--recency", "--recency-half-life", "0s"}, map[string]any{"recency": true, "recency_half_life": "0s"}, "positive Go duration"},
		{"negative", []string{"--recency", "--recency-half-life", "-1h"}, map[string]any{"recency": true, "recency_half_life": "-1h"}, "positive Go duration"},
		{"conversation", []string{"--recency", "--source", "conversation"}, map[string]any{"recency": true, "source": "conversation"}, "not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execute(t, NewRootCommand(Options{}), append([]string{"query", "alpha"}, tc.cli...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CLI err=%v", err)
			}
			_, err = mcpRetrievalOptions(tc.mcp, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MCP err=%v", err)
			}
		})
	}
	for _, args := range []map[string]any{{"recency": "yes"}, {"recency": true, "recency_half_life": 42}} {
		if _, err := mcpRetrievalOptions(args, ""); err == nil {
			t.Fatalf("accepted bad types: %+v", args)
		}
	}
	for _, args := range []map[string]any{{}, {"recency": true}, {"recency": true, "recency_half_life": "720h"}} {
		got, err := mcpRetrievalOptions(args, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Recency != (args["recency"] == true) {
			t.Fatalf("wrong opt-in: %+v", got)
		}
		want := time.Duration(0)
		if args["recency_half_life"] != nil {
			want = 720 * time.Hour
		}
		if got.RecencyHalfLife != want {
			t.Fatalf("half life %s want %s", got.RecencyHalfLife, want)
		}
	}
	if _, err := retrieveUnifiedWithOptions("", t.TempDir(), "", "q", 1, modeLexical, retrievalOptions{Source: retrievalSourceConversation, Recency: true}); err == nil {
		t.Fatal("conversation must reject recency at retrieval boundary")
	}
}
