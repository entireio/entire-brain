package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// doc_contract_test.go is the R0-8 doc-contract lock: user-facing docs, tool
// descriptions, and the CLI/MCP source-mode surface must describe the same
// tested capability matrix. When behavior changes, this test forces the
// documentation to move with it instead of drifting.

func readRepoDoc(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestDocContractCapabilityMatrix(t *testing.T) {
	readme := readRepoDoc(t, "README.md")
	threat := readRepoDoc(t, "docs/recall_threat_model.md")
	guide := readRepoDoc(t, "docs/semantic_mcp_guide.md")

	opts := Options{Version: "test"}

	// Conversation vector support: the README must not describe the CLI
	// conversation vector arm as unsupported, and must document the fusion
	// development flag it references in code.
	if strings.Contains(readme, "`vsearch --source conversation` is unsupported") {
		t.Fatal("README still claims conversation vector search is unsupported")
	}
	for _, want := range []string{
		"ENTIRE_BRAIN_CONVERSATION_FUSION",
		"ENTIRE_BRAIN_BRIEF_CONVERSATION",
		"`--source` (`all` | `fact` |",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing %q", want)
		}
	}

	// Privacy/retention claims: retention exists; the threat model must not
	// describe it as unimplemented, and the guide must list every privacy
	// subcommand the CLI registers.
	if strings.Contains(threat, "not yet implemented") {
		t.Fatal("threat model still describes shipped capability as unimplemented")
	}
	if !strings.Contains(threat, "retention") {
		t.Fatal("threat model must describe the retention policy surface")
	}
	privacy := newPrivacyCommand(Options{Version: "test"})
	for _, sub := range privacy.Commands() {
		name := strings.Fields(sub.Use)[0]
		if !strings.Contains(guide, name) {
			t.Fatalf("semantic guide privacy verb list missing %q", name)
		}
	}

	// MCP surface: search/query advertise source + conversation filters;
	// vsearch advertises neither (the guide documents that limit).
	var searchSchema, vsearchSchema map[string]any
	for _, tool := range mcpToolDefinitions() {
		switch tool["name"] {
		case "brain_search":
			searchSchema = tool["inputSchema"].(map[string]any)
		case "brain_vsearch":
			vsearchSchema = tool["inputSchema"].(map[string]any)
		}
	}
	if searchSchema == nil || vsearchSchema == nil {
		t.Fatal("brain_search/brain_vsearch tool definitions missing")
	}
	searchProps, _ := json.Marshal(searchSchema["properties"])
	vsearchProps, _ := json.Marshal(vsearchSchema["properties"])
	// C2: all three retrieval tools share one strict schema (source,
	// structured filters, concepts).
	for _, arg := range []string{"source", "session_id", "agent", "after", "before", "concepts"} {
		if !strings.Contains(string(searchProps), `"`+arg+`"`) {
			t.Fatalf("brain_search schema missing %q: %s", arg, searchProps)
		}
		if !strings.Contains(string(vsearchProps), `"`+arg+`"`) {
			t.Fatalf("brain_vsearch schema missing %q: %s", arg, vsearchProps)
		}
	}
	if !strings.Contains(guide, "Multi-concept session recall") {
		t.Fatal("semantic guide must document multi-concept recall")
	}
	// The multi-get schema advertises the same batch cap the code enforces.
	for _, tool := range mcpToolDefinitions() {
		if tool["name"] != "brain_multi_get" {
			continue
		}
		schema, _ := json.Marshal(tool["inputSchema"])
		if !strings.Contains(string(schema), `"maxItems":`+fmt.Sprint(maxGetBatchIDs)) {
			t.Fatalf("brain_multi_get schema must advertise maxItems %d: %s", maxGetBatchIDs, schema)
		}
	}

	// CLI verbs: --source is registered on search, query, and vsearch.
	for name, cmd := range map[string]*cobra.Command{
		"search":  newSearchCommand(opts),
		"query":   newQueryCommand(opts),
		"vsearch": newVsearchCommand(opts),
	} {
		if cmd.Flags().Lookup("source") == nil {
			t.Fatalf("%s must take --source", name)
		}
	}
}
