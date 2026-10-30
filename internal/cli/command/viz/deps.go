// Package viz hosts the `entire brain viz` web-interface command as it moves
// out of package cli (COR-2268). The remaining cli-bound seams — repo/target
// resolution, the brain status manifest, the unified retrieval engine, and
// the retrieval-response transport commit — are injected through Deps;
// everything else comes from bmanifest, brainstore, factstore, historystore,
// docstore, semstore, and retrieval directly.
package viz

import (
	"context"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

// Deps are the cli-bound seams the viz command still needs. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// AgentSurfaceTarget resolves the repository path argument for a command.
	AgentSurfaceTarget func(opts base.Options, args []string) string
	// ResolveFactsTarget resolves the repo, brain directory, and the branch
	// the brain is scoped to (the live git branch unless overridden).
	ResolveFactsTarget func(ctx context.Context, opts base.Options, target, branchOverride string) (repoDir, brainDir, branch string, err error)
	// BrainManifest returns the target's brain manifest from the full status
	// report (nil when no brain is built yet).
	BrainManifest func(ctx context.Context, opts base.Options, target string) (*bmanifest.Manifest, error)
	// RetrieveUnified is the unified retrieval engine bound to the cli-side
	// deps; same code path as `entire brain search`.
	RetrieveUnified func(repoDir, brainDir, branch, query string, limit int, mode retrieval.Mode) ([]retrieval.Result, error)
	// CommitRetrievalResponse revalidates the captured privacy policies and
	// runs write under the Brain write lock, binding the cli transport race
	// seams around it.
	CommitRetrievalResponse func(policies []brainstore.RetrievalPrivacyPolicy, write func() error) error
}

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}
