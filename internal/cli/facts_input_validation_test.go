package cli

import (
	"os"
	"path/filepath"
	"testing"
)

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
	f, _ := promoteFixtureWithSourceFacts(t)
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
