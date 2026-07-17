package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func writeLegacyHistoryIdentityFixture(t testing.TB, index historyIndex) (string, *historySourceManifest) {
	t.Helper()
	brainDir, strong := writeDirectHistoryFTSFixture(t, index)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := *strong
	legacy.IndexBytes = 0
	legacy.IndexSHA256 = ""
	legacy.RecordsFingerprint = ""
	manifest.Sources.History = &legacy
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	return brainDir, &legacy
}

func loadLegacyHistoryIdentityCandidate(t testing.TB, brainDir string, source *historySourceManifest) (historyIndex, *historyLegacyIdentity) {
	t.Helper()
	index, identity, err := loadBrainHistoryIndexWithLegacyIdentity(brainDir, source)
	if err != nil {
		t.Fatal(err)
	}
	if identity == nil {
		t.Fatal("legacy history load did not derive an identity")
	}
	return index, identity
}

func assertHistoryIdentityMissing(t testing.TB, brainDir string) {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	source := manifest.Sources.History
	if source == nil {
		t.Fatal("history source missing")
	}
	if source.IndexBytes != 0 || source.IndexSHA256 != "" || source.RecordsFingerprint != "" {
		t.Fatalf("legacy identity unexpectedly persisted: %+v", source)
	}
}

func assertHistoryIdentityPersisted(t testing.TB, brainDir string, identity *historyLegacyIdentity) {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	source := manifest.Sources.History
	if source == nil || source.IndexBytes != identity.IndexBytes || source.IndexSHA256 != identity.IndexSHA256 ||
		source.RecordsFingerprint != identity.RecordsFingerprint {
		t.Fatalf("history identity was not persisted exactly: source=%+v identity=%+v", source, identity)
	}
}

