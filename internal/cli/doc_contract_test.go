package cli

import (
	"github.com/entireio/entire-brain/internal/cli/command/retrieve"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// doc_contract_test.go is the doc-contract lock: user-facing docs, tool
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
	reference := readRepoDoc(t, "docs/reference.md")
	threat := readRepoDoc(t, "docs/recall_threat_model.md")
	guide := readRepoDoc(t, "docs/semantic_features.md")

	opts := Options{Version: "test"}

	// The README describes the search interface; the reference documents
	// the development flags for conversation retrieval.
	if strings.Contains(readme, "`vsearch --source conversation` is unsupported") {
		t.Fatal("README still claims conversation vector search is unsupported")
	}
	if !strings.Contains(readme, "`--source` (`all` | `fact` |") {
		t.Fatal("README missing query source options")
	}
	// ENTIRE_BRAIN_CONVERSATION_FUSION is gone with semantic retrieval
	// (trail #278): the reference must not resurrect it.
	if strings.Contains(reference, "ENTIRE_BRAIN_CONVERSATION_FUSION") {
		t.Fatal("docs/reference.md documents the removed ENTIRE_BRAIN_CONVERSATION_FUSION flag")
	}
	if !strings.Contains(reference, "ENTIRE_BRAIN_BRIEF_CONVERSATION") {
		t.Fatal(`docs/reference.md missing "ENTIRE_BRAIN_BRIEF_CONVERSATION"`)
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
	if strings.Contains(strings.ToLower(threat), "fail-open") || strings.Contains(strings.ToLower(threat), "fail open") {
		t.Fatal("threat model must not describe tombstone corruption as fail-open")
	}
	for _, want := range []string{"fails closed", "unknown-newer", "lifecycle jobs", "egress"} {
		if !strings.Contains(threat, want) {
			t.Fatalf("threat model missing hardened privacy contract %q", want)
		}
	}
	if strings.Contains(guide, "(`history/projection-state-v1.json`, published through the manifest commit)") {
		t.Fatal("semantic guide still describes the pre-generation projection receipt path")
	}
	for _, want := range []string{"generation-addressed projection receipt", "manifest switches", "present_unproven", "[--repo-key <key>]", "repository identity"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("semantic guide missing current lifecycle contract %q", want)
		}
	}
	privacy := newPrivacyCommand(Options{Version: "test"})
	for _, sub := range privacy.Commands() {
		name := strings.Fields(sub.Use)[0]
		if !strings.Contains(guide, name) {
			t.Fatalf("semantic guide privacy verb list missing %q", name)
		}
	}

	if !strings.Contains(guide, "Multi-concept session recall") {
		t.Fatal("semantic guide must document multi-concept recall")
	}

	// CLI verbs: --source is registered on search, query, and vsearch.
	for name, cmd := range map[string]*cobra.Command{
		"search":  retrieve.NewSearch(retrieveCommandConfig(opts)),
		"query":   retrieve.NewQuery(retrieveCommandConfig(opts)),
		"vsearch": retrieve.NewVsearch(retrieveCommandConfig(opts)),
	} {
		if cmd.Flags().Lookup("source") == nil {
			t.Fatalf("%s must take --source", name)
		}
	}

}
