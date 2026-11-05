package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLegacyHostedMarker(t *testing.T, brainDir, dir, repoID, baseURL string) {
	t.Helper()
	abs := filepath.Join(brainDir, "facts", dir)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(hostedFactsBinding{RepoID: repoID, BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs, "hosted-target.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Unanimous legacy per-branch markers are adopted and promoted to the flat
// marker, so the binding survives the flat-store upgrade.
func TestHostedBindingAdoptsAgreeingLegacyMarkers(t *testing.T) {
	brainDir := t.TempDir()
	writeLegacyHostedMarker(t, brainDir, "main-aaaa1111", "repo1", "https://cell.example.com")
	writeLegacyHostedMarker(t, brainDir, "feature-bbbb2222", "repo1", "https://cell.example.com")

	binding, present, err := readHostedFactsBinding(brainDir)
	if err != nil || !present {
		t.Fatalf("binding = %+v present=%v err=%v", binding, present, err)
	}
	if binding.RepoID != "repo1" || binding.BaseURL != "https://cell.example.com" {
		t.Fatalf("wrong binding adopted: %+v", binding)
	}
	flat := filepath.Join(brainDir, filepath.FromSlash(hostedFactsBindingPath()))
	if _, err := os.Stat(flat); err != nil {
		t.Fatalf("agreeing markers must promote to the flat marker: %v", err)
	}
}

// Markers naming different targets are an ambiguity the flat store must not
// resolve silently: the repo-wide sync would ship every branch's memories to
// whichever marker sorted first.
func TestHostedBindingRefusesDisagreeingLegacyMarkers(t *testing.T) {
	brainDir := t.TempDir()
	writeLegacyHostedMarker(t, brainDir, "main-aaaa1111", "repo1", "https://cell-a.example.com")
	writeLegacyHostedMarker(t, brainDir, "feature-bbbb2222", "repo2", "https://cell-b.example.com")

	_, present, err := readHostedFactsBinding(brainDir)
	if err == nil || present {
		t.Fatalf("disagreeing markers must be an error, got present=%v err=%v", present, err)
	}
	if !strings.Contains(err.Error(), "memories sync") {
		t.Fatalf("the error must name the repair: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(hostedFactsBindingPath()))); !os.IsNotExist(statErr) {
		t.Fatalf("no flat marker may be written on disagreement: %v", statErr)
	}
}
