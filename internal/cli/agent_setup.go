package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	brainAgentGuidePath   = ".entire/brain-agent.md"
	brainAgentBegin       = "<!-- entire-brain:begin -->"
	brainAgentEnd         = "<!-- entire-brain:end -->"
	brainInstructionLimit = 4 << 20
)

func newInitAgentsCommand(opts Options) *cobra.Command {
	var repo string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "init-agents [path]",
		Short: "Install the coding-agent guide into AGENTS.md/CLAUDE.md",
		Long:  "Install .entire/brain-agent.md and managed pointer blocks in AGENTS.md and CLAUDE.md. Existing text outside Brain's markers is preserved. Re-running updates the guide without duplicate blocks. This does not build a brain or start services.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if cmd.Flags().Changed("repo") {
					return fmt.Errorf("supply either a path or --repo, not both")
				}
				repo = args[0]
			}
			if repo == "" {
				repo = opts.Env.RepoRoot
			}
			if repo == "" {
				repo = "."
			}
			changed, err := installBrainAgentGuide(repo)
			if err != nil {
				return fmt.Errorf("init-agents: %w", err)
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"changed_files": changed})
			}
			if len(changed) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Agent instructions are already up to date.")
			} else {
				for _, path := range changed {
					if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Updated %s\n", path); err != nil {
						return err
					}
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "Project root (default: host repository or current directory)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit changed file paths as JSON")
	return cmd
}

type brainAgentFile struct {
	name    string
	data    []byte
	mode    os.FileMode
	changed bool
}

func installBrainAgentGuide(dir string) ([]string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	pointer := brainAgentBegin + "\n" +
		"Read .entire/brain-agent.md for this project's Entire Brain operating guide.\n" +
		"@.entire/brain-agent.md\n" + brainAgentEnd + "\n"
	var files []brainAgentFile
	seen := map[string]bool{}
	// Preflight every file before writing: malformed user markers and escaping
	// links must not leave a half-installed guide.
	for _, name := range []string{brainAgentGuidePath, "AGENTS.md", "CLAUDE.md"} {
		target, err := resolveBrainAgentTarget(root, abs, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if seen[target] {
			if target == files[0].name {
				return nil, fmt.Errorf("%s aliases the managed guide", name)
			}
			continue // AGENTS.md and CLAUDE.md may be relative symlink aliases.
		}
		seen[target] = true
		old, mode, err := readBrainInstruction(root, target)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		data := brainAgentGuide
		if name != brainAgentGuidePath {
			data, err = upsertBrainAgentPointer(string(old), pointer)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		files = append(files, brainAgentFile{target, []byte(data), mode, string(old) != data})
	}
	changed := []string{}
	for _, file := range files {
		if !file.changed {
			continue
		}
		if err := root.MkdirAll(filepath.Dir(file.name), 0755); err != nil {
			return changed, err
		}
		if err := writeBrainAgentFile(root, file); err != nil {
			return changed, fmt.Errorf("%s: %w", file.name, err)
		}
		changed = append(changed, file.name)
	}
	return changed, nil
}

// Resolve existing links, including instruction aliases, to repository-relative
// targets. All reads and writes remain rooted, even if a link changes later.
func resolveBrainAgentTarget(root *os.Root, abs, name string) (string, error) {
	pending := strings.Split(filepath.ToSlash(name), "/")
	resolved := ""
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if resolved == "" {
				return "", fmt.Errorf("path escapes the project")
			}
			resolved = filepath.Dir(resolved)
			if resolved == "." {
				resolved = ""
			}
			continue
		}
		candidate := filepath.Join(resolved, part)
		info, err := root.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			// Only the .entire directory and final files may be created.
			if len(pending) > 0 && candidate != ".entire" {
				return "", err
			}
			resolved = candidate
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", fmt.Errorf("too many symbolic links")
			}
			target, err := root.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				target, err = filepath.Rel(abs, target)
				if err != nil || !filepath.IsLocal(target) {
					return "", fmt.Errorf("link escapes the project")
				}
				resolved = ""
			}
			pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
			continue
		}
		if len(pending) > 0 && !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", candidate)
		}
		resolved = candidate
	}
	if !filepath.IsLocal(resolved) {
		return "", fmt.Errorf("path escapes the project")
	}
	return resolved, nil
}

func readBrainInstruction(root *os.Root, name string) ([]byte, os.FileMode, error) {
	// Check type before opening so a FIFO cannot block the installer.
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("expected a regular file")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("expected a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, brainInstructionLimit+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > brainInstructionLimit {
		return nil, 0, fmt.Errorf("instruction file exceeds 4 MiB")
	}
	return data, info.Mode().Perm(), nil
}

func upsertBrainAgentPointer(text, pointer string) (string, error) {
	begins, ends := strings.Count(text, brainAgentBegin), strings.Count(text, brainAgentEnd)
	if begins == 0 && ends == 0 {
		sep := ""
		if text != "" {
			if !strings.HasSuffix(text, "\n") {
				sep = "\n"
			}
			sep += "\n"
		}
		return text + sep + pointer, nil
	}
	begin, end := strings.Index(text, brainAgentBegin), strings.Index(text, brainAgentEnd)
	wholeLine := func(index int, marker string) bool {
		if index < 0 || (index > 0 && text[index-1] != '\n') {
			return false
		}
		rest := text[index+len(marker):]
		return rest == "" || strings.HasPrefix(rest, "\n") || strings.HasPrefix(rest, "\r\n")
	}
	if begins != 1 || ends != 1 || begin >= end || !wholeLine(begin, brainAgentBegin) || !wholeLine(end, brainAgentEnd) {
		return "", fmt.Errorf("malformed Entire Brain markers; back up the file and repair the marked block before rerunning init-agents")
	}
	end += len(brainAgentEnd)
	if strings.HasPrefix(text[end:], "\r\n") {
		end += 2
	} else if strings.HasPrefix(text[end:], "\n") {
		end++
	}
	return text[:begin] + pointer + text[end:], nil
}

// Replacing directory entries atomically avoids truncating user text and avoids
// writing through hard links into files outside the project.
func writeBrainAgentFile(root *os.Root, file brainAgentFile) error {
	temp := filepath.Join(filepath.Dir(file.name), ".brain-agent-"+rand.Text()+".tmp")
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.mode)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if err = f.Chmod(file.mode); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(file.data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(temp, file.name)
}
