package agentsetup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Options is retained for compatibility with mirrored callers. Agent activation
// does not depend on plugin inventory or Brain runtime stores.
type Options struct {
	ListPlugins                  func() (string, error)
	StateDir, ConfigDir, DataDir string
}

// Preview shows the result of activating product without writing activation.
func Preview(repo, product string, opts Options) (string, error) {
	if product != "graph" && product != "brain" {
		return "", fmt.Errorf("unknown product %q", product)
	}
	if repo == "" {
		if product == "brain" {
			return BrainGuide(), nil
		}
		return GraphGuide, nil
	}
	active, err := readActivation(repo)
	if err != nil {
		return "", err
	}
	active[product] = true
	return renderActivation(active), nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("command output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func localCommand(repo, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = repo
	cmd.Env = os.Environ()
	if name == "git" {
		// Match Brain's configured origin, including includes and URL rewrites.
		// remote get-url reads configuration only: no transport, filters, or hooks.
		// A caller's Git worktree overrides must not redirect the explicit target.
		cmd.Env = nil
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			switch key {
			case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.WaitDelay = 2 * time.Second

	out, stderr := &limitedBuffer{limit: 1 << 20}, &limitedBuffer{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}
func pluginList() (string, error) { return localCommand("", "entire", "plugin", "list") }

var pluginName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func parsePluginList(raw string) (map[string]bool, error) {
	installed := map[string]bool{}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 2 && strings.HasPrefix(lines[0], "No plugins installed in ") && strings.HasPrefix(lines[1], "Install one with 'entire plugin install ") {
		return installed, nil
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Managed plugin directory: ") {
		return nil, fmt.Errorf("unrecognized entire plugin list output")
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if !strings.HasPrefix(line, "  ") || len(fields) < 2 || !pluginName.MatchString(fields[0]) {
			return nil, fmt.Errorf("malformed entire plugin list row")
		}
		if installed[fields[0]] {
			return nil, fmt.Errorf("duplicate plugin list entry %s", fields[0])
		}
		installed[fields[0]] = true
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("empty managed plugin listing")
	}
	return installed, nil
}

var _ io.Writer = (*limitedBuffer)(nil)
