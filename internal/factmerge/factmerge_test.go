package factmerge

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"
)

// factFor builds a canonical active fact for tests, mirroring the CLI helper of
// the same name but using only factmerge's exported surface.
func factFor(t *testing.T, text string, paths []string, now time.Time) Record {
	t.Helper()
	p := NormalizePaths(paths)
	return Record{
		ID:         RecordID(text, p),
		Paths:      p,
		Text:       text,
		Branch:     "main",
		Origin:     "distilled",
		Status:     StatusActive,
		Provenance: []Anchor{{SessionID: "s", Line: 1}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// knownTops returns a GC predicate whose known top-levels are the given set.
func knownTops(tops ...string) func(string) bool {
	set := map[string]struct{}{}
	for _, t := range tops {
		set[t] = struct{}{}
	}
	return func(path string) bool {
		top := path
		for i := 0; i < len(path); i++ {
			if path[i] == '.' {
				top = path[:i]
				break
			}
		}
		_, ok := set[top]
		return ok
	}
}

const testRetention = 30 * 24 * time.Hour

func TestRecordIDStableAcrossPathOrder(t *testing.T) {
	a := RecordID("Use tabs, not spaces", []string{"preferences.coding.style", "project.tooling.stack"})
	b := RecordID("Use tabs, not spaces", []string{"preferences.coding.style", "project.tooling.stack"})
	if a != b {
		t.Fatalf("id not deterministic: %s != %s", a, b)
	}
	c := RecordID("  use TABS, not spaces  ", []string{"preferences.coding.style", "project.tooling.stack"})
	if a != c {
		t.Fatalf("id should ignore casing/whitespace: %s != %s", a, c)
	}
	d := RecordID("Use spaces, not tabs", []string{"preferences.coding.style", "project.tooling.stack"})
	if a == d {
		t.Fatalf("distinct statements collided: %s", a)
	}
}

func TestRecordIDIgnoresKind(t *testing.T) {
	paths := NormalizePaths([]string{"constraints.invariants.general"})
	id := RecordID("the index is derived from the ndjson truth", paths)
	if id != RecordID("the index is derived from the ndjson truth", paths) {
		t.Fatal("id must be stable")
	}
	if id == "" {
		t.Fatal("id must be non-empty")
	}
}

func TestNormalizePaths(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"sorts and dedupes", []string{"project.tooling.stack", "architecture.data.flow", "project.tooling.stack"}, []string{"architecture.data.flow", "project.tooling.stack"}},
		{"drops malformed, lowercases the rest", []string{"too.few", "Architecture.Data.Flow", "ok.one.two"}, []string{"architecture.data.flow", "ok.one.two"}},
		{"caps at two", []string{"a.b.c", "d.e.f", "g.h.i"}, []string{"a.b.c", "d.e.f"}},
		{"lowercases and trims", []string{"  PREFERENCES.coding.STYLE "}, []string{"preferences.coding.style"}},
		{"hyphens invalid", []string{"ci-cd.build.step"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizePaths(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestValidPath(t *testing.T) {
	valid := []string{"preferences.coding.style", "a.b.c", "ci_cd.build.step", "x1.y2.z3"}
	invalid := []string{"", "a.b", "a.b.c.d", "A.b.c", "ci-cd.build.step", "1a.b.c", "a..c", "a.b.c "}
	for _, p := range valid {
		if !ValidPath(p) {
			t.Errorf("expected valid: %q", p)
		}
	}
	for _, p := range invalid {
		if ValidPath(p) {
			t.Errorf("expected invalid: %q", p)
		}
	}
}

func TestUpsertDedupesAndUnionsProvenance(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	paths := NormalizePaths([]string{"project.tooling.stack"})
	id := RecordID("Uses Go 1.26", paths)
	base := Record{
		ID:         id,
		Paths:      paths,
		Text:       "Uses Go 1.26",
		Origin:     "distilled",
		Status:     StatusActive,
		Provenance: []Anchor{{SessionID: "s1", Line: 10}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	set := Upsert(nil, base)

	dup := base
	dup.Provenance = []Anchor{{SessionID: "s2", Line: 20}}
	dup.UpdatedAt = later
	set = Upsert(set, dup)

	if len(set) != 1 {
		t.Fatalf("expected dedupe to one record, got %d", len(set))
	}
	if len(set[0].Provenance) != 2 {
		t.Fatalf("expected unioned provenance of 2, got %d", len(set[0].Provenance))
	}
	if !set[0].UpdatedAt.Equal(later) {
		t.Fatalf("expected UpdatedAt to advance to %v, got %v", later, set[0].UpdatedAt)
	}

	set = Upsert(set, dup)
	if len(set[0].Provenance) != 2 {
		t.Fatalf("anchor union not idempotent: got %d", len(set[0].Provenance))
	}

	other := base
	other.Text = "Uses Rust"
	other.ID = RecordID(other.Text, paths)
	set = Upsert(set, other)
	if len(set) != 2 {
		t.Fatalf("expected distinct fact appended, got %d", len(set))
	}
}

func TestApplyActionsNew(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	cand := factFor(t, "The project uses Go.", []string{"project.tooling.stack"}, now)
	out, proposals := ApplyActions(nil, []Action{{Kind: ActionNew, Candidate: cand}}, DefaultConfidenceThreshold, now)
	if len(out) != 1 || out[0].Status != StatusActive {
		t.Fatalf("expected one active fact, got %+v", out)
	}
	if len(proposals) != 0 {
		t.Fatalf("new should not produce proposals")
	}
}

func TestApplyActionsHighConfidenceMerge(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	target := factFor(t, "Checkpoint analysis lives in a dedicated pipeline module.", []string{"architecture.boundaries.rationale"}, now)
	target.Provenance = []Anchor{{SessionID: "s1", Line: 1}}
	cand := factFor(t, "Analysis generation belongs in the pipeline, not the queue.", []string{"architecture.boundaries.rationale"}, later)
	cand.Provenance = []Anchor{{SessionID: "s2", Line: 9}}

	out, proposals := ApplyActions([]Record{target}, []Action{
		{Kind: ActionMerge, TargetID: target.ID, Confidence: 0.9, Candidate: cand},
	}, DefaultConfidenceThreshold, later)

	if len(out) != 1 {
		t.Fatalf("merge should consolidate into one fact, got %d", len(out))
	}
	if out[0].ID != target.ID {
		t.Fatalf("merge should keep the target, got %s", out[0].ID)
	}
	if len(out[0].Provenance) != 2 {
		t.Fatalf("merge should union provenance, got %d", len(out[0].Provenance))
	}
	if !out[0].UpdatedAt.Equal(later) {
		t.Fatalf("merge should bump UpdatedAt")
	}
	if len(proposals) != 0 {
		t.Fatalf("high-confidence merge should not propose")
	}
}

func TestApplyActionsHighConfidenceSupersede(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	old := factFor(t, "Use Supabase for the database.", []string{"project.tooling.stack"}, now)
	newer := factFor(t, "Use MySQL for the database, not Supabase.", []string{"project.tooling.stack"}, later)

	out, proposals := ApplyActions([]Record{old}, []Action{
		{Kind: ActionSupersede, TargetID: old.ID, Confidence: 0.95, Candidate: newer},
	}, DefaultConfidenceThreshold, later)

	if len(out) != 2 {
		t.Fatalf("supersede should retain both facts, got %d", len(out))
	}
	var gotOld, gotNew *Record
	for i := range out {
		switch out[i].ID {
		case old.ID:
			gotOld = &out[i]
		case newer.ID:
			gotNew = &out[i]
		}
	}
	if gotOld == nil || gotNew == nil {
		t.Fatalf("both facts must be present")
	}
	if gotOld.Status != StatusSuperseded || gotOld.SupersededBy != newer.ID {
		t.Fatalf("old fact not marked superseded: %+v", gotOld)
	}
	if gotNew.Status != StatusActive {
		t.Fatalf("replacement should be active")
	}
	if gotNew.Confidence != "0.95" {
		t.Fatalf("action confidence should be stamped on the record, got %q", gotNew.Confidence)
	}
	if !contains(gotNew.RelatedIDs, old.ID) {
		t.Fatalf("replacement should relate to the superseded fact")
	}
	if len(proposals) != 0 {
		t.Fatalf("high-confidence supersede should not propose")
	}
}

func TestApplyActionsLowConfidenceQueuesProposal(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	target := factFor(t, "Tests run with go test.", []string{"workflow.testing.rules"}, now)
	cand := factFor(t, "Always run the full suite before pushing.", []string{"workflow.testing.rules"}, now)

	out, proposals := ApplyActions([]Record{target}, []Action{
		{Kind: ActionSupersede, TargetID: target.ID, Confidence: 0.4, Candidate: cand},
	}, DefaultConfidenceThreshold, now)

	if len(out) != 2 {
		t.Fatalf("low-confidence action must keep both facts active, got %d", len(out))
	}
	for _, r := range out {
		if r.Status != StatusActive {
			t.Fatalf("nothing should be superseded below threshold: %+v", r)
		}
	}
	if len(proposals) != 1 {
		t.Fatalf("expected one queued proposal, got %d", len(proposals))
	}
	p := proposals[0]
	if p.Action != ActionSupersede || p.CandidateID != cand.ID || p.TargetID != target.ID {
		t.Fatalf("proposal fields wrong: %+v", p)
	}
	if !contains(out[0].RelatedIDs, out[1].ID) && !contains(out[1].RelatedIDs, out[0].ID) {
		t.Fatalf("conflicting facts not cross-linked")
	}
}

func TestApplyActionsUnknownTargetDegradesToNew(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	cand := factFor(t, "A brand new fact.", []string{"project.tooling.stack"}, now)
	out, proposals := ApplyActions(nil, []Action{
		{Kind: ActionMerge, TargetID: "fact:doesnotexist", Confidence: 0.99, Candidate: cand},
	}, DefaultConfidenceThreshold, now)
	if len(out) != 1 || out[0].ID != cand.ID {
		t.Fatalf("unknown target should add candidate as new, got %+v", out)
	}
	if len(proposals) != 0 {
		t.Fatalf("unknown target should not propose")
	}
}

func TestApplyActionsChronologicalChain(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "v1 decision.", []string{"architecture.data.flow"}, now)
	b := factFor(t, "v2 decision.", []string{"architecture.data.flow"}, now.Add(time.Hour))
	c := factFor(t, "v3 decision.", []string{"architecture.data.flow"}, now.Add(2*time.Hour))

	out, _ := ApplyActions(nil, []Action{
		{Kind: ActionNew, Candidate: a},
		{Kind: ActionSupersede, TargetID: a.ID, Confidence: 0.9, Candidate: b},
		{Kind: ActionSupersede, TargetID: b.ID, Confidence: 0.9, Candidate: c},
	}, DefaultConfidenceThreshold, now)

	activeCount := 0
	for _, r := range out {
		if r.Status == StatusActive {
			activeCount++
			if r.ID != c.ID {
				t.Fatalf("only the newest fact should be active, found %s", r.ID)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly one active fact at the end of the chain, got %d", activeCount)
	}
	if len(out) != 3 {
		t.Fatalf("superseded facts must be retained, got %d", len(out))
	}
}

func TestApplyProposalMerge(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	target := factFor(t, "target fact", []string{"project.tooling.stack"}, now)
	target.Provenance = []Anchor{{SessionID: "s1"}}
	cand := factFor(t, "candidate fact", []string{"project.tooling.stack"}, now)
	cand.Provenance = []Anchor{{SessionID: "s2"}}
	cand.RelatedIDs = []string{target.ID}
	target.RelatedIDs = []string{cand.ID}

	facts := []Record{target, cand}
	p := Proposal{Action: ActionMerge, CandidateID: cand.ID, TargetID: target.ID}
	out, err := ApplyProposal(facts, p, now)
	if err != nil {
		t.Fatalf("ApplyProposal: %v", err)
	}
	if len(out) != 1 || out[0].ID != target.ID {
		t.Fatalf("merge should leave only the target, got %+v", out)
	}
	if len(out[0].Provenance) != 2 {
		t.Fatalf("merge should union provenance, got %d", len(out[0].Provenance))
	}
	if contains(out[0].RelatedIDs, cand.ID) {
		t.Fatalf("conflict link should be cleared")
	}
}

func TestApplyProposalSupersede(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	target := factFor(t, "old", []string{"project.tooling.stack"}, now)
	cand := factFor(t, "new", []string{"project.tooling.stack"}, now)
	out, err := ApplyProposal([]Record{target, cand}, Proposal{Action: ActionSupersede, CandidateID: cand.ID, TargetID: target.ID}, now)
	if err != nil {
		t.Fatalf("ApplyProposal: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("supersede retains both, got %d", len(out))
	}
	ti := IndexOf(out, target.ID)
	if out[ti].Status != StatusSuperseded || out[ti].SupersededBy != cand.ID {
		t.Fatalf("target not superseded: %+v", out[ti])
	}
}

func TestApplyProposalStale(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	cand := factFor(t, "c", []string{"project.tooling.stack"}, now)
	if _, err := ApplyProposal([]Record{cand}, Proposal{Action: ActionMerge, CandidateID: cand.ID, TargetID: "fact:gone"}, now); err == nil {
		t.Fatalf("expected stale-proposal error")
	}
}

func TestRejectProposalKeepsBothActive(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "a", []string{"project.tooling.stack"}, now)
	b := factFor(t, "b", []string{"project.tooling.stack"}, now)
	a.RelatedIDs = []string{b.ID}
	b.RelatedIDs = []string{a.ID}
	out := RejectProposal([]Record{a, b}, Proposal{Action: ActionSupersede, CandidateID: b.ID, TargetID: a.ID})
	if len(out) != 2 {
		t.Fatalf("reject keeps both, got %d", len(out))
	}
	for _, f := range out {
		if f.Status != StatusActive {
			t.Errorf("reject should not change status: %+v", f)
		}
		if len(f.RelatedIDs) != 0 {
			t.Errorf("conflict link should be cleared: %+v", f.RelatedIDs)
		}
	}
}

func TestPromoteStrategies(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	srcConflict := factFor(t, "source: use MySQL", []string{"project.tooling.stack"}, now)
	srcConflict.Branch = "feature"
	srcUnique := factFor(t, "source: prefers tabs", []string{"preferences.coding.style"}, now)
	srcUnique.Branch = "feature"
	tgtConflict := factFor(t, "target: use Postgres", []string{"project.tooling.stack"}, now)
	tgtConflict.Branch = "main"

	source := []Record{srcConflict, srcUnique}
	target := []Record{tgtConflict}

	t.Run("keep-both", func(t *testing.T) {
		merged, proposals, promoted := Promote(source, target, "keep-both", "main", now)
		if promoted != 2 {
			t.Fatalf("expected 2 promoted, got %d", promoted)
		}
		if len(proposals) != 1 {
			t.Fatalf("conflict should queue 1 proposal, got %d", len(proposals))
		}
		for _, f := range merged {
			if f.ID == tgtConflict.ID && f.Status != StatusActive {
				t.Errorf("keep-both should keep target active")
			}
		}
		for _, f := range merged {
			if f.Branch != "main" {
				t.Errorf("promoted fact not re-stamped to main: %+v", f)
			}
		}
	})

	t.Run("prefer-source", func(t *testing.T) {
		merged, proposals, _ := Promote(source, target, "prefer-source", "main", now)
		if len(proposals) != 0 {
			t.Fatalf("prefer-source should not queue proposals")
		}
		ti := IndexOf(merged, tgtConflict.ID)
		if merged[ti].Status != StatusSuperseded {
			t.Fatalf("prefer-source should supersede the conflicting target fact")
		}
	})

	t.Run("prefer-target", func(t *testing.T) {
		merged, _, promoted := Promote(source, target, "prefer-target", "main", now)
		if promoted != 1 {
			t.Fatalf("prefer-target should promote only the non-conflicting fact, got %d", promoted)
		}
		if IndexOf(merged, srcConflict.ID) >= 0 {
			t.Fatalf("conflicting source fact should be dropped under prefer-target")
		}
	})
}

func TestPromoteIdenticalUnionsProvenance(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	f := factFor(t, "shared fact", []string{"project.tooling.stack"}, now)
	src := f
	src.Branch = "feature"
	src.Provenance = []Anchor{{SessionID: "s2"}}
	tgt := f
	tgt.Branch = "main"
	tgt.Provenance = []Anchor{{SessionID: "s1"}}

	merged, proposals, promoted := Promote([]Record{src}, []Record{tgt}, "keep-both", "main", now)
	if promoted != 0 || len(proposals) != 0 {
		t.Fatalf("identical fact should not be promoted as new or conflict")
	}
	if len(merged) != 1 || len(merged[0].Provenance) != 2 {
		t.Fatalf("identical fact should union provenance, got %+v", merged)
	}
}

func TestRetract(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "active fact", []string{"project.tooling.stack"}, now)
	facts := []Record{a}

	found, changed := Retract(facts, a.ID, now.Add(time.Hour))
	if !found || !changed {
		t.Fatalf("expected found+changed, got found=%v changed=%v", found, changed)
	}
	if facts[0].Status != StatusRetracted || !facts[0].UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("fact not retracted/stamped: %+v", facts[0])
	}
	found, changed = Retract(facts, a.ID, now.Add(2*time.Hour))
	if !found || changed {
		t.Fatalf("re-retract should be found-but-unchanged, got found=%v changed=%v", found, changed)
	}
	if found, _ := Retract(facts, "fact:nope", now); found {
		t.Fatalf("unknown id should not be found")
	}
	res := GC(facts, knownTops("project"), now, testRetention)
	if len(res.Pruned) != 1 || len(res.Kept) != 0 {
		t.Fatalf("gc should prune the retracted fact: %+v", res)
	}
}

func TestGC(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	active := factFor(t, "active", []string{"project.tooling.stack"}, now)
	retracted := factFor(t, "retracted", []string{"project.tooling.stack"}, now)
	retracted.Status = StatusRetracted
	oldSuperseded := factFor(t, "old superseded", []string{"project.tooling.stack"}, now.Add(-60*24*time.Hour))
	oldSuperseded.Status = StatusSuperseded
	recentSuperseded := factFor(t, "recent superseded", []string{"project.tooling.stack"}, now.Add(-1*24*time.Hour))
	recentSuperseded.Status = StatusSuperseded
	orphan := factFor(t, "orphan", []string{"project.tooling.stack"}, now)
	orphan.Paths = []string{"gone.sub.type"} // top-level not known

	result := GC([]Record{active, retracted, oldSuperseded, recentSuperseded, orphan}, knownTops("project"), now, testRetention)

	if len(result.Pruned) != 2 {
		t.Fatalf("expected retracted + old-superseded pruned (2), got %d", len(result.Pruned))
	}
	if len(result.Orphans) != 1 || result.Orphans[0].Text != "orphan" {
		t.Fatalf("expected 1 reported orphan, got %d", len(result.Orphans))
	}
	if IndexOf(result.Kept, orphan.ID) < 0 {
		t.Fatalf("active orphan should be kept, not pruned")
	}
	if IndexOf(result.Kept, recentSuperseded.ID) < 0 {
		t.Fatalf("recent superseded should be retained within the window")
	}
}

func TestNDJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	paths := NormalizePaths([]string{"preferences.coding.style"})
	records := []Record{
		{
			ID:         RecordID("Prefers table-driven tests", paths),
			Paths:      paths,
			Text:       "Prefers table-driven tests",
			Branch:     "main",
			Origin:     "authored",
			Status:     StatusActive,
			Provenance: []Anchor{{SessionID: "s1", Commit: "abc123", Line: 42}},
			CreatedAt:  now,
			UpdatedAt:  now,
		},
	}
	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, records); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	got, err := ParseNDJSON(&buf)
	if err != nil {
		t.Fatalf("ParseNDJSON: %v", err)
	}
	if !reflect.DeepEqual(got, records) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, records)
	}
}

func TestNDJSONWriteSortsDeterministically(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "alpha", []string{"architecture.data.flow"}, now)
	b := factFor(t, "beta", []string{"project.tooling.stack"}, now)
	unsorted := []Record{b, a}

	var buf1 bytes.Buffer
	if err := WriteNDJSON(&buf1, unsorted); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	var buf2 bytes.Buffer
	if err := WriteNDJSON(&buf2, []Record{a, b}); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	if buf1.String() != buf2.String() {
		t.Fatalf("write order should be independent of input order")
	}
}

func TestNDJSONParseSkipsBlankAndReportsMalformed(t *testing.T) {
	good := "{\"id\":\"fact:1\",\"paths\":[\"a.b.c\"],\"text\":\"x\",\"branch\":\"main\",\"origin\":\"authored\",\"status\":\"active\",\"provenance\":[],\"created_at\":\"2026-06-04T12:00:00Z\",\"updated_at\":\"2026-06-04T12:00:00Z\"}"
	recs, err := ParseNDJSON(bytes.NewBufferString("\n" + good + "\n\n"))
	if err != nil {
		t.Fatalf("ParseNDJSON: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "fact:1" {
		t.Fatalf("expected one record, got %+v", recs)
	}
	_, err = ParseNDJSON(bytes.NewBufferString(good + "\nnot json\n"))
	if err == nil {
		t.Fatalf("expected malformed-line error")
	}
	var perr *ParseError
	if !errors.As(err, &perr) || perr.Line != 2 {
		t.Fatalf("expected *ParseError at line 2, got %v", err)
	}
}
