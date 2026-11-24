package cli

import (
	"bytes"
	"context"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Removing settled digest reuse would make an unchanged failure repeatedly
// invoke the scanner and keep watch repair active.
func TestShortTermSettlesUnchangedScanFailure(t *testing.T) {
	dir, _, failed := shortTermFixture(t)
	scans := 0
	scan := func(ctx context.Context, r io.ReadSeeker, path, rel string) ([]historyRecord, int, []string, bool) {
		if rel == failed {
			scans++
			return nil, 0, []string{"history records skipped for " + rel + ": deterministic parse failure"}, false
		}
		return scanSessionFileReaderRecords(ctx, r, path, rel)
	}
	first, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.SettledFailures) != 1 {
		t.Fatalf("missing diagnostic: %+v", first)
	}
	for _, reason := range first.SettledFailures {
		if !strings.Contains(reason, failed) {
			t.Fatalf("diagnostic omits failed path: %q", reason)
		}
	}
	if first.Failed != 0 {
		t.Fatalf("stable parse failure is repairable: %+v", first)
	}
	_, err = buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan)
	if err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatalf("unchanged failure scanned %d times, want 1", scans)
	}
	cache := loadHistoryScanCache(dir)
	if cache.Failures[failed].ContentDigest == "" {
		t.Fatal("failure digest not persisted")
	}
	target := filepath.Join(dir, filepath.FromSlash(failed))
	if err := os.Chtimes(target, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatal("same-content touch retried")
	}
	if err := os.WriteFile(target, []byte("changed bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err != nil {
		t.Fatal(err)
	}
	if scans != 2 {
		t.Fatal("changed digest did not retry")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err != nil {
		t.Fatal(err)
	}
	if len(loadHistoryScanCache(dir).Failures) != 0 {
		t.Fatal("removed failure retained")
	}
}

func TestSettledFailureDoctorReasons(t *testing.T) {
	dir, _, _ := shortTermFixture(t)
	cache := loadHistoryScanCache(dir)
	cache.Failures = map[string]historyScanFailure{"sessions/bad.jsonl": {ContentDigest: "sha256:fixture", Reason: "sessions/bad.jsonl: parse failed"}}
	saveHistoryScanCache(dir, cache)
	checks := settledHistoryFailureChecks(dir)
	if len(checks) != 1 || !strings.Contains(checks[0].Detail, "sessions/bad.jsonl: parse failed") || checks[0].State != "warn" {
		t.Fatalf("settled failure diagnostics = %+v", checks)
	}
}

func TestSettledFailureRefusalDoesNotPublish(t *testing.T) {
	dir, _, failed := shortTermFixture(t)
	before, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(historyScanCachePath)))
	if err != nil {
		t.Fatal(err)
	}
	old := beforeHistoryShortTermInventoryRecheck
	beforeHistoryShortTermInventoryRecheck = func() { _ = os.Remove(filepath.Join(dir, filepath.FromSlash(failed))) }
	t.Cleanup(func() { beforeHistoryShortTermInventoryRecheck = old })
	scan := func(ctx context.Context, r io.ReadSeeker, path, rel string) ([]historyRecord, int, []string, bool) {
		return nil, 0, []string{rel + ": parse failed"}, false
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err == nil {
		t.Fatal("membership change did not refuse")
	}
	after, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(historyScanCachePath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("refused delta published failure cache")
	}
}

func TestSettledFailurePublicationFailureDoesNotSettle(t *testing.T) {
	dir, _, _ := shortTermFixture(t)
	overlay := filepath.Join(dir, filepath.FromSlash(historyShortTermPath))
	if err := os.MkdirAll(overlay, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "block"), []byte("block rename"), 0600); err != nil {
		t.Fatal(err)
	}
	scan := func(ctx context.Context, r io.ReadSeeker, path, rel string) ([]historyRecord, int, []string, bool) {
		return nil, 0, []string{rel + ": parse failed"}, false
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err == nil {
		t.Fatal("overlay publication failure was ignored")
	}
	if len(loadHistoryScanCache(dir).Failures) == 0 {
		t.Fatal("failure identity must persist before overlay publication")
	}
}

func TestSettledFailureFullBuildParityAndVersionRetry(t *testing.T) {
	dir, _, failed := shortTermFixture(t)
	scans := 0
	scan := func(ctx context.Context, r io.ReadSeeker, path, rel string) ([]historyRecord, int, []string, bool) {
		if rel == failed {
			scans++
			return nil, 0, []string{rel + ": deterministic parse failure"}, false
		}
		return scanSessionFileReaderRecords(ctx, r, path, rel)
	}
	manifest, err := bmanifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	index, _, cache, err := buildBrainHistoryIndexSnapshotFromStoreWithScanner(context.Background(), dir, time.Now(), nil, manifest, nil, scan)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.Failures) != 1 || len(index.Warnings) != 1 {
		t.Fatalf("missing full-build diagnostic: %+v / %v", cache.Failures, index.Warnings)
	}
	saveHistoryScanCache(dir, cache)
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatal("delta retried full-build settled failure")
	}
	index, _, cache, err = buildBrainHistoryIndexSnapshotFromStoreWithScanner(context.Background(), dir, time.Now(), nil, manifest, nil, scan)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.Failures) != 1 || len(index.Warnings) != 1 || scans != 1 {
		t.Fatal("consolidation lost or retried settled failure")
	}
	cache.Version = historyScanCacheWireVersion
	cache.ExtractorVersion = historyScanCacheVersion - 1
	saveHistoryScanCache(dir, cache)
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err != nil {
		t.Fatal(err)
	}
	if scans != 2 {
		t.Fatal("extractor change did not retry")
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scanSessionFileReaderRecords); err != nil {
		t.Fatal(err)
	}
	// Recovery requires changed bytes, not merely a different scanner callback.
	target := filepath.Join(dir, filepath.FromSlash(failed))
	if err := os.WriteFile(target, []byte("recovered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scanSessionFileReaderRecords); err != nil {
		t.Fatal(err)
	}
	if len(loadHistoryScanCache(dir).Failures) != 0 {
		t.Fatal("recovered failure retained")
	}
}

