package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBriefImpactSecondHopFailurePreservesContext(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Semantic == nil {
		t.Fatalf("semantic source: manifest=%+v err=%v", manifest, err)
	}
	// The fixture's semantic snapshot names this root. Materialize its declared
	// source locus so the brief's stale-index boundary retains it in the public
	// packet rather than treating it as departed context.
	rootFile := filepath.Join(fixture.repoDir, "internal", "auth", "token.go")
	if err := os.MkdirAll(filepath.Dir(rootFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootFile, []byte("package auth\n\n// fixture source\nfunc ValidateToken(token string) error {\n\treturn nil\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const rootID = "gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken"
	const middleID = "brief-impact-middle"
	const leafID = "brief-impact-malformed-leaf"
	storePath := filepath.Join(fixture.brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath))
	db, err := sql.Open(sqliteDriverName, storePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range []struct{ id, name string }{{middleID, "ImpactMiddle"}, {leafID, "ImpactLeaf"}} {
		if _, err := db.Exec(`INSERT INTO symbols(id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version) VALUES (?, 'function', ?, ?, 'internal/impact.go', 1, 2, 'func()', 'Go', '1')`, symbol.id, symbol.name, symbol.name); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO relations(from_id, to_id, type, confidence, reason) VALUES (?, ?, 'CALLS', 1, 'direct context edge')`, rootID, middleID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	// SQLite retains nonnumeric text in a REAL column. Context only reads the
	// root's direct edge above; impact traversal reaches this row on hop two.
	if _, err := db.Exec(`INSERT INTO relations(from_id, to_id, type, confidence, reason) VALUES (?, ?, 'CALLS', ?, 'malformed second hop')`, middleID, leafID, "not-a-float"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Semantic.Symbols += 2
	manifest.Sources.Semantic.Relations += 2
	if err := writeBrainManifestAndReadme(fixture.brainDir, *manifest); err != nil {
		t.Fatalf("publish adjusted semantic counts: %v", err)
	}

	symbols, relations, neighbors, err := semanticContextFacts(fixture.brainDir, manifest.Sources.Semantic, "ValidateToken", 8, 0)
	if err != nil || !containsSemanticRecord(symbols, rootID) || !containsSemanticRelation(relations, rootID, middleID) || !containsSemanticRecord(neighbors, middleID) {
		t.Fatalf("direct context must remain readable: symbols=%+v relations=%+v neighbors=%+v err=%v", symbols, relations, neighbors, err)
	}

	out, err := execute(t, NewRootCommand(fixture.opts), "brief", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("brief should retain context after impact failure: %v\n%s", err, out)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief packet: %v\n%s", err, out)
	}
	if !containsCompactSemanticRecord(report.Semantic.Context.Symbols, rootID) || !strings.Contains(out, "semantic impact context unavailable") || strings.Contains(out, "semantic context unavailable") {
		t.Fatalf("impact-only failure lost healthy context: context=%+v warnings=%v", report.Semantic.Context, report.Warnings)
	}
}

func containsSemanticRelation(relations []semanticRecord, from, to string) bool {
	for _, relation := range relations {
		if relation.FromID == from && relation.ToID == to {
			return true
		}
	}
	return false
}

func containsSemanticRecord(records []semanticRecord, id string) bool {
	for _, record := range records {
		if record.ID == id {
			return true
		}
	}
	return false
}

func containsCompactSemanticRecord(records []compactSemanticRecord, id string) bool {
	for _, record := range records {
		if record.ID == id {
			return true
		}
	}
	return false
}

func TestBriefFusionIndexLoadFailureIsReportedAndProfiled(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	manifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		t.Fatalf("history source: manifest=%+v err=%v", manifest, err)
	}
	// Initialize the once guard before installing the fixture. Otherwise the
	// first brief call replaces the fake with the bundled, fusion-ineligible
	// embedder and never enters the indexed fusion route.
	old := defaultEmbedder()
	defaultEmbedderInst = &fakeFusionEmbedder{}
	t.Cleanup(func() { defaultEmbedderInst = old })
	indexPath := filepath.Join(fixture.brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath))
	if err := os.WriteFile(indexPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	out, err := execute(t, NewRootCommand(fixture.opts), "brief", "PRIVATE_HISTORY_PAYLOAD", "--json", "--profile-json", profilePath)
	if err != nil {
		t.Fatalf("brief should degrade history index failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "history context unavailable") {
		t.Fatalf("index failure warning missing:\n%s", out)
	}
	var profile brainBriefProfile
	data, readErr := os.ReadFile(profilePath)
	if readErr != nil {
		t.Fatalf("read profile: %v", readErr)
	}
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if !profile.History.IndexLoad.Invoked || profile.History.IndexLoad.ErrorCount != 1 {
		t.Fatalf("index-load failure was not observed: %+v", profile.History.IndexLoad)
	}
	if profile.History.IndexedRank.Invoked {
		t.Fatalf("rank ran after index-load failure: %+v", profile.History.IndexedRank)
	}
}

func TestBriefFusionRejectionFallsBackToLexicalHistory(t *testing.T) {
	for _, backend := range []string{"fts", "in-memory"} {
		t.Run(backend, func(t *testing.T) {
			fixture := newBrainBriefProfileFixture(t)
			manifest, err := loadBrainManifest(fixture.brainDir)
			if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
				t.Fatalf("history source: manifest=%+v err=%v", manifest, err)
			}
			// The regular brief fixture's transcript deliberately has no extractable
			// long-term record. Publish one indexed-only record so the command packet
			// can prove that the FTS fallback, rather than its independent raw scan,
			// preserved this exact history identity.
			const query = "FUSION_LEXICAL_TOKEN"
			const recordID = "fusion-lexical-record-id"
			indexed := historyIndex{GeneratedAt: manifest.Sources.History.GeneratedAt, Records: []historyRecord{{
				ID: recordID, Kind: "decision", Branch: "main", Path: "sessions/main/indexed-only.jsonl", Line: 17,
				Summary: query + " indexed history evidence",
			}}}
			source := *manifest.Sources.History
			source.GeneratedAt = indexed.GeneratedAt
			source.IndexPath = historyIndexPath
			source.IndexDigest = ""
			source.Records = len(indexed.Records)
			if err := publishHistoryComparisonIndex(fixture.brainDir, indexed, &source); err != nil {
				t.Fatalf("publish indexed fusion fixture: %v", err)
			}
			manifest, err = loadBrainManifest(fixture.brainDir)
			if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
				t.Fatalf("published history source: manifest=%+v err=%v", manifest, err)
			}
			index, err := loadBrainHistoryIndex(fixture.brainDir, manifest.Sources.History)
			if err != nil || len(index.Records) != 1 || index.Records[0].ID != recordID {
				t.Fatalf("fixture history index: records=%+v err=%v", index.Records, err)
			}
			want := historyRecordTextMatch(index.Records[0])
			if backend == "in-memory" {
				ftsPath := historyFTSDBPath(fixture.brainDir)
				if err := os.RemoveAll(ftsPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(ftsPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// See the comment in the load-failure test: initialize the process-level
			// once guard before swapping in this deterministic, fusion-eligible fake.
			old := defaultEmbedder()
			defaultEmbedderInst = &fakeFusionEmbedder{fail: func(text string) bool { return text == query }}
			t.Cleanup(func() { defaultEmbedderInst = old })
			profilePath := filepath.Join(t.TempDir(), "profile.json")
			out, err := execute(t, NewRootCommand(fixture.opts), "brief", query, "--json", "--profile-json", profilePath)
			if err != nil {
				t.Fatalf("fusion rejection brief: %v\n%s", err, out)
			}
			var report brainBriefJSONReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("parse brief packet: %v\n%s", err, out)
			}
			if len(report.History.Matches) != 1 || !reflect.DeepEqual(report.History.Matches[0], want) {
				t.Fatalf("fusion rejection did not retain exact lexical history identity: got=%+v want=%+v", report.History.Matches, want)
			}
			var profile brainBriefProfile
			data, readErr := os.ReadFile(profilePath)
			if readErr != nil {
				t.Fatalf("read profile: %v", readErr)
			}
			if err := json.Unmarshal(data, &profile); err != nil {
				t.Fatalf("parse profile: %v", err)
			}
			if !profile.History.IndexLoad.Invoked || profile.History.IndexLoad.ErrorCount != 0 {
				t.Fatalf("fusion route did not load index: %+v", profile.History.IndexLoad)
			}
			if !profile.History.IndexedRank.Invoked || profile.History.IndexedRank.ErrorCount != 0 || profile.History.IndexedRank.OutputCount != 1 {
				t.Fatalf("fusion rejection did not complete lexical rank: %+v", profile.History.IndexedRank)
			}
		})
	}
}
