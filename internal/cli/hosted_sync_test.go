package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/factsync"

	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

// hostedFake is an in-memory brainstore speaking the per-fact wire:
// POST …/brain/facts (identity-verified NDJSON batch, counts reply)
// and GET …/brain/facts (insert-ordered keyset feed, index page tokens,
// empty page terminal). Upserted records join the feed with the caller's
// server-stamped attribution, like the real store.
type hostedFake struct {
	mu           sync.Mutex
	items        []factsync.FeedItem
	index        map[string]int
	requests     int
	upsertCalls  int
	feedTokens   []string
	denyPush     bool   // upsert → 403, the push-grant-denied shape
	denyFeed     bool   // feed GET → 500, the pull-outage shape
	noAccessRepo string // any request for this repo id → 403, the stale-binding shape
	rejectTokens int    // while >0, any request with a pageToken → 400 once each
	callerBy     string
	callerHandle string
}

func (f *hostedFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *hostedFake) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upsertCalls
}

func (f *hostedFake) feedTokensSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.feedTokens...)
}

func (f *hostedFake) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.index[id]
	return ok
}

// seed plants a feed item as if another member had upserted it.
func (f *hostedFake) seed(rec factmerge.Record, trailID, createdBy, handle string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put(factsync.FeedItem{Record: rec, TrailID: trailID, CreatedBy: createdBy, CreatedByHandle: handle, UpdatedAt: "2026-10-07T12:00:00.000Z"})
}

// setTrail rewrites a seeded item's trail id in place (simulates the merge
// stamp arriving later); feed position is unchanged.
func (f *hostedFake) setTrail(id, trailID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.index[id]; ok {
		f.items[i].TrailID = trailID
	}
}

func (f *hostedFake) put(item factsync.FeedItem) {
	if f.index == nil {
		f.index = make(map[string]int)
	}
	if i, ok := f.index[item.Record.ID]; ok {
		// Like the real store: a re-upsert updates the record but keeps the
		// server-owned envelope — first-writer attribution and the trail id
		// stamped at merge survive later upserts of the same fact — and status
		// advances one way (a stale active copy never revives a retirement).
		if f.items[i].Record.Status != factmerge.StatusActive && item.Record.Status == factmerge.StatusActive {
			item.Record.Status = f.items[i].Record.Status
			item.Record.SupersededBy = f.items[i].Record.SupersededBy
		}
		f.items[i].Record = item.Record
		return
	}
	f.index[item.Record.ID] = len(f.items)
	f.items = append(f.items, item)
}

