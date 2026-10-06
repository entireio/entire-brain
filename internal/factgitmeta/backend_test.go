package factgitmeta

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/factgitmeta/gitmeta"
	"github.com/entireio/entire-brain/internal/factsync"
)

func testNow() time.Time { return time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC) }

// newBackend opens a fresh local backend at <tmp>/gitmeta.git.
func newBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gitmeta.git")
	b, err := NewLocalBackend(dir, "gh/entireio/cli", testNow)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	return b, dir
}

// fact builds an active distilled record (mirrors factsync/sync_test.fact): two
// facts with the same text+paths share a content-derived ID.
func fact(text string, paths []string, session string) factmerge.Record {
	np := factmerge.NormalizePaths(paths)
	return factmerge.Record{
		ID:         factmerge.RecordID(text, np),
		Paths:      np,
		Text:       text,
		Branch:     "main",
		Origin:     "distilled",
		Status:     factmerge.StatusActive,
		Provenance: []factmerge.Anchor{{SessionID: session}},
		CreatedAt:  testNow(),
		UpdatedAt:  testNow(),
	}
}

func ndjson(t *testing.T, recs ...factmerge.Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, recs); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	return buf.Bytes()
}

func factTexts(t *testing.T, blob []byte) map[string]bool {
	t.Helper()
	recs, err := factmerge.ParseNDJSON(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("ParseNDJSON: %v", err)
	}
	out := map[string]bool{}
	for _, r := range recs {
		out[r.Text] = true
	}
	return out
}

// TestBackendRoundTripPersistsToDisk proves the create→read round-trip returns
// the EXACT bytes and content ref, writes a real bare git repo, and survives a
// reopen (durability across process restarts).
func TestBackendRoundTripPersistsToDisk(t *testing.T) {
	ctx := context.Background()
	b, dir := newBackend(t)

	// No head yet → Current reports not-found with a nil blob (merge into empty).
	if ref, blob, found, err := b.Current(ctx, "repo", "main"); err != nil || found || ref != "" || blob != nil {
		t.Fatalf("Current on empty = (%q,%v,%v,%v); want (\"\",nil,false,nil)", ref, blob, found, err)
	}

	plaintext := ndjson(t, fact("the build uses bazel", []string{"build/tooling"}, "s1"))
	ref, err := b.Advance(ctx, "repo", "main", "", plaintext)
	if err != nil {
		t.Fatalf("Advance create: %v", err)
	}
	if want := FactSetRef(plaintext); ref != want {
		t.Fatalf("Advance ref = %q; want content ref %q", ref, want)
	}

	gotRef, gotBlob, found, err := b.Current(ctx, "repo", "main")
	if err != nil || !found {
		t.Fatalf("Current after advance: found=%v err=%v", found, err)
	}
	if gotRef != ref {
		t.Fatalf("Current ref = %q; want %q", gotRef, ref)
	}
	if !bytes.Equal(gotBlob, plaintext) {
		t.Fatalf("Current blob = %q; want %q (byte-for-byte)", gotBlob, plaintext)
	}

	// It really wrote a git repo; not an in-memory store.
	for _, rel := range []string{"HEAD", filepath.Join("refs", "meta", "local", "main")} {
		if _, statErr := os.Stat(filepath.Join(dir, rel)); statErr != nil {
			t.Fatalf("expected %s on disk under %s: %v", rel, dir, statErr)
		}
	}

	// A brand-new backend over the SAME dir reads the persisted head back.
	b2, err := NewLocalBackend(dir, "gh/entireio/cli", testNow)
	if err != nil {
		t.Fatalf("reopen backend: %v", err)
	}
	reRef, reBlob, found, err := b2.Current(ctx, "repo", "main")
	if err != nil || !found || reRef != ref || !bytes.Equal(reBlob, plaintext) {
		t.Fatalf("reopened Current = (%q, found=%v, err=%v); want persisted (%q, %q)", reRef, found, err, ref, plaintext)
	}
}