func TestLoadBrainHistoryIndexGenericDoesNotDeriveLegacyIdentity(t *testing.T) {
	brainDir, legacy := writeLegacyHistoryIdentityFixture(t, ftsTestIndex())

	generic, err := loadBrainHistoryIndex(brainDir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if generic.recordsFingerprint != "" {
		t.Fatalf("generic load derived legacy identity: %q", generic.recordsFingerprint)
	}

	explicit, identity, err := loadBrainHistoryIndexWithLegacyIdentity(brainDir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if identity == nil || explicit.recordsFingerprint != identity.RecordsFingerprint {
		t.Fatalf("explicit migration load did not derive identity: index=%q identity=%+v", explicit.recordsFingerprint, identity)
	}
	if !reflect.DeepEqual(generic.Records, explicit.Records) {
		t.Fatal("generic and explicit loaders decoded different index truth")
	}
}

func TestHistoryLegacyIdentityUpgradeRequiresVerifiedTruthAndDirectPayload(t *testing.T) {
	index := ftsTestIndex()
	brainDir, legacy := writeLegacyHistoryIdentityFixture(t, index)
	loaded, identity := loadLegacyHistoryIdentityCandidate(t, brainDir, legacy)

	want, ok := rankHistoryViaFTS(brainDir, loaded, "history", "embedding model", 8)
	if !ok {
		t.Fatal("index-backed ranking unavailable")
	}
	got, used := rankHistoryViaLegacyDirectPayload(brainDir, legacy, identity, "history", "embedding model", 8, historyFTSRelevanceCutoff)
	if !used || !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy direct parity: used=%v got=%#v want=%#v", used, got, want)
	}
	assertHistoryIdentityPersisted(t, brainDir, identity)

	manifestBefore, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	readmeBefore, err := os.ReadFile(filepath.Join(brainDir, exportReadmeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := persistHistoryLegacyIdentity(brainDir, identity); err != nil {
		t.Fatalf("already-upgraded no-op: %v", err)
	}
	readmeAfter, err := os.ReadFile(filepath.Join(brainDir, exportReadmeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readmeAfter, readmeBefore) {
		t.Fatal("identity-only migration rewrote README")
	}
	manifestAfter, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestAfter, manifestBefore) {
		t.Fatal("already-upgraded migration rewrote manifest")
	}
	if _, next, err := loadBrainHistoryIndexWithLegacyIdentity(brainDir, identity.sourceWithIdentity(legacy)); err != nil || next != nil {
		t.Fatalf("upgraded source offered another migration: identity=%+v err=%v", next, err)
	}
}

func TestHistoryLegacyIdentityHashAndParseShareBytesAndReplacementAbortsPersist(t *testing.T) {
	indexA := ftsTestIndex()
	brainDir, legacy := writeLegacyHistoryIdentityFixture(t, indexA)
	path := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	dataA, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decodedA, err := decodeBrainHistoryIndex(dataA, legacy)
	identity := deriveHistoryLegacyIdentity(legacy, dataA, decodedA)
	if err != nil || identity == nil || !reflect.DeepEqual(decodedA.Records, indexA.Records) {
		t.Fatalf("decode A: identity=%+v err=%v", identity, err)
	}

	indexB := indexA
	indexB.Records = append([]historyRecord(nil), indexA.Records...)
	indexB.Records[0].Summary = strings.Replace(indexB.Records[0].Summary, "static", "STATIC", 1)
	dataB, err := json.MarshalIndent(indexB, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dataB = append(dataB, '\n')
	if len(dataB) != len(dataA) {
		t.Fatalf("replacement must retain size: A=%d B=%d", len(dataA), len(dataB))
	}
	if err := writeFileAtomic(path, dataB, 0o600); err != nil {
		t.Fatal(err)
	}
	if identity.IndexSHA256 != historyIndexBytesFingerprint(dataA) || identity.IndexSHA256 == historyIndexBytesFingerprint(dataB) {
		t.Fatal("derived identity did not stay bound to parsed bytes A")
	}

	// The old matching FTS may still serve the already-loaded generation A,
	// preserving first-call behavior, but the lock-time byte check must refuse
	// to bind its identity to the replacement generation B.
	if _, used := rankHistoryViaLegacyDirectPayload(brainDir, legacy, identity, "history", "embedding model", 8, historyFTSRelevanceCutoff); !used {
		t.Fatal("precondition: generation-A direct payload unavailable")
	}
	assertHistoryIdentityMissing(t, brainDir)
}

func TestHistoryLegacyIdentityConcurrentMigrationPreservesNewerFields(t *testing.T) {
	brainDir, legacy := writeLegacyHistoryIdentityFixture(t, ftsTestIndex())
	_, identity := loadLegacyHistoryIdentityCandidate(t, brainDir, legacy)

	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.History.Warnings = []string{"newer concurrent warning"}
	manifest.RepoKey = "gh/example/preserved"
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}

	const workers = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// A busy zero-wait lock is an expected best-effort miss; at least one
			// contender will persist and every contender leaves newer fields alone.
			_ = persistHistoryLegacyIdentity(brainDir, identity)
		}()
	}
	close(start)
	wg.Wait()
	assertHistoryIdentityPersisted(t, brainDir, identity)
	got, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoKey != "gh/example/preserved" || !reflect.DeepEqual(got.Sources.History.Warnings, []string{"newer concurrent warning"}) {
		t.Fatalf("migration lost concurrent manifest fields: repo=%q warnings=%v", got.RepoKey, got.Sources.History.Warnings)
	}
}

func TestHistoryLegacyIdentityRejectsStaleManifestCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*historySourceManifest)
	}{
		{name: "generation", mutate: func(source *historySourceManifest) { source.GeneratedAt = source.GeneratedAt.Add(time.Second) }},
		{name: "path", mutate: func(source *historySourceManifest) { source.IndexPath = "history/replaced.json" }},
		{name: "count", mutate: func(source *historySourceManifest) { source.Records++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, legacy := writeLegacyHistoryIdentityFixture(t, ftsTestIndex())
			_, identity := loadLegacyHistoryIdentityCandidate(t, brainDir, legacy)
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(manifest.Sources.History)
			if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
				t.Fatal(err)
			}
			if err := persistHistoryLegacyIdentity(brainDir, identity); err == nil {
				t.Fatal("stale manifest coordinates were migrated")
			}
			assertHistoryIdentityMissing(t, brainDir)
		})
	}
}

