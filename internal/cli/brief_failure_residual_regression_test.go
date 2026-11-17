package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
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
		if base.ContainsString(all, dropped) {
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
		path := filepath.Join(fixture.brainDir, filepath.FromSlash(patterncore.ProceduresPath))
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
		corpus := filepath.Join(fixture.brainDir, filepath.FromSlash(patterncore.CorpusPath))
		if err := os.MkdirAll(filepath.Dir(corpus), 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(corpus)
		db, err := sql.Open(sqlitedriver.Name, corpus)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range patterncore.CorpusSchema {
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
