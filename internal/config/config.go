package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	Greeting    string            `json:"greeting"`
	DomainSlugs map[string]string `json:"domain_slugs,omitempty"`
}

func Default() Config {
	return Config{Greeting: "Hello from Entire Brain"}
}

func Path(configDir string) (string, error) {
	if configDir == "" {
		return "", errors.New("plugin config dir is empty")
	}
	return filepath.Join(configDir, "brain.json"), nil
}

func Load(configDir string) (Config, error) {
	path, err := Path(configDir)
	if err != nil {
		return Config{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Greeting == "" {
		cfg.Greeting = Default().Greeting
	}
	return cfg, nil
}

func Save(configDir string, cfg Config) error {
	path, err := Path(configDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
