package cli

import (
	"sort"
	"strings"
	"testing"
)

// An agent picking between brain_code and its built-in Grep reads the tool
// DESCRIPTION, never a guide file it may never open. Before this test every
// one of the 37 brain_* descriptions said what the tool returns and none said
// when to reach for it instead of a built-in, so the built-in won by default.
//
// The requirement is asserted against mcpToolDefinitions() itself rather than
// a copied list, and the exemptions are an explicit deny-list: a NEW tool is
// required to carry a cue unless someone adds it below with a reason, so one
// cannot ship silently without one.

// mcpToolsExemptFromSubstitutionCue names the tools that have no built-in an
// agent could plausibly substitute, with the reason. Everything else must tell
// the agent when to use it instead of grep/read/git, and when not to.
var mcpToolsExemptFromSubstitutionCue = map[string]string{
	"brain_status":           "freshness preflight for the brain itself; no built-in reports it",
	"brain_index_status":     "alias of brain_status; same reason",
	"brain_refresh":          "rebuilds brain sources (a write); not a retrieval an agent would substitute",
	"brain_index_repository": "builds the semantic index (a write); not a retrieval",
	"brain_list_projects":    "brain project administration; no built-in equivalent",
	"brain_delete_project":   "destructive brain administration; no built-in equivalent",
	"brain_remember":         "the only tool that writes durable memory; nothing built-in records facts",
	"brain_get":              "fetches an id produced by another brain tool; unreachable without one",
	"brain_multi_get":        "batch form of brain_get; same reason",
	"brain_get_graph_schema": "metadata about the graph itself, not about the repository's code",
	"brain_get_architecture": "alias of brain_get_graph_schema; same reason",
	"brain_ingest_traces":    "imports a runtime trace file into the graph; no built-in equivalent",
	"brain_patterns":         "reads the mined pattern corpus; no built-in produces it",
	"brain_patterns_status":  "freshness of the pattern layer; no built-in reports it",
}

// Naming a built-in is half the cue. A description that says "use instead of"
// without saying instead of WHAT leaves the comparison the agent is actually
// making unresolved.
var mcpBuiltinAlternatives = []string{
	"grep", "glob", "git log", "git diff",
	"file read", "read the file", "read the diff", "re-reading",
}

func TestMCPToolDescriptionsCarryASubstitutionCue(t *testing.T) {
	t.Parallel()

	tools := mcpToolDefinitions()
	if len(tools) == 0 {
		t.Fatal("mcpToolDefinitions() returned no tools; the assertion below would be vacuous")
	}

	seen := map[string]bool{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "" {
			t.Fatalf("tool definition has no name: %v", tool)
		}
		seen[name] = true

		desc, _ := tool["description"].(string)
		if strings.TrimSpace(desc) == "" {
			t.Errorf("%s: description is empty; it is the only thing an agent reads when choosing a tool", name)
			continue
		}
		if reason := mcpToolsExemptFromSubstitutionCue[name]; reason != "" {
			continue
		}

		lower := strings.ToLower(desc)
		if !strings.Contains(lower, "instead of") {
			t.Errorf("%s: description never says when to use it instead of a built-in\n  got: %s", name, desc)
		}
		if !containsAnyMCPBuiltin(lower) {
			t.Errorf("%s: description names no built-in alternative (one of %v)\n  got: %s",
				name, mcpBuiltinAlternatives, desc)
		}
		// "when not to" is as load-bearing as "when to": a tool with no stated
		// limit gets trusted past the point where its index can answer.
		if !strings.Contains(desc, "Not for") {
			t.Errorf("%s: description states no limit; needs a \"Not for ...\" clause saying when to fall back\n  got: %s", name, desc)
		}
	}

	var stale []string
	for name := range mcpToolsExemptFromSubstitutionCue {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("exemptions name tools that no longer exist, so they silently exempt nothing: %v", stale)
	}
}

func containsAnyMCPBuiltin(lowerDesc string) bool {
	for _, alt := range mcpBuiltinAlternatives {
		if strings.Contains(lowerDesc, alt) {
			return true
		}
	}
	return false
}
