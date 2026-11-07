package factstore

// StorageStatus describes the current store after folding legacy sources in
// memory. Reading status never migrates or writes the store. Views counts the
// legacy branch dirs still awaiting migration (the JSON key predates the flat
// store and is kept for its readers).
type StorageStatus struct {
	SchemaVersion int            `json:"schema_version"`
	Format        string         `json:"format"`
	Compatibility bool           `json:"legacy_compatibility"`
	Records       int            `json:"records"`
	Views         int            `json:"views"`
	Cleanup       *CleanupStatus `json:"cleanup,omitempty"`
}

func RepositoryStatus(brainDir string) (StorageStatus, error) {
	combined, err := loadCombined(brainDir)
	if err != nil {
		return StorageStatus{}, err
	}
	format := "repository"
	if combined.storeVersion != repoFactsVersion && (combined.storePresent || len(combined.legacyDirs) > 0) {
		format = "legacy"
	} else if len(combined.legacyDirs) > 0 {
		format = "compatibility"
	}
	version := combined.storeVersion
	if !combined.storePresent {
		version = repoFactsVersion
	}
	report := StorageStatus{SchemaVersion: version, Format: format, Compatibility: len(combined.legacyDirs) > 0, Records: len(combined.byID), Views: len(combined.legacyDirs)}
	cleanup := readCleanupStatus(brainDir)
	dirs, err := legacyCleanupDirs(brainDir)
	if err != nil {
		return StorageStatus{}, err
	}
	cleanup.PendingDirs = len(dirs)
	if len(dirs) > 0 && (cleanup.State == "" || cleanup.State == "complete") {
		cleanup.State = "pending"
	}
	if cleanup.State != "" {
		report.Cleanup = &cleanup
	}
	return report, nil
}

// CleanupRepositoryTemps removes abandoned snapshots under the store lock.
// Privacy callers retain the outer brain write lock over the whole purge.
func CleanupRepositoryTemps(brainDir string) error {
	return withRepoFactStoreLock(brainDir, func() error { return cleanupRepoFactTemps(brainDir) })
}