func newHostedFakeServer(t *testing.T, fake *hostedFake) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.requests++
		w.Header().Set("Content-Type", "application/json")
		if fake.noAccessRepo != "" && strings.Contains(r.URL.Path, "/repos/"+fake.noAccessRepo+"/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/brain/facts"):
			fake.upsertCalls++
			if fake.denyPush {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			records, err := factmerge.ParseNDJSON(r.Body)
			if err != nil || len(records) == 0 || factmerge.VerifyIdentities(records) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			added, changed := 0, 0
			for _, rec := range records {
				if _, ok := fake.index[rec.ID]; ok {
					changed++
				} else {
					added++
				}
				fake.put(factsync.FeedItem{Record: rec, CreatedBy: fake.callerBy, CreatedByHandle: fake.callerHandle, UpdatedAt: "2026-10-07T12:00:00.000Z"})
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"added": added, "changed": changed, "unchanged": 0})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/brain/facts"):
			if fake.denyFeed {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			token := r.URL.Query().Get("pageToken")
			fake.feedTokens = append(fake.feedTokens, token)
			if token != "" && fake.rejectTokens > 0 {
				fake.rejectTokens--
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid pageToken"}`))
				return
			}
			start := 0
			if token != "" {
				n, err := strconv.Atoi(token)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				start = n
			}
			pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
			if pageSize < 1 || pageSize > 100 {
				pageSize = 50
			}
			end := min(start+pageSize, len(fake.items))
			var page []factsync.FeedItem
			if start < end {
				page = fake.items[start:end]
			}
			resp := map[string]any{"items": page, "nextPageToken": nil}
			if len(page) > 0 {
				resp["nextPageToken"] = strconv.Itoa(end)
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hostedSyncTestFact(text string, now time.Time) factRecord {
	return hostedSyncAnchoredFact(text, now, factmerge.Anchor{SessionID: "s"})
}

func hostedSyncAnchoredFact(text string, now time.Time, anchors ...factmerge.Anchor) factRecord {
	paths := normalizeFactPaths([]string{"architecture.sync.wire"})
	if len(paths) == 0 {
		panic("fixture taxonomy path must be valid")
	}
	return factRecord{
		ID:         factRecordID(text, paths),
		Paths:      paths,
		Text:       text,
		Branch:     "main",
		Origin:     factstore.OriginAuthored,
		Status:     factStatusActive,
		Provenance: anchors,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// hostedSyncHarness wires a temp brain dir and cache, a binding to the fake
// server, and a runner that mints a fake token (sub "ulrich") and fails git
// calls. The last return is the fake server's base URL (the bound target).
func hostedSyncHarness(t *testing.T, fake *hostedFake, bind bool) (Options, repoStorage, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	brainDir := t.TempDir()
	storage := repoStorage{Key: "test/repo", BrainDir: brainDir, HeadPath: filepath.Join(t.TempDir(), "HEAD")}
	if fake.callerBy == "" {
		fake.callerBy = "acct-caller"
	}
	if fake.callerHandle == "" {
		fake.callerHandle = "caller"
	}
	srv := newHostedFakeServer(t, fake)
	if bind {
		if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: EntireEnv{RepoRoot: repoDir, PluginCacheDir: t.TempDir()}, Runner: runner, Now: func() time.Time { return now }}
	return opts, storage, repoDir, srv.URL
}

func TestHostedSyncGates(t *testing.T) {
	t.Run("NO_EGRESS wins over a present binding", func(t *testing.T) {
		fake := &hostedFake{}
		opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
		var errOut bytes.Buffer
		if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
			t.Fatal(err)
		}
		if fake.requestCount() != 0 {
			t.Fatalf("NO_EGRESS must make zero requests, got %d", fake.requestCount())
		}
	})
	t.Run("no binding means silent no-op", func(t *testing.T) {
		fake := &hostedFake{}
		opts, storage, repoDir, _ := hostedSyncHarness(t, fake, false)
		var errOut bytes.Buffer
		if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
			t.Fatal(err)
		}
		if fake.requestCount() != 0 {
			t.Fatalf("unconnected repo must make zero requests, got %d", fake.requestCount())
		}
		if errOut.Len() != 0 {
			t.Fatalf("unconnected repo must be silent, got %q", errOut.String())
		}
	})
}

func TestHostedSyncTokenMintFailureIsNonFatal(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("not logged in")
	})
	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("mint failure must be non-fatal, got %v", err)
	}
	if fake.requestCount() != 0 {
		t.Fatalf("mint failure must make zero requests, got %d", fake.requestCount())
	}
	if lines := strings.Count(strings.TrimRight(errOut.String(), "\n"), "\n") + 1; errOut.Len() == 0 || lines != 1 {
		t.Fatalf("expected exactly one stderr line, got %q", errOut.String())
	}
}

