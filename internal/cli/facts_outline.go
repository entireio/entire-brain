package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// facts_outline.go builds the Appendix-D "synthesized hierarchical outline": a
// living, locus-tiered map of the durable facts where each node carries a
// concise rollup summary of its subtree and leaves are atomic facts. The tree
// structure is deterministic (no agent) so `facts map` is useful immediately;
// the summaries are agent-generated and incremental — only nodes whose subtree
// fingerprint changed are re-summarized, so a refresh is cheap.

const factsOutlineFileName = "outline.json"

// factOutlineNode is one tier in the outline. Key is a slash-joined directory
// path ("" is the root); leaves are the facts homed directly at this tier and
// children are the immediate sub-tiers.
type factOutlineNode struct {
	Key         string         `json:"key"`
	Label       string         `json:"label"`
	Summary     string         `json:"summary,omitempty"`
	Fingerprint string         `json:"fingerprint"`
	Children    []string       `json:"children,omitempty"`
	LeafFactIDs []string       `json:"leaf_fact_ids,omitempty"`
	Counts      map[string]int `json:"counts,omitempty"`
}

// factOutline is the persisted artifact (facts/<branch>/outline.json). Nodes are
// keyed by their Key for O(1) incremental lookup; Root is always "".
type factOutline struct {
	GeneratedAt time.Time                  `json:"generated_at"`
	Branch      string                     `json:"branch"`
	Root        string                     `json:"root"`
	Nodes       map[string]factOutlineNode `json:"nodes"`
}

func factsOutlineRelPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), factsOutlineFileName))
}

// parentKey returns the parent tier of a node key ("internal/cli" -> "internal"
// -> ""). The root ("") is its own parent and is handled by callers.
func parentKey(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return ""
}

// outlineLabel is the human display label for a node key: "root" for the root,
// otherwise the last path segment.
func outlineLabel(key string) string {
	if key == "" {
		return "root"
	}
	return filepath.Base(key)
}

// factHomeKey returns the tier a fact lives at: the directory of the deepest
// real source-file path named in its locus, or "" (root) for a cross-cutting
// fact or one with no file-path locus. Only genuine `dir/name.ext` paths tier a
// fact — slash-bearing prose tokens (`allow/deny`, `agent/model`) do not — so
// the outline mirrors the code layout rather than fragmenting into noise nodes.
func factHomeKey(f factRecord) string {
	if factScope(f.Paths) == factScopeCrossCutting {
		return ""
	}
	best := ""
	for _, tok := range factLocusOf(f) {
		if !looksLikeSourcePath(tok) {
			continue
		}
		dir := tok[:strings.LastIndexByte(tok, '/')]
		// Prefer the deepest (most specific) directory among the fact's paths.
		if best == "" || strings.Count(dir, "/") > strings.Count(best, "/") {
			best = dir
		}
	}
	return best
}

