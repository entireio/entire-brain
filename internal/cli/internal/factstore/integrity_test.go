package factstore

import (
	"reflect"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestLoadWithIntegrityMatchesLoadAndInspect(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, []factmerge.Record{repoStoreFact("one memory", "main"), repoStoreFact("another memory", "main")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		source *bmanifest.FactSource
	}{
		{"healthy", &bmanifest.FactSource{Facts: 2}},
		{"lossy", &bmanifest.FactSource{Facts: 3}},
		{"stale", &bmanifest.FactSource{Facts: 1}},
		{"no declared source", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, integrity, err := LoadWithIntegrity(root, tc.source)
			if err != nil {
				t.Fatal(err)
			}
			wantRecords, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(records, wantRecords) {
				t.Fatalf("records diverge from Load: got %d, want %d", len(records), len(wantRecords))
			}
			if wantIntegrity := Inspect(root, tc.source); !reflect.DeepEqual(integrity, wantIntegrity) {
				t.Fatalf("integrity diverges from Inspect: got %+v, want %+v", integrity, wantIntegrity)
			}
		})
	}
}

func TestLoadWithIntegrityCorruptStoreFailsClosedAndReportsUnreadable(t *testing.T) {
	root := t.TempDir()
	if err := brainstore.WriteBrainRelativeFileAtomic(root, RepositoryPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &bmanifest.FactSource{Facts: 1}
	records, integrity, err := LoadWithIntegrity(root, source)
	if err == nil {
		t.Fatal("corrupt store must fail closed")
	}
	if records != nil {
		t.Fatalf("corrupt store must return no records, got %d", len(records))
	}
	if want := Inspect(root, source); !reflect.DeepEqual(integrity, want) {
		t.Fatalf("integrity diverges from Inspect on corruption: got %+v, want %+v", integrity, want)
	}
	if integrity.Mode != DefectUnreadable {
		t.Fatalf("corrupt store must report DefectUnreadable, got %q", integrity.Mode)
	}
}
