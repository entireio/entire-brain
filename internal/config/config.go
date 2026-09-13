package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config is the plugin's persisted configuration: brain.json, and nothing else.
//
// It holds exactly one setting, because exactly one setting is persisted. It
// used to also carry a scaffold `Greeting` that nothing in this program ever
// read, so `config show` reported "Hello from Entire Brain" as if that were the
// plugin's configuration. A field no code consults is not configuration.
type Config struct {
	// DomainSlugs maps a git host to the segment its repositories are stored
	// under in the brain store (github.com -> "gh" and the other well-known
	// hosts are built in; an unknown host is assigned a slug on first sight and
	// recorded here so a repository's identity, and therefore its brain
	// directory, stays stable forever after).
	DomainSlugs map[string]string `json:"domain_slugs,omitempty"`
}

// Default is the configuration of a machine that has never been configured:
// no host slugs learned yet. `config init` writes it so the file and its lock
// exist before the first repository needs a slug recorded.
func Default() Config {
	return Config{}
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
		// A corrupt config must not wedge every command that needs it — repo-key
		// resolution loads it on the hot path, so a hard error there blocks
		// otherwise-unrelated work. Quarantine the bad file (preserved at
		// <path>.corrupt for inspection) and fall back to defaults; a later Save
		// rewrites a clean config.
		quarantinePath := path + ".corrupt"
		if renameErr := os.Rename(path, quarantinePath); renameErr == nil {
			fmt.Fprintf(os.Stderr, "warning: %s was not valid JSON (%v); moved aside to %s and using defaults\n", path, err, quarantinePath)
		} else {
			fmt.Fprintf(os.Stderr, "warning: %s was not valid JSON (%v); using defaults\n", path, err)
		}
		return Default(), nil
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
