package factsync

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// anchoredFact builds a fact whose single provenance anchor carries BOTH opaque
// cross-member ids (session, commit, checkpoint) and LOCAL filesystem coordinates
// (transcript path + line) — the shape SanitizeForEgress must split.
func anchoredFact(text string, paths []string, a factmerge.Anchor, now time.Time) factmerge.Record {
	np := factmerge.NormalizePaths(paths)
	return factmerge.Record{
		ID:         factmerge.RecordID(text, np),
		Paths:      np,
		Text:       text,
		Branch:     "main",
		Origin:     "distilled",
		Status:     factmerge.StatusActive,
		Provenance: []factmerge.Anchor{a},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// TestSyncStripsLocalPathsButKeepsOpaqueProvenance proves the no-leak guarantee: a fact
// synced cross-member arrives with its local transcript path + line REDACTED, while the
// opaque cross-member ids (session, commit, checkpoint) survive — and the serialized
// blob on the shared head contains no trace of the member's local filesystem layout.
func TestSyncStripsLocalPathsButKeepsOpaqueProvenance(t *testing.T) {
	ctx := context.Background()
	srv := &fakeServer{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	const localPath = "sessions/2026-07-07/Users-thomi-secret/transcript.jsonl"
	local := []factmerge.Record{anchoredFact(
		"ci runs on self-hosted runners", []string{"ci.runner.host"},
		factmerge.Anchor{SessionID: "sess-1", Commit: "deadbeef", CheckpointID: "ckpt-9", Transcript: localPath, Line: 42},
		now,
	)}

	if _, err := Sync(ctx, srv, "repo", "main", "member-A", local, now); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The caller's own local slice must be untouched (still has the path locally).
	if local[0].Provenance[0].Transcript != localPath {
		t.Fatalf("SanitizeForEgress mutated the caller's local facts: %q", local[0].Provenance[0].Transcript)
	}

	// The shared head must carry the fact WITHOUT the local path/line, WITH the opaque ids.
	_, blob, found, err := srv.Current(ctx, "repo", "main")
	if err != nil || !found {
		t.Fatalf("Current: found=%v err=%v", found, err)
	}
	if bytes.Contains(blob, []byte("Users-thomi")) || strings.Contains(string(blob), localPath) {
		t.Fatalf("LOCAL PATH LEAKED into the shared head blob:\n%s", blob)
	}
	recs, err := factmerge.ParseNDJSON(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	if len(recs) != 1 || len(recs[0].Provenance) != 1 {
		t.Fatalf("head = %d recs; want 1 with 1 anchor", len(recs))
	}
	a := recs[0].Provenance[0]
	if a.Transcript != "" || a.Line != 0 {
		t.Fatalf("local coords not redacted: transcript=%q line=%d", a.Transcript, a.Line)
	}
	if a.SessionID != "sess-1" || a.Commit != "deadbeef" || a.CheckpointID != "ckpt-9" {
		t.Fatalf("opaque cross-member ids not preserved: %+v", a)
	}
}

// TestSyncEmptyMergeConverges proves the empty-merge guard: a member with NO active
// local facts syncing into an EMPTY head produces an empty merged set, which must be
// reported as converged (nothing to publish) rather than pushed as an empty blob — the
// real server rejects empty plaintext (400), and the fake now mirrors that reject, so a
// missing guard would surface as an error here.
func TestSyncEmptyMergeConverges(t *testing.T) {
	ctx := context.Background()
	srv := &fakeServer{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// A retracted (non-active) local fact: Promote skips it, so the merged set is empty.
	f := fact("stale retracted fact", []string{"x.y.z"}, "s", now)
	f.Status = factmerge.StatusRetracted

	res, err := Sync(ctx, srv, "repo", "main", "member-A", []factmerge.Record{f}, now)
	if err != nil {
		t.Fatalf("empty-merge sync errored (guard missing?): %v", err)
	}
	if res.Published {
		t.Fatalf("empty merge reported Published=true; want converged/no publish")
	}
	// Head must remain empty — nothing was pushed.
	if _, _, found, _ := srv.Current(ctx, "repo", "main"); found {
		t.Fatal("empty merge pushed a blob to the head; want head untouched")
	}
}

// TestSyncUnionsProvenanceAfterSanitize proves sanitization does not break provenance
// union: two members learn the SAME fact (same text+paths → same id) with DIFFERENT
// local transcripts and DIFFERENT sessions. After both sync, the shared fact carries the
// UNION of both sessions, and NEITHER member's local path is present.
func TestSyncUnionsProvenanceAfterSanitize(t *testing.T) {
	ctx := context.Background()
	srv := &fakeServer{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	text, paths := "the primary db is postgres", []string{"data.store.engine"}
	a := []factmerge.Record{anchoredFact(text, paths,
		factmerge.Anchor{SessionID: "sess-A", Transcript: "sessions/A/local-A.jsonl", Line: 1}, now)}
	b := []factmerge.Record{anchoredFact(text, paths,
		factmerge.Anchor{SessionID: "sess-B", Transcript: "sessions/B/local-B.jsonl", Line: 2}, now)}

	if _, err := Sync(ctx, srv, "repo", "main", "member-A", a, now); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ctx, srv, "repo", "main", "member-B", b, now); err != nil {
		t.Fatal(err)
	}

	_, blob, _, _ := srv.Current(ctx, "repo", "main")
	if bytes.Contains(blob, []byte("local-A.jsonl")) || bytes.Contains(blob, []byte("local-B.jsonl")) {
		t.Fatalf("a member local path leaked:\n%s", blob)
	}
	recs, err := factmerge.ParseNDJSON(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("head = %d recs; want 1 (shared fact deduped)", len(recs))
	}
	sessions := map[string]bool{}
	for _, an := range recs[0].Provenance {
		if an.Transcript != "" {
			t.Fatalf("unsanitized anchor in union: %+v", an)
		}
		sessions[an.SessionID] = true
	}
	if !sessions["sess-A"] || !sessions["sess-B"] || len(recs[0].Provenance) != 2 {
		t.Fatalf("provenance union after sanitize = %+v; want sess-A + sess-B", recs[0].Provenance)
	}
}