// looksLikeSourcePath reports whether a locus token is a real source-file path
// (`some/dir/name.ext` with a short lowercase extension), distinguishing it from
// a slash-bearing prose fragment.
func looksLikeSourcePath(tok string) bool {
	// Reject absolute/relative/home-anchored and drive-lettered paths — a tiered
	// home should be a repo-relative source directory, not "..", "~/…", or "d:/…".
	if strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "..") || strings.HasPrefix(tok, "~") || strings.ContainsAny(tok, ": ") {
		return false
	}
	i := strings.LastIndexByte(tok, '/')
	if i <= 0 {
		return false
	}
	base := tok[i+1:]
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return false
	}
	ext := base[dot+1:]
	if len(ext) < 1 || len(ext) > 5 {
		return false
	}
	for _, r := range ext {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// buildFactOutlineTree builds the deterministic outline structure (no summaries)
// from a branch's facts: every active fact is homed at its tier, ancestor tiers
// are materialized, children are wired, and per-node by-kind counts recorded.
// Fingerprints are computed bottom-up so any leaf or structural change beneath a
// node changes that node's fingerprint (driving incremental re-summarization).
func buildFactOutlineTree(branch string, facts []factRecord, now time.Time) factOutline {
	nodes := map[string]*factOutlineNode{}
	ensure := func(key string) *factOutlineNode {
		if n, ok := nodes[key]; ok {
			return n
		}
		n := &factOutlineNode{Key: key, Label: outlineLabel(key), Counts: map[string]int{}}
		nodes[key] = n
		return n
	}
	ensure("") // root always exists
	for _, f := range facts {
		if f.Status != factStatusActive {
			continue
		}
		key := factHomeKey(f)
		ensure(key)
		for anc := key; anc != ""; anc = parentKey(anc) {
			ensure(parentKey(anc))
		}
		n := nodes[key]
		n.LeafFactIDs = append(n.LeafFactIDs, f.ID)
		n.Counts[factKindOrInferred(f)]++
	}
	// Wire children and sort everything for determinism.
	for key := range nodes {
		if key == "" {
			continue
		}
		p := nodes[parentKey(key)]
		p.Children = append(p.Children, key)
	}
	for _, n := range nodes {
		sort.Strings(n.Children)
		sort.Strings(n.LeafFactIDs)
		if len(n.Counts) == 0 {
			n.Counts = nil
		}
	}
	// Bottom-up fingerprints: a node hashes its leaf ids + each child's
	// fingerprint, so a change anywhere in the subtree propagates to the root.
	var fingerprint func(key string) string
	fingerprint = func(key string) string {
		n := nodes[key]
		h := sha256.New()
		for _, id := range n.LeafFactIDs {
			h.Write([]byte(id))
			h.Write([]byte{0})
		}
		for _, c := range n.Children {
			h.Write([]byte(c))
			h.Write([]byte{1})
			h.Write([]byte(fingerprint(c)))
		}
		sum := hex.EncodeToString(h.Sum(nil)[:12])
		n.Fingerprint = sum
		return sum
	}
	fingerprint("")

	out := factOutline{GeneratedAt: now, Branch: branch, Root: "", Nodes: make(map[string]factOutlineNode, len(nodes))}
	for key, n := range nodes {
		out.Nodes[key] = *n
	}
	return out
}

// subtreeFactCount returns the number of facts at and beneath a node.
func subtreeFactCount(outline factOutline, key string) int {
	n, ok := outline.Nodes[key]
	if !ok {
		return 0
	}
	total := len(n.LeafFactIDs)
	for _, c := range n.Children {
		total += subtreeFactCount(outline, c)
	}
	return total
}

func loadFactOutline(brainDir, branch string) (factOutline, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(factsOutlineRelPath(branch)))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return factOutline{}, nil
		}
		return factOutline{}, err
	}
	var outline factOutline
	if err := json.Unmarshal(data, &outline); err != nil {
		return factOutline{}, fmt.Errorf("parse %s: %w", factsOutlineFileName, err)
	}
	if outline.Nodes == nil {
		outline.Nodes = map[string]factOutlineNode{}
	}
	return outline, nil
}

