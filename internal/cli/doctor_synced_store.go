package cli

import (
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// SQLite's write-ahead log is not safe on a directory that something else
// syncs or serves over a network. WAL coordinates readers and writers through
// shared-memory and byte-range locks on files sitting NEXT to the database
// (-wal and -shm), and a file sync agent or an SMB share breaks exactly those
// assumptions: it copies the three files at different moments, locks them while
// uploading, and restores them out of order. The failure is not a clean error —
// it is a corrupted or silently stale index.
//
// The brain opens WAL in several places (doc_fts.go, history_fts.go,
// patterns/schema.go), and on Windows the default store location walks straight
// into the hazard: %LOCALAPPDATA% is normally safe, but OneDrive's "Back up your
// folders" feature redirects user profile folders by default on consumer and
// many corporate machines, and domain environments routinely redirect profiles
// to a UNC share. Neither announces itself to us.
//
// So this does not try to be clever. It reports the two shapes that are both
// detectable without a syscall and overwhelmingly the real cases, and says what
// to do rather than merely that something is wrong.

// syncedStoreDirDetail returns a doctor detail string when dataDir looks like it
// sits on synced or networked storage, or "" when it looks fine.
//
// It is a pure function of (goos, dataDir, environment lookup) so the Windows
// branch is exercised from any machine — the same reason planBrainWatchDaemon
// takes its target OS as an argument.
func syncedStoreDirDetail(goos, dataDir string, getenv func(string) string) string {
	if dataDir == "" {
		return ""
	}
	if isUNCPath(dataDir) {
		return dataDir + " is on a network share (UNC path); SQLite's write-ahead log " +
			"is not safe there and the index can be silently corrupted or stale. Point " +
			base.EnvPluginDataDir + " at a local disk."
	}
	if goos != windowsDoctorGOOS {
		return ""
	}
	// OneDrive exports one variable per connected account. Checking all three
	// covers a personal account, a work/school account, and the legacy single
	// spelling, any of which may be the one that owns the redirected profile.
	for _, name := range []string{"OneDrive", "OneDriveCommercial", "OneDriveConsumer"} {
		root := getenv(name)
		if root == "" {
			continue
		}
		if pathIsUnder(dataDir, root) {
			return dataDir + " is inside the OneDrive folder " + root +
				"; OneDrive syncs the SQLite write-ahead log out from under the brain, " +
				"which can corrupt or silently stale the index. Either exclude that folder " +
				"from syncing, or point " + base.EnvPluginDataDir + " at a path outside OneDrive."
		}
	}
	return ""
}

const windowsDoctorGOOS = "windows"

// isUNCPath reports a \\server\share style path. Checked before the goos branch
// because a UNC store is just as unsafe when something else mounted it.
func isUNCPath(path string) bool {
	if len(path) < 2 {
		return false
	}
	// Accept either separator: a path can reach us in slash form.
	return (path[0] == '\\' || path[0] == '/') && path[0] == path[1]
}

// pathIsUnder reports whether path sits at or below root, comparing case
// insensitively because the Windows filesystem does and OneDrive's exported
// variable does not necessarily match the casing of the resolved store path.
//
// It compares cleaned paths component-wise rather than with strings.HasPrefix,
// so a sibling directory whose name merely starts with the root's name (for
// example OneDrive-Archive beside OneDrive) is not reported.
func pathIsUnder(path, root string) bool {
	normalize := func(p string) string {
		return strings.ToLower(filepath.Clean(strings.ReplaceAll(p, "\\", "/")))
	}
	p, r := normalize(path), normalize(root)
	if r == "" || r == "." {
		return false
	}
	if p == r {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/")
}
