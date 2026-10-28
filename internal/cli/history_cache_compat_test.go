package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestHistoryScanCacheWireCompatibilityAndExtractionInvalidation(t *testing.T) {
	for _, version := range []int{10, historyScanCacheVersion} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			root := t.TempDir()
			cache := historyScanCache{Version: version, Files: map[string]historyScanCacheEntry{"sessions/main/fixture.jsonl": {ContentDigest: "unchanged"}}}
			data, err := encodeHistoryScanCache(cache)
			if err != nil {
				t.Fatal(err)
			}
			gz, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			var wire historyScanCache
			if err := json.NewDecoder(gz).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if wire.Version != 10 {
				t.Fatalf("stable clients cannot read version %d", wire.Version)
			}
			if version == historyScanCacheVersion && wire.ExtractorVersion != historyScanCacheVersion {
				t.Fatal("lost extraction revision")
			}
			path := filepath.Join(root, historyScanCachePath)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got := loadHistoryScanCache(root)
			if (len(got.Files) == 1) != (version == historyScanCacheVersion) {
				t.Fatalf("wrong parser cache reuse for %d: %+v", version, got)
			}
			// A released writer drops the unknown parser revision. Upgrade must rescan.
			wire.ExtractorVersion = 0
			data, err = encodeHistoryScanCache(wire)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if len(loadHistoryScanCache(root).Files) != 0 {
				t.Fatal("old writer cache reused by new parser")
			}
		})
	}
}

// A cache written by the previous extractor (wire 10, extractor_version 11:
// compact dialect read, but tool_use.result output not mined) must be rescanned
// rather than reused by extractor 12.
func TestHistoryScanCachePreviousExtractorRevisionRescans(t *testing.T) {
	root := t.TempDir()
	cache := historyScanCache{Version: historyScanCacheWireVersion, ExtractorVersion: historyScanCacheVersion - 1, Files: map[string]historyScanCacheEntry{"sessions/main/fixture.jsonl": {ContentDigest: "unchanged"}}}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(cache); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, historyScanCachePath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadHistoryScanCache(root); len(got.Files) != 0 {
		t.Fatalf("extractor %d cache reused by extractor %d: %+v", historyScanCacheVersion-1, historyScanCacheVersion, got)
	}
}
