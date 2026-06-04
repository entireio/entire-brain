package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
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
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":aa/bb/metadata.json"): {stdout: `{"session_id":"fresh"}`},
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
	cache.next["oid-1"] = []byte("hello")
	saveCheckpointMetadataCache(path, cache)

	reloaded := loadCheckpointMetadataCache(path)
	if string(reloaded.prev["oid-1"]) != "hello" {
		t.Fatalf("round-trip lost blob: %+v", reloaded.prev)
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
	if err := writeFileAtomic(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := loadCheckpointMetadataCache(path); len(got.prev) != 0 {
		t.Fatalf("version mismatch not discarded: %+v", got.prev)
	}
}
