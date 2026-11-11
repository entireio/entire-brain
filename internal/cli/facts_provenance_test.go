package cli

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
)

func provRecord(text, path, kind, status string, anchors ...factmerge.Anchor) factmerge.Record {
	paths := []string{path}
	if len(anchors) == 0 {
		anchors = []factmerge.Anchor{{SessionID: "sess-a"}}
	}
	return factmerge.Record{
		ID:         factmerge.RecordID(text, paths),
		Paths:      paths,
		Kind:       kind,
		Text:       text,
		Branch:     "main",
		Origin:     factmerge.OriginAuthored,
		Status:     status,
		Provenance: anchors,
		CreatedAt:  time.Unix(0, 0).UTC(),
		UpdatedAt:  time.Unix(0, 0).UTC(),
	}
}

func edgeTypesBetween(graph provenanceGraph, from, to string) []string {
	var types []string
	for _, edge := range graph.Edges {
		if edge.From == from && edge.To == to {
			types = append(types, edge.Type)
		}
	}
	return types
}

func nodeByID(graph provenanceGraph, id string) (provenanceNode, bool) {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return provenanceNode{}, false
}

// The lineage question recall cannot answer: a superseded record and what
// replaced it, reachable in both directions.
func TestProvenanceWalksTheSupersedeChainBothWays(t *testing.T) {
	t.Parallel()

	old := provRecord("the daemon reads the manifest first", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusSuperseded)
	new := provRecord("the daemon takes the lock first", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	old.SupersededBy = new.ID
	records := []factmerge.Record{old, new}

	// Forward: from the retired record to its replacement.
	forward := buildProvenanceGraph(records, "main", []string{old.ID}, 2)
	if got := edgeTypesBetween(forward, old.ID, new.ID); len(got) != 1 || got[0] != provEdgeSupersededBy {
		t.Fatalf("want a superseded-by edge from the retired record, got %v", got)
	}

	// Reverse: from the live record back to what it replaced. No field holds
	// this direction, so it is the one a reader cannot get without the graph.
	reverse := buildProvenanceGraph(records, "main", []string{new.ID}, 2)
	if got := edgeTypesBetween(reverse, new.ID, old.ID); len(got) != 1 || got[0] != provEdgeSupersedes {
		t.Fatalf("want a supersedes edge back to the replaced record, got %v", got)
	}

	// A superseded record must appear with its status, not be filtered out
	// the way recall filters it.
	node, ok := nodeByID(reverse, old.ID)
	if !ok {
		t.Fatal("the superseded record is missing from the graph")
	}
	if node.Status != factmerge.StatusSuperseded {
		t.Errorf("status = %q, want superseded: hiding it is what recall does and why this surface exists", node.Status)
	}
}

func TestProvenanceCarriesTypedRelationsAsEdges(t *testing.T) {
	t.Parallel()

	a := provRecord("the lock is taken before the read", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	b := provRecord("the read happens before the lock", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	a = factmerge.AddConflictLink(a, b.ID)
	b = factmerge.AddConflictLink(b, a.ID)
	a.Relations = factmerge.AppendRelation(a.Relations, "fact:elsewhere", factmerge.RelationDerivedFrom)

	graph := buildProvenanceGraph([]factmerge.Record{a, b}, "main", []string{a.ID}, 1)

	if got := edgeTypesBetween(graph, a.ID, b.ID); len(got) != 1 || got[0] != factmerge.RelationContradicts {
		t.Errorf("want one contradicts edge, got %v", got)
	}
	if got := edgeTypesBetween(graph, a.ID, "fact:elsewhere"); len(got) != 1 || got[0] != factmerge.RelationDerivedFrom {
		t.Errorf("want the derived-from edge, got %v", got)
	}
}

// RelatedIDs mirrors every typed contradicts edge, so walking both would
// render each conflict twice; and an entry written by an older build carries
// no type, so showing it as a typed edge would invent a claim.
func TestProvenanceDoesNotWalkTheUntypedMirror(t *testing.T) {
	t.Parallel()

	a := provRecord("the lock is taken before the read", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	// A record as an older build would have left it: the bag, no typed edge.
	a.RelatedIDs = []string{"fact:legacypeer"}

	graph := buildProvenanceGraph([]factmerge.Record{a}, "main", []string{a.ID}, 2)

	for _, edge := range graph.Edges {
		if edge.To == "fact:legacypeer" {
			t.Fatalf("an untyped RelatedIDs entry was rendered as a %q edge, inventing a claim its writer never made", edge.Type)
		}
	}
}

// A memory's citation is part of the memory, so anchors attach at every depth
// rather than consuming a hop.
func TestProvenanceAttachesCheckpointStreamAnchors(t *testing.T) {
	t.Parallel()

	record := provRecord("the daemon reads the manifest first", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive,
		factmerge.Anchor{SessionID: "sess-1", Commit: "abc123", CheckpointID: "ck-9", Transcript: "sessions/main/x.jsonl", Line: 42})

	graph := buildProvenanceGraph([]factmerge.Record{record}, "main", []string{record.ID}, 0)

	for _, want := range []string{"session:sess-1", "commit:abc123", "checkpoint:ck-9"} {
		node, ok := nodeByID(graph, want)
		if !ok {
			t.Errorf("anchor node %s missing", want)
			continue
		}
		if got := edgeTypesBetween(graph, record.ID, want); len(got) != 1 || got[0] != provEdgeAnchoredIn {
			t.Errorf("want one anchored-in edge to %s, got %v", want, got)
		}
		if want == "session:sess-1" && (node.Transcript == "" || node.Line != 42) {
			t.Errorf("the session node must carry the transcript and line: %+v", node)
		}
	}
	// Depth 0 means no memory-to-memory hops, but anchors must still appear.
	if graph.MemoryCount() != 1 {
		t.Errorf("memory count = %d, want 1", graph.MemoryCount())
	}
}

func TestProvenanceDepthBoundsTheWalk(t *testing.T) {
	t.Parallel()

	a := provRecord("claim a", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	b := provRecord("claim b", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	c := provRecord("claim c", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	a.Relations = factmerge.AppendRelation(a.Relations, b.ID, factmerge.RelationDerivedFrom)
	b.Relations = factmerge.AppendRelation(b.Relations, c.ID, factmerge.RelationDerivedFrom)
	records := []factmerge.Record{a, b, c}

	one := buildProvenanceGraph(records, "main", []string{a.ID}, 1)
	if _, ok := nodeByID(one, c.ID); ok {
		t.Error("depth 1 must not reach a two-hop record")
	}
	two := buildProvenanceGraph(records, "main", []string{a.ID}, 2)
	if _, ok := nodeByID(two, c.ID); !ok {
		t.Error("depth 2 must reach the two-hop record")
	}
}

// A cycle is the normal shape here: a conflict cross-link points both ways.
func TestProvenanceTerminatesOnACycle(t *testing.T) {
	t.Parallel()

	a := provRecord("claim a", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	b := provRecord("claim b", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	a = factmerge.AddConflictLink(a, b.ID)
	b = factmerge.AddConflictLink(b, a.ID)

	done := make(chan provenanceGraph, 1)
	go func() {
		done <- buildProvenanceGraph([]factmerge.Record{a, b}, "main", []string{a.ID}, 50)
	}()
	select {
	case graph := <-done:
		if graph.MemoryCount() != 2 {
			t.Errorf("a two-record cycle must yield two memory nodes, got %d", graph.MemoryCount())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the walk did not terminate on a cycle")
	}
}

// An id referenced by a relation whose record is on another branch must still
// appear, or the edge points at nothing and the reader cannot tell a missing
// record from a rendering bug.
func TestProvenanceKeepsDanglingTargetsVisible(t *testing.T) {
	t.Parallel()

	a := provRecord("claim a", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
	a.Relations = factmerge.AppendRelation(a.Relations, "fact:onanotherbranch", factmerge.RelationDerivedFrom)

	graph := buildProvenanceGraph([]factmerge.Record{a}, "main", []string{a.ID}, 2)
	node, ok := nodeByID(graph, "fact:onanotherbranch")
	if !ok {
		t.Fatal("the dangling target must still be a node")
	}
	if node.Text != "" || node.Kind != "" {
		t.Errorf("a record not on this branch must carry no content: %+v", node)
	}

	var out strings.Builder
	renderProvenanceGraph(&out, graph)
	if !strings.Contains(out.String(), "not on this branch") {
		t.Errorf("the rendering must say why the node is empty:\n%s", out.String())
	}
}

// The reverse query is what makes reasoning capture inspectable: "what did
// this session establish", not "what do we know about X".
func TestMemoriesAnchoredAtFindsEveryMemoryForASource(t *testing.T) {
	t.Parallel()

	one := provRecord("claim one", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive,
		factmerge.Anchor{SessionID: "sess-1", Commit: "abcdef1234"})
	two := provRecord("claim two", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive,
		factmerge.Anchor{SessionID: "sess-1"})
	three := provRecord("claim three", "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive,
		factmerge.Anchor{SessionID: "sess-2"})
	records := []factmerge.Record{one, two, three}

	if got := memoriesAnchoredAt(records, provNodeSession, "sess-1"); len(got) != 2 {
		t.Errorf("want both memories from sess-1, got %v", got)
	}
	if got := memoriesAnchoredAt(records, provNodeSession, "sess"); len(got) != 0 {
		t.Errorf("a session id is opaque, so a prefix must NOT match and silently widen the answer: %v", got)
	}
	// A commit may be abbreviated, the way every other git-facing surface
	// accepts one.
	if got := memoriesAnchoredAt(records, provNodeCommit, "abcdef1"); len(got) != 1 {
		t.Errorf("an abbreviated commit must match: %v", got)
	}
	if got := memoriesAnchoredAt(records, provNodeCommit, "abcde"); len(got) != 0 {
		t.Errorf("a sha shorter than git's own minimum must not match: %v", got)
	}
}

// A memory's ancestor may have been recorded on a different branch, and it is
// still its ancestor. The store is flat per repository now, so the walk is
// repository-wide; this is the test that fails if a branch filter is ever
// reintroduced into the lineage read, which would answer "nothing replaced
// this" when something did.
func TestProvenanceWalksAcrossBranches(t *testing.T) {
	t.Parallel()

	older := provRecord("the daemon polled for completion", "architecture.daemon.flow", factmerge.KindDecision, factmerge.StatusSuperseded)
	older.Branch = "feature/suhaan"
	newer := provRecord("the daemon waits on the child", "architecture.daemon.flow", factmerge.KindDecision, factmerge.StatusActive)
	newer.Branch = "main"
	older.SupersededBy = newer.ID

	graph := buildProvenanceGraph([]factmerge.Record{older, newer}, "main", []string{newer.ID}, 2)

	if graph.MemoryCount() != 2 {
		t.Fatalf("the walk must reach a memory recorded on another branch: %d node(s)", graph.MemoryCount())
	}
	var sawSupersedes bool
	for _, edge := range graph.Edges {
		if edge.From == newer.ID && edge.To == older.ID && edge.Type == provEdgeSupersedes {
			sawSupersedes = true
		}
	}
	if !sawSupersedes {
		t.Errorf("the supersede edge must cross the branch boundary: %+v", graph.Edges)
	}
	// And each node still reports the branch it was recorded on, which is the
	// only place that information lives once the store is flat.
	branches := map[string]string{}
	for _, node := range graph.Nodes {
		branches[node.ID] = node.Branch
	}
	if branches[older.ID] != "feature/suhaan" || branches[newer.ID] != "main" {
		t.Errorf("per-node branch provenance lost: %v", branches)
	}
}

// A dense clique must not run unbounded, and a truncated graph must say so:
// the absence of an edge in a partial graph is not evidence that none exists.
func TestProvenanceCapMarksTheGraphPartial(t *testing.T) {
	t.Parallel()

	var records []factmerge.Record
	ids := make([]string, 0, provenanceNodeCap+50)
	for i := 0; i < provenanceNodeCap+50; i++ {
		r := provRecord("claim number "+strings.Repeat("x", i%7)+strconv.Itoa(i), "architecture.daemon.flow", factmerge.KindInvariant, factmerge.StatusActive)
		records = append(records, r)
		ids = append(ids, r.ID)
	}
	// Chain them so the walk has somewhere to go at every depth.
	for i := range records {
		if i+1 < len(records) {
			records[i].Relations = factmerge.AppendRelation(records[i].Relations, ids[i+1], factmerge.RelationDerivedFrom)
		}
	}

	graph := buildProvenanceGraph(records, "main", []string{ids[0]}, 10000)
	if !graph.Truncated {
		t.Fatalf("a %d-record chain walked to an unbounded depth must hit the %d-node cap", len(records), provenanceNodeCap)
	}
	if len(graph.Warnings) == 0 || !strings.Contains(graph.Warnings[0], "partial") {
		t.Errorf("a truncated graph must warn that it is partial: %v", graph.Warnings)
	}
	var out strings.Builder
	renderProvenanceGraph(&out, graph)
	if !strings.Contains(out.String(), "[PARTIAL]") {
		t.Errorf("the human rendering must mark a partial graph:\n%s", firstRunes(out.String(), 200))
	}
}

func TestProvenanceOptionsRefuseAmbiguousRequests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts factsProvenanceOptions
		want string
	}{
		{"nothing given", factsProvenanceOptions{}, "pass a memory id"},
		{"id and selector", factsProvenanceOptions{id: "fact:a", session: "s"}, "cannot both be given"},
		{"two selectors", factsProvenanceOptions{session: "s", commit: "c"}, "cannot be combined"},
		{"negative depth", factsProvenanceOptions{id: "fact:a", depth: -1}, "cannot be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.opts.validate()
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want %q in: %v", tc.want, err)
			}
		})
	}

	if err := (factsProvenanceOptions{id: "fact:a", depth: 2}).validate(); err != nil {
		t.Errorf("a plain id request must be accepted: %v", err)
	}
	if err := (factsProvenanceOptions{session: "s", depth: 0}).validate(); err != nil {
		t.Errorf("depth 0 is meaningful (anchors only) and must be accepted: %v", err)
	}
}

func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
