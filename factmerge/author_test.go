package factmerge

import (
	"bytes"
	"testing"
	"time"
)

func TestAuthorIsOutsideIdentityAndOrdering(t *testing.T) {
	paths := []string{"project.testing"}
	r := Record{ID: RecordID("fact text", paths), Paths: paths, Text: "fact text",
		Branch: "main", Origin: "authored", Status: "active",
		Provenance: []Anchor{{SessionID: "s"}}, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
	withAuthor := r
	withAuthor.Author = "evisdren"

	if RecordID(withAuthor.Text, withAuthor.Paths) != r.ID {
		t.Fatal("author must not affect RecordID")
	}

	// Author must not change a record's sort position: a two-record store
	// serializes its IDs in the same order whether or not one is attributed.
	other := Record{Paths: []string{"architecture.sync"}, Text: "another fact",
		Branch: "main", Origin: "authored", Status: "active",
		Provenance: []Anchor{{SessionID: "s"}}, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
	other.ID = RecordID(other.Text, other.Paths)

	idOrder := func(records []Record) []string {
		var buf bytes.Buffer
		if err := WriteNDJSON(&buf, records); err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseNDJSON(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(parsed))
		for i, rec := range parsed {
			ids[i] = rec.ID
		}
		return ids
	}
	plain := idOrder([]Record{other, r})
	attributed := idOrder([]Record{other, withAuthor})
	if len(plain) != 2 || len(attributed) != 2 || plain[0] != attributed[0] || plain[1] != attributed[1] {
		t.Fatalf("author must not affect deterministic NDJSON ordering: %v vs %v", plain, attributed)
	}

	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, []Record{other, withAuthor}); err != nil {
		t.Fatal(err)
	}
	round, err := ParseNDJSON(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range round {
		if rec.Author == "evisdren" {
			found = true
		}
	}
	if !found {
		t.Fatal("author must round-trip through NDJSON")
	}
}

func TestUpsertFillsEmptyAuthorWithoutOverwriting(t *testing.T) {
	paths := []string{"project.testing"}
	stored := Record{ID: RecordID("fact text", paths), Paths: paths, Text: "fact text",
		Branch: "main", Origin: "authored", Status: "active",
		Provenance: []Anchor{{SessionID: "s1"}}, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
	incoming := stored
	incoming.Author = "evisdren"
	incoming.Provenance = []Anchor{{SessionID: "s2"}}

	got := Upsert([]Record{stored}, incoming)
	if len(got) != 1 {
		t.Fatalf("expected upsert to collapse, got %d records", len(got))
	}
	if got[0].Author != "evisdren" {
		t.Fatalf("empty stored author must be filled from incoming, got %q", got[0].Author)
	}

	overwrite := incoming
	overwrite.Author = "someone-else"
	got = Upsert(got, overwrite)
	if got[0].Author != "evisdren" {
		t.Fatalf("non-empty stored author must not be overwritten, got %q", got[0].Author)
	}
}
