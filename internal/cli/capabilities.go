package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type brainSourceCapability struct {
	Name             string   `json:"name"`
	Default          bool     `json:"default"`
	Experimental     bool     `json:"experimental"`
	Keyword          bool     `json:"keyword"`
	SemanticCompiled bool     `json:"semantic_compiled"`
	SemanticRequires []string `json:"semantic_requires"`
}

type brainCapabilities struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	Scope         string `json:"scope"`
	Build         struct {
		BrainCGO     bool   `json:"brain_cgo"`
		SQLiteDriver string `json:"sqlite_driver"`
	} `json:"build"`
	Query struct {
		DefaultMode                string   `json:"default_mode"`
		Modes                      []string `json:"modes"`
		Inputs                     []string `json:"inputs"`
		ModeFlagsMutuallyExclusive bool     `json:"mode_flags_mutually_exclusive"`
	} `json:"query"`
	Sources       []brainSourceCapability `json:"sources"`
	Features      []string                `json:"features"`
	Experimental  []string                `json:"experimental"`
	GraphProvider struct {
		CapabilitiesCommand string `json:"capabilities_command"`
		Note                string `json:"note"`
	} `json:"graph_provider"`
	ReadinessCommand string `json:"readiness_command"`
}

func compiledBrainCapabilities(version string) brainCapabilities {
	var c brainCapabilities
	c.SchemaVersion, c.Version, c.Scope = 1, version, "compiled; not repository readiness"
	c.Build.BrainCGO, c.Build.SQLiteDriver = brainCGOBuild, sqliteDriverName
	// Semantic retrieval was removed (trail #278: BM25 lexical matched or beat
	// the embedder stack end-to-end). The "semantic" mode and the per-source
	// semantic fields stay in the schema reporting unavailable, so committed
	// agent-guides that validate this report keep parsing.
	c.Query.DefaultMode = "keyword"
	c.Query.Modes = []string{"keyword"}
	c.Query.Inputs = []string{"positional", "--query"}
	c.Query.ModeFlagsMutuallyExclusive = true
	removed := []string{"semantic retrieval removed (trail #278); BM25 lexical is the only ranking"}
	c.Sources = []brainSourceCapability{
		{retrievalSourceFact, true, false, true, false, removed},
		{retrievalSourceHistory, true, false, true, false, removed},
		{retrievalSourceDoc, true, false, true, false, removed},
		{retrievalSourceConversation, false, true, true, false, removed},
	}
	c.Features = []string{"agent_instructions", "bm25_retrieval", "durable_facts", "task_briefs", "workspaces", "mcp", "graph_inspection", "freshness_checks"}
	c.Experimental = []string{"conversation_retrieval", "compact_brief_formats", "recall_expansion", "deterministic_evidence_recall"}
	c.GraphProvider.CapabilitiesCommand = "entire graph capabilities --json"
	c.GraphProvider.Note = "Languages and relation types belong to the installed Graph provider; this command does not invoke it."
	c.ReadinessCommand = "entire brain status --details --json"
	return c
}

func newCapabilitiesCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "List supported features and retrieval sources",
		Long:  "List this binary's supported features and their requirements. Does not read repository state, initialize an embedder, invoke Graph, or create a brain. Use status --details for repository readiness; Graph reports its own parsed languages and relation types.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := compiledBrainCapabilities(opts.Version)
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(c)
			}
			out := cmd.OutOrStdout()
			var text strings.Builder
			fmt.Fprintf(&text, "Entire Brain %s\nBuild: %s (brain_cgo=%t)\n\n", c.Version, c.Build.SQLiteDriver, c.Build.BrainCGO)
			fmt.Fprintln(&text, "Retrieval: BM25 keyword (semantic retrieval removed)")
			fmt.Fprintln(&text, "Sources:")
			for _, source := range c.Sources {
				scope := "default"
				if source.Experimental {
					scope = "experimental, explicit opt-in"
				}
				fmt.Fprintf(&text, "  %s: keyword; semantic compiled=%t (%s)\n    Semantic requires: %s\n", source.Name, source.SemanticCompiled, scope, strings.Join(source.SemanticRequires, ", "))
			}
			fmt.Fprintf(&text, "\nFeatures: %s\nExperimental: %s\n", strings.Join(c.Features, ", "), strings.Join(c.Experimental, ", "))
			fmt.Fprintf(&text, "\nRepository readiness: %s\nLanguages and relations: %s\n", c.ReadinessCommand, c.GraphProvider.CapabilitiesCommand)
			_, err := fmt.Fprint(out, text.String())
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable capability inventory")
	return cmd
}