func writeFactOutline(brainDir, branch string, outline factOutline) error {
	rel := factsOutlineRelPath(branch)
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
		return err
	}
	data, err := json.MarshalIndent(outline, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

type outlineGenOptions struct {
	agent        string
	agentCommand []string
	model        string
	effort       string
	budget       int // max agent calls per run (0 = unlimited)
	minFacts     int // skip nodes whose subtree has fewer facts than this
	force        bool
	run          distillAgentRunner
}

// outlinePostOrder lists node keys children-before-parents, so a parent's
// summary can roll up its already-summarized children.
func outlinePostOrder(outline factOutline) []string {
	var order []string
	var visit func(key string)
	visit = func(key string) {
		n, ok := outline.Nodes[key]
		if !ok {
			return
		}
		for _, c := range n.Children {
			visit(c)
		}
		order = append(order, key)
	}
	visit(outline.Root)
	return order
}

// outlineNodeInput renders the agent input for one node: its own leaf facts plus
// the already-generated summaries of its sub-areas, so the rollup reflects the
// whole subtree without resending every descendant fact.
func outlineNodeInput(outline factOutline, key string, byID map[string]factRecord) string {
	n := outline.Nodes[key]
	var b strings.Builder
	if len(n.LeafFactIDs) > 0 {
		fmt.Fprintf(&b, "Facts at %s:\n", outlineDisplayKey(key))
		shown := 0
		for _, id := range n.LeafFactIDs {
			if shown >= 30 { // bound the prompt on a hot node
				break
			}
			if f, ok := byID[id]; ok {
				fmt.Fprintf(&b, "- %s\n", f.Text)
				shown++
			}
		}
	}
	var subs []string
	for _, c := range n.Children {
		if cs := outline.Nodes[c].Summary; cs != "" {
			subs = append(subs, fmt.Sprintf("- %s: %s", outline.Nodes[c].Label, cs))
		}
	}
	if len(subs) > 0 {
		b.WriteString("Sub-areas:\n")
		b.WriteString(strings.Join(subs, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func outlineDisplayKey(key string) string {
	if key == "" {
		return "the repository root (cross-cutting facts)"
	}
	return key
}

// outlineSummaryPrompt instructs the agent to roll a node's facts into one
// concise sentence. Like the other agent prompts it must not begin with a dash.
func outlineSummaryPrompt(label string) string {
	return "Summarize, in ONE concise sentence (no preamble, no list), what the durable repository facts on stdin collectively convey about \"" + label + "\". " +
		"Capture the through-line a returning engineer needs, not a restatement of each fact. Output only the sentence."
}

// outlineCleanSummary extracts a single-sentence summary from the agent output:
// the first non-empty line, de-bulleted and length-capped.
func outlineCleanSummary(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*• ")
		if line != "" {
			return truncateString(line, 280)
		}
	}
	return ""
}

// generateFactOutline builds the structure and fills node summaries with the
// agent, reusing prior summaries for unchanged subtrees (incremental). It is
// best-effort: a failed agent call leaves that node's summary empty (the
// structure still renders) and never aborts the run. Returns the outline and the
// number of summaries (re)generated this run.
func generateFactOutline(ctx context.Context, repoDir, brainDir, branch string, facts []factRecord, opts outlineGenOptions, now time.Time) (factOutline, int, error) {
	prev, _ := loadFactOutline(brainDir, branch)
	tree := buildFactOutlineTree(branch, facts, now)
	byID := make(map[string]factRecord, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	run := opts.run
	if run == nil {
		run = execDistillAgent
	}
	agentAvailable := opts.agent != "" && opts.agent != "none"
	calls, regenerated := 0, 0
	for _, key := range outlinePostOrder(tree) {
		n := tree.Nodes[key]
		if subtreeFactCount(tree, key) < opts.minFacts {
			continue
		}
		if !opts.force {
			if old, ok := prev.Nodes[key]; ok && old.Fingerprint == n.Fingerprint && old.Summary != "" {
				n.Summary = old.Summary
				tree.Nodes[key] = n
				continue
			}
		}
		// Without an agent, keep the deterministic structure and any carried-over
		// summaries, but generate no new ones (no tokens spent).
		if !agentAvailable {
			continue
		}
		if opts.budget > 0 && calls >= opts.budget {
			continue // out of budget; leave the summary empty
		}
		input := outlineNodeInput(tree, key, byID)
		if strings.TrimSpace(input) == "" {
			continue
		}
		args, err := distillAgentCommandArgs(opts.agent, opts.agentCommand, outlineSummaryPrompt(n.Label))
		if err != nil {
			return tree, regenerated, err
		}
		args = injectAgentEffort(injectAgentModel(args, opts.agent, opts.model), opts.agent, opts.effort)
		out, runErr := run(ctx, repoDir, args, []byte(input), defaultDistillTimeout)
		calls++
		if runErr != nil {
			continue // best-effort
		}
		if s := outlineCleanSummary(out); s != "" {
			n.Summary = s
			regenerated++
		}
		tree.Nodes[key] = n
	}
	return tree, regenerated, nil
}

// renderFactOutline writes the outline as an indented, summary-first map with
// progressive disclosure: rooted at `path` (a node key, "" = whole tree) and
// limited to `depth` levels below it. Each line is "<label> — <summary> (N
// facts)[ kinds]" so a human orients top-down and drills in with --path.
func renderFactOutline(w *strings.Builder, outline factOutline, path string, depth int) {
	root := path
	if _, ok := outline.Nodes[root]; !ok {
		return
	}
	var walk func(key string, level int)
	walk = func(key string, level int) {
		if depth > 0 && level > depth {
			return
		}
		n := outline.Nodes[key]
		indent := strings.Repeat("  ", level)
		total := subtreeFactCount(outline, key)
		line := fmt.Sprintf("%s%s", indent, n.Label)
		if n.Summary != "" {
			line += " — " + n.Summary
		}
		line += fmt.Sprintf(" (%d facts)", total)
		w.WriteString(line + "\n")
		for _, c := range n.Children {
			walk(c, level+1)
		}
	}
	walk(root, 0)
}
