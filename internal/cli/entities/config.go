package entities

import (
	"context"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// Config supplies the shared command options and the assembly package's storage
// and privacy operations. Dependencies are per invocation, never global hooks.
type Config struct {
	base.Options
	Deps Deps
}

// Deps keeps the entity index independent of command assembly, manifest loading,
// and checkpoint/privacy policy. The assembly package supplies these operations
// so entity reads use the same privacy guards and local-only checkpoint policy
// as the other command surfaces.
type Deps struct {
	RepoStoragePaths       func(context.Context, base.CommandRunner, base.EntireEnv, string) (base.RepoStorage, error)
	GitmetaDirForKey       func(base.EntireEnv, string) (string, error)
	SessionIDsByCheckpoint func(context.Context, base.Options, string, string) (map[string][]string, error)
	SessionBranchByID      func(context.Context, base.Options, string) map[string]string
	SaveIndexCache         func(string, IndexCache) error
	SaveIndexCacheContext  func(context.Context, string, IndexCache) error
	BrainInvocation        func(string) string
}