func TestHostedSyncPublishesAndPulls(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("local fact A", now)
	remoteFact := hostedSyncTestFact("remote fact B", now)
	fake := &hostedFake{}
	fake.seed(remoteFact, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{localFact}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}

	if !fake.has(localFact.ID) {
		t.Fatal("the local fact must be upserted to the hosted store")
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	var pulled *factRecord
	for i := range local {
		if local[i].ID == remoteFact.ID {
			pulled = &local[i]
		}
	}
	if pulled == nil {
		t.Fatalf("remote fact must be pulled into the local store, got %d records", len(local))
	}
	if pulled.Author != "teammate" {
		t.Fatalf("pulled fact must carry the server-stamped author, got %q", pulled.Author)
	}
	if err := checkHostedFactsBinding(storage.BrainDir, "main", "repo1", baseURL); err != nil {
		t.Fatalf("per-branch hosted binding must be recorded: %v", err)
	}
	if !strings.Contains(errOut.String(), "hosted sync: branch main") {
		t.Fatalf("expected a summary line, got %q", errOut.String())
	}
}

func TestHostedSyncEmptyStoreSkipsPush(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	remoteFact := hostedSyncTestFact("remote fact B", now)
	fake := &hostedFake{}
	fake.seed(remoteFact, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if fake.upsertCount() != 0 {
		t.Fatalf("an empty store must not post an upsert, got %d calls", fake.upsertCount())
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 || local[0].ID != remoteFact.ID {
		t.Fatalf("pull must still fill the store, got %+v", local)
	}
}

func TestHostedSyncFilterDropsUnreachable(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	dropped := hostedSyncAnchoredFact("foreign unmerged fact", now, factmerge.Anchor{SessionID: "s1", Commit: "deadbeef"})
	keptViaTrail := hostedSyncAnchoredFact("foreign merged fact", now, factmerge.Anchor{SessionID: "s2", Commit: "deadbeef"})
	fake := &hostedFake{}
	fake.seed(dropped, "", "acct-teammate", "teammate")
	fake.seed(keptViaTrail, "trail-7", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, rec := range local {
		ids[rec.ID] = true
	}
	if ids[dropped.ID] {
		t.Fatal("a foreign fact with no reachable anchor and no trail must be filtered out")
	}
	if !ids[keptViaTrail.ID] {
		t.Fatal("a merged trail id marks the fact main-visible even with unreachable commits")
	}
}

func TestHostedSyncMemberFactsFollowTheAccount(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	// fakeJWT's sub is "ulrich": the member's own fact, pushed from another
	// machine, carries their account id and an unreachable commit anchor.
	ownFact := hostedSyncAnchoredFact("my fact from my laptop", now, factmerge.Anchor{SessionID: "s1", Commit: "deadbeef"})
	fake := &hostedFake{}
	fake.seed(ownFact, "", "ulrich", "ulrich")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 || local[0].ID != ownFact.ID {
		t.Fatalf("the member's own facts must follow their account, got %+v", local)
	}
}

func TestHostedSyncCursorResumes(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	first := hostedSyncTestFact("first remote fact", now)
	fake := &hostedFake{}
	fake.seed(first, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	second := hostedSyncTestFact("second remote fact", now)
	fake.seed(second, "trail-1", "acct-teammate", "teammate")

	before := len(fake.feedTokensSeen())
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	for _, token := range fake.feedTokensSeen()[before:] {
		if token == "" {
			t.Fatal("the second sync must resume from the stored cursor, not replay from the start")
		}
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, rec := range local {
		ids[rec.ID] = true
	}
	if !ids[first.ID] || !ids[second.ID] {
		t.Fatalf("both feed facts must be local after the second sync, got %v", ids)
	}
}

func TestHostedSyncCursorResetOnRejectedToken(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	first := hostedSyncTestFact("first remote fact", now)
	fake := &hostedFake{}
	fake.seed(first, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	second := hostedSyncTestFact("second remote fact", now)
	fake.seed(second, "trail-1", "acct-teammate", "teammate")
	fake.mu.Lock()
	fake.rejectTokens = 1
	fake.mu.Unlock()

	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, rec := range local {
		ids[rec.ID] = true
	}
	if !ids[first.ID] || !ids[second.ID] {
		t.Fatalf("a rejected cursor must trigger a full refetch that converges, got %v", ids)
	}
}

func TestHostedSyncRemovesFilterRejectedCachedFacts(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("this machine's unsynced fact", now)
	teammate := hostedSyncAnchoredFact("teammate fact on a recycled branch", now, factmerge.Anchor{SessionID: "s1", Commit: "c1"})
	fake := &hostedFake{denyPush: true} // keep localFact out of the hosted feed
	fake.seed(teammate, "", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{localFact}); err != nil {
		t.Fatal(err)
	}
	reachable := true
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		if name == "git" && len(args) == 4 && args[0] == "merge-base" && args[1] == "--is-ancestor" && args[2] == "c1" && reachable {
			return nil, nil, nil
		}
		return nil, nil, errors.New("not available")
	})

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, rec := range local {
		ids[rec.ID] = true
	}
	if !ids[teammate.ID] || !ids[localFact.ID] {
		t.Fatalf("first sync must hold both facts, got %v", ids)
	}

	// HEAD moved (recycle/rename): the anchor commit is no longer an ancestor.
	reachable = false
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err = factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids = map[string]bool{}
	for _, rec := range local {
		ids[rec.ID] = true
	}
	if ids[teammate.ID] {
		t.Fatal("a cached fact that no longer passes the filter must leave the branch store")
	}
	if !ids[localFact.ID] {
		t.Fatal("this machine's own unsynced fact must survive the rebuild")
	}
}

func TestPullPreservesLocalStatus(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	retracted := hostedSyncTestFact("contested fact", now)
	retracted.Status = factStatusRetracted
	remoteActive := hostedSyncTestFact("contested fact", now)

	fake := &hostedFake{}
	fake.seed(remoteActive, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{retracted}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range local {
		if rec.ID == retracted.ID && rec.Status != factStatusRetracted {
			t.Fatalf("local retraction must survive the pull, got status %q", rec.Status)
		}
	}
}

func TestPullRefusesForgedFeedWithoutWrite(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("local fact A", now)
	forged := hostedSyncTestFact("remote fact B", now)
	forged.ID = "fact:forged-id-that-does-not-match-content"

	fake := &hostedFake{}
	fake.mu.Lock()
	fake.put(factsync.FeedItem{Record: forged, TrailID: "trail-1", CreatedBy: "acct-teammate", CreatedByHandle: "teammate"})
	fake.mu.Unlock()
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{localFact}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(storage.BrainDir, filepath.FromSlash(factstore.FileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("sync failure must be non-fatal, got %v", err)
	}
	after, err := os.ReadFile(filepath.Join(storage.BrainDir, filepath.FromSlash(factstore.FileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a forged feed must leave the local store byte-identical")
	}
	if errOut.Len() == 0 {
		t.Fatal("a refused feed must leave a stderr note")
	}
}

func TestPullServerAttributionBeatsBlobAuthor(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	forgedAuthor := hostedSyncTestFact("remote fact with forged author", now)
	forgedAuthor.Author = "mallory"
	fake := &hostedFake{}
	fake.seed(forgedAuthor, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range local {
		if rec.ID == forgedAuthor.ID && rec.Author != "teammate" {
			t.Fatalf("the server-stamped attribution must beat a blob-carried author, got %q", rec.Author)
		}
	}
}

// TestHostedSyncNeverConnectsWithoutBinding pins the default-off model: the
// ONLY ways a repo starts syncing are `entire brain connect` and the (future)
// onboarding flow, both of which write the binding in the foreground. Even
// with placement resolvable and a token available, a background trigger with
// no binding must stay local, write nothing, and say nothing.
func TestHostedSyncNeverConnectsWithoutBinding(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, false)
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			return []byte(`{"repoId":"repo1","apiUrl":"` + baseURL + `"}` + "\n"), nil, nil
		}
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if fake.requestCount() != 0 {
		t.Fatalf("an unbound repo must make zero hosted requests, got %d", fake.requestCount())
	}
	if _, present, _ := brainstore.ReadHostedRepoBinding(storage.BrainDir); present {
		t.Fatal("a background trigger must never write the binding")
	}
	if errOut.Len() != 0 {
		t.Fatalf("staying local is the default, not an event: %q", errOut.String())
	}
}

// TestHostedSyncSkipsCorruptBindingWithoutReconnect pins the privacy rule
// that killed the old corrupt-binding self-heal: an unreadable binding may
// hide a recorded opt-out (`entire brain disconnect`), so NO read error may
// fall through to auto-connect. Skip loudly and leave the file alone; the
// remedy is an explicit `entire brain connect`.
func TestHostedSyncSkipsCorruptBindingWithoutReconnect(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, false)
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			return []byte(`{"repoId":"repo1","apiUrl":"` + baseURL + `"}` + "\n"), nil, nil
		}
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	bindingPath := filepath.Join(storage.BrainDir, brainstore.HostedRepoBindingFile)
	if err := os.WriteFile(bindingPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if fake.requestCount() != 0 {
		t.Fatalf("a corrupt binding must not sync anywhere, got %d requests", fake.requestCount())
	}
	data, err := os.ReadFile(bindingPath)
	if err != nil || string(data) != "{not json" {
		t.Fatalf("a corrupt binding must survive untouched, got %q err=%v", data, err)
	}
	if !strings.Contains(errOut.String(), "skipped") {
		t.Fatalf("skip must be reported: %q", errOut.String())
	}
}

// TestHostedSyncSkipsUnreadableBindingWithoutRewrite bounds the self-heal:
// only a PARSE error (corrupt JSON, content unrecoverable) may fall through to
// re-resolution. Any other read failure says nothing about what the binding
// holds — rewriting it could silently replace an explicitly configured custom
// target with default placement and redirect publishes to another repo.
func TestHostedSyncSkipsUnreadableBindingWithoutRewrite(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, false)
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			return []byte(`{"repoId":"repo1","apiUrl":"` + baseURL + `"}` + "\n"), nil, nil
		}
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	// A directory at the binding path fails the read with a non-parse error on
	// every platform; chmod 0o000 is a read no-op on Windows.
	bindingPath := filepath.Join(storage.BrainDir, brainstore.HostedRepoBindingFile)
	if err := os.Mkdir(bindingPath, 0o700); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if fake.requestCount() != 0 {
		t.Fatalf("an unreadable binding must not sync anywhere, got %d requests", fake.requestCount())
	}
	if info, err := os.Stat(bindingPath); err != nil || !info.IsDir() {
		t.Fatalf("unreadable binding must survive untouched: info=%v err=%v", info, err)
	}
	out := errOut.String()
	if !strings.Contains(out, "skipped") || strings.Contains(out, "re-resolving") {
		t.Fatalf("a read failure must skip without re-resolution: %q", out)
	}
}

func TestHostedSyncOptedOutSkipsSyncAndAutoConnect(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, false)
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: baseURL, OptedOut: true}); err != nil {
		t.Fatal(err)
	}
	shellOuts := 0
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		shellOuts++
		return nil, nil, errors.New("not available")
	})

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if shellOuts != 0 || fake.requestCount() != 0 {
		t.Fatalf("an opted-out repo must make no shell-outs and no requests, got shellOuts=%d requests=%d", shellOuts, fake.requestCount())
	}
	if errOut.Len() != 0 {
		t.Fatalf("an opted-out repo must be silent, got %q", errOut.String())
	}
}