func TestHistoryLegacyIdentityNoPayloadCorruptPayloadAndReadOnlyFailOpen(t *testing.T) {
	t.Run("no FTS", func(t *testing.T) {
		index := ftsTestIndex()
		brainDir := t.TempDir()
		data, err := json.MarshalIndent(index, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, '\n')
		legacy := &historySourceManifest{GeneratedAt: index.GeneratedAt, IndexPath: historyIndexPath, Records: len(index.Records)}
		if err := writeBrainRelativeFileAtomic(brainDir, historyIndexPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, Sources: &brainSources{History: legacy}}); err != nil {
			t.Fatal(err)
		}
		_, identity := loadLegacyHistoryIdentityCandidate(t, brainDir, legacy)
		if _, used := rankHistoryViaLegacyDirectPayload(brainDir, legacy, identity, "history", "embedding model", 8, historyFTSRelevanceCutoff); used {
			t.Fatal("missing FTS payload triggered migration")
		}
		assertHistoryIdentityMissing(t, brainDir)
		if _, err := os.Stat(historyFTSDBPath(brainDir)); !os.IsNotExist(err) {
			t.Fatalf("migration probe created FTS: %v", err)
		}
	})

	t.Run("corrupt matched payload", func(t *testing.T) {
		brainDir, legacy := writeLegacyHistoryIdentityFixture(t, ftsTestIndex())
		_, identity := loadLegacyHistoryIdentityCandidate(t, brainDir, legacy)
		db, err := sql.Open(sqliteDriverName, historyFTSDBPath(brainDir))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE history_records SET terms_json = '{' WHERE id = 'd1'`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, used := rankHistoryViaLegacyDirectPayload(brainDir, legacy, identity, "history", "embedding model", 8, historyFTSRelevanceCutoff); used {
			t.Fatal("corrupt FTS payload triggered migration")
		}
		assertHistoryIdentityMissing(t, brainDir)
	})

	t.Run("read-only lock", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX file mode test")
		}
		brainDir, legacy := writeLegacyHistoryIdentityFixture(t, ftsTestIndex())
		loaded, identity := loadLegacyHistoryIdentityCandidate(t, brainDir, legacy)
		want, ok := rankHistoryViaFTS(brainDir, loaded, "history", "embedding model", 8)
		if !ok {
			t.Fatal("reference ranking unavailable")
		}
		lockPath := brainWriteLockPath(brainDir)
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lockPath, nil, 0o400); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(lockPath, 0o600)
		got, used := rankHistoryViaLegacyDirectPayload(brainDir, legacy, identity, "history", "embedding model", 8, historyFTSRelevanceCutoff)
		if !used || !reflect.DeepEqual(got, want) {
			t.Fatalf("read-only migration changed results: used=%v got=%#v want=%#v", used, got, want)
		}
		assertHistoryIdentityMissing(t, brainDir)
	})
}

func TestHistoryLegacyIdentityManifestReplacementIsAtomic(t *testing.T) {
	brainDir, legacy := writeLegacyHistoryIdentityFixture(t, ftsTestIndex())
	_, identity := loadLegacyHistoryIdentityCandidate(t, brainDir, legacy)
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	oldData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(manifestPath)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				var manifest exportManifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}()
	}
	if err := persistHistoryLegacyIdentity(brainDir, identity); err != nil {
		close(stop)
		wg.Wait()
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("reader observed non-atomic manifest: %v", err)
	default:
	}
	newData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldData, newData) {
		t.Fatal("migration did not replace legacy manifest")
	}
	if matches, err := filepath.Glob(filepath.Join(brainDir, "."+exportManifestFileName+".tmp-*")); err != nil || len(matches) != 0 {
		t.Fatalf("atomic manifest temp leak: matches=%v err=%v", matches, err)
	}
	if info, err := os.Stat(manifestPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode after migration: info=%v err=%v", info, err)
	}
}

func TestHistoryLegacyIdentityLockRecheckAllocationsAreSizeIndependent(t *testing.T) {
	writeIndex := func(records int) (string, *historySourceManifest) {
		index := historyIndex{GeneratedAt: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)}
		for i := 0; i < records; i++ {
			index.Records = append(index.Records, historyRecord{ID: "history:" + strconv.Itoa(i), Kind: "decision", Path: "sessions/main/a.jsonl", Line: i + 1, Summary: strings.Repeat("payload ", 64)})
		}
		return writeDirectHistoryFTSFixture(t, index)
	}
	smallDir, small := writeIndex(1)
	largeDir, large := writeIndex(2048)
	measure := func(brainDir string, source *historySourceManifest) float64 {
		return testing.AllocsPerRun(5, func() {
			fingerprint, err := historyIndexFingerprintForIdentityRecheck(brainDir, source.IndexPath, source.IndexBytes)
			if err != nil || fingerprint != source.IndexSHA256 {
				t.Fatalf("stream identity: fingerprint=%q err=%v", fingerprint, err)
			}
		})
	}
	smallAllocs := measure(smallDir, small)
	largeAllocs := measure(largeDir, large)
	if largeAllocs > smallAllocs+2 {
		t.Fatalf("lock-time identity allocations scale with file size: small=%.0f large=%.0f", smallAllocs, largeAllocs)
	}
}

func TestBrainBriefLegacyHistoryIdentityFirstAndDirectPacketsAreExact(t *testing.T) {
	fixture := newBrainBriefRawHistoryEndToEndFixture(t)
	storage, err := repoStoragePaths(context.Background(), fixture.opts.Runner, fixture.opts.Env, fixture.opts.Env.RepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := runBrainBriefRawHistoryEndToEnd(t, fixture, brainBriefRawHistoryMatchesObserved)

	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.History.IndexBytes = 0
	manifest.Sources.History.IndexSHA256 = ""
	manifest.Sources.History.RecordsFingerprint = ""
	if err := writeBrainManifestAndReadme(storage.BrainDir, *manifest); err != nil {
		t.Fatal(err)
	}

	first, firstProfile := runLegacyIdentityProfiledBrief(t, fixture)
	if !firstProfile.History.IndexLoad.Invoked {
		t.Fatal("first legacy brief did not take verified index fallback")
	}
	// Reload the now-strong source for an exact assertion without reconstructing
	// identity values from profile counts.
	upgraded, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Sources.History.IndexBytes <= 0 || !validHistorySHA256(upgraded.Sources.History.IndexSHA256) || !validHistorySHA256(upgraded.Sources.History.RecordsFingerprint) {
		t.Fatalf("first brief did not persist strong identity: %+v", upgraded.Sources.History)
	}
	second, secondProfile := runLegacyIdentityProfiledBrief(t, fixture)
	if secondProfile.History.IndexLoad.Invoked || !secondProfile.History.IndexedRank.Invoked {
		t.Fatalf("second brief did not use direct payload: %+v", secondProfile.History)
	}
	if first != want || second != want {
		t.Fatalf("history migration changed packet bytes\nwant (%d): %s\nfirst (%d): %s\nsecond (%d): %s", len(want), want, len(first), first, len(second), second)
	}
}

func runLegacyIdentityProfiledBrief(t testing.TB, fixture brainBriefRawHistoryEndToEndFixture) (string, brainBriefProfile) {
	t.Helper()
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	briefOpts := brainBriefOptions{json: true, limit: brainBriefDefaultLimit, noSemantic: true, profileJSON: profilePath}
	if err := runBrainBriefWithRawHistoryMatcher(context.Background(), cmd, fixture.opts, briefOpts, fixture.task, brainBriefRawHistoryMatchesObserved); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	var profile brainBriefProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	return out.String(), profile
}
