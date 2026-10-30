package factstore

import "sort"

// StorageStatus describes the current store after reconciling legacy changes
// in memory. Reading status never migrates or writes the store.
type StorageStatus struct {
	SchemaVersion int    `json:"schema_version"`
	Format        string `json:"format"`
	Compatibility bool   `json:"legacy_compatibility"`
	Records       int    `json:"records"`
	Views         int    `json:"views"`
}

func RepositoryStatus(brainDir string) (StorageStatus, error) {
	_, native, err := readRepoFactStore(brainDir)
	if err != nil {
		return StorageStatus{}, err
	}
	s, err := reconcileRepoFactStore(brainDir)
	if err != nil {
		return StorageStatus{}, err
	}
	if err := s.compact(); err != nil {
		return StorageStatus{}, err
	}
	format := "legacy"
	if native {
		format = "repository"
	}
	return StorageStatus{1, format, true, len(s.Records), len(s.Views)}, nil
}

// RepositoryViewDirs lists native view directories even when their legacy
// projections are missing. The boolean indicates whether a snapshot exists.
func RepositoryViewDirs(brainDir string) ([]string, bool, error) {
	s, native, err := readRepoFactStore(brainDir)
	if err != nil {
		return nil, native, err
	}
	dirs := make([]string, 0, len(s.Views))
	for dir := range s.Views {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, native, nil
}

// CleanupRepositoryTemps removes abandoned snapshots under the store lock.
// Privacy callers retain the outer brain write lock over the whole purge.
func CleanupRepositoryTemps(brainDir string) error {
	return withRepoFactStoreLock(brainDir, func() error { return cleanupRepoFactTemps(brainDir) })
}
