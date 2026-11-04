package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// treeFixtureWithFacts seeds a verify fixture with one fact on "feature".
func treeFixtureWithFacts(t *testing.T) verifyFixture {
	t.Helper()
	f := newVerifyFixture(t)
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	fact := factRecord{
		ID: factRecordID("deploys use blue-green cutover", paths), Paths: paths,
		Text: "deploys use blue-green cutover", Branch: "feature", Origin: factstore.OriginDistilled,
		Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "feature", []factRecord{fact})
	return f
}

func TestNullExpansionCacheCanBeRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := loadExpansionCache(path)
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("null cache panicked: %v", p)
		}
	}()
	cache.set("query", "terms")
	if err := cache.save(); err != nil {
		t.Fatal(err)
	}
	if got, ok := loadExpansionCache(path).get("query"); !ok || got != "terms" {
		t.Fatalf("cache did not persist: %q, %v", got, ok)
	}
}

func TestFactsTreeRejectsNegativeLeaves(t *testing.T) {
	f := treeFixtureWithFacts(t)
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("negative leaves panicked: %v", p)
		}
	}()
	for _, format := range [][]string{nil, {"--json"}} {
		args := append([]string{"facts", "tree", "--branch", "feature", "--leaves", "-1"}, format...)
		if _, err := execute(t, NewRootCommand(f.opts), args...); err == nil {
			t.Error("negative leaves accepted")
		}
	}
}
