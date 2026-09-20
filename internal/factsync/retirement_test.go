package factsync

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/factmerge"
)

func headFacts(t *testing.T, srv *fakeServer) []factmerge.Record {
	t.Helper()
	_, plaintext, found, err := srv.Current(context.Background(), "repo", "main")
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	if !found {
		return nil
	}
	head, err := factmerge.ParseNDJSON(bytes.NewReader(plaintext))
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	return head
}

// A member who retracts a fact and syncs must have the retraction reach the
// shared head. Promote carries only ACTIVE source facts, so without an explicit
// retirement pass the merge is byte-identical to the head, Advance reports "no
// change", and every other member keeps reading a statement its owner declared
// false.
func TestSyncPropagatesRetraction(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	srv := &fakeServer{}
	f := fact("The service runs on port 8080.", []string{"project.tooling.stack"}, "s1", now)

	if _, err := Sync(context.Background(), srv, "repo", "main", "alice@example.com", []factmerge.Record{f}, now); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	retracted := f
	retracted.Status = factmerge.StatusRetracted
	retracted.UpdatedAt = now.Add(time.Hour)
	res, err := Sync(context.Background(), srv, "repo", "main", "alice@example.com", []factmerge.Record{retracted}, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("retract sync: %v", err)
	}
	if !res.Published {
		t.Errorf("retraction sync reported no change; the head still carries the fact as active")
	}

	head := headFacts(t, srv)
	i := factmerge.IndexOf(head, f.ID)
	if i < 0 {
		t.Fatalf("the fact vanished from the head entirely: %+v", head)
	}
	if head[i].Status != factmerge.StatusRetracted {
		t.Fatalf("shared head still says %q after the owner retracted the fact", head[i].Status)
	}
	// Retiring must not drop the record's provenance.
	if len(head[i].Provenance) == 0 {
		t.Errorf("retired fact lost its provenance: %+v", head[i])
	}

	// Idempotent: syncing the same retired local state again changes nothing.
	again, err := Sync(context.Background(), srv, "repo", "main", "alice@example.com", []factmerge.Record{retracted}, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second retract sync: %v", err)
	}
	if again.Published {
		t.Errorf("re-syncing an already-retired fact advanced the head again")
	}
}

// A supersede a member applied locally must reach the head too: the retired
// target flips to superseded and keeps pointing at the replacement, which is
// promoted alongside it.
func TestSyncPropagatesSupersede(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	srv := &fakeServer{}
	old := fact("The service runs on port 8080.", []string{"project.tooling.stack"}, "s1", now)

	if _, err := Sync(context.Background(), srv, "repo", "main", "alice@example.com", []factmerge.Record{old}, now); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	replacement := fact("The service runs on port 9090.", []string{"project.tooling.stack"}, "s2", now.Add(time.Hour))
	retired := old
	retired.Status = factmerge.StatusSuperseded
	retired.SupersededBy = replacement.ID
	retired.UpdatedAt = now.Add(time.Hour)

	if _, err := Sync(context.Background(), srv, "repo", "main", "alice@example.com", []factmerge.Record{retired, replacement}, now.Add(time.Hour)); err != nil {
		t.Fatalf("supersede sync: %v", err)
	}

	head := headFacts(t, srv)
	ti := factmerge.IndexOf(head, old.ID)
	if ti < 0 {
		t.Fatalf("superseded fact must be retained, not deleted: %+v", head)
	}
	if head[ti].Status != factmerge.StatusSuperseded || head[ti].SupersededBy != replacement.ID {
		t.Fatalf("head target not superseded: %+v", head[ti])
	}
	if factmerge.IndexOf(head, replacement.ID) < 0 {
		t.Fatalf("the replacement fact was not promoted: %+v", head)
	}
}

// Retiring must never REVIVE: a fact another member already retired on the head
// stays retired when this member still holds it as active.
func TestSyncDoesNotReviveHeadRetraction(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	srv := &fakeServer{}
	f := fact("The service runs on port 8080.", []string{"project.tooling.stack"}, "s1", now)

	if _, err := Sync(context.Background(), srv, "repo", "main", "alice@example.com", []factmerge.Record{f}, now); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	retracted := f
	retracted.Status = factmerge.StatusRetracted
	if _, err := Sync(context.Background(), srv, "repo", "main", "alice@example.com", []factmerge.Record{retracted}, now.Add(time.Hour)); err != nil {
		t.Fatalf("retract sync: %v", err)
	}
	// A second member still carries it as active.
	if _, err := Sync(context.Background(), srv, "repo", "main", "bob@example.com", []factmerge.Record{f}, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("peer sync: %v", err)
	}

	head := headFacts(t, srv)
	i := factmerge.IndexOf(head, f.ID)
	if i < 0 || head[i].Status != factmerge.StatusRetracted {
		t.Fatalf("a peer sync revived a retracted fact: %+v", head)
	}
}
