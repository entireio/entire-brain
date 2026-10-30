package base

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// backgroundPausedFile is machine-level, beside the watch plan, because the
// thing it pauses — the one watcher and every worker it and the hooks launch —
// is machine-level too.
const backgroundPausedFile = "paused"

func backgroundPausedPath(env EntireEnv) (string, error) {
	dirs, err := ResolvePluginDirs(env)
	if err != nil {
		return "", fmt.Errorf("resolve plugin state dir: %w", err)
	}
	return filepath.Join(dirs.State, backgroundPausedFile), nil
}

// BackgroundPaused fails open: a state dir that cannot be resolved or read
// means "not paused", because a brain that silently stops updating is worse
// than one that keeps going after a pause it could not see.
func BackgroundPaused(env EntireEnv) bool {
	path, err := backgroundPausedPath(env)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func SetBackgroundPaused(env EntireEnv, paused bool) error {
	path, err := backgroundPausedPath(env)
	if err != nil {
		return err
	}
	if !paused {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}
