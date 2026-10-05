package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIssueStoreReadRejectsOversizedState(t *testing.T) {
	for _, rel := range []string{"issues/state.json", "issues/derived/vectors.json"} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			valid := []byte(`{"version":1}`)
			if err := os.WriteFile(path, valid, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := issueStore(dir).Read(path)
			if err != nil || !bytes.Equal(got, valid) {
				t.Fatalf("valid state read: %q, %v", got, err)
			}
			if err := os.Truncate(path, defaultMaxReadBytes+1); err != nil {
				t.Fatal(err)
			}
			got, err = issueStore(dir).Read(path)
			var overflow *readBoundExceededError
			if !errors.As(err, &overflow) || got != nil {
				t.Fatalf("oversized state returned %d bytes with error %v", len(got), err)
			}
		})
	}
}
