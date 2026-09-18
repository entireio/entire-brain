package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBriefResidualLikelyFilesEnforcesCombinedCap(t *testing.T) {
	counts := map[string]int{}
	for i, rel := range []string{
		"src/alpha.go", "src/bravo.go", "src/charlie.go", "src/delta.go",
		"src/echo.go", "src/foxtrot.go", "src/golf.go", "src/hotel.go",
		"src/alpha_test.go", "src/bravo_test.go", "src/charlie_test.go",
		"src/delta_test.go", "src/echo_test.go", "src/foxtrot_test.go",
	} {
		counts[rel] = 100 - i
	}
	edits, tests, all := brainBriefLikelyFileGroupsForRepoAndFilenameCounts(t.TempDir(), brainBriefReport{}, "", nil, counts)
	if len(edits) != 8 || len(tests) != 6 || len(all) != 12 {
		t.Fatalf("budgets edits=%d tests=%d all=%d: %#v", len(edits), len(tests), len(all), all)
	}
	wantAll := append(append([]string{}, edits...), tests[:4]...)
	if !reflect.DeepEqual(all, wantAll) {
		t.Fatalf("combined cap/order=%#v, want edit prefix plus first four tests %#v", all, wantAll)
	}
	for _, dropped := range tests[4:] {
		if containsString(all, dropped) {
			t.Fatalf("combined cap retained tail %q in %#v", dropped, all)
		}
	}
}

func TestBriefResidualActionKeywordDoesNotReplaceRealSymbol(t *testing.T) {
	metadata := brainBriefMetadataStringActionsForFile("provider.ts", `realProvider(
if (enabled) {
  metadata: { step: 1 }
}`)
	if len(metadata) != 1 || metadata[0].Symbol != "realProvider" {
		t.Fatalf("metadata actions=%#v", metadata)
	}

	previous := brainBriefPreviousResponseActionsForFile("agent.ts", `realAgent(
for (const item of items) {
  let previousResponseId = null
}`)
	if len(previous) != 1 || previous[0].Symbol != "realAgent" {
		t.Fatalf("previous-response actions=%#v", previous)
	}
}

