package cli

import (
	"context"

	"github.com/entireio/entire-brain/internal/cli/command/repoidentity"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// repoIdentityConfig binds the storage-set resolver at the assembly boundary.
// repo-identity is the one verb that reads the storage set WITHOUT the
// conflict refusal (a repair tool blocked by the condition it repairs is not a
// repair tool), so it gets the set resolver rather than repoStoragePaths.
func repoIdentityConfig(opts Options) repoidentity.Config {
	return repoidentity.Config{Options: opts, Deps: repoidentity.Deps{
		ResolveRepoStorageSet: func(ctx context.Context, runner base.CommandRunner, env base.EntireEnv, repoDir string) (repoidentity.StorageSet, error) {
			set, err := resolveRepoStorageSet(ctx, runner, env, repoDir)
			if err != nil {
				return repoidentity.StorageSet{}, err
			}
			return repoidentity.StorageSet{Canonical: set.Canonical, Populated: set.Populated}, nil
		},
	}}
}
