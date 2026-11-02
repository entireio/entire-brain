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

	// Deletion claims: the brain mirrors the checkpoint refs, so the threat
	// model must describe capture-layer deletion and its residuals, and must
	// not resurrect the removed brain-local privacy commands.
	if strings.Contains(threat, "not yet implemented") {
		t.Fatal("threat model still describes shipped capability as unimplemented")
	}
	for _, want := range []string{"entire checkpoint delete", "refresh", "Convergence window", "egress"} {
		if !strings.Contains(threat, want) {
			t.Fatalf("threat model missing mirror deletion contract %q", want)
		}
	}
	for _, gone := range []string{"privacy exclude", "privacy purge", "privacy verify", "privacy retention"} {
		if strings.Contains(threat, gone) || strings.Contains(guide, gone) {
			t.Fatalf("docs still describe the removed %q command", gone)
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
