package cli

// Symlink/non-regular hardening for the legacy fold inside factstore.Load:
// the flat store reads legacy branch files read-only, and none of these
// vectors may pull bytes from outside the brain. Ported from the pre-flat
// fact-inventory tests.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func legacyBranchDir(t *testing.T, brainDir, dir string) string {
	t.Helper()
	abs := filepath.Join(brainDir, factstore.DirName, dir)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestLegacyFoldRefusesASymlinkedFactsFile(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "elsewhere.ndjson")
	if err := os.WriteFile(secret,
		[]byte(`{"id":"fact:leak","branch":"evil","status":"active","text":"CONTENT FROM OUTSIDE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := legacyBranchDir(t, brainDir, "main-00000000")
	target := filepath.Join(dir, "facts.ndjson")
	if err := os.Symlink(secret, target); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fixture is not a symlink (err=%v)", err)
	}
	records, err := factstore.Load(brainDir)
	if err == nil {
		for _, record := range records {
			if record.Text == "CONTENT FROM OUTSIDE" {
				t.Fatal("content from outside the brain was returned")
			}
		}
		t.Fatal("a symlinked legacy fact store must be refused")
	}
}

func TestLegacyFoldRefusesASymlinkedFactsDirectory(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	outside := t.TempDir()
	branchDir := filepath.Join(outside, "main-00000000")
	if err := os.MkdirAll(branchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(branchDir, "facts.ndjson"),
		[]byte(`{"id":"fact:leak","branch":"evil","status":"active","text":"CONTENT FROM OUTSIDE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(brainDir, factstore.DirName)); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}
	records, err := factstore.Load(brainDir)
	if err == nil {
		for _, record := range records {
			if record.Text == "CONTENT FROM OUTSIDE" {
				t.Fatal("content from outside the brain was returned")
			}
		}
		t.Fatal("a symlinked facts directory must be refused")
	}
}

func TestLegacyFoldSkipsASymlinkedBranchDirectory(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "facts.ndjson"),
		[]byte(`{"id":"fact:leak","branch":"evil","status":"active","text":"CONTENT FROM OUTSIDE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, factstore.DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(brainDir, factstore.DirName, "escape-00000000")); err != nil {
		t.Skipf("this filesystem cannot create symlinks: %v", err)
	}
	records, err := factstore.Load(brainDir)
	if err == nil {
		for _, record := range records {
			if record.Text == "CONTENT FROM OUTSIDE" {
				t.Fatal("content from outside the brain was returned")
			}
		}
		return // skipped silently is acceptable; leaking is not
	}
}

func TestLegacyFoldRefusesANonRegularFactStore(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	dir := legacyBranchDir(t, brainDir, "main-00000000")
	store := filepath.Join(dir, "facts.ndjson")
	if err := os.Mkdir(store, 0o755); err != nil {
		t.Fatal(err)
	}
	info, statErr := os.Lstat(store)
	if statErr != nil || info.Mode().IsRegular() {
		t.Fatalf("fixture is a regular file, so it tests nothing (err=%v)", statErr)
	}
	if _, err := factstore.Load(brainDir); err == nil {
		t.Fatal("a non-regular legacy fact store must be refused")
	}
}
