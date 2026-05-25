package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ashtom/entire-brain/internal/config"
	"github.com/spf13/cobra"
)

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
		Short: "Print plugin configuration as JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir, err := requireConfigDir(env)
			if err != nil {
				return err
			}
			cfg, err := config.Load(configDir)
			if err != nil {
				return err
			}
			encoded, err := json.MarshalIndent(cfg, "", "  ")
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