func TestBriefResidualMalformedKnowledgeStoresEmitNoPacket(t *testing.T) {
	t.Run("legacy patterns", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		path := filepath.Join(fixture.brainDir, filepath.FromSlash(patternsProceduresPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{broken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(fixture.opts), "brief", "metadata", "--json")
		if err == nil || !strings.Contains(err.Error(), "parse procedure line") || !strings.Contains(out, `"code": "command_failed"`) || strings.Contains(out, `"task"`) {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})

	t.Run("themes query", func(t *testing.T) {
		fixture := newBrainBriefOutputFixture(t)
		corpus := filepath.Join(fixture.brainDir, filepath.FromSlash(patternCorpusPath))
		if err := os.MkdirAll(filepath.Dir(corpus), 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(corpus)
		db, err := sql.Open(sqliteDriverName, corpus)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range patternCorpusSchema {
			if _, err := db.Exec(stmt); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`DROP TABLE themes`); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE themes(id TEXT PRIMARY KEY)`); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(fixture.opts), "brief", "metadata", "--json")
		if err == nil || !strings.Contains(err.Error(), "no such column") || !strings.Contains(out, `"code": "command_failed"`) || strings.Contains(out, `"task"`) {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
}

func TestBriefResidualCorruptFTSPayloadRebuildsFromCanonicalHistory(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	canonical := ftsTestIndex()
	donor, source := writeDirectHistoryFTSFixture(t, canonical)
	indexData, err := os.ReadFile(filepath.Join(donor, filepath.FromSlash(historyIndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(fixture.brainDir, historyIndexPath, indexData, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.History = source
	if err := writeBrainManifestAndReadme(fixture.brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(fixture.brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryFTS(fixture.brainDir, index)
	if err != nil || db == nil {
		t.Fatalf("prime FTS db=%v err=%v", db, err)
	}
	if _, err := db.Exec(`UPDATE history_records SET terms_json = '{'`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(fixture.opts), "brief", "embedding model", "--json")
	if err != nil {
		t.Fatalf("brief did not recover canonical history: err=%v\n%s", err, out)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode recovered brief: %v\n%s", err, out)
	}
	if len(report.History.Matches) != 1 || report.History.Matches[0].Path != canonical.Records[0].Path || report.History.Matches[0].Line != canonical.Records[0].Line || report.History.Matches[0].Excerpt != canonical.Records[0].Summary {
		t.Fatalf("recovered history=%#v, want exact canonical d1 provenance", report.History.Matches)
	}
	check, err := sql.Open(sqliteDriverName, historyFTSDBPath(fixture.brainDir))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var terms string
	if err := check.QueryRow(`SELECT terms_json FROM history_records WHERE id = 'd1'`).Scan(&terms); err != nil || terms != "null" {
		t.Fatalf("derived FTS payload not repaired: terms=%q err=%v", terms, err)
	}
}

func TestBriefResidualLegacyHistoryFallsBackInMemoryWhenFTSUnavailable(t *testing.T) {
	fixture := newBrainBriefRawHistoryEndToEndFixture(t)
	want := runBrainBriefRawHistoryEndToEnd(t, fixture, brainBriefRawHistoryMatchesObserved)
	storage, err := repoStoragePaths(context.Background(), fixture.opts.Runner, fixture.opts.Env, fixture.opts.Env.RepoRoot)
	if err != nil {
		t.Fatal(err)
	}
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
	ftsPath := historyFTSDBPath(storage.BrainDir)
	if err := os.RemoveAll(ftsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ftsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	got, profile := runLegacyIdentityProfiledBrief(t, fixture)
	if got != want {
		t.Fatalf("in-memory legacy fallback changed packet\nwant: %s\ngot: %s", want, got)
	}
	if !profile.History.IndexLoad.Invoked || !profile.History.IndexedRank.Invoked || profile.History.IndexedRank.ErrorCount != 0 {
		t.Fatalf("legacy fallback profile=%+v", profile.History)
	}
}

func TestBriefResidualProfileRecordsCanonicalHistoryLoadFailure(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty canonical history fixture")
	}
	data[0] = '['
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Sources.History.IndexBytes = int64(len(data))
	manifest.Sources.History.IndexSHA256 = historyIndexBytesFingerprint(data)
	if err := writeBrainManifestAndReadme(fixture.brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(historyFTSDBPath(fixture.brainDir))
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	out, err := execute(t, NewRootCommand(fixture.opts), "brief", "history", "--json", "--profile-json", profilePath)
	if err != nil {
		t.Fatalf("brief should degrade with warning: %v\n%s", err, out)
	}
	profileData, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	var profile brainBriefProfile
	if err := json.Unmarshal(profileData, &profile); err != nil {
		t.Fatal(err)
	}
	if !profile.History.IndexLoad.Invoked || profile.History.IndexLoad.ErrorCount != 1 {
		t.Fatalf("history profile=%+v", profile.History)
	}
	if !strings.Contains(out, "history context unavailable") {
		t.Fatalf("public warning absent: %s", out)
	}
}

func TestBriefResidualCorruptFTSRebuildContentionFallsBackAndRetries(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	canonical := ftsTestIndex()
	donor, source := writeDirectHistoryFTSFixture(t, canonical)
	data, err := os.ReadFile(filepath.Join(donor, filepath.FromSlash(historyIndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(fixture.brainDir, historyIndexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.History = source
	if err := writeBrainManifestAndReadme(fixture.brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(fixture.brainDir, source)
	if err != nil {
		t.Fatal(err)
	}
	prime, err := openHistoryFTS(fixture.brainDir, index)
	if err != nil || prime == nil {
		t.Fatalf("prime db=%v err=%v", prime, err)
	}
	if _, err := prime.Exec(`UPDATE history_records SET terms_json = '{'`); err != nil {
		_ = prime.Close()
		t.Fatal(err)
	}
	if err := prime.Close(); err != nil {
		t.Fatal(err)
	}
	lockDB, err := sql.Open(sqliteDriverName, historyFTSDBPath(fixture.brainDir))
	if err != nil {
		t.Fatal(err)
	}
	defer lockDB.Close()
	lockDB.SetMaxOpenConns(1)
	if _, err := lockDB.Exec(`BEGIN IMMEDIATE`); err != nil {
		_ = lockDB.Close()
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(fixture.opts), "brief", "embedding model", "--json")
	if _, rollbackErr := lockDB.Exec(`ROLLBACK`); rollbackErr != nil {
		t.Fatalf("rollback: %v", rollbackErr)
	}
	if err != nil {
		t.Fatalf("contention fallback: %v\n%s", err, out)
	}
	assertBriefResidualCanonicalD1(t, out, canonical)

	retried, err := execute(t, NewRootCommand(fixture.opts), "brief", "embedding model", "--json")
	if err != nil {
		t.Fatalf("repair retry: %v\n%s", err, retried)
	}
	assertBriefResidualCanonicalD1(t, retried, canonical)
	repaired, err := sql.Open(sqliteDriverName, historyFTSDBPath(fixture.brainDir))
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()
	var terms string
	if err := repaired.QueryRow(`SELECT terms_json FROM history_records WHERE id = 'd1'`).Scan(&terms); err != nil || terms != "null" {
		t.Fatalf("retry did not repair d1 payload: terms=%q err=%v", terms, err)
	}
}

func assertBriefResidualCanonicalD1(t *testing.T, packet string, canonical historyIndex) {
	t.Helper()
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(packet), &report); err != nil {
		t.Fatalf("decode brief: %v\n%s", err, packet)
	}
	if len(report.History.Matches) != 1 || report.History.Matches[0].Path != canonical.Records[0].Path || report.History.Matches[0].Line != canonical.Records[0].Line || report.History.Matches[0].Excerpt != canonical.Records[0].Summary {
		t.Fatalf("history=%#v, want exact canonical d1", report.History.Matches)
	}
}
