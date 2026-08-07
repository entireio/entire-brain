package cli

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const losslessConcurrencyTestLockTimeout = 30 * time.Second

func withBrainWriteLockForLosslessConcurrencyTest(brainDir string, fn func() error) error {
	unlock, err := acquireBrainWriteLockTimeout(brainDir, losslessConcurrencyTestLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func TestConcurrentFactCommitsAreLossless(t *testing.T) {
	brainDir := t.TempDir()
	branch := "main"
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	const workers = 32
	makeFact := func(text string, paths []string, at time.Time) factRecord {
		p := normalizeFactPaths(paths)
		return factRecord{
			ID:        factRecordID(text, p),
			Paths:     p,
			Text:      text,
			Branch:    branch,
			Origin:    factOriginDistilled,
			Status:    factStatusActive,
			CreatedAt: at,
			UpdatedAt: at,
		}
	}

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			unique := makeFact(fmt.Sprintf("unique fact %02d", i), []string{"project.tooling.stack"}, now.Add(time.Duration(i)*time.Second))
			unique.Provenance = []factAnchor{{SessionID: fmt.Sprintf("unique-%02d", i), Line: i + 1}}
			shared := makeFact("shared fact", []string{"preferences.coding.style"}, now)
			shared.Provenance = []factAnchor{{SessionID: fmt.Sprintf("shared-%02d", i), Line: i + 1}}
			errs <- withBrainWriteLockForLosslessConcurrencyTest(brainDir, func() error {
				facts, err := loadFacts(brainDir, branch)
				if err != nil {
					return err
				}
				facts = upsertFact(facts, unique)
				facts = upsertFact(facts, shared)
				if err := writeFacts(brainDir, branch, facts); err != nil {
					return err
				}
				return updateFactSourceManifestLocked(brainDir, now)
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent commit: %v", err)
		}
	}

	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if got, want := len(facts), workers+1; got != want {
		t.Fatalf("facts len = %d, want %d: %+v", got, want, facts)
	}
	ids := map[string]factRecord{}
	for _, fact := range facts {
		if _, exists := ids[fact.ID]; exists {
			t.Fatalf("duplicate fact id on disk: %s", fact.ID)
		}
		ids[fact.ID] = fact
	}
	sharedID := factRecordID("shared fact", normalizeFactPaths([]string{"preferences.coding.style"}))
	shared, ok := ids[sharedID]
	if !ok {
		t.Fatalf("shared fact missing")
	}
	if got := len(shared.Provenance); got != workers {
		t.Fatalf("shared provenance len = %d, want %d: %+v", got, workers, shared.Provenance)
	}
	for i := 0; i < workers; i++ {
		id := factRecordID(fmt.Sprintf("unique fact %02d", i), normalizeFactPaths([]string{"project.tooling.stack"}))
		if _, ok := ids[id]; !ok {
			t.Fatalf("unique fact %d missing", i)
		}
	}
	sorted := append([]factRecord(nil), facts...)
	sortFactRecords(sorted)
	if !reflect.DeepEqual(facts, sorted) {
		var idsOnDisk []string
		for _, fact := range facts {
			idsOnDisk = append(idsOnDisk, fact.ID)
		}
		t.Fatalf("facts are not deterministically sorted: %s", strings.Join(idsOnDisk, ", "))
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Facts == nil {
		t.Fatalf("facts source missing from manifest: %+v", manifest.Sources)
	}
	if manifest.Sources.Facts.Facts != workers+1 || manifest.Sources.Facts.Distilled != workers+1 {
		t.Fatalf("manifest fact counts wrong: %+v", manifest.Sources.Facts)
	}
}