func TestSetHostedOptOutKeepsCoordinatesAndHealsCorruption(t *testing.T) {
	brainDir := t.TempDir()
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: "https://cell.api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := brainstore.SetHostedOptOut(brainDir); err != nil {
		t.Fatal(err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(brainDir)
	if err != nil || !present || !binding.OptedOut || binding.RepoID != "repo1" {
		t.Fatalf("opt-out must keep the coordinates: present=%v err=%v binding=%+v", present, err, binding)
	}

	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, brainstore.HostedRepoBindingFile, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := brainstore.SetHostedOptOut(brainDir); err != nil {
		t.Fatalf("a corrupt state file must not block the opt-out, got %v", err)
	}
	binding, present, err = brainstore.ReadHostedRepoBinding(brainDir)
	if err != nil || !present || !binding.OptedOut {
		t.Fatalf("corrupt file must be replaced by a bare opt-out: present=%v err=%v binding=%+v", present, err, binding)
	}
}

func TestHostedSyncNoEgressBlocksAutoConnect(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, false)
	placementCalls := 0
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		placementCalls++
		return nil, nil, errors.New("not available")
	})
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if placementCalls != 0 || fake.requestCount() != 0 {
		t.Fatalf("NO_EGRESS must block even placement resolution, got placement=%d requests=%d", placementCalls, fake.requestCount())
	}
}

