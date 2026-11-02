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
	for _, version := range []int{10, 11} {
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
			if version == 11 && wire.ExtractorVersion != 11 {
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
			if (len(got.Files) == 1) != (version == 11) {
				t.Fatalf("wrong parser cache reuse for %d: %+v", version, got)
			}
			inspected, err := loadHistoryScanCacheForPrivacy(root)
			if err != nil || len(inspected.Files) != 1 {
				t.Fatalf("privacy missed old records: %+v, %v", inspected, err)
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
