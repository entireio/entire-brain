package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A FOURTH execution vector, alongside the three neutralized in git_harden.go:
// content FILTERS.
//
//	filter.<driver>.clean     runs when git converts WORKTREE content to its
//	                          stored form. `git diff HEAD` compares the HEAD
//	                          blob against the worktree file, so it must clean
//	                          the worktree copy first -- which means the driver
//	                          executes under the exact hardened argv the other
//	                          three vectors are neutralized by:
//	                            -c core.fsmonitor=false diff --no-ext-diff \
//	                               --no-textconv --binary HEAD --
//	filter.<driver>.smudge    the checkout direction. Not reachable from the
//	                          read paths today, neutralized anyway so a future
//	                          call path that checks content out is covered.
//	filter.<driver>.process   a long-running protocol filter that SUPERSEDES
//	                          clean and smudge, so blanking only .clean leaves
//	                          an armed repository live.
//
// All three were confirmed against git 2.54.0 by arming them in a scratch
// repository and observing the payload run. The driver is selected by an
// in-tree .gitattributes entry (`* filter=pwn`); the command comes from config.
//
// WHY NOT --no-ext-diff-style flags: there are none. git offers no per-command
// switch that disables the filter machinery for diff.
//
// WHY NOT GIT_ATTR_SOURCE: pointing attributes at the empty tree does stop an
// in-tree .gitattributes from selecting a driver, and it neutralizes both
// vectors -- but it is BYPASSABLE. .git/info/attributes is not a tree, is not
// covered by GIT_ATTR_SOURCE, and is carried by a cloned repository just like
// .git/config. Verified: with the driver selected from .git/info/attributes the
// payload still ran. It would also silently change diff output by discarding
// every legitimate attribute (-diff, text, eol, binary).
//
// So the neutralizer blanks the driver's COMMANDS, which works no matter which
// attributes source selected it. `required` is forced false at the same time:
// git aborts a read outright ("fatal: ... clean filter 'x' failed") when a
// required filter has no command, and turning code execution into a hard
// failure to index is just a different denial.
//
// WHY THE ENVIRONMENT AND NOT `-c`: a config subsection may contain "=", and
// .gitattributes can select it. `-c` splits at the FIRST "=", so
// `-c filter.na=me.clean=` sets "filter.na" to "me.clean=" and leaves the real
// driver armed -- verified: the payload runs. GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n carry key and value in separate variables, so no driver
// name can escape the neutralizer.
//
// WHY ONLY local AND worktree SCOPE: the finding is that a REPOSITORY must not
// make git run code. A driver in the user's own ~/.gitconfig is not the
// repository's code -- `git lfs install` writes filter.lfs.clean there -- and
// blanking it would silently corrupt every diff of an LFS repository, since git
// would compare a real worktree file against a stored pointer. Local and
// worktree scope are exactly the scopes the repository itself carries.

// gitConfigOverride is one key/value pair forced onto a git invocation.
type gitConfigOverride struct {
	Key   string
	Value string
}

// gitFilterOverrideKeys are the three keys that name an executable, plus the
// flag that decides whether blanking them is fatal.
var gitFilterOverrideKeys = []struct{ suffix, value string }{
	{"clean", ""},
	{"smudge", ""},
	{"process", ""},
	{"required", "false"},
}

// repoFilterEnumerateTimeout bounds the one `git config` probe per repository.
const repoFilterEnumerateTimeout = 5 * time.Second

type repoFilterCacheEntry struct {
	signature string
	overrides []gitConfigOverride
}

var repoFilterCache sync.Map // repoDir -> repoFilterCacheEntry

// repoFilterDriverOverrides returns the config overrides that disarm every
// filter driver the REPOSITORY configures. The common case -- no repo-local
// filter driver at all -- returns nil and costs nothing beyond the probe.
//
// The result is cached per repository, keyed by a stat signature of the config
// files it can come from, so a config that changes underneath us is picked up
// while a steady repository costs one `git config` for the whole process. That
// matters: history indexing runs thousands of git commands, and a fork per
// command would be a visible slowdown.
func repoFilterDriverOverrides(ctx context.Context, repoDir string) []gitConfigOverride {
	if strings.TrimSpace(repoDir) == "" {
		return nil
	}
	signature := repoConfigSignature(repoDir)
	if cached, ok := repoFilterCache.Load(repoDir); ok {
		if entry := cached.(repoFilterCacheEntry); entry.signature == signature {
			return entry.overrides
		}
	}
	overrides := enumerateRepoFilterDrivers(ctx, repoDir)
	repoFilterCache.Store(repoDir, repoFilterCacheEntry{signature: signature, overrides: overrides})
	return overrides
}

