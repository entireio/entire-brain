package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedDocumentUsesConfiguredCopyLimit(t *testing.T) {
	for _, limit := range []int{16, defaultSeedMaxFileBytes + 128} {
		repo, out := t.TempDir(), t.TempDir()
		source := strings.Repeat("a", limit+32)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		opts := seedCommandOptions{maxFileBytes: limit, maxFiles: 10, includeTests: true}
		scan, err := scanSeedRepository(context.Background(), &fakeCommandRunner{}, repo, "test", opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(scan.Docs) != 1 {
			t.Fatalf("scan docs: %+v", scan.Docs)
		}
		if err := writeSeedArtifacts(out, &scan); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, scan.Docs[0].SeedPath))
		if err != nil {
			t.Fatal(err)
		}
		want := source[:limit] + "\n\n[truncated]\n"
		if string(data) != want {
			t.Errorf("limit=%d copied=%d wanted=%d", limit, len(data), len(want))
		}
		if !scan.Docs[0].Truncated || scan.Docs[0].Bytes != int64(limit) {
			t.Errorf("metadata does not describe copied source bytes: %+v", scan.Docs[0])
		}
	}
}