// TestBackendAdvanceNoChange proves re-advancing the identical content from the
// current ref is a no-op (ErrNoChange) and does not move the head.
func TestBackendAdvanceNoChange(t *testing.T) {
	ctx := context.Background()
	b, _ := newBackend(t)

	plaintext := ndjson(t, fact("a", []string{"x"}, "s1"))
	ref, err := b.Advance(ctx, "repo", "main", "", plaintext)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}

	if _, err := b.Advance(ctx, "repo", "main", ref, plaintext); !errors.Is(err, factsync.ErrNoChange) {
		t.Fatalf("Advance identical content = %v; want ErrNoChange", err)
	}
	// Head unchanged.
	if gotRef, _, _, _ := b.Current(ctx, "repo", "main"); gotRef != ref {
		t.Fatalf("head moved on no-change: %q != %q", gotRef, ref)
	}
}

// TestBackendAdvanceConflict proves both conflict shapes (a stale/unknown
// oldRef and a create over an existing head) map to ErrConflict, leave the head
// untouched (no partial write), and that advancing from the correct ref then
// succeeds.
func TestBackendAdvanceConflict(t *testing.T) {
	ctx := context.Background()
	b, _ := newBackend(t)

	v1 := ndjson(t, fact("a", []string{"x"}, "s1"))
	ref1, err := b.Advance(ctx, "repo", "main", "", v1)
	if err != nil {
		t.Fatalf("advance v1: %v", err)
	}

	v2 := ndjson(t, fact("a", []string{"x"}, "s1"), fact("b", []string{"y"}, "s2"))

	// Stale oldRef → conflict.
	if _, err := b.Advance(ctx, "repo", "main", "facts-stale", v2); !errors.Is(err, factsync.ErrConflict) {
		t.Fatalf("Advance stale oldRef = %v; want ErrConflict", err)
	}
	// Create (oldRef="") over an existing head → conflict.
	if _, err := b.Advance(ctx, "repo", "main", "", v2); !errors.Is(err, factsync.ErrConflict) {
		t.Fatalf("Advance create-over-existing = %v; want ErrConflict", err)
	}
	// Mutation-verified: the head is UNCHANGED after the failed advances.
	if gotRef, gotBlob, _, _ := b.Current(ctx, "repo", "main"); gotRef != ref1 || !bytes.Equal(gotBlob, v1) {
		t.Fatalf("head changed after conflict: ref=%q", gotRef)
	}

	// Advancing from the CORRECT current ref succeeds and moves the head.
	ref2, err := b.Advance(ctx, "repo", "main", ref1, v2)
	if err != nil {
		t.Fatalf("advance v2 from correct ref: %v", err)
	}
	if ref2 == ref1 {
		t.Fatalf("ref did not advance: still %q", ref1)
	}
	_, gotBlob, _, _ := b.Current(ctx, "repo", "main")
	if got := factTexts(t, gotBlob); !got["a"] || !got["b"] {
		t.Fatalf("post-advance head missing facts: %v", got)
	}
}

// interposeOnce wraps a Server and, once armed, runs a competing member's full
// Sync immediately after the next Current; staling the caller's read so its
// following Advance loses the value-CAS and must re-read + re-merge. It mirrors
// factsync/sync_test's conflictOnceServer, but drives the REAL local backend.
type interposeOnce struct {
	inner      factsync.Server
	competitor []factmerge.Record
	now        time.Time
	armed      bool
}

func (s *interposeOnce) Current(ctx context.Context, repoID, branch string) (string, []byte, bool, error) {
	ref, blob, found, err := s.inner.Current(ctx, repoID, branch)
	if s.armed {
		s.armed = false
		if _, aerr := factsync.Sync(ctx, s.inner, repoID, branch, "member-C", s.competitor, s.now); aerr != nil {
			return "", nil, false, aerr
		}
	}
	return ref, blob, found, err
}

func (s *interposeOnce) Advance(ctx context.Context, repoID, branch, oldRef string, plaintext []byte) (string, error) {
	return s.inner.Advance(ctx, repoID, branch, oldRef, plaintext)
}

