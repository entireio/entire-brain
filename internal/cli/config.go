package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ashtom/entire-brain/internal/config"
	"github.com/spf13/cobra"
)

// configReport is what `config show` prints.
//
// It reports the configuration that actually governs this plugin's behaviour,
// resolved: where the store lives, which repository is bound, what brain.json
// persists, and the two operator switches this binary reads at runtime. It used
// to print a scaffold `{"greeting": "Hello from Entire Brain"}` -- the whole of
// the Config struct at the time -- which `scripts/install.sh` writes as step 3
// of a documented install, so the placeholder shipped.
//
// Every field is backed by something this binary reads. Nothing here is a
// setting invented for the report. The optional ENTIRE_BRAIN_* tuning variables
// documented in the README are deliberately absent: they are read at their point
// of use and several resolve only by probing a server, which a read-only
// reporting command must not do.
type configReport struct {
	// ConfigFile is brain.json: the only file this plugin persists
	// configuration to. Reported whether or not it exists, because "not written
	// yet" is the answer a reader needs when a setting did not take effect.
	ConfigFile       string `json:"config_file"`
	ConfigFileExists bool   `json:"config_file_exists"`
	// Directories are the resolved plugin directories (ENTIRE_PLUGIN_*_DIR, else
	// XDG, else the home fallback). `doctor` reports the raw variables and the
	// directories' health; this reports what they resolved to.
	Directories configDirectoriesReport `json:"directories"`
	// BrainStore is the root every repository's brain directory hangs off.
	BrainStore string `json:"brain_store"`
	// RepoRoot is the repository the host CLI bound this invocation to
	// (ENTIRE_REPO_ROOT). Empty when the binary was run standalone.
	RepoRoot string `json:"repo_root,omitempty"`
	// DomainSlugs is the persisted content of brain.json. Always emitted, even
	// when empty, so the shape of this report does not change under a caller.
	DomainSlugs map[string]string `json:"domain_slugs"`
	// NoEgress is the master local-only switch (ENTIRE_BRAIN_NO_EGRESS /
	// ENTIRE_BRAIN_LOCAL_ONLY) that hosted publish, fact sync and non-local
	// agents fail closed against.
	NoEgress bool `json:"no_egress"`
	// MCPGraphBinary is the semantic-provider binary the MCP server resolves
	// from its own environment (ENTIRE_BRAIN_GRAPH_BINARY, else "entire"). It is
	// named for the surface that reads it: the plain CLI takes --graph-binary.
	MCPGraphBinary string `json:"mcp_graph_binary"`
}

type configDirectoriesReport struct {
	Config string `json:"config"`
	Data   string `json:"data"`
	State  string `json:"state"`
	Cache  string `json:"cache"`
}

func newConfigCommand(env EntireEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect or initialize plugin configuration",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the plugin config path",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := configPath(env)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the effective plugin configuration as JSON",
		Long: `Show prints the configuration that governs this plugin, resolved: the
config file and whether it exists, the plugin directories, the brain store root,
the bound repository, the host slugs brain.json persists, and the operator
switches this binary reads (the local-only egress gate and the MCP provider
binary).

The optional ENTIRE_BRAIN_* tuning variables are documented in the README; they
are read where they are used and are not reported here.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := buildConfigReport(env)
			if err != nil {
				return err
			}
			encoded, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Write the default plugin configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir, err := requireConfigDir(env)
			if err != nil {
				return err
			}
			if err := config.Save(configDir, config.Default()); err != nil {
				return err
			}
			path, err := config.Path(configDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			return nil
		},
	})

	return cmd
}

func buildConfigReport(env EntireEnv) (configReport, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return configReport{}, err
	}
	path, err := config.Path(dirs.Config)
	if err != nil {
		return configReport{}, err
	}
	// Lstat, not Stat: "the file is there" must not depend on a symlink's
	// target resolving, and this report never follows one.
	_, statErr := os.Lstat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return configReport{}, fmt.Errorf("inspect config file %s: %w", path, statErr)
	}
	cfg, err := config.Load(dirs.Config)
	if err != nil {
		return configReport{}, err
	}
	slugs := cfg.DomainSlugs
	if slugs == nil {
		slugs = map[string]string{}
	}
	return configReport{
		ConfigFile:       path,
		ConfigFileExists: statErr == nil,
		Directories: configDirectoriesReport{
			Config: dirs.Config,
			Data:   dirs.Data,
			State:  dirs.State,
			Cache:  dirs.Cache,
		},
		BrainStore:     filepath.Join(dirs.Data, repoStoreDirName),
		RepoRoot:       env.RepoRoot,
		DomainSlugs:    slugs,
		NoEgress:       brainNoEgressMode(),
		MCPGraphBinary: mcpGraphBinary(),
	}, nil
}

func configPath(env EntireEnv) (string, error) {
	configDir, err := requireConfigDir(env)
	if err != nil {
		return "", err
	}
	return config.Path(configDir)
}

func requireConfigDir(env EntireEnv) (string, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	if dirs.Config == "" {
		return "", errors.New("plugin config dir is empty")
	}
	return dirs.Config, nil
}
