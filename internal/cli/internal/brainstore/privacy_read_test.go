package brainstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrivacyReadersOnlyReportConfirmedAbsence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	readers := []struct {
		name, wrongType string
		read            func(string) (bool, error)
	}{
		{"info", "dir", func(rel string) (bool, error) {
			_, present, err := PrivacyArtifactInfo(root, rel, "fixture")
			return present, err
		}},
		{"artifact", "dir", func(rel string) (bool, error) {
			_, present, err := ReadPrivacyArtifact(root, rel, "fixture", 100)
			return present, err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			if present, err := reader.read("missing"); present || err != nil {
				t.Fatalf("missing: present=%v err=%v", present, err)
			}
			for _, rel := range []string{reader.wrongType, "../outside"} {
				if present, err := reader.read(rel); !present || err == nil || !strings.Contains(err.Error(), StateUnsafeCode) {
					t.Fatalf("unsafe %q: present=%v err=%v", rel, present, err)
				}
			}
			t.Run("stat failure", func(t *testing.T) {
				original := PrivacyLstat
				PrivacyLstat = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
				t.Cleanup(func() { PrivacyLstat = original })
				if present, err := reader.read("file"); !present || !errors.Is(err, os.ErrPermission) {
					t.Fatalf("unknown existence: present=%v err=%v", present, err)
				}
			})
			for _, target := range []string{"file", "dir", "missing"} {
				t.Run("symlink to "+target, func(t *testing.T) {
					link := reader.name + "-link-" + target
					if err := os.Symlink(filepath.Join(root, target), filepath.Join(root, link)); err != nil {
						if runtime.GOOS == "windows" {
							t.Skipf("symlink unavailable: %v", err)
						}
						t.Fatal(err)
					}
					if present, err := reader.read(link); !present || err == nil || !strings.Contains(err.Error(), StateUnsafeCode) {
						t.Fatalf("symlink: present=%v err=%v", present, err)
					}
				})
			}
		})
	}
}

func TestPrivacyOpenFailuresAreUnsafe(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{os.ErrPermission, os.ErrNotExist} {
		t.Run(cause.Error(), func(t *testing.T) {
			original := PrivacyOpen
			PrivacyOpen = func(string, int, os.FileMode) (*os.File, error) { return nil, cause }
			t.Cleanup(func() { PrivacyOpen = original })
			data, present, err := ReadPrivacyArtifact(root, "file", "fixture", 100)
			if data != nil || !present || !errors.Is(err, cause) || !strings.Contains(err.Error(), StateUnsafeCode) {
				t.Fatalf("artifact open: data=%q present=%v err=%v", data, present, err)
			}
		})
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "evidence" {
		t.Fatalf("source changed: data=%q err=%v", data, err)
	}
}
