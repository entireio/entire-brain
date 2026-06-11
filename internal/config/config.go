package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	return loadPath(path)
}

func loadPath(path string) (Config, error) {
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
	unlock, err := acquireConfigLock(configDir)
	if err != nil {
		return err
	}
	defer unlock()
	return savePath(path, cfg)
}

func Update(configDir string, fn func(*Config) error) (Config, error) {
	path, err := Path(configDir)
	if err != nil {
		return Config{}, err
	}
	unlock, err := acquireConfigLock(configDir)
	if err != nil {
		return Config{}, err
	}
	defer unlock()

	cfg, err := loadPath(path)
	if err != nil {
		return Config{}, err
	}
	if err := fn(&cfg); err != nil {
		return Config{}, err
	}
	if err := savePath(path, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func savePath(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if err := writeConfigFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func acquireConfigLock(configDir string) (func(), error) {
	if configDir == "" {
		return nil, errors.New("plugin config dir is empty")
	}
	lockPath := filepath.Join(configDir, "locks", "config.lock")
	lock, err := acquireFileLock(lockPath, "config_locked", 10*time.Second)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}
