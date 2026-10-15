//go:build !windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Git publishes index generations by rename. This stress fixture relies on
// replacing an open file, which is supported on Unix.
func TestScratchIndexContentAndModTimeStayInOneGeneration(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "index")
	times := []time.Time{time.Unix(100, 0), time.Unix(200, 0)}
	contents := []string{strings.Repeat("A", 64<<10), strings.Repeat("B", 64<<10)}
	writeGeneration := func(i int) error {
		next := filepath.Join(dir, "next")
		if err := os.WriteFile(next, []byte(contents[i]), 0600); err != nil {
			return err
		}
		if err := os.Chtimes(next, times[i], times[i]); err != nil {
			return err
		}
		return os.Rename(next, index)
	}
	if err := writeGeneration(0); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := writeGeneration(i % 2); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 200; i++ {
		runner := &envRecordingRunner{indexPath: index}
		if _, _, err := runGitWithScratchIndex(context.Background(), runner, dir, "diff", "HEAD"); err != nil {
			t.Fatal(err)
		}
		for _, call := range runner.calls {
			if call.env["GIT_INDEX_FILE"] == "" {
				continue
			}
			generation := 0
			if call.indexSeen == contents[1] {
				generation = 1
			} else if call.indexSeen != contents[0] {
				t.Fatalf("scratch content does not match either atomic generation")
			}
			if !call.indexModTime.Equal(times[generation]) {
				t.Fatalf("scratch content and mtime belong to different index generations: content=%d, mtime=%s", generation, call.indexModTime)
			}
		}
	}
}
