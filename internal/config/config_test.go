package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
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
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadFillsMissingGreeting(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "brain.json"), []byte("{}\n"), 0o600); err != nil {
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

func TestUpdateSerializesConcurrentDomainSlugWrites(t *testing.T) {
	dataDir := t.TempDir()
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := fmt.Sprintf("host-%02d.example.invalid", i)
			_, err := Update(dataDir, func(cfg *Config) error {
				if cfg.DomainSlugs == nil {
					cfg.DomainSlugs = make(map[string]string)
				}
				cfg.DomainSlugs[host] = fmt.Sprintf("h%02d", i)
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.DomainSlugs) != writers {
		t.Fatalf("DomainSlugs len = %d, want %d: %#v", len(got.DomainSlugs), writers, got.DomainSlugs)
	}
	for i := 0; i < writers; i++ {
		host := fmt.Sprintf("host-%02d.example.invalid", i)
		if got.DomainSlugs[host] != fmt.Sprintf("h%02d", i) {
			t.Fatalf("DomainSlugs[%s] = %q", host, got.DomainSlugs[host])
		}
	}
}

func TestSaveReacquiresWhenLockFileRemains(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := filepath.Join(dataDir, "locks", "config.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir lock dir: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("stale metadata\n"), 0o600); err != nil {
		t.Fatalf("write stale lock file: %v", err)
	}
	if err := Save(dataDir, Config{Greeting: "after stale"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Greeting != "after stale" {
		t.Fatalf("Greeting = %q", got.Greeting)
	}
}