func TestPullAppliesRemoteRetirement(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localActive := hostedSyncTestFact("fact the team retired", now)
	remoteRetired := localActive
	remoteRetired.Status = factStatusRetracted
	remoteRetired.UpdatedAt = now.Add(time.Hour)

	fake := &hostedFake{}
	fake.seed(remoteRetired, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{localActive}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range local {
		if rec.ID == localActive.ID && rec.Status != factStatusRetracted {
			t.Fatalf("a retirement settled in the hosted store must reach the local store, got status %q", rec.Status)
		}
	}
}

func TestPullNeverRevivesFromAnActiveFeed(t *testing.T) {
	// The inverse of TestPullAppliesRemoteRetirement: lifecycle moves one way.
	// (TestPullPreservesLocalStatus covers the full sync; this pins the pair.)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localRetired := hostedSyncTestFact("fact this member retired", now)
	localRetired.Status = factStatusRetracted
	remoteActive := hostedSyncTestFact("fact this member retired", now)

	fake := &hostedFake{}
	fake.seed(remoteActive, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{localRetired}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range local {
		if rec.ID == localRetired.ID && rec.Status != factStatusRetracted {
			t.Fatalf("the pull must never revive a locally retired fact, got status %q", rec.Status)
		}
	}
}

func TestHostedSyncFallsBackToPullOnlyWhenPublishDenied(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("local fact A", now)
	remoteFact := hostedSyncTestFact("remote fact B", now)
	fake := &hostedFake{denyPush: true}
	fake.seed(remoteFact, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{localFact}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("publish denial must be non-fatal, got %v", err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range local {
		if rec.ID == remoteFact.ID && rec.Author == "teammate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a denied publish must still pull teammates' facts, got %d local records", len(local))
	}
	// The branch did NOT sync: the marker stays absent so the blind-spot note
	// keeps firing until a full sync succeeds.
	if err := checkHostedFactsBinding(storage.BrainDir, "main", "repo1", baseURL); err == nil {
		t.Fatal("a pull-only run must not mark the branch as synced")
	}
	if !strings.Contains(errOut.String(), "pull-only") {
		t.Fatalf("expected a pull-only note on stderr, got %q", errOut.String())
	}
	// A read-only grant (push 403, pull 200) is a working degraded state, not
	// a stale binding: it must never trip the no-access pause (COR-2279).
	if binding := readBindingOrFatal(t, storage.BrainDir); !binding.NoAccessUntil.IsZero() {
		t.Fatalf("a push-only denial must not pause the repo, got %v", binding.NoAccessUntil)
	}
}

func TestPullOnlyRejectsAForgedFeed(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("local fact A", now)
	forged := hostedSyncTestFact("remote fact B", now)
	forged.ID = "fact:forged-id-that-does-not-match-content"
	fake := &hostedFake{denyPush: true}
	fake.mu.Lock()
	fake.put(factsync.FeedItem{Record: forged, TrailID: "trail-1", CreatedBy: "acct-teammate", CreatedByHandle: "teammate"})
	fake.mu.Unlock()
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{localFact}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(storage.BrainDir, filepath.FromSlash(factstore.FileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("must be non-fatal, got %v", err)
	}
	after, err := os.ReadFile(filepath.Join(storage.BrainDir, filepath.FromSlash(factstore.FileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a forged feed must leave the local store byte-identical on the pull-only path too")
	}
}

func TestPullRemovalSparesLocallyDistilledFacts(t *testing.T) {
	// Attribution collision: two members distill the same text+paths, the
	// teammate's push wins first-writer attribution, and this machine's copy —
	// session-anchored, commit unreachable — fails the filter. The local copy
	// carries a transcript anchor (local-only provenance SanitizeForEgress
	// strips on push), which proves it was distilled HERE: it must survive.
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	mine := hostedSyncAnchoredFact("independently distilled fact", now,
		factmerge.Anchor{SessionID: "local-session", Transcript: "export/sessions/local.ndjson", Line: 3})
	theirs := hostedSyncAnchoredFact("independently distilled fact", now,
		factmerge.Anchor{SessionID: "their-session"})
	fake := &hostedFake{denyPush: true}
	fake.seed(theirs, "", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{mine}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range local {
		if rec.ID == mine.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a locally distilled fact must survive the removal pass even when the feed's copy fails the filter")
	}
}

func TestPullRemovalDisabledWithoutMemberIdentity(t *testing.T) {
	// A token whose sub claim cannot be read disables the member rule; with it
	// the removal pass must stand down too, or every previously pushed
	// session-anchored fact on this machine would be silently evicted.
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cached := hostedSyncAnchoredFact("fact this machine pushed earlier", now, factmerge.Anchor{SessionID: "s1"})
	fake := &hostedFake{}
	fake.seed(cached, "", "ulrich", "ulrich")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{cached}); err != nil {
		t.Fatal(err)
	}
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte("aa.bb.cc\n"), nil, nil // passes looksLikeJWT, sub unreadable
		}
		return nil, nil, errors.New("not available")
	})

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range local {
		if rec.ID == cached.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("with no member identity the removal pass must stand down, not evict the machine's facts")
	}
}

func TestPullRemovalSparesLocallyAnchoredAuthoredFacts(t *testing.T) {
	// Bugbot PR#403: an authored fact has session/commit anchors but no
	// transcript, so the distill stand-down does not cover it. When a teammate
	// independently filed the same text+paths first, the feed copy carries
	// THEIR anchors and fails the filter — but the local copy's own commit
	// anchor is reachable from HEAD and must keep the record alive.
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	mine := hostedSyncAnchoredFact("independently authored fact", now,
		factmerge.Anchor{SessionID: "local-session", Commit: "c-local"})
	theirs := hostedSyncAnchoredFact("independently authored fact", now,
		factmerge.Anchor{SessionID: "their-session"})
	fake := &hostedFake{denyPush: true} // keep the local copy's anchors out of the feed
	fake.seed(theirs, "", "acct-teammate", "teammate")
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{mine}); err != nil {
		t.Fatal(err)
	}
	opts.Runner = clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		if name == "git" && len(args) == 4 && args[0] == "merge-base" && args[1] == "--is-ancestor" && args[2] == "c-local" {
			return nil, nil, nil
		}
		return nil, nil, errors.New("not available")
	})

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range local {
		if rec.ID == mine.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a local record whose own anchors pass the filter must survive the removal pass")
	}
}

