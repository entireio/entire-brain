package agentsetup

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// These records are written by Brain setup.go and export.go, respectively.
// Neither daemon liveness nor index freshness participates in mode selection.
type brainSetupRecord struct {
	SchemaVersion  int       `json:"schema_version"`
	UpdatedAt      time.Time `json:"updated_at"`
	Workspace      string    `json:"workspace,omitempty"`
	DaemonName     string    `json:"daemon_name,omitempty"`
	Interval       string    `json:"interval,omitempty"`
	DistillEvery   string    `json:"distill_every,omitempty"`
	Model          string    `json:"model,omitempty"`
	Effort         string    `json:"effort,omitempty"`
	Agent          string    `json:"agent,omitempty"`
	BackfillBudget *int      `json:"backfill_budget,omitempty"`
}

func brainDirectory(explicit, override, xdg, fallback string) (string, error) {
	dir := explicit
	if dir == "" {
		dir = os.Getenv(override)
	}
	if dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("%s must be absolute", override)
		}
		return dir, nil
	}
	if dir = os.Getenv(xdg); filepath.IsAbs(dir) {
		return filepath.Join(dir, "entire"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback, "entire"), nil
}
func readRecord(dir, name string) ([]byte, bool, error) {
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	var info os.FileInfo
	prefix := ""
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		prefix = filepath.Join(prefix, part)
		info, err = root.Lstat(prefix)
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("setup state %s is a symbolic link", filepath.Join(dir, prefix))
		}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 {
		return nil, false, fmt.Errorf("setup state %s is not a readable regular file", filepath.Join(dir, name))
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("setup record is not regular")
	}
	const limit = 16 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > limit {
		return nil, false, fmt.Errorf("setup record exceeds size limit")
	}
	return data, true, nil
}
func repositoryBrain(repo string, opts Options) (bool, error) {
	state, err := brainDirectory(opts.StateDir, "ENTIRE_BRAIN_STATE_DIR", "XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return false, err
	}
	config, err := brainDirectory(opts.ConfigDir, "ENTIRE_BRAIN_CONFIG_DIR", "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return false, err
	}
	data, err := brainDirectory(opts.DataDir, "ENTIRE_BRAIN_DATA_DIR", "XDG_DATA_HOME", filepath.Join(".local", "share"))
	if err != nil {
		return false, err
	}
	if opts.DataDir == "" && os.Getenv("ENTIRE_BRAIN_DATA_DIR") == "" {
		data = filepath.Join(data, "plugins", "data", "brain")
	}
	remote := ""
	if _, err := os.Lstat(filepath.Join(repo, ".git")); err == nil {
		remote, err = localCommand(repo, "git", "remote", "get-url", "origin")
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 2) {
			return false, fmt.Errorf("read repository identity: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	keys, err := brainKeys(repo, strings.TrimSpace(remote), config)
	if err != nil {
		return false, err
	}
	found := 0
	for _, key := range keys {
		if !safeKey(key) {
			return false, fmt.Errorf("unsafe Brain repository identity %q", key)
		}
		record, present, err := readRecord(state, filepath.Join("repos", filepath.FromSlash(key), "setup.json"))
		if err != nil {
			return false, err
		}
		if present {
			var setup brainSetupRecord
			if err := json.Unmarshal(record, &setup); err != nil {
				return false, fmt.Errorf("malformed Brain setup record: %w", err)
			}
			if setup.SchemaVersion != 1 {
				return false, fmt.Errorf("unsupported Brain setup record schema %d", setup.SchemaVersion)
			}
			found++
			continue
		}
		// Legacy/manual build and refresh paths can create a Brain without setup.
		record, present, err = readRecord(data, filepath.Join("repos", filepath.FromSlash(key), "manifest.json"))
		if err != nil {
			return false, err
		}
		if present {
			var manifest struct {
				SchemaVersion int    `json:"schema_version"`
				RepoKey       string `json:"repo_key"`
				RepoRoot      string `json:"repo_root"`
			}
			if err := json.Unmarshal(record, &manifest); err != nil {
				return false, fmt.Errorf("malformed Brain manifest: %w", err)
			}
			if manifest.SchemaVersion < 1 || manifest.SchemaVersion > 3 {
				return false, fmt.Errorf("unsupported Brain manifest schema %d", manifest.SchemaVersion)
			}
			if manifest.RepoKey != "" && manifest.RepoKey != key {
				return false, fmt.Errorf("Brain manifest repository identity mismatch")
			}
			found++
		}
	}
	if found > 1 {
		return false, fmt.Errorf("conflicting canonical and legacy Brain setup records; resolve repository identity before generating instructions")
	}
	return found == 1, nil
}

var unsafeComponent = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
var scpRemote = regexp.MustCompile(`^(?:[^@]+@)?([^:/]+):(.+)$`)

func cleanComponent(s string) string {
	return strings.Trim(unsafeComponent.ReplaceAllString(s, "-"), "-._")
}
func normalizePath(s string) []string {
	s = strings.TrimSuffix(strings.Trim(strings.TrimSpace(s), "/"), ".git")
	var parts []string
	for _, p := range strings.Split(s, "/") {
		if p = cleanComponent(strings.ToLower(p)); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
func localKey(p string) string {
	p = filepath.Clean(p)
	sum := sha256.Sum256([]byte(p))
	base := cleanComponent(filepath.Base(p))
	if base == "" {
		base = "repo"
	}
	return fmt.Sprintf("local/%s-%x", base, sum[:6])
}
func brainKeys(repo, remote, config string) ([]string, error) {
	var host, path string
	if u, err := url.Parse(remote); err == nil && strings.Contains(remote, "://") && u.Host != "" {
		host = strings.ToLower(u.Hostname())
		path = u.Path
		if strings.EqualFold(u.Scheme, "entire") {
			parts := normalizePath(path)
			if len(parts) < 3 {
				return nil, fmt.Errorf("invalid Entire repository identity")
			}
			return []string{strings.Join(parts, "/")}, nil
		}
	} else if m := scpRemote.FindStringSubmatch(remote); len(m) == 3 && !(len(m[1]) == 1 && (strings.HasPrefix(m[2], "/") || strings.Contains(m[2], "\\") || !strings.Contains(m[2], "/"))) {
		host = strings.ToLower(m[1])
		path = m[2]
	} else if h, p, ok := strings.Cut(remote, "/"); ok && strings.Contains(h, ".") {
		host = strings.ToLower(h)
		path = p
	}
	parts := normalizePath(path)
	if host == "" || len(parts) == 0 {
		canonical, err := filepath.EvalSymlinks(repo)
		if err != nil {
			return nil, err
		}
		keys := []string{localKey(canonical)}
		// Brain formerly resolved only the root's final component, retaining
		// symlinked ancestors. Check that spelling as well as the caller's.
		finalRoot, err := finalComponentRoot(repo)
		if err != nil {
			return nil, err
		}
		for _, spelling := range []string{finalRoot, repo} {
			if legacy := localKey(spelling); !slices.Contains(keys, legacy) {
				keys = append(keys, legacy)
			}
		}
		return keys, nil
	}
	slug := map[string]string{"github.com": "gh", "gitlab.com": "gl", "bitbucket.org": "bb", "entire.io": "et", "tangled.org": "tg", "code.storage": "cs"}[host]
	if slug == "" {
		data, exists, err := readRecord(config, "brain.json")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, nil
		}
		var cfg struct {
			DomainSlugs map[string]string `json:"domain_slugs"`
		}
		if strings.TrimSpace(string(data)) == "null" {
			return nil, fmt.Errorf("malformed Brain configuration")
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("malformed Brain configuration: %w", err)
		}
		slug = cfg.DomainSlugs[host]
		if slug == "" {
			return nil, nil
		}
	}
	if !safeKey(slug) {
		return nil, fmt.Errorf("invalid Brain host mapping")
	}
	return []string{strings.Join(append([]string{slug}, parts...), "/")}, nil
}

func finalComponentRoot(repo string) (string, error) {
	current := filepath.Clean(repo)
	for range 40 {
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		target, err := os.Readlink(current)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			current = filepath.Clean(target)
		} else {
			current = filepath.Join(filepath.Dir(current), target)
		}
	}
	return "", fmt.Errorf("too many repository root symbolic links")
}
