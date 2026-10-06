package factsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
)

// fakeServer mirrors entire-api's FactSetStore semantics in-memory: a content-addressed
// head (ref = facts-<sha256(plaintext)>) advanced under CAS. Advance returns ErrNoChange
// when the merged blob equals the current content (newRef==oldRef, checked first, exactly
// as FactSetStore.Advance short-circuits before store/CAS) and ErrConflict when the head
// moved under the caller (oldRef != current). Thread-safe for concurrent members.
type fakeServer struct {
	mu    sync.Mutex
	ref   string
	blob  []byte
	found bool
}

func contentRef(plaintext []byte) string {
	sum := sha256.Sum256(plaintext)
	return "facts-" + hex.EncodeToString(sum[:])
}

func (s *fakeServer) Current(_ context.Context, _, _ string) (string, []byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.found {
		return "", nil, false, nil
	}
	cp := make([]byte, len(s.blob))
	copy(cp, s.blob)
	return s.ref, cp, true, nil
}

func (s *fakeServer) Advance(_ context.Context, _, _, oldRef string, plaintext []byte) (string, error) {
	// Mirror entire-api's FactSetStore.Advance / the POST endpoint: empty plaintext is a
	// hard reject (a head always points at content). If the fake accepted it, it would mask
	// a real 400 the runner must never trigger — Sync's empty-merge guard prevents it.
	if len(plaintext) == 0 {
		return "", errors.New("factsync: empty plaintext (a fact-set head always points at content)")
	}
	newRef := contentRef(plaintext)
	if newRef == oldRef {
		return "", ErrNoChange
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := ""
	if s.found {
		cur = s.ref
	}
	if oldRef != cur {
		return "", ErrConflict
	}
	s.ref, s.blob, s.found = newRef, append([]byte(nil), plaintext...), true
	return newRef, nil
}

// fact builds a fully-formed active distilled Record. Two facts with the same text and
// paths share a content-derived ID, so members that independently learn the same fact
// collapse to one record whose provenance is the UNION of their anchors.
func fact(text string, paths []string, session string, now time.Time) factmerge.Record {
	np := factmerge.NormalizePaths(paths)
	return factmerge.Record{
		ID:         factmerge.RecordID(text, np),
		Paths:      np,
		Text:       text,
		Branch:     "main",
		Origin:     "distilled",
		Status:     factmerge.StatusActive,
		Provenance: []factmerge.Anchor{{SessionID: session}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func sessionSet(a factmerge.Anchor) string { return a.SessionID }

// TestSyncConvergesAndUnionsProvenance is the load-bearing M2.3 proof with the REAL
// merge engine: two members concurrently sync into one branch. A shared fact (same
// text+paths → same id) carries a DIFFERENT provenance anchor per member; distinct
// facts are member-unique. After both converge, the head must contain every distinct
// fact plus ONE copy of the shared fact whose provenance is the UNION of both members'
// anchors — nothing dropped, exact dup collapsed, provenance merged.
func TestSyncConvergesAndUnionsProvenance(t *testing.T) {
	ctx := context.Background()
	srv := &fakeServer{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	shared := "the build uses bazel"
	sharedPaths := []string{"build/tooling"}

	memberA := []factmerge.Record{
		fact(shared, sharedPaths, "session-A", now),
		fact("api timeouts default to 30s", []string{"api/config"}, "session-A", now),
	}
	memberB := []factmerge.Record{
		fact(shared, sharedPaths, "session-B", now),
		fact("migrations run on deploy", []string{"ops/deploy"}, "session-B", now),
	}

	var wg sync.WaitGroup
	res := make([]Result, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); res[0], errs[0] = Sync(ctx, srv, "repo", "main", "member-A", memberA, now) }()
	go func() { defer wg.Done(); res[1], errs[1] = Sync(ctx, srv, "repo", "main", "member-B", memberB, now) }()
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("member %d sync failed: %v", i, errs[i])
		}
	}

	// Read the converged head.
	_, blob, found, err := srv.Current(ctx, "repo", "main")
	if err != nil || !found {
		t.Fatalf("Current after convergence: found=%v err=%v", found, err)
	}
	recs, err := factmerge.ParseNDJSON(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("parse converged head: %v", err)
	}

	byText := map[string]factmerge.Record{}
	for _, r := range recs {
		if _, dup := byText[r.Text]; dup {
			t.Fatalf("duplicate fact text in converged head: %q", r.Text)
		}
		byText[r.Text] = r
	}
	for _, want := range []string{shared, "api timeouts default to 30s", "migrations run on deploy"} {
		if _, ok := byText[want]; !ok {
			t.Fatalf("converged head missing fact %q; got %d records", want, len(recs))
		}
	}
	if len(recs) != 3 {
		t.Fatalf("converged head has %d records; want 3 (union, shared deduped)", len(recs))
	}

	// The shared fact's provenance must be the UNION of both members' sessions.
	sh := byText[shared]
	sessions := map[string]bool{}
	for _, a := range sh.Provenance {
		sessions[sessionSet(a)] = true
	}
	if !sessions["session-A"] || !sessions["session-B"] || len(sh.Provenance) != 2 {
		t.Fatalf("shared fact provenance = %+v; want union of session-A and session-B", sh.Provenance)
	}
}

// TestSyncRetriesOnConflict deterministically exercises the CAS retry path: a member
// reads the head, a competing writer advances it underneath, and the member's Advance
// then loses the swap (ErrConflict), forcing a re-read + re-merge that succeeds and
// preserves BOTH the competitor's and the member's facts.
func TestSyncRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	// Seed the head with a competitor's fact so the member starts from a non-empty head,
	// then interpose ANOTHER competitor write between the member's read and its advance.
	srv := &conflictOnceServer{
		inner:     &fakeServer{},
		intercept: fact("seeded competitor fact", []string{"misc/seed"}, "session-C", now),
		now:       now,
	}
	// Prime the head to v1 via the competitor.
	if _, err := Sync(ctx, srv.inner, "repo", "main", "member-C", []factmerge.Record{srv.intercept}, now); err != nil {
		t.Fatalf("prime head: %v", err)
	}
	srv.arm() // next Current will be followed by an interposed competing advance

	member := []factmerge.Record{fact("member local fact", []string{"api/config"}, "session-M", now)}
	res, err := Sync(ctx, srv, "repo", "main", "member-M", member, now)
	if err != nil {
		t.Fatalf("member sync: %v", err)
	}
	if res.Attempts < 2 {
		t.Fatalf("expected a retry (Attempts>=2), got %d", res.Attempts)
	}

	_, blob, _, _ := srv.inner.Current(ctx, "repo", "main")
	recs, err := factmerge.ParseNDJSON(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	texts := map[string]bool{}
	for _, r := range recs {
		texts[r.Text] = true
	}
	for _, want := range []string{"seeded competitor fact", "member local fact"} {
		if !texts[want] {
			t.Fatalf("post-retry head missing %q; got %v", want, texts)
		}
	}
}

// conflictOnceServer wraps fakeServer and, once armed, interposes exactly one competing
// advance immediately after the next Current — deterministically staling the caller's
// read so its following Advance loses the CAS and must retry.
type conflictOnceServer struct {
	inner     *fakeServer
	intercept factmerge.Record
	now       time.Time
	armed     bool
}

func (s *conflictOnceServer) arm() { s.armed = true }

func (s *conflictOnceServer) Current(ctx context.Context, repoID, branch string) (string, []byte, bool, error) {
	ref, blob, found, err := s.inner.Current(ctx, repoID, branch)
	if s.armed {
		s.armed = false
		// Interpose a competing writer that advances the head, staling the ref just read.
		competitor := fact("interposed competitor fact", []string{"misc/interpose"}, "session-X", s.now)
		if _, aerr := Sync(ctx, s.inner, repoID, branch, "member-X", []factmerge.Record{competitor}, s.now); aerr != nil {
			return "", nil, false, aerr
		}
	}
	return ref, blob, found, err
}

func (s *conflictOnceServer) Advance(ctx context.Context, repoID, branch, oldRef string, plaintext []byte) (string, error) {
	return s.inner.Advance(ctx, repoID, branch, oldRef, plaintext)
}