func TestHostedSyncMarkerRequiresSuccessfulPull(t *testing.T) {
	// Bugbot PR#403: the marker means "this branch synced against this
	// target". An empty-store push succeeds without contacting the server, so
	// a failed pull must leave the marker unwritten or the blind-spot note is
	// silenced on a checkout that never exchanged anything.
	fake := &hostedFake{denyFeed: true}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("pull failure must be non-fatal, got %v", err)
	}
	if err := checkHostedFactsBinding(storage.BrainDir, "main", "repo1", baseURL); err == nil {
		t.Fatal("a failed pull must not mark the branch as synced")
	}
	if !strings.Contains(errOut.String(), "pull failed") {
		t.Fatalf("expected a pull failure note, got %q", errOut.String())
	}
}

func TestPushSkipsUnverifiableRecordsAndUploadsTheRest(t *testing.T) {
	// One bad record must not hold the rest of the store hostage: records
	// whose content id does not re-derive are skipped with a note, everything
	// else uploads, and the sync still counts as completed.
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	good := hostedSyncTestFact("healthy local fact", now)
	corrupt := hostedSyncTestFact("corrupted local fact", now)
	corrupt.ID = "fact:id-that-does-not-match-content"
	fake := &hostedFake{}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{good, corrupt}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if !fake.has(good.ID) {
		t.Fatal("the verifiable record must be uploaded")
	}
	if fake.has(corrupt.ID) {
		t.Fatal("an unverifiable record must never leave the machine")
	}
	if !strings.Contains(errOut.String(), "skipped 1 unpublishable") {
		t.Fatalf("the skip must be reported, got %q", errOut.String())
	}
	if err := checkHostedFactsBinding(storage.BrainDir, "main", "repo1", baseURL); err != nil {
		t.Fatalf("a salvage push that uploaded everything publishable still completes the sync: %v", err)
	}
}

