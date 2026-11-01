package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestTreePathOIDsParsesLsTreeOutput(t *testing.T) {
	out := "100644 blob aaaa111\tcheckpoints/aa/meta.json\n" +
		"100644 blob bbbb222\tdir with space/file.json\n" +
		"garbage line without tab\n"
	oids := treePathOIDs([]byte(out))
	if oids["checkpoints/aa/meta.json"] != "aaaa111" {
		t.Fatalf("oid for meta.json = %q", oids["checkpoints/aa/meta.json"])
	}
	if oids["dir with space/file.json"] != "bbbb222" {
		t.Fatalf("oid for spaced path = %q", oids["dir with space/file.json"])
	}
	if len(oids) != 2 {
		t.Fatalf("unexpected oid count: %+v", oids)
	}
}

func TestCheckpointBlobReaderUsesCache(t *testing.T) {
	// A populated prev cache must satisfy reads without ever shelling out to
	// git. The runner has no responses, so any cat-file call errors and fails
	// the test — proving the cache short-circuits the subprocess.
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cache := newCheckpointMetadataCache(map[string][]byte{
		"oid-1": []byte(`{"session_id":"cached"}`),
	})
	reader := &checkpointBlobReader{
		runner: runner,
		gitDir: "/repo",
		ref:    v1MainRef,
		oids:   map[string]string{"aa/bb/metadata.json": "oid-1"},
		cache:  cache,
	}
	got, err := reader.read(context.Background(), "aa/bb/metadata.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"session_id":"cached"}` {
		t.Fatalf("cached read returned %q", got)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("cache hit still shelled out to git: %+v", runner.calls)
	}
	// Seen entries must be carried into next so they survive the next persist.
	if string(cache.next["oid-1"]) != `{"session_id":"cached"}` {
		t.Fatalf("seen entry not promoted to next: %+v", cache.next)
	}
}

func TestCheckpointBlobReaderMissFetchesAndRecords(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", "oid-9"): {stdout: `{"session_id":"fresh"}`},
	}}
	cache := newCheckpointMetadataCache(nil)
	reader := &checkpointBlobReader{
		runner: runner,
		gitDir: "/repo",
		ref:    v1MainRef,
		oids:   map[string]string{"aa/bb/metadata.json": "oid-9"},
		cache:  cache,
	}
	got, err := reader.read(context.Background(), "aa/bb/metadata.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"session_id":"fresh"}` {
		t.Fatalf("fresh read returned %q", got)
	}
	if string(cache.next["oid-9"]) != `{"session_id":"fresh"}` {
		t.Fatalf("fetched blob not recorded under its oid: %+v", cache.next)
	}
}

func TestCheckpointMetadataCacheRoundTripAndVersionGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.FromSlash(checkpointMetadataCachePath))

	cache := newCheckpointMetadataCache(nil)
	cache.active = true // a reader populated `next` this run, so the cache is persistable
	cache.next["oid-1"] = []byte("hello")
	saveCheckpointMetadataCache(path, cache)

	reloaded := loadCheckpointMetadataCache(path)
	if string(reloaded.prev["oid-1"]) != "hello" {
		t.Fatalf("round-trip lost blob: %+v", reloaded.prev)
	}

	// Clobber guard: an inactive cache (caching disabled this run) must NOT
	// overwrite the good on-disk cache with an empty blob map.
	inactive := newCheckpointMetadataCache(nil)
	saveCheckpointMetadataCache(path, inactive)
	if guarded := loadCheckpointMetadataCache(path); string(guarded.prev["oid-1"]) != "hello" {
		t.Fatalf("inactive cache clobbered good on-disk cache: %+v", guarded.prev)
	}

	// A version bump must invalidate the on-disk cache. Write a valid gzipped
	// payload so the version check (not the gzip decode) is what rejects it.
	bumped := checkpointMetadataCacheFile{Version: checkpointMetadataCacheVersion + 1, Blobs: map[string][]byte{"oid-2": []byte("stale")}}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(bumped); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := brainstore.WriteFileAtomic(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := loadCheckpointMetadataCache(path); len(got.prev) != 0 {
		t.Fatalf("version mismatch not discarded: %+v", got.prev)
	}
}

// The reader before version 2 could record a moved ref's bytes under an older
// blob's id, so entries it wrote cannot be trusted as content-addressed.
func TestCheckpointMetadataCacheDiscardsEntriesFromTheRacyReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), filepath.FromSlash(checkpointMetadataCachePath))
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	file := checkpointMetadataCacheFile{Version: 1, Blobs: map[string][]byte{"oid-1": []byte("possibly another blob's bytes")}}
	if err := json.NewEncoder(gz).Encode(file); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := brainstore.WriteFileAtomic(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := loadCheckpointMetadataCache(path); len(got.prev) != 0 {
		t.Fatalf("version 1 entries were trusted: %+v", got.prev)
	}
}

// Once a listing succeeded, a path it does not name is absent from that tree.
// Reading it through the ref would mix two versions of the checkpoint.
func TestCheckpointBlobReaderRefusesPathOutsideListing(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":aa/bb/metadata.json"): {stdout: `{"session_id":"from a later tree"}`},
	}}
	reader := &checkpointBlobReader{
		runner: runner,
		gitDir: "/repo",
		ref:    v1MainRef,
		oids:   map[string]string{"aa/bb/other.json": "oid-9"},
		cache:  newCheckpointMetadataCache(nil),
	}
	if got, err := reader.read(context.Background(), "aa/bb/metadata.json"); err == nil {
		t.Fatalf("read a path outside the listing through the ref: %q", got)
	}
}
