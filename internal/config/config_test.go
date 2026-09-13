package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestLoadQuarantinesCorruptConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brain.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("corrupt config should not be fatal: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("expected defaults, got %+v", cfg)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file should be quarantined to .corrupt: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original corrupt file should have been moved aside, stat err = %v", err)
	}
}

func TestLoadReturnsDefaultWhenMissing(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("Load = %+v, want %+v", cfg, Default())
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	want := Config{DomainSlugs: map[string]string{"git.example.invalid": "ab"}}
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

// An empty object is a configured machine that has learned nothing yet, and it
// must load as the default rather than being back-filled with anything.
func TestLoadTreatsEmptyObjectAsDefault(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "brain.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("Load = %+v, want default %+v", got, Default())
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
	after := Config{DomainSlugs: map[string]string{"after.stale.invalid": "as"}}
	if err := Save(dataDir, after); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, after) {
		t.Fatalf("Load = %+v, want %+v", got, after)
	}
}