func TestPushWithNothingPublishableIsPullOnly(t *testing.T) {
	// Every record unverifiable → nothing was shared; the branch must not be
	// marked as synced.
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	corrupt := hostedSyncTestFact("corrupted local fact", now)
	corrupt.ID = "fact:id-that-does-not-match-content"
	fake := &hostedFake{}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{corrupt}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if fake.upsertCount() != 0 {
		t.Fatalf("nothing publishable must mean no upsert, got %d calls", fake.upsertCount())
	}
	if err := checkHostedFactsBinding(storage.BrainDir, "main", "repo1", baseURL); err == nil {
		t.Fatal("a store that shared nothing must not be marked synced")
	}
}

// repoViewRunner mints tokens like the harness runner and serves `entire repo
// view --json` with the given placement; everything else (git, version) fails,
// which passes the host-CLI version gate.
func repoViewRunner(repoID, apiURL string) clitest.CommandRunnerFunc {
	return clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			return []byte(`{"repoId":"` + repoID + `","apiUrl":"` + apiURL + `"}`), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
}

func readBindingOrFatal(t *testing.T, brainDir string) brainstore.HostedRepoBinding {
	t.Helper()
	binding, present, err := brainstore.ReadHostedRepoBinding(brainDir)
	if err != nil || !present {
		t.Fatalf("binding must stay readable: present=%v err=%v", present, err)
	}
	return binding
}

// A denied pull means the binding itself is stale (deleted repo or lost
// grant). When placement does not resolve to a replacement repo, the sync
// records a pause so background triggers stop feeding the server 403s, says
// so once, and stays silent until the pause passes (COR-2279).
func TestHostedSyncPullNoAccessPausesRepo(t *testing.T) {
	fake := &hostedFake{noAccessRepo: "repo1"}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("a denied pull must be non-fatal, got %v", err)
	}
	if !strings.Contains(errOut.String(), "no access") || !strings.Contains(errOut.String(), "connect") {
		t.Fatalf("want one actionable no-access line naming connect, got %q", errOut.String())
	}
	binding := readBindingOrFatal(t, storage.BrainDir)
	wantUntil := opts.Now().UTC().Add(hostedNoAccessBackoff)
	if !binding.NoAccessUntil.Equal(wantUntil) {
		t.Fatalf("NoAccessUntil = %v, want %v", binding.NoAccessUntil, wantUntil)
	}
	if binding.RepoID != "repo1" {
		t.Fatalf("a pause must not change the bound repo, got %q", binding.RepoID)
	}

	// While paused: zero requests, zero noise — same silence as the opt-out
	// gate. Status carries the explanation instead.
	before := fake.requestCount()
	var quiet bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &quiet, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if fake.requestCount() != before {
		t.Fatalf("a paused repo must make zero requests, got %d more", fake.requestCount()-before)
	}
	if quiet.Len() != 0 {
		t.Fatalf("a paused repo must be silent, got %q", quiet.String())
	}
	hosted, _ := buildHostedStatus(storage.BrainDir, nil, opts.Now().UTC())
	if hosted.NoAccessUntil.IsZero() || !strings.Contains(hostedStatusLine(hosted), "paused") {
		t.Fatalf("status must surface the pause, got %+v / %q", hosted, hostedStatusLine(hosted))
	}

	// After the pause: the gate reopens on its own.
	opts.Now = func() time.Time { return wantUntil.Add(time.Minute) }
	if _, err := hostedFactsSyncAndPull(context.Background(), &quiet, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	if fake.requestCount() == before {
		t.Fatal("an expired pause must let the sync try again")
	}
}

