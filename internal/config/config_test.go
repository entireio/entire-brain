package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReturnsDefaultWhenMissing(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Greeting != Default().Greeting {
		t.Fatalf("Greeting = %q, want %q", cfg.Greeting, Default().Greeting)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	want := Config{Greeting: "hi"}
	if err := Save(dataDir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadFillsMissingGreeting(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Greeting != Default().Greeting {
		t.Fatalf("Greeting = %q, want default", got.Greeting)
	}
}