// TestBackendTwoWriterInterleaveViaSync drives factsync.Sync against the real
// local backend through a deterministic conflict: member M reads, competitor C
// advances the head underneath, M's Advance conflicts, and Sync re-reads +
// re-merges. The converged head must hold ALL THREE members' facts; nothing
// dropped through the conflict/retry.
func TestBackendTwoWriterInterleaveViaSync(t *testing.T) {
	ctx := context.Background()
	now := testNow()
	b, _ := newBackend(t)

	memberA := []factmerge.Record{fact("api timeouts default to 30s", []string{"api/config"}, "s-A")}
	if _, err := factsync.Sync(ctx, b, "repo", "main", "member-A", memberA, now); err != nil {
		t.Fatalf("prime head with member A: %v", err)
	}

	wrapped := &interposeOnce{
		inner:      b,
		competitor: []factmerge.Record{fact("migrations run on deploy", []string{"ops/deploy"}, "s-C")},
		now:        now,
		armed:      true,
	}
	memberM := []factmerge.Record{fact("the build uses bazel", []string{"build/tooling"}, "s-M")}
	res, err := factsync.Sync(ctx, wrapped, "repo", "main", "member-M", memberM, now)
	if err != nil {
		t.Fatalf("member M sync: %v", err)
	}
	if res.Attempts < 2 {
		t.Fatalf("expected a retry after the interposed conflict (Attempts>=2), got %d", res.Attempts)
	}
	if !res.Published {
		t.Fatalf("member M sync did not publish")
	}

	_, blob, found, err := b.Current(ctx, "repo", "main")
	if err != nil || !found {
		t.Fatalf("final Current: found=%v err=%v", found, err)
	}
	got := factTexts(t, blob)
	for _, want := range []string{"api timeouts default to 30s", "migrations run on deploy", "the build uses bazel"} {
		if !got[want] {
			t.Fatalf("converged head missing %q; got %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("converged head has %d distinct facts; want 3", len(got))
	}
}

// TestBackendConcurrentCreateNoLostUpdate is the regression for the create-path
// race: two backends on the SAME on-disk repo (two processes) both create the
// head from empty at once. go-git's create ref write is UNCONDITIONAL (it skips
// the absence check when the old ref is nil), so before the advisory lock both
// creates "won" and one member's facts were silently dropped. With the lock,
// exactly one wins and the loser gets ErrConflict; Sync then re-reads and
// re-merges, never dropping a member's facts.
func TestBackendConcurrentCreateNoLostUpdate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gitmeta.git")
	bA, err := NewLocalBackend(dir, "gh/entireio/cli", testNow)
	if err != nil {
		t.Fatalf("NewLocalBackend A: %v", err)
	}
	bB, err := NewLocalBackend(dir, "gh/entireio/cli", testNow)
	if err != nil {
		t.Fatalf("NewLocalBackend B: %v", err)
	}

	blobA := ndjson(t, fact("fact A", []string{"topic/a"}, "sA"))
	blobB := ndjson(t, fact("fact B", []string{"topic/b"}, "sB"))

	type res struct {
		ref string
		err error
	}
	ch := make(chan res, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r, e := bA.Advance(context.Background(), "repo", "main", "", blobA)
		ch <- res{r, e}
	}()
	go func() {
		defer wg.Done()
		r, e := bB.Advance(context.Background(), "repo", "main", "", blobB)
		ch <- res{r, e}
	}()
	wg.Wait()
	close(ch)

	wins, conflicts := 0, 0
	for r := range ch {
		switch {
		case r.err == nil && r.ref != "":
			wins++
		case errors.Is(r.err, factsync.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected Advance result: ref=%q err=%v", r.ref, r.err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent create: want exactly 1 win + 1 conflict, got wins=%d conflicts=%d", wins, conflicts)
	}

	// The head + blob left on disk are consistent; the racer did not corrupt it.
	ref, blob, found, err := bA.Current(context.Background(), "repo", "main")
	if err != nil {
		t.Fatalf("Current after race: %v", err)
	}
	if !found {
		t.Fatal("head missing after a successful create")
	}
	if FactSetRef(blob) != ref {
		t.Fatalf("blob/head mismatch after race: blob ref %s != head %s", FactSetRef(blob), ref)
	}
}

// TestBackendBlobDoesNotAccumulate is the regression for unbounded blob growth:
// the live git-meta state must hold exactly ONE blob record (the current
// fact-set), not one per historical version, or Materialize/Serialize grow
// O(versions) on every sync. Prior versions live in git history, not the tree.
func TestBackendBlobDoesNotAccumulate(t *testing.T) {
	b, _ := newBackend(t)
	ctx := context.Background()
	oldRef := ""
	for i := 0; i < 4; i++ {
		blob := ndjson(t, fact("fact "+strconv.Itoa(i), []string{"topic/x"}, "s"+strconv.Itoa(i)))
		ref, err := b.Advance(ctx, "repo", "main", oldRef, blob)
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		oldRef = ref
	}
	st, _, err := b.state()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	blobs := 0
	for _, s := range st.Strings {
		if strings.HasSuffix(s.Key, ":blob") {
			blobs++
		}
	}
	if blobs != 1 {
		t.Fatalf("want exactly 1 live blob record after 4 advances, got %d", blobs)
	}
}

// --- MetaStore -------------------------------------------------------------

// TestMetaStoreTryUpdateDoesNotWaitOnTheLock pins the non-blocking write path.
// Update takes flock(2), which no Go context can interrupt, so a caller under a
// deadline (the entity index's freshness tick, running inside a session-end
// hook) must be able to give up instead of hanging for as long as whichever
// other writer holds the store.
func TestMetaStoreTryUpdateDoesNotWaitOnTheLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gitmeta.git")
	store, err := OpenMetaStore(dir, testNow)
	if err != nil {
		t.Fatalf("OpenMetaStore: %v", err)
	}
	release, err := store.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}

	// A second acquisition must report contention, not block.
	if _, err := store.TryLock(); !errors.Is(err, ErrLockBusy) {
		release()
		t.Fatalf("second TryLock = %v, want ErrLockBusy", err)
	}

	done := make(chan error, 1)
	go func() {
		_, updateErr := store.TryUpdate(1, func(st gitmeta.State) (gitmeta.State, error) {
			return st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: "brain:test", Value: "v"}), nil
		})
		done <- updateErr
	}()
	select {
	case updateErr := <-done:
		if !errors.Is(updateErr, ErrLockBusy) {
			t.Fatalf("contended TryUpdate = %v, want ErrLockBusy", updateErr)
		}
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("TryUpdate blocked on a held lock")
	}

	tip, err := store.Tip()
	if err != nil || tip != "" {
		t.Fatalf("a refused update still wrote: tip=%q err=%v", tip, err)
	}

	// Once released, the same call succeeds.
	release()
	if _, err := store.TryUpdate(1, func(st gitmeta.State) (gitmeta.State, error) {
		return st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: "brain:test", Value: "v"}), nil
	}); err != nil {
		t.Fatalf("uncontended TryUpdate: %v", err)
	}
	if value, ok, err := store.ReadString(projectTarget, "brain:test"); err != nil || !ok || value != "v" {
		t.Fatalf("ReadString = (%q,%v,%v)", value, ok, err)
	}
}