// Deleted-and-reonboarded: placement resolves the same origin to a DIFFERENT
// repo id in the SAME cell. The sync rebinds and finishes against the new
// repo instead of pausing — the recorded consent carries over.
func TestHostedSyncNoAccessRebindsReonboardedRepo(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	remoteFact := hostedSyncTestFact("remote fact B", now)
	fake := &hostedFake{noAccessRepo: "repo1"}
	fake.seed(remoteFact, "trail-1", "acct-teammate", "teammate")
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)
	opts.Runner = repoViewRunner("repo2", baseURL)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	binding := readBindingOrFatal(t, storage.BrainDir)
	if binding.RepoID != "repo2" || binding.BaseURL != baseURL {
		t.Fatalf("binding = %+v, want repo2 at the same cell", binding)
	}
	if !binding.NoAccessUntil.IsZero() {
		t.Fatalf("a successful rebind must not pause, got %v", binding.NoAccessUntil)
	}
	if !strings.Contains(errOut.String(), "rebound") {
		t.Fatalf("want a rebind note, got %q", errOut.String())
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range local {
		found = found || rec.ID == remoteFact.ID
	}
	if !found {
		t.Fatal("the retried pull against the new repo must land")
	}
	if err := checkHostedFactsBinding(storage.BrainDir, "main", "repo2", baseURL); err != nil {
		t.Fatalf("the per-branch marker must record the NEW repo: %v", err)
	}
}

// Placement that resolves to a different CELL never retargets the binding: an
// explicit --repo-id/--api-url binding (staging, non-default cell) must not
// be silently moved to the resolved placement. It pauses instead.
func TestHostedSyncRebindRefusesDifferentCell(t *testing.T) {
	fake := &hostedFake{noAccessRepo: "repo1"}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	opts.Runner = repoViewRunner("repo2", "https://other-cell.example")

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	binding := readBindingOrFatal(t, storage.BrainDir)
	if binding.RepoID != "repo1" {
		t.Fatalf("a cross-cell resolution must not rebind, got %q", binding.RepoID)
	}
	if binding.NoAccessUntil.IsZero() {
		t.Fatal("a refused rebind must still pause the repo")
	}
}

// A rebind that lands on a repo that ALSO denies access pauses instead of
// resolving placement again — one re-resolution per run.
func TestHostedSyncReboundRepoStillDeniedPauses(t *testing.T) {
	// This server denies every repo id, so repo1 (bound) and repo2 (rebound)
	// both 403.
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(deny.Close)
	opts, storage, repoDir, _ := hostedSyncHarness(t, &hostedFake{}, false)
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: deny.URL}); err != nil {
		t.Fatal(err)
	}
	opts.Runner = repoViewRunner("repo2", deny.URL)

	var errOut bytes.Buffer
	if _, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	binding := readBindingOrFatal(t, storage.BrainDir)
	if binding.RepoID != "repo2" {
		t.Fatalf("the rebind itself should have landed, got %q", binding.RepoID)
	}
	if binding.NoAccessUntil.IsZero() {
		t.Fatal("a denied rebound repo must pause rather than resolve placement again")
	}
}

// A pause whose guarded write declines — the binding changed hands between
// the denial and the write (concurrent connect/disconnect) — must not claim
// "sync paused": no pause was recorded and the new binding owns the state.
func TestPauseNoAccessSilentWhenBindingChanged(t *testing.T) {
	opts, storage, _, baseURL := hostedSyncHarness(t, &hostedFake{}, true)

	var errOut bytes.Buffer
	pauseHostedSyncNoAccess(&errOut, opts, storage.BrainDir, brainstore.HostedRepoBinding{RepoID: "stale-repo", BaseURL: baseURL})
	if errOut.Len() != 0 {
		t.Fatalf("a declined pause write must print nothing, got %q", errOut.String())
	}
	binding := readBindingOrFatal(t, storage.BrainDir)
	if !binding.NoAccessUntil.IsZero() {
		t.Fatalf("the current binding must not be paused, got %v", binding.NoAccessUntil)
	}
}

// Once the pause expires and the grant is back, the next successful pull must
// retire the recorded pause — and status must never report an expired pause
// as active in the meantime (PR #535 review finding).
func TestHostedSyncSuccessClearsExpiredNoAccessPause(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)
	expired := opts.Now().UTC().Add(-time.Minute)
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{
		RepoID: "repo1", BaseURL: baseURL, NoAccessUntil: expired,
	}); err != nil {
		t.Fatal(err)
	}

	hosted, _ := buildHostedStatus(storage.BrainDir, nil, opts.Now().UTC())
	if !hosted.NoAccessUntil.IsZero() {
		t.Fatalf("status must not report an expired pause as active, got %v", hosted.NoAccessUntil)
	}

	var errOut bytes.Buffer
	outcome, err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.PullSucceeded {
		t.Fatalf("an expired pause must not block the sync: %q", errOut.String())
	}
	if binding := readBindingOrFatal(t, storage.BrainDir); !binding.NoAccessUntil.IsZero() {
		t.Fatalf("a successful pull must retire the pause, got %v", binding.NoAccessUntil)
	}
}
