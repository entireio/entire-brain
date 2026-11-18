package agentsetup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Existing names remain redirects for clients with explicit legacy imports.
// There is exactly one workflow body, in Path; generated files are not setup state.
const legacyRedirect = "Read the repository-root .entire/agent-guide.md for the current agent workflow.\n"

func migrateMarkers(content []byte) ([]byte, error) {
	for _, product := range []string{"graph", "brain"} {
		begin, end := []byte("<!-- entire-"+product+":begin -->"), []byte("<!-- entire-"+product+":end -->")
		n, m := bytes.Count(content, begin), bytes.Count(content, end)
		if n == 0 && m == 0 {
			continue
		}
		a, b := bytes.Index(content, begin), bytes.Index(content, end)
		if n != 1 || m != 1 || a >= b {
			return nil, fmt.Errorf("malformed Entire %s managed markers; repair before regenerating", product)
		}
		content = append(append([]byte{}, content[:a]...), content[b+len(end):]...)
	}
	return content, nil
}

func preflightLegacy(root *os.Root, abs string) ([]string, error) {
	var names []string
	for _, name := range []string{".entire/graph-agent.md", ".entire/brain-agent.md"} {
		if err := ensureContainedInRepo(root, name, filepath.Join(abs, name), false, ".entire"); err != nil {
			return nil, err
		}
		info, err := statContainedFile(root, name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular guide", name)
		}
		if err := ensureNoSharedInode(root, name); err != nil {
			return nil, err
		}
		for _, other := range []string{"AGENTS.md", "CLAUDE.md"} {
			target, err := statContainedFile(root, other)
			if err == nil && os.SameFile(info, target) {
				return nil, fmt.Errorf("legacy guide %s aliases instruction file %s", name, other)
			}
		}
		// The canonical guide may already be the target of a legacy symlink.
		target, err := statContainedFile(root, Path)
		if err == nil && os.SameFile(info, target) {
			continue
		}
		resolved, err := resolveContainedName(root, name)
		if err != nil {
			return nil, err
		}
		canonical, err := resolveContainedName(root, Path)
		if err != nil {
			return nil, err
		}
		if sameResolvedName(resolved, canonical) {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}
func writeLegacy(root *os.Root, names []string) error {
	for _, name := range names {
		// Recheck identity after materializing the canonical guide and before writes.
		a, ea := statContainedFile(root, name)
		b, eb := statContainedFile(root, Path)
		if ea == nil && eb == nil && os.SameFile(a, b) {
			continue
		}
		if err := writeContainedFile(root, name, []byte(legacyRedirect), 0644); err != nil {
			return err
		}
	}
	return nil
}

// Context gives explicit roots precedence over the host root; otherwise it finds
// the nearest Git root without invoking Git. Explicit roots can be non-Git projects.
func Context(explicit, host string) (string, error) {
	p := explicit
	if p == "" {
		p = host
	}
	if p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("project root is not a directory: %s", abs)
		}
		return abs, nil
	}
	p, err := os.Getwd()
	if err != nil {
		return "", err
	}
	ceilings := gitCeilingDirectories()
	for {
		_, err := os.Lstat(filepath.Join(p, ".git"))
		if err == nil {
			return p, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p || isGitCeiling(parent, ceilings) {
			return "", nil
		}
		p = parent
	}
}

// gitCeilingDirectories reads GIT_CEILING_DIRECTORIES the way git does: a
// list of absolute directories discovery must not step up into. Git itself
// honours it, so a walk that finds repositories "without invoking Git" has to
// as well, or it answers with a repository git would refuse to see -- the
// enclosing home-folder repository the variable is usually set to fence off.
// Empty and relative entries are ignored, as git ignores them.
func gitCeilingDirectories() []string {
	var ceilings []string
	for _, entry := range filepath.SplitList(os.Getenv("GIT_CEILING_DIRECTORIES")) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		ceilings = append(ceilings, comparablePath(entry))
	}
	return ceilings
}

// isGitCeiling reports whether dir is one of the ceilings. The walk climbs one
// directory at a time from the start, so it reaches the nearest ceiling above
// it before anything higher; stopping there is git's "do not chdir up into"
// rule. The starting directory is examined even when it is itself a ceiling,
// which is also what git does.
func isGitCeiling(dir string, ceilings []string) bool {
	if len(ceilings) == 0 {
		return false
	}
	d := comparablePath(dir)
	for _, c := range ceilings {
		if d == c {
			return true
		}
	}
	return false
}

// comparablePath is lexical on purpose: cleaned, and case-folded on Windows,
// where NTFS paths are case-insensitive. It never touches the filesystem,
// because a ceiling entry is caller-controlled and resolving one can reach a
// UNC path and perform network I/O. The cost is that an entry spelled
// differently from the walk (an 8.3 short name against the long one) does not
// match, which leaves the walk exactly as unfenced as it was before.
func comparablePath(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// safeKey permits only Brain's normalized repository-key components.
func safeKey(key string) bool {
	if key == "" || strings.Contains(key, "\\") {
		return false
	}
	parts := strings.Split(key, "/")
	if parts[0] == "workspaces" {
		return false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || cleanComponent(p) != p {
			return false
		}
	}
	return true
}

var unsafeComponent = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func cleanComponent(s string) string {
	return strings.Trim(unsafeComponent.ReplaceAllString(s, "-"), "-._")
}
