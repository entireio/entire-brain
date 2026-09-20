package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathCustomHostDoesNotAllocateOrRewriteConfig(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, state := range []string{"missing", "bound", "corrupt"} {
			t.Run(map[bool]string{false: "url/", true: "local/"}[local]+state, func(t *testing.T) {
				root := t.TempDir()
				configDir := filepath.Join(root, "config")
				repo := t.TempDir()
				body := ""
				if state == "bound" {
					body = `{"domain_slugs":{"custom.invalid":"old"}}`
				} else if state == "corrupt" {
					body = "{"
				}
				if body != "" {
					if err := os.Mkdir(configDir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(configDir, "brain.json"), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				env := EntireEnv{PluginConfigDir: configDir, PluginDataDir: filepath.Join(root, "data"), PluginStateDir: filepath.Join(root, "state"), PluginCacheDir: filepath.Join(root, "cache")}
				runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repo}, fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "https://custom.invalid/org/repo"}}}
				target := "https://custom.invalid/org/repo"
				if local {
					target = repo
				}
				output, err := execute(t, NewRootCommand(Options{Env: env, Runner: runner}), "path", target)
				if state == "bound" {
					if err != nil || !strings.Contains(output, filepath.Join("old", "org", "repo")) {
						t.Errorf("established binding lost: %q %v", output, err)
					}
				} else if err == nil {
					t.Errorf("unresolved host should require explicit allocation: %q", output)
				}
				if state == "missing" {
					if _, err := os.Stat(configDir); !os.IsNotExist(err) {
						t.Errorf("getter created config: %v", err)
					}
				} else {
					data, err := os.ReadFile(filepath.Join(configDir, "brain.json"))
					if err != nil || string(data) != body {
						t.Errorf("getter changed config: %q %v", data, err)
					}
					entries, err := os.ReadDir(configDir)
					if err != nil || len(entries) != 1 {
						t.Errorf("getter wrote config side files: %v %v", entries, err)
					}
				}
			})
		}
	}
}
