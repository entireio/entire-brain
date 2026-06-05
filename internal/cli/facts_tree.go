package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// factTreeNode groups a branch's facts into a navigable hierarchy keyed by
// taxonomy path. category nodes hold subcategory.type leaves; each leaf holds
// the facts at that exact path. Counts roll up so a caller can read the top of
// the tree (categories) and drill into only the subtree it needs — progressive
// disclosure that keeps both an agent's token load and a human's reading bounded.
type factTreeNode struct {
	Label    string          `json:"label"`
	Count    int             `json:"count"`
	Children []*factTreeNode `json:"children,omitempty"`
	Facts    []factRecord    `json:"facts,omitempty"`
}

// buildFactTree organizes active facts into a two-level outline: top-level
// category (the first path segment) → full taxonomy path (the leaf). Only active
// facts are included unless includeAll. A fact under two paths appears under
// both. Nodes and leaves are ordered by descending count, then label, so the
// densest, most load-bearing areas surface first.
func buildFactTree(facts []factRecord, includeAll bool) *factTreeNode {
	type leaf struct {
		records []factRecord
	}
	cats := map[string]map[string]*leaf{}
	for _, f := range facts {
		if !includeAll && f.Status != factStatusActive {
			continue
		}
		for _, path := range f.Paths {
			cat := factTopLevel(path)
			if cats[cat] == nil {
				cats[cat] = map[string]*leaf{}
			}
			if cats[cat][path] == nil {
				cats[cat][path] = &leaf{}
			}
			cats[cat][path].records = append(cats[cat][path].records, f)
		}
	}

	root := &factTreeNode{Label: "facts"}
	for cat, leaves := range cats {
		catNode := &factTreeNode{Label: cat}
		for path, l := range leaves {
			records := append([]factRecord(nil), l.records...)
			sortFactsByImportance(records)
			catNode.Children = append(catNode.Children, &factTreeNode{Label: path, Count: len(records), Facts: records})
			catNode.Count += len(records)
		}
		sortTreeChildren(catNode.Children)
		root.Children = append(root.Children, catNode)
		root.Count += catNode.Count
	}
	sortTreeChildren(root.Children)
	return root
}

// sortFactsByImportance orders facts within a leaf by a crude importance proxy:
// more provenance anchors first (corroborated by more sessions), then most
// recently updated. This surfaces the load-bearing facts as a leaf's
// representatives.
func sortFactsByImportance(records []factRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if len(records[i].Provenance) != len(records[j].Provenance) {
			return len(records[i].Provenance) > len(records[j].Provenance)
		}
		return records[i].UpdatedAt.After(records[j].UpdatedAt)
	})
}

func sortTreeChildren(children []*factTreeNode) {
	sort.SliceStable(children, func(i, j int) bool {
		if children[i].Count != children[j].Count {
			return children[i].Count > children[j].Count
		}
		return children[i].Label < children[j].Label
	})
}

// filterTreeByPath prunes the tree to the subtree under the given path prefix
// (a category like "constraints" or a full path). Returns nil if nothing
// matches.
func filterTreeByPath(root *factTreeNode, prefix string) *factTreeNode {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return root
	}
	out := &factTreeNode{Label: root.Label}
	for _, cat := range root.Children {
		if cat.Label == prefix {
			out.Children = append(out.Children, cat)
			out.Count += cat.Count
			continue
		}
		kept := &factTreeNode{Label: cat.Label}
		for _, leaf := range cat.Children {
			if leaf.Label == prefix || strings.HasPrefix(leaf.Label, prefix+".") {
				kept.Children = append(kept.Children, leaf)
				kept.Count += leaf.Count
			}
		}
		if len(kept.Children) > 0 {
			out.Children = append(out.Children, kept)
			out.Count += kept.Count
		}
	}
	if len(out.Children) == 0 {
		return nil
	}
	return out
}

// renderFactTree writes the outline. depth 1 prints categories only; depth 2
// adds the path leaves; depth >= 3 adds up to `leaves` sample facts per leaf
// with a "+N more" pointer so the rest is reachable via recall or a deeper
// drill, never silently hidden.
func renderFactTree(cmd *cobra.Command, root *factTreeNode, depth, leaves int) {
	for _, cat := range root.Children {
		fmt.Fprintf(cmd.OutOrStdout(), "%s (%d)\n", cat.Label, cat.Count)
		if depth < 2 {
			continue
		}
		for _, leaf := range cat.Children {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s (%d)\n", leaf.Label, leaf.Count)
			if depth < 3 {
				continue
			}
			shown := leaf.Facts
			if len(shown) > leaves {
				shown = shown[:leaves]
			}
			for _, f := range shown {
				fmt.Fprintf(cmd.OutOrStdout(), "    - %s\n", truncateString(f.Text, 140))
			}
			if rest := leaf.Count - len(shown); rest > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "    (+%d more — drill with --path %s)\n", rest, leaf.Label)
			}
		}
	}
}

func newFactsTreeCommand(opts Options) *cobra.Command {
	var (
		branch     string
		path       string
		depth      int
		leaves     int
		includeAll bool
		scope      string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "tree",
		Short: "Show facts as a navigable hierarchy with progressive disclosure",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateScopeFlag(scope); err != nil {
				return err
			}
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			facts, err := loadFacts(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			facts = filterFactsByScope(facts, scope)
			tree := buildFactTree(facts, includeAll)
			if path != "" {
				if pruned := filterTreeByPath(tree, path); pruned != nil {
					tree = pruned
				} else {
					tree = &factTreeNode{Label: "facts"}
				}
			}
			if jsonOut {
				return writeJSON(cmd, tree)
			}
			if tree.Count == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no facts on %s%s\n", resolvedBranch, pathSuffix(path))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d facts on %s%s\n", tree.Count, resolvedBranch, pathSuffix(path))
			renderFactTree(cmd, tree, depth, leaves)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to outline (default: current branch)")
	cmd.Flags().StringVar(&path, "path", "", "Drill into a category or path prefix (e.g. constraints or preferences.coding)")
	cmd.Flags().IntVar(&depth, "depth", 3, "Outline depth: 1=categories, 2=+paths, 3=+sample facts")
	cmd.Flags().IntVar(&leaves, "leaves", 3, "Sample facts to show per path at depth 3")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include superseded and retracted facts")
	cmd.Flags().StringVar(&scope, "scope", "", "Restrict to 'local' (code/subsystem) or 'cross-cutting' (preferences/workflow) facts")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the tree as JSON")
	return cmd
}

func pathSuffix(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return " under " + path
}
