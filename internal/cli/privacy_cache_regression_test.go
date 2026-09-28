package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivacyVerifyRejectsMalformedAndFutureCachesWithoutMutation(t *testing.T) {
	encoded := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	zipped := func(data []byte) []byte {
		t.Helper()
		var out bytes.Buffer
		w := gzip.NewWriter(&out)
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	brokenCRC := zipped([]byte(`{"version":1,"files":{}}`))
	brokenCRC[len(brokenCRC)-8] ^= 0xff
	cases := []struct {
		name, rel, code string
		data            []byte
	}{
		{"overlay invalid JSON", historyShortTermPath, memoryErrStateCorrupt, []byte("{broken")},
		{"overlay absent map", historyShortTermPath, memoryErrStateCorrupt, encoded(shortTermIndex{Version: historyShortTermVersion})},
		{"overlay future version", historyShortTermPath, memoryErrUnsupportedVersion, encoded(shortTermIndex{Version: historyShortTermVersion + 1, ReconcilerVersion: historyShortTermReconcilerVersion, Files: map[string]shortTermFile{}})},
		{"overlay future reconciler", historyShortTermPath, memoryErrUnsupportedVersion, encoded(shortTermIndex{Version: historyShortTermVersion, ReconcilerVersion: historyShortTermReconcilerVersion + 1, Files: map[string]shortTermFile{}})},
		{"scan invalid gzip", historyScanCachePath, memoryErrStateCorrupt, []byte("not gzip")},
		{"scan checksum mismatch", historyScanCachePath, memoryErrStateCorrupt, brokenCRC},
		{"scan invalid JSON", historyScanCachePath, memoryErrStateCorrupt, zipped([]byte("{broken"))},
		{"scan absent map", historyScanCachePath, memoryErrStateCorrupt, zipped(encoded(historyScanCache{Version: historyScanCacheVersion}))},
		{"scan future version", historyScanCachePath, memoryErrUnsupportedVersion, zipped(encoded(historyScanCache{Version: historyScanCacheVersion + 1, Files: map[string]historyScanCacheEntry{}}))},
		{"distill invalid JSON", distillCachePath, memoryErrStateCorrupt, []byte("{broken")},
		{"distill absent map", distillCachePath, memoryErrStateCorrupt, encoded(distillCache{Version: distillCacheVersion})},
		{"distill future version", distillCachePath, memoryErrUnsupportedVersion, encoded(distillCache{Version: distillCacheVersion + 1, Sessions: map[string]string{}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, dir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
			if out, err := execute(t, NewRootCommand(opts), "privacy", "exclude", "secret-sess", "--json"); err != nil {
				t.Fatalf("exclude: %v %s", err, out)
			}
			path := filepath.Join(dir, filepath.FromSlash(tc.rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			before := privacyTreeDigest(t, dir)
			stdout, _, err := executeSplit(t, NewRootCommand(opts), "privacy", "verify", "--json")
			if err == nil || !strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), tc.rel) || stdout != "" {
				t.Fatalf("verification accepted %s: stdout=%q err=%v", tc.rel, stdout, err)
			}
			if after := privacyTreeDigest(t, dir); after != before {
				t.Fatal("verification mutated corrupt cache or committed data")
			}
		})
	}
}

func TestPrivacyRetentionReportsRetainedSharedFactCopies(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			opts, dir := privacyRegressionCommandFixture(t, time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))
			manifest, err := loadBrainManifest(dir)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Sources.Facts = &factSourceManifest{Branches: []string{"main"}, Facts: 2}
			if err := writeBrainManifestAndReadme(dir, *manifest); err != nil {
				t.Fatal(err)
			}
			facts := []factRecord{
				{ID: "fact:selected", Branch: "main", Text: "selected fact", Status: factStatusActive, Provenance: []factAnchor{{SessionID: "clean-sess"}}},
				{ID: "fact:retained", Branch: "main", Text: "retained fact", Status: factStatusActive, Provenance: []factAnchor{{SessionID: "secret-sess"}}},
			}
			if err := writeFacts(dir, "main", facts); err != nil {
				t.Fatal(err)
			}
			shared, err := gitmetaDirForKey(opts.Env, "gh/acme/privacy-regression")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(shared, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(shared, "shared-facts")
			if err := os.WriteFile(marker, []byte("shared copy must remain explicitly reported"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := privacyTreeDigest(t, shared)
			args := []string{"privacy", "retention", "--max-age", "25h", "--purge"}
			if format == "json" {
				args = append(args, "--json")
			}
			out, err := execute(t, NewRootCommand(opts), args...)
			if err != nil {
				t.Fatalf("retention: %v %s", err, out)
			}
			if format == "json" {
				var payload struct {
					Caveats  []string             `json:"caveats"`
					Sessions []retentionPlanEntry `json:"sessions"`
				}
				if err := json.Unmarshal([]byte(out), &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Caveats) != 1 || !strings.Contains(payload.Caveats[0], shared) || len(payload.Sessions) != 1 || payload.Sessions[0].SessionID != "clean-sess" {
					t.Fatalf("missing precise retention caveat: %+v", payload)
				}
			} else if !strings.Contains(out, "NOT purged:") || !strings.Contains(out, shared) || !strings.Contains(out, "no deletion semantics") {
				t.Fatalf("missing retention caveat: %s", out)
			}
			remaining, err := loadFacts(dir, "main")
			if err != nil || len(remaining) != 1 || remaining[0].ID != "fact:retained" {
				t.Fatalf("retention fact selection: %+v %v", remaining, err)
			}
			if privacyTreeDigest(t, shared) != before {
				t.Fatal("local retention silently mutated shared copies")
			}
		})
	}
}

func TestPrivacyRetentionRefusesUnresolvedRepoAndCorruptApplyPlan(t *testing.T) {
	for _, kind := range []string{"repository", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			opts, dir := privacyRegressionCommandFixture(t, time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))
			if kind == "repository" {
				opts.Env.RepoRoot = filepath.Join("relative", "unresolved-repository")
				opts.Runner = &fakeCommandRunner{}
			} else if err := os.WriteFile(filepath.Join(dir, exportManifestFileName), []byte("{corrupt retention source"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := purgeCommandDataDigest(t, dir)
			stdout, _, err := executeSplit(t, NewRootCommand(opts), "privacy", "retention", "--max-age", "1h", "--purge")
			if err == nil || stdout != "" {
				t.Fatalf("invalid %s retention succeeded: %q %v", kind, stdout, err)
			}
			if kind == "manifest" && !strings.Contains(err.Error(), "manifest") {
				t.Fatalf("did not reach invalid manifest: %v", err)
			}
			if purgeCommandDataDigest(t, dir) != before {
				t.Fatal("refused retention changed committed data")
			}
		})
	}
}
