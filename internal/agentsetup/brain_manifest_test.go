package agentsetup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryBrainLargeManifestWithoutSetup(t *testing.T) {
	for _, size := range []int{18569433, 17220291, 80 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			repo := t.TempDir()
			canonical, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{StateDir: t.TempDir(), ConfigDir: t.TempDir(), DataDir: t.TempDir()}
			dir := filepath.Join(opts.DataDir, "repos", localKey(canonical))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "manifest.json")
			prefix := `{"schema_version":3,"sessions":[{"summary":{"intent":"`
			suffix := `"}}]}`
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix); err != nil {
				t.Fatal(err)
			}
			f.Close()
			found, err := repositoryBrain(repo, opts)
			if err != nil || !found {
				t.Fatalf("Brain detection: found=%v err=%v", found, err)
			}
			// The same bytes remain oversized for unrelated operational records.
			if _, _, err := readRecord(dir, "manifest.json"); err == nil {
				t.Fatal("setup record ceiling was weakened")
			}
		})
	}
}

func TestRepositoryBrainManifestValidation(t *testing.T) {
	for _, body := range []string{`{"schema_version":3`, `{"schema_version":3} {}`, `{"schema_version":3}x`, `{"schema_version":4}`, `{}`, `null`} {
		t.Run(body, func(t *testing.T) {
			repo := t.TempDir()
			canonical, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{StateDir: t.TempDir(), ConfigDir: t.TempDir(), DataDir: t.TempDir()}
			dir := filepath.Join(opts.DataDir, "repos", localKey(canonical))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := repositoryBrain(repo, opts); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
