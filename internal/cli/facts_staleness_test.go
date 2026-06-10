package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFactLocusDrift(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "internal", "cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "cli", "alive.go"), []byte("package cli\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Stored locus mixing: a present path, a departed path, a bare file name,
	// and a plain identifier. Only the departed PATH drifts: bare names and
	// identifiers can't be resolved honestly without a repo walk or the
	// semantic store (deferred to the overlay seam), so they never flag.
	f := factRecord{ID: "f1", Text: "x", Locus: []string{
		"internal/cli/alive.go",
		"internal/cli/departed.go",
		"departed.go",
		"rankfactsfused",
	}}
	drift := factLocusDrift(repo, f)
	if len(drift) != 1 || drift[0] != "internal/cli/departed.go" {
		t.Fatalf("drift = %v, want only the departed path token", drift)
	}

	// Path traversal tokens are never resolved.
	evil := factRecord{ID: "f2", Text: "x", Locus: []string{"../outside/secret.go"}}
	if drift := factLocusDrift(repo, evil); len(drift) != 0 {
		t.Fatalf("traversal token must be skipped, got %v", drift)
	}

	// Prose constructs containing a slash — all observed in live locus data —
	// must never be mistaken for file paths: a drift flag that cries wolf
	// trains the agent to ignore it.
	prose := factRecord{ID: "f4", Text: "x", Locus: []string{
		"a/b", "expected/current", "provider/head", "install/list/remove", "seed/*", "rpc/mcp",
	}}
	if drift := factLocusDrift(repo, prose); len(drift) != 0 {
		t.Fatalf("prose slash-tokens must not drift, got %v", drift)
	}

	// No repo dir (bundle-only brain): no drift claims at all.
	if drift := factLocusDrift("", f); len(drift) != 0 {
		t.Fatalf("empty repoDir must report nothing, got %v", drift)
	}

	// Derived locus (no stored field) drifts the same way: the fact names a
	// departed path in its text.
	derived := factRecord{ID: "f3", Text: "the chunker in `internal/cli/old_chunker.go` caps lines"}
	if drift := factLocusDrift(repo, derived); len(drift) != 1 || drift[0] != "internal/cli/old_chunker.go" {
		t.Fatalf("derived-locus drift = %v, want the departed path", drift)
	}
}

func TestFactsLocusDriftMapsOnlyDrifted(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "ok.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts := []factRecord{
		{ID: "clean", Locus: []string{"ok.go"}}, // bare name: never checked, never drifts
		{ID: "drifted", Locus: []string{"gone/missing.go"}},
	}

	got := factsLocusDrift(repo, facts)
	if len(got) != 1 || len(got["drifted"]) != 1 {
		t.Fatalf("expected only the drifted fact in the map, got %v", got)
	}
	if factsLocusDrift(repo, facts[:1]) != nil {
		t.Fatal("no drift should yield a nil map (clean JSON omission)")
	}
}
