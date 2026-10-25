package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDistillTemplateEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alt.md")
	content := "---\nname: alt\n---\nALT BODY with ${TAXONOMY_BLOCK}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envDistillTemplate, path)
	body, err := distillTemplate()
	if err != nil {
		t.Fatalf("distillTemplate: %v", err)
	}
	if !strings.HasPrefix(body, "ALT BODY") {
		t.Errorf("frontmatter not stripped or wrong body: %q", body)
	}
}

func TestDistillTemplateEnvOverrideMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.md")
	t.Setenv(envDistillTemplate, missing)
	_, err := distillTemplate()
	if err == nil {
		t.Fatal("expected error for missing override file, got embedded template")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the path %q: %v", missing, err)
	}
}

func TestRenderDistillPromptOverrideWithoutMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alt.md")
	if err := os.WriteFile(path, []byte("no marker here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envDistillTemplate, path)
	_, err := renderDistillPrompt(defaultFactTaxonomy(time.Now()))
	if err == nil || !strings.Contains(err.Error(), distillTaxonomyMarker) {
		t.Fatalf("expected missing-marker error, got %v", err)
	}
}

func TestFactsMaxPerChunkEnvOverride(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	taxonomy := defaultFactTaxonomy(now)
	var lines []string
	for i := 0; i < factsMaxPerChunk+4; i++ {
		lines = append(lines, "project.tooling.stack\tFact number "+string(rune('a'+i)))
	}
	output := strings.Join(lines, "\n")

	t.Setenv(envFactsMaxPerChunk, "2")
	records, warnings := distilledFactsFromOutput(output, taxonomy, factAnchor{SessionID: "s1"}, "main", now)
	if len(records) != 2 {
		t.Fatalf("env cap 2: got %d records", len(records))
	}
	if !containsWarning(warnings, "extra lines dropped") {
		t.Errorf("expected cap warning, got %v", warnings)
	}

	os.Unsetenv(envFactsMaxPerChunk)
	records, _ = distilledFactsFromOutput(output, taxonomy, factAnchor{SessionID: "s1"}, "main", now)
	if len(records) != factsMaxPerChunk {
		t.Fatalf("env unset: got %d records, want %d", len(records), factsMaxPerChunk)
	}
}

func TestFactsMaxPerChunkFromEnvRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"0", "-1", "x"} {
		t.Setenv(envFactsMaxPerChunk, bad)
		if _, err := factsMaxPerChunkFromEnv(); err == nil {
			t.Errorf("value %q: expected error", bad)
		}
	}
	os.Unsetenv(envFactsMaxPerChunk)
	n, err := factsMaxPerChunkFromEnv()
	if err != nil || n != factsMaxPerChunk {
		t.Errorf("unset: got %d, %v; want %d, nil", n, err, factsMaxPerChunk)
	}
}
