package factmerge

import (
	"reflect"
	"testing"
)

func TestBuildPossibleSameSubjectIsNeutralAndDeterministic(t *testing.T) {
	left := Record{ID: "fact:a", Branch: "main", Status: StatusActive, Kind: "invariant", Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/graph.go"}}
	right := Record{ID: "fact:b", Branch: "main", Status: StatusActive, Kind: "invariant", Paths: []string{"architecture.cache.policy"}, Locus: []string{"INTERNAL/CLI/GRAPH.GO", "ignored"}}
	owners := []RelationshipOwner{{CandidateID: "candidate:b", SourceSessionID: "session:2"}, {CandidateID: "candidate:a", SourceSessionID: "session:1"}, {CandidateID: "candidate:b", SourceSessionID: "session:2"}}
	got, ok, err := BuildPossibleSameSubject(left, right, owners)
	if err != nil || !ok {
		t.Fatalf("BuildPossibleSameSubject() = (%+v, %v, %v)", got, ok, err)
	}
	if got.Kind != RelationshipKindPossibleSameSubject || got.ID == "" || !reflect.DeepEqual(got.FactIDs, []string{"fact:a", "fact:b"}) {
		t.Fatalf("unexpected neutral relationship: %+v", got)
	}
	if got.Subject != (RelationshipSubject{Kind: "invariant", TopLevel: "architecture", StrongLocus: "internal/cli/graph.go"}) {
		t.Fatalf("subject = %+v", got.Subject)
	}
	wantOwners := []RelationshipOwner{{CandidateID: "candidate:a", SourceSessionID: "session:1"}, {CandidateID: "candidate:b", SourceSessionID: "session:2"}}
	if !reflect.DeepEqual(got.Owners, wantOwners) {
		t.Fatalf("owners = %+v, want %+v", got.Owners, wantOwners)
	}
	if err := ValidateRelationshipProposal(got); err != nil {
		t.Fatalf("ValidateRelationshipProposal() error = %v", err)
	}

	reversed, ok, err := BuildPossibleSameSubject(right, left, []RelationshipOwner{{CandidateID: "candidate:a", SourceSessionID: "session:1"}})
	if err != nil || !ok || reversed.ID != got.ID || !reflect.DeepEqual(reversed.FactIDs, got.FactIDs) {
		t.Fatalf("reversed build = (%+v, %v, %v), want same identity", reversed, ok, err)
	}
}

func TestBuildPossibleSameSubjectRejectsBadReconcileAndWeakBlocks(t *testing.T) {
	base := Record{ID: "fact:a", Branch: "main", Status: StatusActive, Kind: KindInvariant, Paths: []string{"architecture.data.flow"}}
	cases := []struct {
		name  string
		left  Record
		right Record
	}{
		{
			name:  "flag and repository key are not a subject",
			left:  base,
			right: Record{ID: "fact:b", Branch: "main", Status: StatusActive, Kind: "constraint", Paths: []string{"architecture.data.cache"}, Locus: []string{"repo-key"}},
		},
		{
			name:  "plain taxonomy-like locus is not strong",
			left:  base,
			right: Record{ID: "fact:b", Branch: "main", Status: StatusActive, Kind: "constraint", Paths: []string{"architecture.data.cache"}, Locus: []string{"architecture"}},
		},
		{
			name:  "different kind",
			left:  Record{ID: "fact:a", Branch: "main", Status: StatusActive, Kind: KindConvention, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/graph.go"}},
			right: Record{ID: "fact:b", Branch: "main", Status: StatusActive, Kind: "invariant", Paths: []string{"architecture.data.cache"}, Locus: []string{"internal/cli/graph.go"}},
		},
		{
			name:  "different top level",
			left:  Record{ID: "fact:a", Branch: "main", Status: StatusActive, Kind: KindInvariant, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/graph.go"}},
			right: Record{ID: "fact:b", Branch: "main", Status: StatusActive, Kind: KindInvariant, Paths: []string{"preferences.coding.style"}, Locus: []string{"internal/cli/graph.go"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.left.Locus == nil {
				tc.left.Locus = []string{"--no-network"}
			}
			if _, ok, err := BuildPossibleSameSubject(tc.left, tc.right, []RelationshipOwner{{CandidateID: "candidate", SourceSessionID: "session"}}); err != nil || ok {
				t.Fatalf("BuildPossibleSameSubject() ok=%v err=%v, want rejected", ok, err)
			}
		})
	}
}

func TestRelationshipOwnerLifecycleAndValidation(t *testing.T) {
	owners := UnionRelationshipOwners(
		[]RelationshipOwner{{CandidateID: "candidate:b", SourceSessionID: "session:2"}},
		[]RelationshipOwner{{CandidateID: "candidate:a", SourceSessionID: "session:1"}, {CandidateID: "candidate:b", SourceSessionID: "session:2"}, {CandidateID: "", SourceSessionID: "bad"}},
	)
	want := []RelationshipOwner{{CandidateID: "candidate:a", SourceSessionID: "session:1"}, {CandidateID: "candidate:b", SourceSessionID: "session:2"}}
	if !reflect.DeepEqual(owners, want) {
		t.Fatalf("UnionRelationshipOwners() = %+v, want %+v", owners, want)
	}
	remaining, removed := RemoveRelationshipOwner(owners, RelationshipOwner{CandidateID: "candidate:a", SourceSessionID: "session:1"})
	if !removed || !reflect.DeepEqual(remaining, want[1:]) {
		t.Fatalf("RemoveRelationshipOwner() = %+v, %v", remaining, removed)
	}
	if _, ok := StrongRelationshipLocus("--no-network"); ok {
		t.Fatal("flag was accepted as strong locus")
	}
	if got, ok := StrongRelationshipLocus("`RepositoryKey`"); !ok || got != "repositorykey" {
		t.Fatalf("quoted identifier = %q, %v", got, ok)
	}
	if got, ok := StrongRelationshipLocus("repositorykey"); !ok || got != "repositorykey" {
		t.Fatalf("normalized identifier = %q, %v", got, ok)
	}
	if _, ok := StrongRelationshipLocus("architecture"); ok {
		t.Fatal("generic taxonomy word was accepted as strong locus")
	}
}

func TestStrongRelationshipLocusForRecordRequiresCodeEvidenceForBareLoci(t *testing.T) {
	positive := []struct {
		name  string
		value string
		text  string
	}{
		{name: "camel case", value: "graphnodeid", text: "GraphNodeID is stable."},
		{name: "backtick", value: "repositorykey", text: "The `RepositoryKey` identifies the repository."},
		{name: "path", value: "internal/cli/graph.go", text: "The graph writer uses internal/cli/graph.go."},
		{name: "qualified", value: "settings.load", text: "settings.Load reads the persisted settings."},
		{name: "snake case", value: "graph_node_id", text: "graph_node_id is serialized in the snapshot."},
	}
	for _, tc := range positive {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := StrongRelationshipLocusForRecord(Record{Text: tc.text}, tc.value)
			if !ok || got != tc.value {
				t.Fatalf("StrongRelationshipLocusForRecord() = %q, %v; want %q, true", got, ok, tc.value)
			}
		})
	}

	negative := []struct {
		name  string
		value string
		text  string
	}{
		{name: "ordinary prose", value: "graphnodeid", text: "The graph node id is stable."},
		{name: "cache", value: "cache", text: "The cache is rebuilt on startup."},
		{name: "handler", value: "handler", text: "The handler serves requests."},
		{name: "config", value: "config", text: "The config is loaded at startup."},
		{name: "migration", value: "migration", text: "The migration runs once."},
		{name: "product name", value: "entire", text: "`entire graph index` rebuilds the snapshot."},
		{name: "untrusted locus", value: "repositorykey", text: "The repository key identifies the repository."},
	}
	for _, tc := range negative {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := StrongRelationshipLocusForRecord(Record{Text: tc.text}, tc.value); ok {
				t.Fatalf("StrongRelationshipLocusForRecord() = %q, true; want rejected", got)
			}
		})
	}
}

