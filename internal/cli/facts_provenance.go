package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
)

// RC.M3 — the provenance graph.
//
// Recall answers "what do you know about X". Neither recall nor any other
// surface answers "where did this piece of reasoning come from, what
// disagrees with it, and what replaced it" — the lineage questions. The
// material was all there (anchors address the checkpoint stream; RC.M1 added
// typed record-to-record edges) with nothing to walk it.
//
// Two deliberate non-goals:
//
// It records NO vitality receipt. Serving a memory here would count as a
// serve, and the reasoning-capture exit test is "a memory served on more than
// one distinct day" — so an inspection surface that logged serves would move
// the metric by being run. That is the same circularity that made ranking
// served memories higher a closed negative; a tool must not improve the
// number it exists to explain.
//
// It is branch-scoped, like every other fact read, but it does NOT report a
// bare "not found": facts are partitioned per branch and a branch file is a
// fork-time copy, so the overwhelmingly likely cause of a miss is the wrong
// partition rather than a missing record. It says which branch holds the id.

const factsProvenanceSchemaVersion = 1

// Node types. A memory id is already namespaced ("fact:..."), and the
// checkpoint-stream ids are namespaced here so one id space addresses the
// whole graph and an edge never has to say what kind of thing it points at.
const (
	provNodeMemory     = "memory"
	provNodeSession    = "session"
	provNodeCommit     = "commit"
	provNodeCheckpoint = "checkpoint"
)

// Edge types. The three relation types come straight from the record; the
// supersede pair is derived from Record.SupersededBy in both directions,
// because the reverse direction ("what did this replace") is the one a reader
// chasing lineage actually asks and no field holds it.
const (
	provEdgeSupersedes   = "supersedes"
	provEdgeSupersededBy = "superseded-by"
	provEdgeAnchoredIn   = "anchored-in"
)

// provenanceNodeCap bounds the walk. Same-path facts form cliques — the
// promote path cross-links every conflicting pair — so a contradicts
// traversal is quadratic in a dense neighbourhood. When the cap fires the
// graph is PARTIAL and says so, following this repo's rule that a truncated
// scan may never be reported as a complete one.
const provenanceNodeCap = 400