// enumerateRepoFilterDrivers asks git which filter.* keys the repository itself
// sets. It spawns git directly rather than going through the hardened runner:
// the runner calls back into this function, and `git config` reads no worktree
// content, so no filter can run during the probe.
//
// Spawning directly means hardenedGitArgs does not run here, so this is the one
// git invocation in the binary that has to carry gitHardenConfig itself.
// Splicing the shared slice in rather than repeating its flags is what keeps
// the probe from drifting behind the chokepoint: core.hooksPath was added to
// gitHardenConfig after a `.git/hooks/post-index-change` was found to execute
// during indexing, and a hand-copied `-c core.fsmonitor=false` here would have
// silently missed it. `git config` does not touch the index and so cannot fire
// post-index-change today; the flags are applied because "every git spawn is
// hardened" is a cheaper invariant to keep than a per-call-site argument about
// which hooks a subcommand can reach.
func enumerateRepoFilterDrivers(ctx context.Context, repoDir string) []gitConfigOverride {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, repoFilterEnumerateTimeout)
	defer cancel()
	// -z keeps the output unambiguous for a subsection name containing a
	// newline; --show-scope tells local/worktree apart from global/system.
	args := append(slices.Clone(gitHardenConfig),
		"config", "-z", "--show-scope", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process|required)$`)
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 simply means "no matching keys", which is the norm.
		// Any other failure leaves us without an enumeration; there is nothing
		// safe to invent, and the three other vectors remain neutralized.
		return nil
	}
	fields := strings.Split(string(out), "\x00")
	seen := map[string]bool{}
	var drivers []string
	for i := 0; i+1 < len(fields); i += 2 {
		scope, key := fields[i], fields[i+1]
		if scope != "local" && scope != "worktree" {
			continue
		}
		driver, ok := filterDriverFromConfigKey(key)
		if !ok || seen[driver] {
			continue
		}
		seen[driver] = true
		drivers = append(drivers, driver)
	}
	var overrides []gitConfigOverride
	for _, driver := range drivers {
		for _, k := range gitFilterOverrideKeys {
			overrides = append(overrides, gitConfigOverride{Key: "filter." + driver + "." + k.suffix, Value: k.value})
		}
	}
	return overrides
}

// filterDriverFromConfigKey pulls "na=me" out of "filter.na=me.clean". The
// driver name is everything between the first and last dot, so a name
// containing dots, equals signs or spaces round-trips intact.
func filterDriverFromConfigKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "filter.")
	if !ok {
		return "", false
	}
	dot := strings.LastIndex(rest, ".")
	if dot <= 0 {
		return "", false
	}
	return rest[:dot], true
}

// repoConfigSignature is a cheap fingerprint of the config files a repository
// can carry its own filter drivers in. An empty signature (a repoDir that is
// not a repository root, so the files cannot be located) still caches, which is
// safe: a repository does not rewrite its own config mid-run without already
// having execution.
func repoConfigSignature(repoDir string) string {
	gitDir := filepath.Join(repoDir, ".git")
	if info, err := os.Stat(gitDir); err == nil && !info.IsDir() {
		// A worktree or submodule checkout: ".git" is a file naming the real dir.
		if data, err := os.ReadFile(gitDir); err == nil {
			if pointed, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:"); ok {
				gitDir = strings.TrimSpace(pointed)
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(repoDir, gitDir)
				}
			}
		}
	}
	var b strings.Builder
	for _, name := range []string{"config", "config.worktree"} {
		info, err := os.Stat(filepath.Join(gitDir, name))
		if err != nil {
			b.WriteString("-|")
			continue
		}
		fmt.Fprintf(&b, "%d:%d|", info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}

// gitConfigOverrideEnv appends overrides to base as GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n entries, continuing any GIT_CONFIG_COUNT the caller's
// environment already set rather than clobbering it.
func gitConfigOverrideEnv(base []string, overrides []gitConfigOverride) []string {
	if len(overrides) == 0 {
		return base
	}
	next := 0
	for _, entry := range base {
		if value, ok := strings.CutPrefix(entry, "GIT_CONFIG_COUNT="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
				next = n
			}
		}
	}
	out := make([]string, 0, len(base)+2*len(overrides)+1)
	for _, entry := range base {
		if strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") {
			continue
		}
		out = append(out, entry)
	}
	for _, override := range overrides {
		out = append(out,
			"GIT_CONFIG_KEY_"+strconv.Itoa(next)+"="+override.Key,
			"GIT_CONFIG_VALUE_"+strconv.Itoa(next)+"="+override.Value,
		)
		next++
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(next))
}
