package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostedRepoBindingRoundTrip(t *testing.T) {
	brainDir := t.TempDir()
	in := hostedRepoBinding{RepoID: "01M2Q9WATHXVM46CD8Y4D3AX8W", BaseURL: "https://aws-us-east-2.api.partial.to/api/v1/", Jurisdiction: "us"}
	if err := writeHostedRepoBinding(brainDir, in); err != nil {
		t.Fatal(err)
	}
	got, present, err := readHostedRepoBinding(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("binding must be present after write")
	}
	if got.RepoID != in.RepoID || got.Jurisdiction != in.Jurisdiction {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.BaseURL != "https://aws-us-east-2.api.partial.to/api/v1" {
		t.Fatalf("base URL must be stored without trailing slash, got %q", got.BaseURL)
	}
}

func TestHostedRepoBindingAbsent(t *testing.T) {
	_, present, err := readHostedRepoBinding(t.TempDir())
	if err != nil {
		t.Fatalf("absent binding must not be an error, got %v", err)
	}
	if present {
		t.Fatal("absent binding must report present=false")
	}
}

func TestHostedRepoBindingCorrupt(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(brainDir, "hosted.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readHostedRepoBinding(brainDir); err == nil {
		t.Fatal("corrupt binding must return an error")
	}
}
