package cli

import (
	"os"

	"github.com/entireio/entire-brain/internal/cli/entities"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// entityConfig binds the existing storage/privacy policies at the assembly
// boundary. Feature packages never import cli, and no mutable hook is copied.
func entityConfig(opts Options) entities.Config {
	return entities.Config{Options: opts, Deps: entities.Deps{
		RepoStoragePaths:       repoStoragePaths,
		GitmetaDirForKey:       gitmetaDirForKey,
		SessionIDsByCheckpoint: sessionIDsByCheckpoint,
		SessionBranchByID:      sessionBranchByID,
		SaveIndexCache:         saveEntityIndexCache,
		SaveIndexCacheContext:  saveEntityIndexCacheContext,
		BrainInvocation:        func(verb string) string { return base.BrainInvocation(os.LookupEnv, verb) },
	}}
}