func TestBuildPossibleSameSubjectRejectsOrdinaryBareLociDespiteCoincidence(t *testing.T) {
	for _, locus := range []string{"cache", "handler", "config", "migration"} {
		t.Run(locus, func(t *testing.T) {
			left := Record{
				ID: "fact:left", Branch: "main", Status: StatusActive, Kind: KindInvariant,
				Paths: []string{"architecture.data.flow"}, Locus: []string{locus},
				Text: "The " + locus + " is refreshed during startup.",
			}
			right := left
			right.ID = "fact:right"
			if _, ok, err := BuildPossibleSameSubject(left, right, []RelationshipOwner{{CandidateID: "candidate", SourceSessionID: "session"}}); err != nil || ok {
				t.Fatalf("BuildPossibleSameSubject() ok=%v err=%v, want rejected", ok, err)
			}
		})
	}
}

func TestBuildPossibleSameSubjectAdmitsRecordProvenBareLocus(t *testing.T) {
	left := Record{
		ID: "fact:left", Branch: "main", Status: StatusActive, Kind: KindInvariant,
		Paths: []string{"architecture.data.flow"}, Locus: []string{"graphnodeid"},
		Text: "The writer validates GraphNodeID before storage.",
	}
	right := left
	right.ID = "fact:right"
	right.Text = "GraphNodeID remains stable across compaction."
	proposal, ok, err := BuildPossibleSameSubject(left, right, []RelationshipOwner{{CandidateID: "candidate", SourceSessionID: "session"}})
	if err != nil || !ok {
		t.Fatalf("BuildPossibleSameSubject() = (%+v, %v, %v), want admitted", proposal, ok, err)
	}
	if proposal.Subject.StrongLocus != "graphnodeid" {
		t.Fatalf("subject strong locus = %q", proposal.Subject.StrongLocus)
	}
}

func TestValidateRelationshipProposalAcceptsCanonicalNormalizedBareSubject(t *testing.T) {
	ids := []string{"fact:a", "fact:b"}
	subject := RelationshipSubject{Kind: KindInvariant, TopLevel: "architecture", StrongLocus: "graphnodeid"}
	id, err := RelationshipProposalID("main", ids, subject)
	if err != nil {
		t.Fatal(err)
	}
	proposal := RelationshipProposal{
		ID: id, Branch: "main", Kind: RelationshipKindPossibleSameSubject,
		FactIDs: ids, Subject: subject,
		Owners: []RelationshipOwner{{CandidateID: "candidate", SourceSessionID: "session"}},
	}
	if err := ValidateRelationshipProposal(proposal); err != nil {
		t.Fatalf("ValidateRelationshipProposal() error = %v", err)
	}
}