type provenanceNode struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Depth int    `json:"depth"`
	// Memory-only fields.
	Kind   string `json:"kind,omitempty"`
	Status string `json:"status,omitempty"`
	Origin string `json:"origin,omitempty"`
	// Branch is the branch this memory was recorded on. The walk spans
	// branches (the store is flat per repository), so this is the only place
	// that tells a reader where a node in the chain came from; the graph's
	// own Branch field says only where the walk ran.
	Branch     string   `json:"branch,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	Locus      []string `json:"locus,omitempty"`
	Text       string   `json:"text,omitempty"`
	Transcript string   `json:"transcript,omitempty"`
	Line       int      `json:"line,omitempty"`
}

type provenanceEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

type provenanceGraph struct {
	SchemaVersion int              `json:"schema_version"`
	Branch        string           `json:"branch"`
	Roots         []string         `json:"roots"`
	Depth         int              `json:"depth"`
	Nodes         []provenanceNode `json:"nodes"`
	Edges         []provenanceEdge `json:"edges"`
	// Truncated reports that the node cap stopped the walk, so the absence of
	// an edge here is not evidence that none exists.
	Truncated bool     `json:"truncated"`
	Warnings  []string `json:"warnings,omitempty"`
}

// MemoryCount reports how many memory nodes the graph holds, which is the
// number a human summary should lead with: the checkpoint-stream nodes are
// citations, not reasoning.
func (g provenanceGraph) MemoryCount() int {
	n := 0
	for _, node := range g.Nodes {
		if node.Type == provNodeMemory {
			n++
		}
	}
	return n
}

// buildProvenanceGraph walks out from roots over memory-to-memory edges to
// depth hops, attaching every included memory's checkpoint-stream anchors.
//
// Superseded and retracted records ARE traversed and included. Recall hides
// them, which is exactly why lineage needs its own surface: "what replaced
// this, and what did it replace" is unanswerable from the active set alone.
func buildProvenanceGraph(records []factmerge.Record, branch string, roots []string, depth int) provenanceGraph {
	graph := provenanceGraph{
		SchemaVersion: factsProvenanceSchemaVersion,
		Branch:        branch,
		Roots:         roots,
		Depth:         depth,
	}

	byID := make(map[string]factmerge.Record, len(records))
	for _, record := range records {
		byID[record.ID] = record
	}
	// The reverse supersede index. Record.SupersededBy points forward only.
	supersedes := map[string][]string{}
	for _, record := range records {
		if record.SupersededBy != "" {
			supersedes[record.SupersededBy] = append(supersedes[record.SupersededBy], record.ID)
		}
	}
	for target := range supersedes {
		sort.Strings(supersedes[target])
	}

	seen := map[string]int{}
	edgeSeen := map[string]struct{}{}
	addEdge := func(from, to, typ string) {
		key := from + "\x00" + to + "\x00" + typ
		if _, ok := edgeSeen[key]; ok {
			return
		}
		edgeSeen[key] = struct{}{}
		graph.Edges = append(graph.Edges, provenanceEdge{From: from, To: to, Type: typ})
	}

	type queued struct {
		id    string
		depth int
	}
	var queue []queued
	for _, root := range roots {
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = 0
		queue = append(queue, queued{id: root, depth: 0})
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		record, ok := byID[current.id]
		if !ok {
			// A dangling edge target: an id referenced by a relation whose
			// record is not on this branch. Recorded as a node so the edge
			// does not point at nothing, and marked by having no fields.
			graph.Nodes = append(graph.Nodes, provenanceNode{ID: current.id, Type: provNodeMemory, Depth: current.depth})
			continue
		}

		graph.Nodes = append(graph.Nodes, provenanceNode{
			ID:     record.ID,
			Type:   provNodeMemory,
			Depth:  current.depth,
			Kind:   record.Kind,
			Status: record.Status,
			Origin: record.Origin,
			Branch: record.Branch,
			Paths:  record.Paths,
			Locus:  record.Locus,
			Text:   record.Text,
		})

		// Anchors are attached at every depth: a memory's citation is part of
		// the memory, not a further hop.
		for _, anchor := range record.Provenance {
			for _, pair := range []struct{ typ, value string }{
				{provNodeSession, anchor.SessionID},
				{provNodeCommit, anchor.Commit},
				{provNodeCheckpoint, anchor.CheckpointID},
			} {
				value := strings.TrimSpace(pair.value)
				if value == "" {
					continue
				}
				id := pair.typ + ":" + value
				if _, ok := seen[id]; !ok {
					seen[id] = current.depth
					node := provenanceNode{ID: id, Type: pair.typ, Depth: current.depth}
					if pair.typ == provNodeSession {
						node.Transcript = anchor.Transcript
						node.Line = anchor.Line
					}
					graph.Nodes = append(graph.Nodes, node)
				}
				addEdge(record.ID, id, provEdgeAnchoredIn)
			}
		}

		if current.depth >= depth {
			continue
		}

		for _, next := range provenanceNeighbours(record, supersedes[record.ID]) {
			addEdge(record.ID, next.id, next.typ)
			if _, ok := seen[next.id]; ok {
				continue
			}
			if len(seen) >= provenanceNodeCap {
				graph.Truncated = true
				continue
			}
			seen[next.id] = current.depth + 1
			queue = append(queue, queued{id: next.id, depth: current.depth + 1})
		}
	}

	if graph.Truncated {
		graph.Warnings = append(graph.Warnings,
			fmt.Sprintf("graph stopped at the %d-node cap: it is partial, and a missing edge here is not evidence that none exists", provenanceNodeCap))
	}
	return graph
}

type provenanceNeighbour struct {
	id  string
	typ string
}

// provenanceNeighbours returns the memory-to-memory edges leaving a record, in
// a deterministic order so two runs over the same store render identically.
//
// RelatedIDs is NOT walked. Every writer of it also writes the typed
// contradicts edge (factmerge.AddConflictLink), so walking both would
// duplicate each conflict under two names; and a RelatedIDs entry written by
// an older build carries no type, so rendering it as a typed edge would
// invent a claim the writer never made. The untyped bag stays a compatibility
// mirror.
func provenanceNeighbours(record factmerge.Record, supersedes []string) []provenanceNeighbour {
	var out []provenanceNeighbour
	for _, relation := range record.Relations {
		if strings.TrimSpace(relation.TargetID) == "" {
			continue
		}
		out = append(out, provenanceNeighbour{id: relation.TargetID, typ: relation.Type})
	}
	if record.SupersededBy != "" {
		out = append(out, provenanceNeighbour{id: record.SupersededBy, typ: provEdgeSupersededBy})
	}
	for _, earlier := range supersedes {
		out = append(out, provenanceNeighbour{id: earlier, typ: provEdgeSupersedes})
	}
	return out
}

// memoriesAnchoredAt returns the ids of every memory whose provenance cites
// the given session, commit or checkpoint. This is the reverse query, and the
// one that makes reasoning capture inspectable: it answers "what did this
// session establish" rather than "what do we know about X".
func memoriesAnchoredAt(records []factmerge.Record, nodeType, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var ids []string
	for _, record := range records {
		for _, anchor := range record.Provenance {
			var candidate string
			switch nodeType {
			case provNodeSession:
				candidate = anchor.SessionID
			case provNodeCommit:
				candidate = anchor.Commit
			case provNodeCheckpoint:
				candidate = anchor.CheckpointID
			}
			if candidate == "" {
				continue
			}
			// Commits are matched by prefix so an abbreviated sha works, the
			// way every other git-facing surface accepts one. Sessions and
			// checkpoints are matched exactly: they are opaque ids, and a
			// prefix match on an opaque id silently widens the answer.
			matched := candidate == value
			if !matched && nodeType == provNodeCommit && len(value) >= 7 && strings.HasPrefix(candidate, value) {
				matched = true
			}
			if matched {
				ids = append(ids, record.ID)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// renderProvenanceGraph prints the graph depth-first from each root, so a
// reader follows one lineage at a time instead of reading a flat edge list.
func renderProvenanceGraph(out *strings.Builder, graph provenanceGraph) {
	nodes := make(map[string]provenanceNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	outgoing := map[string][]provenanceEdge{}
	for _, edge := range graph.Edges {
		outgoing[edge.From] = append(outgoing[edge.From], edge)
	}

	fmt.Fprintf(out, "provenance on %s: %d memory node(s), %d edge(s)",
		graph.Branch, graph.MemoryCount(), len(graph.Edges))
	if graph.Truncated {
		out.WriteString(" [PARTIAL]")
	}
	out.WriteString("\n")

	printed := map[string]struct{}{}
	var walk func(id string, indent int)
	walk = func(id string, indent int) {
		if _, ok := printed[id]; ok {
			fmt.Fprintf(out, "%s%s (already shown)\n", strings.Repeat("  ", indent+1), id)
			return
		}
		printed[id] = struct{}{}
		node := nodes[id]
		pad := strings.Repeat("  ", indent+1)
		switch node.Type {
		case provNodeMemory:
			if node.Text == "" {
				fmt.Fprintf(out, "%s%s (not on this branch)\n", pad, id)
			} else {
				fmt.Fprintf(out, "%s%s [%s/%s] %s\n", pad, id, node.Kind, node.Status, provenanceSummaryLine(node.Text, 110))
			}
		default:
			fmt.Fprintf(out, "%s%s\n", pad, id)
		}
		for _, edge := range outgoing[id] {
			fmt.Fprintf(out, "%s  --%s-->\n", pad, edge.Type)
			walk(edge.To, indent+2)
		}
	}
	for _, root := range graph.Roots {
		walk(root, 0)
	}
	for _, warning := range graph.Warnings {
		fmt.Fprintf(out, "  note: %s\n", warning)
	}
}

// provenanceSummaryLine collapses a memory's text to one line for the human
// rendering. It is rune-safe for the same reason the review truncation is:
// memory text may be any encoding the repository holds.
func provenanceSummaryLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
