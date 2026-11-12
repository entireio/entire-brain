package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// maintainFactsStorage runs as part of ordinary permitted write activity. A
// paused watcher never calls it; foreground writes retain their normal meaning.
// Each store is independent and uses a try-lock, so maintenance never queues
// behind another writer or holds repository and global locks together.
func maintainFactsStorage(ctx context.Context, out io.Writer, opts Options, brainDir string) {
	globalDir, err := factstore.GlobalBrainDir(opts.Env)
	if err != nil {
		fmt.Fprintf(out, "warning: global memory cleanup: %v\n", err)
	}
	for _, target := range []struct {
		dir    string
		global bool
	}{{brainDir, false}, {globalDir, true}} {
		if ctx.Err() != nil {
			return
		}
		if target.dir == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(target.dir, "facts")); os.IsNotExist(err) {
			continue
		}
		unlock, err := brainstore.AcquireBrainWriteLockTimeout(target.dir, 0)
		if errors.Is(err, brainstore.ErrFileLockTimeout) {
			continue
		}
		if err != nil {
			fmt.Fprintf(out, "warning: memory cleanup: %v\n", err)
			continue
		}
		summary, migrateErr := factstore.MigrateRepository(target.dir)
		if migrateErr == nil && summary.Changed() && !target.global {
			migrateErr = updateFactSourceManifestLocked(target.dir, opts.Now().UTC())
		}
		unlock()
		if migrateErr != nil {
			fmt.Fprintf(out, "warning: memory cleanup: %v\n", migrateErr)
			continue
		}
		if summary.Cleanup.PendingDirs > 0 {
			cleanup, cleanupErr := factstore.CleanupLegacy(target.dir)
			if cleanupErr != nil {
				fmt.Fprintf(out, "warning: memory cleanup: %v\n", cleanupErr)
			} else {
				summary.Cleanup = cleanup
			}
		}
		if summary.StragglerFiles > 0 {
			fmt.Fprintln(out, "warning: an older entire-brain wrote memories after migration; they were upgraded — update that machine")
		}
		// One line, and only when the retained set CHANGES: this set needs a
		// human decision and never shrinks on its own, so an ungated warning
		// reprinted the full list on every write, forever. The list itself
		// lives in facts/.cleanup-status.json and `memories storage`.
		if summary.Cleanup.State == "retained" && factstore.CleanupNoticeDue(target.dir, summary.Cleanup) {
			fmt.Fprintf(out, "\nwarning: memory cleanup kept recovery data from the pre-flat memory store; run `%s memories storage` for details\n\n",
				base.SetupBrainCommand(base.SetupCommandPrefix(os.LookupEnv)))
		}
	}
}