// TestMetaStoreReadStringMatchesMaterializedState pins the targeted read
// against the full materialization it exists to avoid.
func TestMetaStoreReadStringMatchesMaterializedState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gitmeta.git")
	store, err := OpenMetaStore(dir, testNow)
	if err != nil {
		t.Fatalf("OpenMetaStore: %v", err)
	}
	// Absent ref: not an error, just absent.
	if _, ok, err := store.ReadString(projectTarget, "brain:absent"); err != nil || ok {
		t.Fatalf("read before any write = (%v,%v)", ok, err)
	}
	if _, err := store.Update(2, func(st gitmeta.State) (gitmeta.State, error) {
		st = st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: "brain:one", Value: "1"})
		st = st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: "brain:two", Value: "2"})
		return st, nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	state, err := store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	for _, key := range []string{"brain:one", "brain:two", "brain:absent"} {
		wantValue, wantOK := state.CurrentString(projectTarget, key)
		gotValue, gotOK, err := store.ReadString(projectTarget, key)
		if err != nil {
			t.Fatalf("ReadString(%s): %v", key, err)
		}
		if gotOK != wantOK || gotValue != wantValue {
			t.Fatalf("ReadString(%s) = (%q,%v), materialized (%q,%v)", key, gotValue, gotOK, wantValue, wantOK)
		}
	}
}