func TestSettledFailureMasksPriorRecords(t *testing.T) {
	dir, changed, _ := shortTermFixture(t)
	scan := func(ctx context.Context, r io.ReadSeeker, path, rel string) ([]historyRecord, int, []string, bool) {
		if rel == changed {
			return nil, 0, []string{rel + ": parse failed"}, false
		}
		return scanSessionFileReaderRecords(ctx, r, path, rel)
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err != nil {
		t.Fatal(err)
	}
	manifest, err := bmanifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh := loadFreshHistoryOverlay(dir, manifest.Sources.History)
	if !fresh.replaced[changed] {
		t.Fatal("settled changed transcript leaves stale long-term records searchable")
	}
}

func TestSettledFailureCacheWriteFailureDoesNotPublish(t *testing.T) {
	dir, _, _ := shortTermFixture(t)
	path := filepath.Join(dir, filepath.FromSlash(historyScanCachePath))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	scan := func(context.Context, io.ReadSeeker, string, string) ([]historyRecord, int, []string, bool) {
		return nil, 0, nil, false
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err == nil {
		t.Fatal("cache write failure ignored")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(historyShortTermPath))); !os.IsNotExist(err) {
		t.Fatal("cache write failure published settled overlay")
	}
}

func TestSettledFailureEmptyWarningAndCancellation(t *testing.T) {
	for _, cancelScan := range []bool{false, true} {
		dir, _, _ := shortTermFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		scan := func(context.Context, io.ReadSeeker, string, string) ([]historyRecord, int, []string, bool) {
			if cancelScan {
				cancel()
			}
			return nil, 0, nil, false
		}
		manifest, err := bmanifest.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, _, cache, err := buildBrainHistoryIndexSnapshotFromStoreWithScanner(ctx, dir, time.Now(), nil, manifest, nil, scan)
		if cancelScan {
			if err == nil {
				t.Fatal("cancelled full build settled failure")
			}
		} else {
			for path, f := range cache.Failures {
				if !strings.Contains(f.Reason, path) || !strings.Contains(f.Reason, "parse failed") {
					t.Fatal("full build missing fallback", f)
				}
			}
		}
		cancel()
		ctx, cancel = context.WithCancel(context.Background())
		_, err = buildHistoryShortTermLockedContextWithScanner(ctx, dir, time.Now(), scan)
		if cancelScan && err == nil {
			t.Fatal("cancelled delta settled failure")
		}
		cancel()
	}
}

func TestSettledFailureForegroundAndDoctorAfterSilentPass(t *testing.T) {
	dir, _, _ := shortTermFixture(t)
	scan := func(context.Context, io.ReadSeeker, string, string) ([]historyRecord, int, []string, bool) {
		return nil, 0, nil, false
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(context.Background(), dir, time.Now(), scan); err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(storage.BrainDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, storage.BrainDir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runRefreshDelta(cmd, opts, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "parse failed") {
		t.Fatal("foreground lost silent-pass diagnostic:", out.String())
	}
	doctor := newDoctorCommand(opts)
	doctor.SetOut(&out)
	doctor.SetErr(&out)
	doctor.SetArgs([]string{"--json", "--fail-on", "none"})
	if err := doctor.Execute(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	plainDoctor := newDoctorCommand(opts)
	plainDoctor.SetOut(&out)
	plainDoctor.SetErr(&out)
	plainDoctor.SetArgs([]string{"--fail-on", "none"})
	if err := plainDoctor.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "parse failed") || !strings.Contains(out.String(), "sessions/main/") {
		t.Fatalf("plain doctor hides settled reasons: %s", &out)
	}
	checks := brainDoctorChecks(context.Background(), opts, repoDir)
	found := false
	for _, check := range checks {
		if check.Name == "history_freshness" {
			found = true
			if check.State != "ok" || strings.Contains(check.Detail, "refresh") {
				t.Fatalf("settled-only gap recommends repair: %+v", check)
			}
		}
	}
	if !found || !strings.Contains(out.String(), "history_settled_failure") {
		t.Fatal("doctor omitted settled failures")
	}
}

func TestSettledFailureDeadlineDoesNotPublish(t *testing.T) {
	dir, _, _ := shortTermFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	manifest, err := bmanifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := buildBrainHistoryIndexSnapshotFromStoreWithScanner(ctx, dir, time.Now(), nil, manifest, nil, scanSessionFileReaderRecords); err == nil {
		t.Fatal("expired full build succeeded")
	}
	if _, err := buildHistoryShortTermLockedContextWithScanner(ctx, dir, time.Now(), scanSessionFileReaderRecords); err == nil {
		t.Fatal("expired delta succeeded")
	}
	if len(loadHistoryScanCache(dir).Failures) != 0 {
		t.Fatal("deadline persisted failure")
	}
}
