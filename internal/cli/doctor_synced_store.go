package cli

import (
	"path"
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

// pathIsUnder reports whether candidate sits at or below root, comparing case
// insensitively because the Windows filesystem does and OneDrive's exported
// variable does not necessarily match the casing of the resolved store path.
//
// It compares whole components rather than raw prefixes, so a sibling directory
// whose name merely starts with the root's name (OneDrive-Archive beside
// OneDrive) is not reported.
//
// THIS FUNCTION MUST NOT USE path/filepath. That is not a style preference: it
// is the bug this file shipped with. The first version normalised separators to
// "/" and then called filepath.Clean, which on Windows -- and ONLY on Windows --
// turns them straight back into "\". The component check that followed looked
// for a prefix ending in "/", so on the one platform the whole OneDrive branch
// exists for, it could never match and doctor could never warn. Every test
// passed on darwin, where filepath.Clean leaves "/" alone, which is precisely
// how it reached a Windows machine.
//
// Normalising with package path instead makes the comparison identical on every
// OS, which is also what lets the tests below be real cross-OS tests rather than
// darwin-shaped ones. TestSyncedStoreComparisonIsOSIndependent pins the rule.
func pathIsUnder(candidate, root string) bool {
	c, r := normalizePathForCompare(candidate), normalizePathForCompare(root)
	if r == "" || r == "." {
		return false
	}
	if c == r {
		return true
	}
	return strings.HasPrefix(c, strings.TrimSuffix(r, "/")+"/")
}

// normalizePathForCompare folds a path to a lower-case, slash-separated,
// cleaned form using package path, never package filepath. See pathIsUnder.
func normalizePathForCompare(p string) string {
	if p == "" {
		return ""
	}
	return strings.ToLower(path.Clean(strings.ReplaceAll(p, "\\", "/")))
}
