package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// semanticCancelSnapshot writes a snapshot big enough that building a store from
// it takes long enough to cancel part-way through.
func semanticCancelSnapshot(t *testing.T, files, symbols int) ([]byte, semanticHeader, semanticCounts) {
	t.Helper()
	header := semanticHeader{SchemaVersion: "1.0", Provider: "entire-graph", Commit: "deadbeef", Tree: "cafebabe"}
	var buf bytes.Buffer
	line, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	buf.Write(line)
	buf.WriteByte('\n')
	write := func(record semanticRecord) {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	for i := 0; i < files; i++ {
		write(semanticRecord{RecordType: "file", FilePath: fmt.Sprintf("internal/pkg%d/file%d.go", i%32, i), Language: "go"})
	}
	for i := 0; i < symbols; i++ {
		write(semanticRecord{
			RecordType: "symbol",
			ID:         fmt.Sprintf("symbol:%d", i),
			Kind:       "function",
			Name:       fmt.Sprintf("Symbol%d", i),
			FilePath:   fmt.Sprintf("internal/pkg%d/file%d.go", i%32, i%files),
			Signature:  strings.Repeat("arg string, ", 6),
			Language:   "go",
		})
	}
	return buf.Bytes(), header, semanticCounts{Files: files, Symbols: symbols}
}

// TestTheStoreBuildStopsWhenItsDeadlineHasPassed.
//
// brain_index_repository advertises "Calls are capped at 60 seconds" and
// returned at ~113 seconds of wall clock, because the deadline decided only WHEN
// THE ERROR WAS CHOSEN, not when the call returned: buildSemanticGeneration took
// no context at all, so once the provider stream finished the store build ran to
// completion and the cap was reported afterwards. A host configured to the
// advertised 60 seconds therefore timed out on exactly the calls the cap existed
// to protect it from.
//
// Measured against the real repository with the cap set to 40 seconds: before,
// the call returned at 50.78s; after, at 41.56s.
func TestTheStoreBuildStopsWhenItsDeadlineHasPassed(t *testing.T) {
	t.Parallel()

	snapshot, header, counts := semanticCancelSnapshot(t, 100, 5000)
	repoDir := t.TempDir()

	// How long does the whole build take when nothing interrupts it?
	full := t.TempDir()
	start := time.Now()
	if _, _, err := buildSemanticGeneration(context.Background(), full, repoDir, "whole", bytes.NewReader(snapshot),
		semanticGenerationContentSuffix(snapshot), header, counts, time.Now().UTC()); err != nil {
		t.Fatalf("uninterrupted build: %v", err)
	}
	uninterrupted := time.Since(start)
	t.Logf("uninterrupted build took %s", uninterrupted)
	if uninterrupted < 150*time.Millisecond {
		t.Skipf("build finished in %s, too fast to observe a mid-build deadline", uninterrupted)
	}

	// Now give it a deadline that expires a fraction of the way in.
	brainDir := t.TempDir()
	deadline := uninterrupted / 8
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start = time.Now()
	generation, _, err := buildSemanticGeneration(ctx, brainDir, repoDir, "cut", bytes.NewReader(snapshot),
		semanticGenerationContentSuffix(snapshot), header, counts, time.Now().UTC())
	elapsed := time.Since(start)
	t.Logf("interrupted build returned after %s (deadline %s)", elapsed, deadline)

	if err == nil {
		t.Fatalf("an expired build reported success after %s (uninterrupted takes %s); generation %q", elapsed, uninterrupted, generation)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("build failed for the wrong reason: %v", err)
	}
	// The point of the deadline is the WALL CLOCK, not the error text: the build
	// has to stop near its deadline, not run to completion and then be thrown
	// away. Half the uninterrupted time is a generous bound that still fails
	// outright when nothing checks the context.
	if elapsed > uninterrupted/2 {
		t.Errorf("build ran %s past a %s deadline (uninterrupted build takes %s); the deadline did not interrupt the work",
			elapsed, deadline, uninterrupted)
	}
	// Nothing half-built is left behind.
	generations := filepath.Join(brainDir, semanticDirName, semanticGenerationsDir)
	entries, readErr := os.ReadDir(generations)
	if readErr == nil {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".tmp-") {
				t.Errorf("an interrupted build left a generation behind: %s", entry.Name())
			}
		}
	}
}

// TestAnIndexStageRefusesToStartPastItsDeadline covers the other half: work
// between stages (provider verification, snapshot write, store build, overlay,
// manifest) must not begin once the caller has already given up.
func TestAnIndexStageRefusesToStartPastItsDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := semanticIndexStageDeadline(ctx, "building store")
	if err == nil {
		t.Fatal("an expired index started its next stage anyway")
	}
	if !strings.Contains(err.Error(), "building store") {
		t.Errorf("the error does not say which stage the time went to: %v", err)
	}
}
