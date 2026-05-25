package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/entireio/entire-plugin-template/internal/config"
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
			dataDir, err := requireDataDir(env)
			if err != nil {
				return err
			}
			cfg, err := config.Load(dataDir)
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
			dataDir, err := requireDataDir(env)
			if err != nil {
				return err
			}
			if err := config.Save(dataDir, config.Default()); err != nil {
				return err
			}
			path, err := config.Path(dataDir)
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
	dataDir, err := requireDataDir(env)
	if err != nil {
		return "", err
	}
	return config.Path(dataDir)
}

func requireDataDir(env EntireEnv) (string, error) {
	if env.PluginDataDir == "" {
		return "", errors.New("ENTIRE_PLUGIN_DATA_DIR is unset; run through `entire plugin-template` or set it for local testing")
	}
	return env.PluginDataDir, nil
}
